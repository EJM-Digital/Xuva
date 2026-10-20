package jobs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/EJM-Digital/Xuva/server/internal/resources"
)

type Job func(context.Context)

type Queue struct {
	Name      string                  `json:"name"`
	Class     resources.WorkloadClass `json:"class"`
	Workers   int                     `json:"workers"`
	MaxQueued int                     `json:"maxQueued"`
	jobs      chan Job
	started   sync.Once
	active    atomic.Int64
}

type Registry struct {
	Scan      *Queue
	Probe     *Queue
	Transcode *Queue
}

var ErrQueueSaturated = errors.New("queue is saturated")

func NewRegistry(manager *resources.Manager) *Registry {
	limits := manager.Limits()
	return &Registry{
		Scan:      NewQueue("scan", resources.Background, limits.ScanWorkers),
		Probe:     NewQueue("probe", resources.Background, limits.ProbeWorkers),
		Transcode: NewQueue("transcode", resources.PlaybackCritical, limits.TranscodeWorkers),
	}
}

func NewQueue(name string, class resources.WorkloadClass, workers int) *Queue {
	if workers < 1 {
		workers = 1
	}
	maxQueued := workers * 4
	if maxQueued < 4 {
		maxQueued = 4
	}
	return &Queue{
		Name:      name,
		Class:     class,
		Workers:   workers,
		MaxQueued: maxQueued,
		jobs:      make(chan Job, maxQueued),
	}
}

func (q *Queue) Submit(ctx context.Context, job Job) error {
	if job == nil {
		return errors.New("nil job")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	select {
	case q.jobs <- job:
		return nil
	default:
		return ErrQueueSaturated
	}
}

func (q *Queue) Start(ctx context.Context) {
	q.started.Do(func() {
		for i := 0; i < q.Workers; i++ {
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case job := <-q.jobs:
						q.active.Add(1)
						job(ctx)
						q.active.Add(-1)
					}
				}
			}()
		}
	})
}

func (r *Registry) Start(ctx context.Context) {
	r.Scan.Start(ctx)
	r.Probe.Start(ctx)
	r.Transcode.Start(ctx)
}

func (r *Registry) Snapshot() []map[string]any {
	queues := []*Queue{r.Scan, r.Probe, r.Transcode}
	output := make([]map[string]any, 0, len(queues))
	for _, queue := range queues {
		active := queue.active.Load()
		output = append(output, map[string]any{
			"name":              queue.Name,
			"class":             queue.Class,
			"workers":           queue.Workers,
			"maxQueued":         queue.MaxQueued,
			"queued":            len(queue.jobs),
			"active":            active,
			"workerUtilization": float64(active) / float64(queue.Workers),
			"queueUtilization":  float64(len(queue.jobs)) / float64(queue.MaxQueued),
		})
	}
	return output
}
