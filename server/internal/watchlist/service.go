package watchlist

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/database"
)

const DefaultUserID = "local"

type Item struct {
	UserID     string   `json:"userId"`
	MediaID    string   `json:"mediaId"`
	Kind       string   `json:"kind"`
	Title      string   `json:"title"`
	Year       *int     `json:"year,omitempty"`
	PosterURL  string   `json:"posterUrl,omitempty"`
	BackdropURL string  `json:"backdropUrl,omitempty"`
	Genres     []string `json:"genres,omitempty"`
	AddedAt    string   `json:"addedAt"`
}

type AddRequest struct {
	MediaID     string   `json:"mediaId"`
	Kind        string   `json:"kind"`
	Title       string   `json:"title"`
	Year        *int     `json:"year,omitempty"`
	PosterURL   string   `json:"posterUrl,omitempty"`
	BackdropURL string   `json:"backdropUrl,omitempty"`
	Genres      []string `json:"genres,omitempty"`
}

type Service struct {
	db *sql.DB
}

func NewService(database *database.Service) *Service {
	return &Service{db: database.DB()}
}

func (s *Service) List(ctx context.Context, userID string) ([]Item, error) {
	if userID == "" {
		userID = DefaultUserID
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id, media_id, kind, title, year, poster_url, backdrop_url, genres_json, added_at
		FROM watchlist_items
		WHERE user_id = ?
		ORDER BY added_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Item
	for rows.Next() {
		var item Item
		var year sql.NullInt64
		var genresJSON sql.NullString
		if err := rows.Scan(&item.UserID, &item.MediaID, &item.Kind, &item.Title, &year, &item.PosterURL, &item.BackdropURL, &genresJSON, &item.AddedAt); err != nil {
			return nil, err
		}
		if year.Valid {
			y := int(year.Int64)
			item.Year = &y
		}
		if genresJSON.Valid && genresJSON.String != "" {
			_ = json.Unmarshal([]byte(genresJSON.String), &item.Genres)
		}
		out = append(out, item)
	}
	if out == nil {
		out = []Item{}
	}
	return out, rows.Err()
}

func (s *Service) Add(ctx context.Context, userID string, req AddRequest) (Item, error) {
	if userID == "" {
		userID = DefaultUserID
	}
	if strings.TrimSpace(req.MediaID) == "" {
		return Item{}, errors.New("mediaId is required")
	}
	if req.Kind != "movie" && req.Kind != "series" {
		return Item{}, errors.New("kind must be movie or series")
	}

	var genresJSON string
	if len(req.Genres) > 0 {
		b, _ := json.Marshal(req.Genres)
		genresJSON = string(b)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO watchlist_items(user_id, media_id, kind, title, year, poster_url, backdrop_url, genres_json, added_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, media_id, kind) DO NOTHING
	`, userID, req.MediaID, req.Kind, req.Title, yearPtr(req.Year), req.PosterURL, req.BackdropURL, genresJSON, now)
	if err != nil {
		return Item{}, err
	}

	item := Item{
		UserID:      userID,
		MediaID:     req.MediaID,
		Kind:        req.Kind,
		Title:       req.Title,
		Year:        req.Year,
		PosterURL:   req.PosterURL,
		BackdropURL: req.BackdropURL,
		Genres:      req.Genres,
		AddedAt:     now,
	}
	return item, nil
}

func (s *Service) Remove(ctx context.Context, userID, mediaID, kind string) error {
	if userID == "" {
		userID = DefaultUserID
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM watchlist_items WHERE user_id = ? AND media_id = ? AND kind = ?
	`, userID, mediaID, kind)
	return err
}

func yearPtr(y *int) any {
	if y == nil {
		return nil
	}
	return *y
}
