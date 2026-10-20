package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/events"
)

// Notification is a persisted in-app notification surfaced in the bell drawer.
type Notification struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Message   string `json:"message,omitempty"`
	Link      string `json:"link,omitempty"`
	Dismissed bool   `json:"dismissed"`
	CreatedAt string `json:"createdAt"`
}

// Service subscribes to the event bus and persists interesting events as
// notifications, and provides methods to list and dismiss them.
type Service struct {
	db  *sql.DB
	bus *events.Bus
}

// NewService creates a new Service.
func NewService(db *sql.DB, bus *events.Bus) *Service {
	return &Service{db: db, bus: bus}
}

// Start launches the event listener in a background goroutine.
func (s *Service) Start(ctx context.Context) {
	go s.listen(ctx)
}

func (s *Service) listen(ctx context.Context) {
	ch, cancel := s.bus.Subscribe(ctx)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if n := eventToNotification(ev); n != nil {
				_ = s.insert(ctx, n)
				_ = s.trim(ctx, 100)
			}
		}
	}
}

func eventToNotification(ev events.Event) *Notification {
	var d map[string]any
	if b, err := json.Marshal(ev.Data); err == nil {
		_ = json.Unmarshal(b, &d)
	}

	switch ev.Type {
	case "scan.failed":
		libKind := kindLabel(strVal(d, "kind"))
		msg := strVal(d, "error")
		if msg == "" {
			msg = "An error occurred during the scan."
		}
		return &Notification{
			Kind:    "scan.failed",
			Title:   libKind + " scan failed",
			Message: msg,
			Link:    "/settings",
		}

	case "scan.completed":
		mediaFiles := int64(floatVal(d, "mediaFiles"))
		if mediaFiles == 0 {
			return nil
		}
		libKind := kindLabel(strVal(d, "kind"))
		return &Notification{
			Kind:    "scan.completed",
			Title:   libKind + " scan finished",
			Message: fmt.Sprintf("Found %d media file%s.", mediaFiles, plural(mediaFiles)),
		}

	default:
		return nil
	}
}

func (s *Service) insert(ctx context.Context, n *Notification) error {
	id := fmt.Sprintf("n-%d", time.Now().UnixNano())
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO notifications(id, kind, title, message, link, dismissed, created_at)
		 VALUES(?, ?, ?, ?, ?, 0, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`,
		id, n.Kind, n.Title, n.Message, n.Link,
	)
	return err
}

func (s *Service) trim(ctx context.Context, keep int) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM notifications WHERE id IN (
			SELECT id FROM notifications ORDER BY created_at DESC LIMIT -1 OFFSET ?
		)`, keep,
	)
	return err
}

// List returns undismissed notifications newest-first.
func (s *Service) List(ctx context.Context) ([]Notification, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, title, message, link, dismissed, created_at
		 FROM notifications
		 WHERE dismissed = 0
		 ORDER BY created_at DESC
		 LIMIT 50`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		var n Notification
		var dismissed int
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &n.Message, &n.Link, &dismissed, &n.CreatedAt); err != nil {
			return nil, err
		}
		n.Dismissed = dismissed != 0
		out = append(out, n)
	}
	return out, rows.Err()
}

// Dismiss marks a single notification as dismissed.
func (s *Service) Dismiss(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET dismissed = 1 WHERE id = ?`, id)
	return err
}

// DismissAll marks all notifications as dismissed.
func (s *Service) DismissAll(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET dismissed = 1`)
	return err
}

// ── helpers ──────────────────────────────────────────────────────────────────

func strVal(d map[string]any, key string) string {
	if d == nil {
		return ""
	}
	v, _ := d[key]
	s, _ := v.(string)
	return s
}

func floatVal(d map[string]any, key string) float64 {
	if d == nil {
		return 0
	}
	v, _ := d[key]
	f, _ := v.(float64)
	return f
}

func kindLabel(kind string) string {
	switch kind {
	case "movies":
		return "Movies"
	case "tv":
		return "TV"
	default:
		return "Library"
	}
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}
