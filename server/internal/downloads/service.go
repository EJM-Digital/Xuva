package downloads

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/events"
	"github.com/EJM-Digital/Xuva/server/internal/jobs"
	runtimestore "github.com/EJM-Digital/Xuva/server/internal/runtime"
)

type Status string

const (
	ProfileOriginal = "original"
	ProfileBalanced = "balanced"
	ProfileTravel   = "travel"

	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

type Request struct {
	MediaSourceID string `json:"mediaSourceId"`
	TargetProfile string `json:"targetProfile"`
	SourcePath    string `json:"sourcePath,omitempty"`
	SourceName    string `json:"sourceName,omitempty"`
	Acceleration  string `json:"acceleration,omitempty"`
	VideoEncoder  string `json:"videoEncoder,omitempty"`
}

type Job struct {
	ID            string    `json:"id"`
	MediaSourceID string    `json:"mediaSourceId"`
	TargetProfile string    `json:"targetProfile"`
	Status        Status    `json:"status"`
	SourcePath    string    `json:"sourcePath,omitempty"`
	OutputPath    string    `json:"outputPath,omitempty"`
	Command       []string  `json:"command,omitempty"`
	Acceleration  string    `json:"acceleration,omitempty"`
	VideoEncoder  string    `json:"videoEncoder,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	StartedAt     time.Time `json:"startedAt,omitempty"`
	CompletedAt   time.Time `json:"completedAt,omitempty"`
	Error         string    `json:"error,omitempty"`
}

type Service struct {
	events  *events.Bus
	runtime *runtimestore.Store
	queue   *jobs.Queue
	ffmpeg  string
	outDir  string
	nextID  atomic.Uint64
	mu      sync.RWMutex
	jobs    map[string]Job
}

func NewService(eventBus *events.Bus, queue *jobs.Queue, ffmpegPath string, outputDir string) *Service {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if outputDir == "" {
		outputDir = filepath.Join("data", "downloads")
	}
	return &Service{events: eventBus, queue: queue, ffmpeg: ffmpegPath, outDir: outputDir, jobs: map[string]Job{}}
}

func NewPersistentService(ctx context.Context, eventBus *events.Bus, queue *jobs.Queue, ffmpegPath string, outputDir string, store *runtimestore.Store) (*Service, error) {
	service := NewService(eventBus, queue, ffmpegPath, outputDir)
	service.runtime = store
	if err := service.recover(ctx); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *Service) Start(ctx context.Context, request Request) (Job, error) {
	if request.MediaSourceID == "" {
		return Job{}, errors.New("media source id is required")
	}
	if request.SourcePath == "" {
		return Job{}, errors.New("source path is required")
	}
	if request.TargetProfile == "" {
		request.TargetProfile = ProfileOriginal
	}
	if !validProfile(request.TargetProfile) {
		return Job{}, errors.New("unsupported download profile")
	}
	job := Job{
		ID:            s.nextJobID(),
		MediaSourceID: request.MediaSourceID,
		TargetProfile: request.TargetProfile,
		SourcePath:    request.SourcePath,
		Acceleration:  request.Acceleration,
		VideoEncoder:  request.VideoEncoder,
		Status:        StatusQueued,
		CreatedAt:     time.Now().UTC(),
	}
	job.OutputPath = s.outputPath(job, request.SourceName)
	job.Command = s.command(job)
	s.store(job)
	_ = s.persist(context.Background(), job)
	s.publish("download.queued", job)

	if request.TargetProfile == ProfileOriginal {
		job.Status = StatusCompleted
		job.StartedAt = time.Now().UTC()
		job.CompletedAt = job.StartedAt
		s.store(job)
		_ = s.persist(context.Background(), job)
		s.publish("download.completed", job)
		return job, nil
	}

	if err := s.queue.Submit(ctx, func(workerCtx context.Context) {
		job, _ := s.Get(job.ID)
		job.Status = StatusRunning
		job.StartedAt = time.Now().UTC()
		s.store(job)
		_ = s.persist(context.Background(), job)
		s.publish("download.running", job)
		if err := s.execute(workerCtx, job); err != nil {
			job.Status = StatusFailed
			job.Error = err.Error()
		} else {
			job.Status = StatusCompleted
		}
		job.CompletedAt = time.Now().UTC()
		s.store(job)
		_ = s.persist(context.Background(), job)
		s.publish("download.completed", job)
	}); err != nil {
		job.Status = StatusFailed
		job.Error = err.Error()
		job.CompletedAt = time.Now().UTC()
		s.store(job)
		_ = s.persist(context.Background(), job)
		return Job{}, err
	}
	return job, nil
}

func (s *Service) execute(ctx context.Context, job Job) error {
	if err := os.MkdirAll(filepath.Dir(job.OutputPath), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, s.ffmpeg, job.Command[1:]...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if len(output) > 0 {
			return errors.New(string(output))
		}
		return err
	}
	return nil
}

func (s *Service) command(job Job) []string {
	args := []string{s.ffmpeg, "-y", "-i", job.SourcePath, "-map", "0:v:0", "-map", "0:a?", "-map", "0:s?", "-movflags", "+faststart"}
	switch job.TargetProfile {
	case ProfileTravel:
		args = appendVideoEncoder(args, job, "3000k", "3500k", "7000k")
		args = append(args, "-vf", "scale='min(1280,iw)':-2", "-c:a", "aac", "-b:a", "160k", "-c:s", "mov_text")
	case ProfileBalanced:
		args = appendVideoEncoder(args, job, "8000k", "9000k", "18000k")
		args = append(args, "-vf", "scale='min(1920,iw)':-2", "-c:a", "aac", "-b:a", "384k", "-c:s", "mov_text")
	default:
		args = append(args, "-c", "copy")
	}
	return append(args, job.OutputPath)
}

func appendVideoEncoder(args []string, job Job, bitrate string, maxrate string, bufsize string) []string {
	if job.Acceleration == "hardware" && job.VideoEncoder != "" {
		return append(args, "-c:v", job.VideoEncoder, "-b:v", bitrate, "-maxrate", maxrate, "-bufsize", bufsize)
	}
	return append(args, "-c:v", "libx264", "-preset", "veryfast", "-b:v", bitrate, "-maxrate", maxrate, "-bufsize", bufsize)
}

func (s *Service) outputPath(job Job, sourceName string) string {
	base := sourceName
	if base == "" {
		base = filepath.Base(job.SourcePath)
	}
	ext := filepath.Ext(base)
	name := base[:len(base)-len(ext)]
	if name == "" {
		name = job.MediaSourceID
	}
	if job.TargetProfile == ProfileOriginal {
		return job.SourcePath
	}
	return filepath.Join(s.outDir, job.ID+"_"+safeName(name)+"_"+job.TargetProfile+".mp4")
}

func (s *Service) Get(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	return job, ok
}

func (s *Service) List() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	output := make([]Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		output = append(output, job)
	}
	sort.Slice(output, func(i, j int) bool { return output[i].CreatedAt.After(output[j].CreatedAt) })
	return output
}

func (s *Service) Cleanup(ctx context.Context, terminalRetention time.Duration) (int64, error) {
	if terminalRetention <= 0 {
		return 0, nil
	}
	return s.runtime.CleanupTerminal(ctx, "download", time.Now().UTC().Add(-terminalRetention), string(StatusCompleted), string(StatusFailed))
}

func validProfile(profile string) bool {
	return profile == ProfileOriginal || profile == ProfileBalanced || profile == ProfileTravel
}

func safeName(value string) string {
	output := make([]rune, 0, len(value))
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			output = append(output, char)
		} else if char == ' ' || char == '.' {
			output = append(output, '_')
		}
	}
	if len(output) == 0 {
		return "download"
	}
	return string(output)
}

func (s *Service) store(job Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
}

func (s *Service) publish(eventType string, job Job) {
	if s.events != nil {
		s.events.Publish(eventType, job)
	}
}

func (s *Service) recover(ctx context.Context) error {
	entities, err := s.runtime.List(ctx, "download")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, entity := range entities {
		var job Job
		if err := json.Unmarshal([]byte(entity.PayloadJSON), &job); err != nil {
			continue
		}
		switch job.Status {
		case StatusQueued, StatusRunning:
			job.Status = StatusFailed
			job.CompletedAt = now
			job.Error = "Download preparation was interrupted by server shutdown or restart."
			s.store(job)
			_ = s.persist(ctx, job)
		default:
			s.store(job)
		}
	}
	return nil
}

func (s *Service) persist(ctx context.Context, job Job) error {
	if s.runtime == nil {
		return nil
	}
	payload, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return s.runtime.Save(ctx, runtimestore.Entity{
		Type:        "download",
		ID:          job.ID,
		Status:      string(job.Status),
		PayloadJSON: string(payload),
		CreatedAt:   job.CreatedAt,
		UpdatedAt:   jobUpdatedAt(job),
		HeartbeatAt: jobUpdatedAt(job),
		CompletedAt: job.CompletedAt,
	})
}

func (s *Service) nextJobID() string {
	return "download_" + time.Now().UTC().Format("20060102T150405") + "_" + stringID(s.nextID.Add(1))
}

func jobUpdatedAt(job Job) time.Time {
	if !job.CompletedAt.IsZero() {
		return job.CompletedAt
	}
	if !job.StartedAt.IsZero() {
		return job.StartedAt
	}
	return job.CreatedAt
}

func stringID(value uint64) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[i:])
}
