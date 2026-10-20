package playstate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/database"
	"github.com/EJM-Digital/Xuva/server/internal/events"
)

const DefaultUserID = "local"

type State struct {
	UserID          string  `json:"userId"`
	MediaSourceID   string  `json:"mediaSourceId"`
	Watched         bool    `json:"watched"`
	ProgressSeconds float64 `json:"progressSeconds"`
	DurationSeconds float64 `json:"durationSeconds"`
	Percent         float64 `json:"percent"`
	LastPlayedAt    string  `json:"lastPlayedAt"`
	UpdatedAt       string  `json:"updatedAt"`
}

type Update struct {
	UserID          string  `json:"userId"`
	Watched         *bool   `json:"watched,omitempty"`
	ProgressSeconds float64 `json:"progressSeconds"`
	DurationSeconds float64 `json:"durationSeconds"`
}

type RecentItem struct {
	State
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	RelPath string `json:"relPath"`
}

type Service struct {
	db     *sql.DB
	events *events.Bus
}

func NewService(database *database.Service, eventBus *events.Bus) *Service {
	return &Service{db: database.DB(), events: eventBus}
}

func (s *Service) Get(ctx context.Context, userID string, mediaSourceID string) (State, bool, error) {
	if userID == "" {
		userID = DefaultUserID
	}
	var state State
	var watched int
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, media_source_id, watched, progress_seconds, duration_seconds, last_played_at, updated_at
		FROM playback_states
		WHERE user_id = ? AND media_source_id = ?
	`, userID, mediaSourceID).Scan(&state.UserID, &state.MediaSourceID, &watched, &state.ProgressSeconds, &state.DurationSeconds, &state.LastPlayedAt, &state.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return State{UserID: userID, MediaSourceID: mediaSourceID}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	state.Watched = watched != 0
	state.Percent = percent(state.ProgressSeconds, state.DurationSeconds)
	return state, true, nil
}

// GetBatch fetches playback states for a set of media source IDs in a single
// query. Returns a map of mediaSourceID → State for entries that exist.
func (s *Service) GetBatch(ctx context.Context, userID string, mediaSourceIDs []string) (map[string]State, error) {
	if len(mediaSourceIDs) == 0 {
		return nil, nil
	}
	if userID == "" {
		userID = DefaultUserID
	}
	placeholders := make([]string, len(mediaSourceIDs))
	args := make([]any, 0, 1+len(mediaSourceIDs))
	args = append(args, userID)
	for i, id := range mediaSourceIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := `SELECT user_id, media_source_id, watched, progress_seconds, duration_seconds, last_played_at, updated_at
		FROM playback_states
		WHERE user_id = ? AND media_source_id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]State, len(mediaSourceIDs))
	for rows.Next() {
		var st State
		var watched int
		if err := rows.Scan(&st.UserID, &st.MediaSourceID, &watched, &st.ProgressSeconds, &st.DurationSeconds, &st.LastPlayedAt, &st.UpdatedAt); err != nil {
			return nil, err
		}
		st.Watched = watched != 0
		st.Percent = percent(st.ProgressSeconds, st.DurationSeconds)
		out[st.MediaSourceID] = st
	}
	return out, rows.Err()
}

func (s *Service) Set(ctx context.Context, mediaSourceID string, update Update) (State, error) {
	if mediaSourceID == "" {
		return State{}, errors.New("media source id is required")
	}
	if update.UserID == "" {
		update.UserID = DefaultUserID
	}
	watched := false
	if update.Watched != nil {
		watched = *update.Watched
	} else if update.DurationSeconds > 0 && update.ProgressSeconds >= update.DurationSeconds*0.9 {
		watched = true
	}
	now := timestamp(time.Now())
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO playback_states(user_id, media_source_id, watched, progress_seconds, duration_seconds, last_played_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, media_source_id) DO UPDATE SET
			watched = excluded.watched,
			progress_seconds = excluded.progress_seconds,
			duration_seconds = excluded.duration_seconds,
			last_played_at = excluded.last_played_at,
			updated_at = excluded.updated_at
	`, update.UserID, mediaSourceID, boolInt(watched), nonNegative(update.ProgressSeconds), nonNegative(update.DurationSeconds), now, now)
	if err != nil {
		return State{}, err
	}
	state, _, err := s.Get(ctx, update.UserID, mediaSourceID)
	if err == nil {
		s.events.Publish("playback.state.updated", state)
	}
	return state, err
}

func (s *Service) MarkWatched(ctx context.Context, mediaSourceID string, userID string, watched bool) (State, error) {
	duration := 0.0
	progress := 0.0
	if current, ok, err := s.Get(ctx, userID, mediaSourceID); err == nil && ok {
		duration = current.DurationSeconds
		if watched {
			progress = current.DurationSeconds
		} else {
			progress = current.ProgressSeconds
		}
	}
	return s.Set(ctx, mediaSourceID, Update{UserID: userID, Watched: &watched, ProgressSeconds: progress, DurationSeconds: duration})
}

func (s *Service) Recent(ctx context.Context, userID string, limit int) ([]RecentItem, error) {
	if userID == "" {
		userID = DefaultUserID
	}
	if limit <= 0 || limit > 100 {
		limit = 24
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT ps.user_id, ps.media_source_id, ps.watched, ps.progress_seconds, ps.duration_seconds, ps.last_played_at, ps.updated_at,
			ms.name, ms.kind, ms.rel_path
		FROM playback_states ps
		JOIN media_sources ms ON ms.id = ps.media_source_id
		WHERE ps.user_id = ?
		AND ps.progress_seconds > 0
		AND ps.watched = 0
		ORDER BY ps.last_played_at DESC
		LIMIT ?
	`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	output := []RecentItem{}
	for rows.Next() {
		var item RecentItem
		var watched int
		if err := rows.Scan(&item.UserID, &item.MediaSourceID, &watched, &item.ProgressSeconds, &item.DurationSeconds, &item.LastPlayedAt, &item.UpdatedAt, &item.Name, &item.Kind, &item.RelPath); err != nil {
			return nil, err
		}
		item.Watched = watched != 0
		item.Percent = percent(item.ProgressSeconds, item.DurationSeconds)
		output = append(output, item)
	}
	return output, rows.Err()
}

func nonNegative(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}

func percent(progress float64, duration float64) float64 {
	if duration <= 0 {
		return 0
	}
	value := progress / duration
	if value > 1 {
		return 1
	}
	if value < 0 {
		return 0
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func timestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
