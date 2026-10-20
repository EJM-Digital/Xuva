package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/EJM-Digital/Xuva/server/internal/resources"
)

func TestRegistryUsesSeparateQueues(t *testing.T) {
	manager := resources.NewManager(resources.Limits{
		ScanWorkers:      1,
		ProbeWorkers:     2,
		TranscodeWorkers: 1,
	})
	registry := NewRegistry(manager)

	if registry.Scan == registry.Probe || registry.Scan == registry.Transcode || registry.Probe == registry.Transcode {
		t.Fatal("expected separate scan, probe, and transcode queues")
	}
	if registry.Scan.Class != resources.Background {
		t.Fatalf("expected scan queue to be background, got %q", registry.Scan.Class)
	}
	if registry.Probe.Class != resources.Background {
		t.Fatalf("expected probe queue to be background, got %q", registry.Probe.Class)
	}
	if registry.Transcode.Class != resources.PlaybackCritical {
		t.Fatalf("expected transcode queue to be playback critical, got %q", registry.Transcode.Class)
	}
}

func TestQueueNormalizesInvalidWorkerCount(t *testing.T) {
	queue := NewQueue("scan", resources.Background, 0)

	if queue.Workers != 1 {
		t.Fatalf("expected workers to normalize to 1, got %d", queue.Workers)
	}
	if queue.MaxQueued < 4 {
		t.Fatalf("expected max queued floor of 4, got %d", queue.MaxQueued)
	}
}

func TestQueueSubmitRejectsWhenSaturated(t *testing.T) {
	queue := NewQueue("scan", resources.Background, 1)
	ctx := context.Background()
	for i := 0; i < queue.MaxQueued; i++ {
		if err := queue.Submit(ctx, func(context.Context) {}); err != nil {
			t.Fatalf("fill queue failed at %d: %v", i, err)
		}
	}
	err := queue.Submit(ctx, func(context.Context) {})
	if !errors.Is(err, ErrQueueSaturated) {
		t.Fatalf("expected ErrQueueSaturated, got %v", err)
	}
}
