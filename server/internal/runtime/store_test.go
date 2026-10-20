package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/database"
)

func TestStoreCleanupTerminal(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	store := NewStore(db)
	old := time.Now().UTC().Add(-48 * time.Hour)
	recent := time.Now().UTC()
	if err := store.Save(ctx, Entity{Type: "session", ID: "old", Status: "stale", PayloadJSON: `{}`, CreatedAt: old, UpdatedAt: old, HeartbeatAt: old, CompletedAt: old}); err != nil {
		t.Fatalf("save old: %v", err)
	}
	if err := store.Save(ctx, Entity{Type: "session", ID: "recent", Status: "stale", PayloadJSON: `{}`, CreatedAt: recent, UpdatedAt: recent, HeartbeatAt: recent, CompletedAt: recent}); err != nil {
		t.Fatalf("save recent: %v", err)
	}

	deleted, err := store.CleanupTerminal(ctx, "session", time.Now().UTC().Add(-24*time.Hour), "stale", "stopped")
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected one deleted row, got %d", deleted)
	}
	entities, err := store.List(ctx, "session")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entities) != 1 || entities[0].ID != "recent" {
		t.Fatalf("expected recent entity to remain, got %#v", entities)
	}
}
