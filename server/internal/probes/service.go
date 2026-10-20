package probes

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/catalog"
	"github.com/EJM-Digital/Xuva/server/internal/events"
	"github.com/EJM-Digital/Xuva/server/internal/jobs"
	"github.com/EJM-Digital/Xuva/server/internal/probe"
	runtimestore "github.com/EJM-Digital/Xuva/server/internal/runtime"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

type Request struct {
	MediaSourceID string `json:"mediaSourceId,omitempty"`
	Limit         int    `json:"limit,omitempty"`
}

type Job struct {
	ID            string    `json:"id"`
	Status        Status    `json:"status"`
	MediaSourceID string    `json:"mediaSourceId,omitempty"`
	Limit         int       `json:"limit"`
	CreatedAt     time.Time `json:"createdAt"`
	StartedAt     time.Time `json:"startedAt,omitempty"`
	CompletedAt   time.Time `json:"completedAt,omitempty"`
	Total         int       `json:"total"`
	Completed     int       `json:"completed"`
	Failed        int       `json:"failed"`
	LastPath      string    `json:"lastPath,omitempty"`
	Error         string    `json:"error,omitempty"`
}

type Service struct {
	events  *events.Bus
	runtime *runtimestore.Store
	queue   *jobs.Queue
	catalog *catalog.Service
	probe   *probe.Service

	nextID atomic.Uint64
	mu     sync.RWMutex
	jobs   map[string]Job
}

func NewService(eventBus *events.Bus, queue *jobs.Queue, catalogService *catalog.Service, probeService *probe.Service) *Service {
	return &Service{events: eventBus, queue: queue, catalog: catalogService, probe: probeService, jobs: map[string]Job{}}
}

func NewPersistentService(ctx context.Context, eventBus *events.Bus, queue *jobs.Queue, catalogService *catalog.Service, probeService *probe.Service, store *runtimestore.Store) (*Service, error) {
	service := NewService(eventBus, queue, catalogService, probeService)
	service.runtime = store
	if err := service.recover(ctx); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *Service) Start(ctx context.Context, request Request) (Job, error) {
	// limit=0 means probe all unprobed files; negative is invalid.
	if request.Limit < 0 {
		request.Limit = 0
	}
	job := Job{ID: s.nextJobID(), Status: StatusQueued, MediaSourceID: request.MediaSourceID, Limit: request.Limit, CreatedAt: time.Now().UTC()}
	s.store(job)
	_ = s.persist(context.Background(), job)
	s.publish("probe.queued", job)
	if err := s.queue.Submit(ctx, func(workerCtx context.Context) { s.run(workerCtx, job.ID, request) }); err != nil {
		job.Status = StatusFailed
		job.Error = err.Error()
		job.CompletedAt = time.Now().UTC()
		s.store(job)
		_ = s.persist(context.Background(), job)
		s.publish("probe.failed", job)
		return Job{}, err
	}
	return job, nil
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

func (s *Service) run(ctx context.Context, id string, request Request) {
	job, _ := s.Get(id)
	job.Status = StatusRunning
	job.StartedAt = time.Now().UTC()
	s.store(job)
	_ = s.persist(context.Background(), job)
	s.publish("probe.started", job)

	var sources []catalog.MediaSourceItem
	var err error
	if request.MediaSourceID != "" {
		var source catalog.MediaSourceItem
		var ok bool
		source, ok, err = s.catalog.GetMediaSource(ctx, request.MediaSourceID)
		if ok {
			sources = []catalog.MediaSourceItem{source}
		}
	} else {
		sources, err = s.catalog.ListMediaSources(ctx, request.Limit, true)
	}
	if err != nil {
		s.fail(id, err)
		return
	}
	job.Total = len(sources)
	s.store(job)
	_ = s.persist(context.Background(), job)
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			s.fail(id, err)
			return
		}
		job, _ = s.Get(id)
		job.LastPath = source.RelPath
		s.store(job)
		_ = s.persist(context.Background(), job)
		result, err := s.probe.Probe(ctx, source.Path)
		if err != nil {
			job.Failed++
		} else {
			_ = s.catalog.SaveProbe(ctx, source.ID, catalog.ProbeResult{
				Container:       result.Container,
				DurationSeconds: result.DurationSeconds,
				Bitrate:         result.Bitrate,
				VideoCodec:      result.VideoCodec,
				VideoProfile:    result.VideoProfile,
				VideoLevel:      result.VideoLevel,
				VideoBitDepth:   result.VideoBitDepth,
				VideoFrameRate:  result.VideoFrameRate,
				PixelFormat:     result.PixelFormat,
				ColorPrimaries:  result.ColorPrimaries,
				ColorTransfer:   result.ColorTransfer,
				ColorSpace:      result.ColorSpace,
				HDRFormat:       result.HDRFormat,
				Width:           result.Width,
				Height:          result.Height,
				AudioStreams:    result.AudioStreams,
				SubtitleStreams: result.SubtitleStreams,
				RawJSON:         result.RawJSON,
			})
			job.Completed++
		}
		s.store(job)
		_ = s.persist(context.Background(), job)
		s.publish("probe.progress", job)
	}
	job.Status = StatusCompleted
	job.CompletedAt = time.Now().UTC()
	s.store(job)
	_ = s.persist(context.Background(), job)
	s.publish("probe.completed", job)
}

func (s *Service) fail(id string, err error) {
	job, ok := s.Get(id)
	if !ok {
		return
	}
	job.Status = StatusFailed
	job.Error = err.Error()
	job.CompletedAt = time.Now().UTC()
	s.store(job)
	_ = s.persist(context.Background(), job)
	s.publish("probe.failed", job)
}

func (s *Service) Cleanup(ctx context.Context, terminalRetention time.Duration) (int64, error) {
	if terminalRetention <= 0 {
		return 0, nil
	}
	return s.runtime.CleanupTerminal(ctx, "probe", time.Now().UTC().Add(-terminalRetention), string(StatusCompleted), string(StatusFailed))
}

func (s *Service) store(job Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
}

func (s *Service) publish(eventType string, job Job) { s.events.Publish(eventType, job) }

func (s *Service) recover(ctx context.Context) error {
	entities, err := s.runtime.List(ctx, "probe")
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
			job.Error = "Probe was interrupted by server shutdown or restart."
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
		Type:        "probe",
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
	return "probe_" + time.Now().UTC().Format("20060102T150405") + "_" + stringID(s.nextID.Add(1))
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
