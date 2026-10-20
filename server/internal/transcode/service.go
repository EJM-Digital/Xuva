package transcode

import (
	"bytes"
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

type Mode string
type Status string

const (
	ModeRemux     Mode = "remux"
	ModeTranscode Mode = "transcode"
	// ModeRemuxAudio copies the video stream as-is and re-encodes only the
	// selected audio track to AAC, repackaging into MP4 with faststart. This
	// is the fast path for files whose video plays directly in the client but
	// whose audio codec (DTS, TrueHD) isn't supported — same approach
	// Plex/Jellyfin take for web playback of HEVC+DTS Blu-ray remuxes.
	ModeRemuxAudio Mode = "remux-audio"

	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	// StatusStreaming means the job is still running but the first HLS segment
	// and manifest are on disk — the client can begin playback immediately.
	StatusStreaming  Status = "streaming"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusTimeout   Status = "failed-timeout"
	StatusCanceled  Status = "cancelled"
)

type Request struct {
	MediaSourceID   string `json:"mediaSourceId"`
	Mode            Mode   `json:"mode"`
	SourcePath      string `json:"sourcePath,omitempty"`
	AudioTrackIndex int    `json:"audioTrackIndex,omitempty"`
	Acceleration    string `json:"acceleration,omitempty"`
	VideoEncoder    string `json:"videoEncoder,omitempty"`
	TimeoutSeconds  int    `json:"timeoutSeconds,omitempty"`
	MaxAttempts     int    `json:"maxAttempts,omitempty"`
	// OutputFormat controls how the output is packaged. "hls" produces segmented
	// HLS (.ts segments + index.m3u8); the default (empty/"mp4") is a single MP4.
	OutputFormat string `json:"outputFormat,omitempty"`
}

type Job struct {
	ID              string    `json:"id"`
	MediaSourceID   string    `json:"mediaSourceId"`
	Mode            Mode      `json:"mode"`
	Status          Status    `json:"status"`
	SourcePath      string    `json:"sourcePath,omitempty"`
	OutputPath      string    `json:"outputPath,omitempty"`
	// OutputDir is set for HLS jobs and contains the directory where index.m3u8
	// and segment_NNNNN.ts files are written. OutputPath is index.m3u8 itself.
	OutputDir       string    `json:"outputDir,omitempty"`
	OutputFormat    string    `json:"outputFormat,omitempty"`
	AudioTrackIndex int       `json:"audioTrackIndex,omitempty"`
	Command         []string  `json:"command,omitempty"`
	Acceleration    string    `json:"acceleration,omitempty"`
	VideoEncoder    string    `json:"videoEncoder,omitempty"`
	Attempts        int       `json:"attempts"`
	MaxAttempts     int       `json:"maxAttempts"`
	Timeout         string    `json:"timeout,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	StartedAt       time.Time `json:"startedAt,omitempty"`
	CompletedAt     time.Time `json:"completedAt,omitempty"`
	Error           string    `json:"error,omitempty"`
	FailureClass    string    `json:"failureClass,omitempty"`
	ReasonCode      string    `json:"reasonCode,omitempty"`
	Remediation     string    `json:"remediation,omitempty"`
	// LastAccessedAt is bumped every time a client polls the playback route
	// and matches this job (FindActive hit). The reaper uses it to detect
	// abandoned transcodes: if no client has asked about a queued/running job
	// for ReaperIdleTimeout, the job is cancelled and ffmpeg is killed.
	// Without this, navigating away from the "Preparing…" panel leaves
	// ffmpeg burning CPU until the file completes — exactly the bug the
	// user reported with movie 1408.
	LastAccessedAt time.Time `json:"lastAccessedAt,omitempty"`
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
	cancels map[string]context.CancelFunc
}

func NewService(eventBus *events.Bus, queue *jobs.Queue, ffmpegPath string, outputDir string) *Service {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if outputDir == "" {
		outputDir = filepath.Join("data", "transcode")
	}
	return &Service{events: eventBus, queue: queue, ffmpeg: ffmpegPath, outDir: outputDir, jobs: map[string]Job{}, cancels: map[string]context.CancelFunc{}}
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
	if request.Mode == "" {
		request.Mode = ModeRemux
	}
	maxAttempts := request.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 2
	}
	now := time.Now().UTC()
	job := Job{ID: s.nextJobID(), MediaSourceID: request.MediaSourceID, Mode: request.Mode, SourcePath: request.SourcePath, AudioTrackIndex: request.AudioTrackIndex, Acceleration: request.Acceleration, VideoEncoder: request.VideoEncoder, OutputFormat: request.OutputFormat, Status: StatusQueued, CreatedAt: now, LastAccessedAt: now, MaxAttempts: maxAttempts}
	if request.TimeoutSeconds > 0 {
		job.Timeout = (time.Duration(request.TimeoutSeconds) * time.Second).String()
	}
	if job.OutputFormat == "hls" {
		dir := filepath.Join(s.outDir, job.ID)
		job.OutputDir = dir
		job.OutputPath = filepath.Join(dir, "index.m3u8")
	} else {
		job.OutputPath = filepath.Join(s.outDir, job.ID+".mp4")
	}
	job.Command = s.command(job)
	s.store(job)
	_ = s.persist(context.Background(), job)
	s.publish("transcode.queued", job)
	if err := s.queue.Submit(ctx, func(workerCtx context.Context) {
		job, _ := s.Get(job.ID)
		job.Status = StatusRunning
		job.StartedAt = time.Now().UTC()
		s.store(job)
		_ = s.persist(context.Background(), job)
		s.publish("transcode.started", job)
		ctx := workerCtx
		cancel := func() {}
		if request.TimeoutSeconds > 0 {
			ctx, cancel = context.WithTimeout(workerCtx, time.Duration(request.TimeoutSeconds)*time.Second)
		} else {
			ctx, cancel = context.WithCancel(workerCtx)
		}
		s.storeCancel(job.ID, cancel)
		defer cancel()
		defer s.clearCancel(job.ID)
		result := s.executeWithRetry(ctx, job)
		if result.Err != nil {
			job.Status = statusForFailure(result.Failure)
			job.Error = result.Err.Error()
			job.FailureClass = result.Failure.Class
			job.ReasonCode = result.Failure.ReasonCode
			job.Remediation = result.Failure.Remediation
			job.Attempts = result.Attempts
			_ = os.Remove(job.OutputPath)
		} else {
			job.Status = StatusCompleted
			job.Attempts = result.Attempts
		}
		job.CompletedAt = time.Now().UTC()
		s.store(job)
		_ = s.persist(context.Background(), job)
		if job.Status == StatusCompleted {
			s.publish("transcode.completed", job)
		} else {
			s.publish("transcode.failed", job)
		}
	}); err != nil {
		job.Status = StatusFailed
		job.CompletedAt = time.Now().UTC()
		job.FailureClass = "queue_rejected"
		job.ReasonCode = "transcode_queue_rejected"
		job.Error = err.Error()
		job.Remediation = "Retry after current playback work has capacity."
		s.store(job)
		_ = s.persist(context.Background(), job)
		return Job{}, err
	}
	return job, nil
}

type executeResult struct {
	Attempts int
	Failure  Failure
	Err      error
}

func (s *Service) executeWithRetry(ctx context.Context, job Job) executeResult {
	maxAttempts := job.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	var last Failure
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		job.Attempts = attempt
		s.store(job)
		_ = s.persist(context.Background(), job)
		err := s.execute(ctx, job)
		if err == nil {
			return executeResult{Attempts: attempt}
		}
		last = ClassifyFailure(err.Error(), ctx.Err())
		lastErr = err
		if !last.Retryable || attempt == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			last = ClassifyFailure("", ctx.Err())
			lastErr = ctx.Err()
			return executeResult{Attempts: attempt, Failure: last, Err: lastErr}
		case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
		}
	}
	return executeResult{Attempts: maxAttempts, Failure: last, Err: lastErr}
}

func (s *Service) execute(ctx context.Context, job Job) error {
	if job.SourcePath == "" {
		return errors.New("source path is required")
	}
	if job.OutputFormat == "hls" {
		return s.executeHLS(ctx, job)
	}
	if err := os.MkdirAll(filepath.Dir(job.OutputPath), 0o755); err != nil {
		return err
	}
	args := job.Command[1:]
	cmd := exec.CommandContext(ctx, s.ffmpeg, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(output) > 0 {
			return errors.New(string(output))
		}
		return err
	}
	return nil
}

// executeHLS starts FFmpeg for HLS segmented output, marks the job as
// StatusStreaming as soon as the first manifest appears (~4 s), then waits
// for FFmpeg to finish. The caller's worker goroutine remains blocked until
// completion; in the meantime the HTTP handler serves segments from disk.
func (s *Service) executeHLS(ctx context.Context, job Job) error {
	if err := os.MkdirAll(job.OutputDir, 0o755); err != nil {
		return err
	}
	args := job.Command[1:]
	cmd := exec.CommandContext(ctx, s.ffmpeg, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	// Poll until the manifest appears (first segment written after ~hls_time seconds).
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()

	streamingMarked := false
	for !streamingMarked {
		select {
		case <-tick.C:
			if _, err := os.Stat(job.OutputPath); err == nil {
				j, ok := s.Get(job.ID)
				if ok && (j.Status == StatusRunning) {
					j.Status = StatusStreaming
					s.store(j)
					_ = s.persist(context.Background(), j)
					s.publish("transcode.streaming", j)
				}
				streamingMarked = true
			}
		case <-deadline.C:
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return errors.New("timeout waiting for first HLS segment")
		case <-ctx.Done():
			// CommandContext will have killed the process; just wait for cleanup.
			_ = cmd.Wait()
			return ctx.Err()
		}
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if msg := stderr.String(); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

func (s *Service) Cancel(id string) (Job, bool) {
	job, ok := s.Get(id)
	if !ok {
		return Job{}, false
	}
	if job.Status != StatusQueued && job.Status != StatusRunning {
		return job, true
	}
	if cancel := s.cancelFor(id); cancel != nil {
		cancel()
	}
	job.Status = StatusCanceled
	job.CompletedAt = time.Now().UTC()
	failure := ClassifyFailure("", context.Canceled)
	job.FailureClass = failure.Class
	job.ReasonCode = failure.ReasonCode
	job.Remediation = failure.Remediation
	job.Error = failure.ReasonText
	if job.OutputDir != "" {
		_ = os.RemoveAll(job.OutputDir)
	} else {
		_ = os.Remove(job.OutputPath)
	}
	s.store(job)
	_ = s.persist(context.Background(), job)
	s.publish("transcode.cancelled", job)
	return job, true
}

func (s *Service) Cleanup(ctx context.Context, terminalRetention time.Duration) (int64, error) {
	if terminalRetention <= 0 {
		return 0, nil
	}
	return s.runtime.CleanupTerminal(ctx, "transcode", time.Now().UTC().Add(-terminalRetention), string(StatusCompleted), string(StatusFailed), string(StatusTimeout), string(StatusCanceled))
}

func (s *Service) command(job Job) []string {
	args := []string{s.ffmpeg, "-y", "-i", job.SourcePath}
	switch job.Mode {
	case ModeTranscode:
		args = append(args, "-map", "0:v:0")
		if job.AudioTrackIndex > 0 {
			args = append(args, "-map", "0:"+intString(job.AudioTrackIndex)+"?")
		} else {
			args = append(args, "-map", "0:a?")
		}
		if job.Acceleration == "hardware" && job.VideoEncoder != "" {
			args = append(args, "-c:v", job.VideoEncoder, "-b:v", "8000k", "-maxrate", "9000k", "-bufsize", "18000k")
		} else {
			args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "20")
		}
		args = append(args, "-c:a", "aac")
		if job.OutputFormat == "hls" {
			args = hlsOutputArgs(args, job)
		} else {
			args = append(args, "-movflags", "+faststart", job.OutputPath)
		}
	case ModeRemuxAudio:
		// Video copy, audio re-encode. Same audio mapping rules as transcode
		// (explicit track index when requested, first audio stream otherwise).
		args = append(args, "-map", "0:v:0")
		if job.AudioTrackIndex > 0 {
			args = append(args, "-map", "0:"+intString(job.AudioTrackIndex)+"?")
		} else {
			args = append(args, "-map", "0:a:0?")
		}
		args = append(args, "-c:v", "copy", "-c:a", "aac", "-ac", "2", "-b:a", "192k", "-movflags", "+faststart", job.OutputPath)
	default:
		args = append(args, "-map", "0", "-c", "copy", "-movflags", "+faststart", job.OutputPath)
	}
	return args
}

// hlsOutputArgs appends the HLS packaging flags to args, writing 4-second
// MPEG-TS segments and an event-type manifest that grows until FFmpeg exits.
func hlsOutputArgs(args []string, job Job) []string {
	return append(args,
		"-f", "hls",
		"-hls_time", "4",
		"-hls_playlist_type", "event",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", filepath.Join(job.OutputDir, "segment_%05d.ts"),
		job.OutputPath,
	)
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

func (s *Service) FindCompleted(mediaSourceID string, mode Mode, audioTrackIndex int) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, job := range s.jobs {
		if job.MediaSourceID == mediaSourceID && job.Mode == mode && job.AudioTrackIndex == audioTrackIndex && job.Status == StatusCompleted && job.OutputPath != "" {
			if _, err := os.Stat(job.OutputPath); err == nil {
				return job, true
			}
		}
	}
	return Job{}, false
}

func (s *Service) FindActive(mediaSourceID string, mode Mode, audioTrackIndex int) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		if job.MediaSourceID == mediaSourceID && job.Mode == mode && job.AudioTrackIndex == audioTrackIndex && (job.Status == StatusQueued || job.Status == StatusRunning || job.Status == StatusStreaming) {
			// Bump LastAccessedAt so the reaper knows a client is still polling
			// this job. If polling stops (browser closed, navigated away), the
			// reaper will pick up the staleness and cancel the orphan.
			job.LastAccessedAt = time.Now().UTC()
			s.jobs[job.ID] = job
			return job, true
		}
	}
	return Job{}, false
}

// FindAnyActiveTranscode returns the first queued, running, or streaming
// ModeTranscode job for the given media source, regardless of audio track.
// Used by the session enrichment path to tag sessions with the encoder label.
func (s *Service) FindAnyActiveTranscode(mediaSourceID string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, job := range s.jobs {
		if job.MediaSourceID == mediaSourceID && job.Mode == ModeTranscode &&
			(job.Status == StatusQueued || job.Status == StatusRunning || job.Status == StatusStreaming) {
			return job, true
		}
	}
	return Job{}, false
}

// ReapIdleJobs scans active jobs and cancels any whose LastAccessedAt is
// older than idleTimeout. Called periodically from the background reaper
// goroutine started by RunReaper. Returns the IDs of jobs cancelled so
// callers can log them.
//
// Rationale: when a user clicks Play on a file that needs transcoding,
// the route handler queues a job and the frontend polls the route every
// 6s waiting for the output URL. If the user navigates away mid-prepare
// (closes tab, hits Back) the frontend's onDestroy fires DELETE
// /api/work/{id} — but only if the unload completed cleanly. Tab crashes,
// network drops, devtools throttling, etc. can all skip that cleanup.
// The reaper is the safety net: no polling = no client = nothing for the
// output to be delivered to = cancel and free the CPU.
func (s *Service) ReapIdleJobs(idleTimeout time.Duration) []string {
	if idleTimeout <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().Add(-idleTimeout)
	s.mu.RLock()
	ids := make([]string, 0)
	for _, job := range s.jobs {
		if job.Status != StatusQueued && job.Status != StatusRunning && job.Status != StatusStreaming {
			continue
		}
		// Belt-and-braces: a freshly-queued job (LastAccessedAt zero or very
		// recent because CreatedAt set it) isn't reaped on its first tick.
		if job.LastAccessedAt.IsZero() || job.LastAccessedAt.After(cutoff) {
			continue
		}
		ids = append(ids, job.ID)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		s.Cancel(id)
	}
	return ids
}

// RunReaper starts a background loop that calls ReapIdleJobs on the given
// interval until ctx is cancelled. Call once at startup.
func (s *Service) RunReaper(ctx context.Context, interval, idleTimeout time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if cancelled := s.ReapIdleJobs(idleTimeout); len(cancelled) > 0 {
					// Publish so the dashboard's event stream can show the
					// "Cancelled abandoned transcode" notification, and so
					// future log inspectors can see the trail.
					if s.events != nil {
						s.events.Publish("transcode.reaped", map[string]any{
							"jobIds":        cancelled,
							"idleTimeoutMs": idleTimeout.Milliseconds(),
						})
					}
				}
			}
		}
	}()
}

func (s *Service) CancelActiveForMediaSource(mediaSourceID string) int {
	if mediaSourceID == "" {
		return 0
	}
	s.mu.RLock()
	ids := make([]string, 0, len(s.jobs))
	for _, job := range s.jobs {
		if job.MediaSourceID != mediaSourceID {
			continue
		}
		if job.Status == StatusQueued || job.Status == StatusRunning || job.Status == StatusStreaming {
			ids = append(ids, job.ID)
		}
	}
	s.mu.RUnlock()
	cancelled := 0
	for _, id := range ids {
		if _, ok := s.Cancel(id); ok {
			cancelled++
		}
	}
	return cancelled
}

func (s *Service) store(job Job) { s.mu.Lock(); defer s.mu.Unlock(); s.jobs[job.ID] = job }
func (s *Service) recover(ctx context.Context) error {
	entities, err := s.runtime.List(ctx, "transcode")
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
		case StatusQueued, StatusRunning, StatusStreaming:
			job.Status = StatusFailed
			job.CompletedAt = now
			job.FailureClass = "recovered_after_restart"
			job.ReasonCode = "transcode_recovered_after_restart"
			job.Remediation = "Start the conversion again if this playback still needs it."
			job.Error = "Transcode was interrupted by server shutdown or restart."
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
		Type:        "transcode",
		ID:          job.ID,
		Status:      string(job.Status),
		PayloadJSON: string(payload),
		CreatedAt:   job.CreatedAt,
		UpdatedAt:   updatedAt(job),
		HeartbeatAt: updatedAt(job),
		CompletedAt: job.CompletedAt,
	})
}
func (s *Service) storeCancel(id string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels[id] = cancel
}
func (s *Service) clearCancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancels, id)
}
func (s *Service) cancelFor(id string) context.CancelFunc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cancels[id]
}
func (s *Service) publish(eventType string, job Job) {
	if s.events != nil {
		s.events.Publish(eventType, job)
	}
}
func (s *Service) nextJobID() string {
	return "work_" + time.Now().UTC().Format("20060102T150405") + "_" + stringID(s.nextID.Add(1))
}

func updatedAt(job Job) time.Time {
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

func intString(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}
