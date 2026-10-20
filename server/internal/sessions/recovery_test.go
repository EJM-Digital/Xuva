package sessions

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/database"
	"github.com/EJM-Digital/Xuva/server/internal/events"
	runtimestore "github.com/EJM-Digital/Xuva/server/internal/runtime"
)

func TestPersistentServiceRecoversRecentSession(t *testing.T) {
	ctx := context.Background()
	store, closeDB := testRuntimeStore(t)
	defer closeDB()

	first, err := NewPersistentService(ctx, events.NewBus(8), store)
	if err != nil {
		t.Fatalf("create first service: %v", err)
	}
	session, err := first.Start(StartRequest{MediaSourceID: "source-1", Title: "Movie", ProgressSeconds: 12})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	second, err := NewPersistentService(ctx, events.NewBus(8), store)
	if err != nil {
		t.Fatalf("create second service: %v", err)
	}
	recovered, ok := second.Get(session.ID)
	if !ok {
		t.Fatal("expected recent session to recover after restart")
	}
	if recovered.Status != "playing" || recovered.Progress != 12 {
		t.Fatalf("unexpected recovered session: %#v", recovered)
	}
}

func TestPersistentServiceMarksExpiredSessionStale(t *testing.T) {
	ctx := context.Background()
	store, closeDB := testRuntimeStore(t)
	defer closeDB()

	old := time.Now().UTC().Add(-30 * time.Minute)
	session := Session{ID: "session-old", UserID: "local", DeviceID: "web", MediaSourceID: "source-1", Status: "playing", StartedAt: old, UpdatedAt: old}
	payload, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := store.Save(ctx, runtimestore.Entity{Type: "session", ID: session.ID, Status: session.Status, PayloadJSON: string(payload), CreatedAt: old, UpdatedAt: old, HeartbeatAt: old}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	service, err := NewPersistentService(ctx, events.NewBus(8), store)
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	if _, ok := service.Get(session.ID); ok {
		t.Fatal("expired session should not remain active after recovery")
	}
	entities, err := store.List(ctx, "session")
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(entities) != 1 || entities[0].Status != "stale" {
		t.Fatalf("expected persisted stale session, got %#v", entities)
	}
}

func testRuntimeStore(t *testing.T) (*runtimestore.Store, func()) {
	t.Helper()
	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return runtimestore.NewStore(db), func() { _ = db.Close() }
}
