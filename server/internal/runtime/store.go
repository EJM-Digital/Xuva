package runtime

import (
	"context"
	"database/sql"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/database"
)

type Entity struct {
	Type        string
	ID          string
	Status      string
	PayloadJSON string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	HeartbeatAt time.Time
	CompletedAt time.Time
}

type Store struct {
	db *sql.DB
}

func NewStore(databaseService *database.Service) *Store {
	if databaseService == nil {
		return nil
	}
	return &Store{db: databaseService.DB()}
}

func (s *Store) Save(ctx context.Context, entity Entity) error {
	if s == nil || s.db == nil {
		return nil
	}
	now := time.Now().UTC()
	if entity.CreatedAt.IsZero() {
		entity.CreatedAt = now
	}
	if entity.UpdatedAt.IsZero() {
		entity.UpdatedAt = now
	}
	if entity.HeartbeatAt.IsZero() {
		entity.HeartbeatAt = entity.UpdatedAt
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runtime_entities(entity_type, id, status, payload_json, created_at, updated_at, heartbeat_at, completed_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(entity_type, id) DO UPDATE SET
			status = excluded.status,
			payload_json = excluded.payload_json,
			updated_at = excluded.updated_at,
			heartbeat_at = excluded.heartbeat_at,
			completed_at = excluded.completed_at
	`, entity.Type, entity.ID, entity.Status, entity.PayloadJSON, formatTime(entity.CreatedAt), formatTime(entity.UpdatedAt), formatTime(entity.HeartbeatAt), formatTime(entity.CompletedAt))
	return err
}

func (s *Store) List(ctx context.Context, entityType string) ([]Entity, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT entity_type, id, status, payload_json, created_at, updated_at, heartbeat_at, completed_at
		FROM runtime_entities
		WHERE entity_type = ?
		ORDER BY updated_at DESC
	`, entityType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var output []Entity
	for rows.Next() {
		var entity Entity
		var createdAt, updatedAt, heartbeatAt, completedAt string
		if err := rows.Scan(&entity.Type, &entity.ID, &entity.Status, &entity.PayloadJSON, &createdAt, &updatedAt, &heartbeatAt, &completedAt); err != nil {
			return nil, err
		}
		entity.CreatedAt = parseTime(createdAt)
		entity.UpdatedAt = parseTime(updatedAt)
		entity.HeartbeatAt = parseTime(heartbeatAt)
		entity.CompletedAt = parseTime(completedAt)
		output = append(output, entity)
	}
	return output, rows.Err()
}

func (s *Store) Delete(ctx context.Context, entityType string, id string) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM runtime_entities WHERE entity_type = ? AND id = ?`, entityType, id)
	return err
}

func (s *Store) CleanupTerminal(ctx context.Context, entityType string, cutoff time.Time, terminalStatuses ...string) (int64, error) {
	if s == nil || s.db == nil || len(terminalStatuses) == 0 {
		return 0, nil
	}
	query := `DELETE FROM runtime_entities WHERE entity_type = ? AND updated_at < ? AND status IN (?`
	args := []any{entityType, formatTime(cutoff)}
	for i := 1; i < len(terminalStatuses); i++ {
		query += ", ?"
	}
	query += ")"
	for _, status := range terminalStatuses {
		args = append(args, status)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
