package auth

import (
	"context"
	"testing"

	"github.com/EJM-Digital/Xuva/server/internal/database"
)

func TestBootstrapUserClaimsPlaceholderAdminRow(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	service := NewService(db, false)
	if _, err := service.BootstrapUser(ctx, BootstrapOptions{
		Username:    "owner",
		DisplayName: "Owner",
		Password:    "owner-password-123!",
	}); err != nil {
		t.Fatalf("bootstrap user: %v", err)
	}

	var adminCount int
	var username string
	var role string
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(username), ''), COALESCE(MAX(role), '') FROM users WHERE id = 'admin'`).Scan(&adminCount, &username, &role); err != nil {
		t.Fatalf("query admin row: %v", err)
	}
	if adminCount != 1 {
		t.Fatalf("expected exactly one admin row, got %d", adminCount)
	}
	if username != "owner" || role != "admin" {
		t.Fatalf("expected claimed admin row, got username=%q role=%q", username, role)
	}

	if _, _, _, err := service.Authenticate(ctx, "owner", "owner-password-123!", "127.0.0.1", "test-agent"); err != nil {
		t.Fatalf("authenticate bootstrap owner: %v", err)
	}
}

func TestDeleteUserPreventsLastAdmin(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	service := NewService(db, false)
	if _, err := service.BootstrapUser(ctx, BootstrapOptions{
		Username:    "owner",
		DisplayName: "Owner",
		Password:    "owner-password-123!",
	}); err != nil {
		t.Fatalf("bootstrap user: %v", err)
	}
	if err := service.DeleteUser(ctx, "admin"); err != ErrLastAdmin {
		t.Fatalf("expected ErrLastAdmin, got %v", err)
	}
}

func TestResolveSessionRemainsStable(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	service := NewService(db, false)
	if _, err := service.BootstrapUser(ctx, BootstrapOptions{
		Username:    "owner",
		DisplayName: "Owner",
		Password:    "owner-password-123!",
	}); err != nil {
		t.Fatalf("bootstrap user: %v", err)
	}

	_, _, token, err := service.Authenticate(ctx, "owner", "owner-password-123!", "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	for i := 0; i < 12; i++ {
		resolved, err := service.Resolve(ctx, token, "127.0.0.1", "test-agent")
		if err != nil {
			t.Fatalf("resolve failed: %v", err)
		}
		if resolved.Rotated {
			t.Fatalf("session rotated unexpectedly")
		}
		if resolved.Token != token {
			t.Fatalf("token changed unexpectedly")
		}
	}
}
