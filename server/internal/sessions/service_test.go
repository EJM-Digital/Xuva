package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/events"
)

func TestInspectorIncludesRouteReasonTracksAndLoad(t *testing.T) {
	service := NewService(events.NewBus(8))
	session, err := service.Start(StartRequest{
		MediaSourceID:  "source-1",
		DeviceID:       "web",
		ClientProfile:  "web",
		Route:          "direct",
		Mode:           "Direct Play",
		ReasonCode:     "direct_play_supported",
		ReasonText:     "Direct playback is available.",
		ServerImpact:   "Low impact",
		Bitrate:        8_000_000,
		SelectedTracks: map[string]string{"audio": "ENG AAC", "subtitles": "Off"},
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	inspector, ok := service.Inspector(session.ID)
	if !ok {
		t.Fatalf("expected inspector")
	}
	if inspector.Route != "direct" || inspector.Mode != "Direct Play" || inspector.ReasonCode != "direct_play_supported" {
		t.Fatalf("unexpected inspector route fields: %#v", inspector)
	}
	if inspector.SelectedTracks["audio"] != "ENG AAC" || inspector.ServerImpact != "Low impact" || inspector.Bitrate != 8_000_000 {
		t.Fatalf("unexpected inspector detail fields: %#v", inspector)
	}
}

func TestTrackChangesUpdateInspectorInNearRealTime(t *testing.T) {
	service := NewService(events.NewBus(8))
	session, err := service.Start(StartRequest{MediaSourceID: "source-1"})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	_, ok := service.Update(session.ID, UpdateRequest{SelectedTracks: map[string]string{"subtitles": "English SRT"}})
	if !ok {
		t.Fatalf("update session")
	}
	inspector, _ := service.Inspector(session.ID)
	if inspector.SelectedTracks["subtitles"] != "English SRT" {
		t.Fatalf("expected subtitle track update, got %#v", inspector)
	}
	if time.Since(inspector.UpdatedAt) > time.Second {
		t.Fatalf("expected near-real-time update, got %s", inspector.UpdatedAt)
	}
}

func TestRouteTransitionsAreVisible(t *testing.T) {
	bus := events.NewBus(8)
	service := NewService(bus)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, stop := bus.Subscribe(ctx)
	defer stop()
	session, err := service.Start(StartRequest{MediaSourceID: "source-1", Route: "direct", ReasonCode: "direct_play_supported"})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	_, ok := service.Update(session.ID, UpdateRequest{Route: "transcode", ReasonCode: "video_conversion_required", ReasonText: "Video conversion required."})
	if !ok {
		t.Fatalf("update session")
	}
	inspector, _ := service.Inspector(session.ID)
	if len(inspector.RouteHistory) != 1 {
		t.Fatalf("expected one route transition, got %#v", inspector.RouteHistory)
	}
	if inspector.RouteHistory[0].FromRoute != "direct" || inspector.RouteHistory[0].ToRoute != "transcode" {
		t.Fatalf("unexpected route transition: %#v", inspector.RouteHistory[0])
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-ch:
			if event.Type == "session.route.changed" {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for route change event")
		}
	}
}

func TestSlowSubscribersDoNotBlockSessionUpdates(t *testing.T) {
	bus := events.NewBus(1)
	service := NewService(bus)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, stop := bus.Subscribe(ctx)
	defer stop()
	session, err := service.Start(StartRequest{MediaSourceID: "source-1"})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			service.Update(session.ID, UpdateRequest{ProgressSeconds: float64(i)})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("session updates blocked behind slow SSE subscriber")
	}
}
