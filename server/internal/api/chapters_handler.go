package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/auth"
	"github.com/EJM-Digital/Xuva/server/internal/chapters"
)

func chaptersGetHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "media source id required")
			return
		}
		if deps.Chapters == nil {
			writeJSON(w, http.StatusOK, chapters.Chapters{MediaSourceID: id})
			return
		}
		ch, _, err := deps.Chapters.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read chapters")
			return
		}
		writeJSON(w, http.StatusOK, ch)
	}
}

func chaptersAnalyzeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "media source id required")
			return
		}
		if deps.Chapters == nil || deps.Catalog == nil {
			writeJSON(w, http.StatusOK, map[string]string{"status": "unavailable"})
			return
		}
		src, ok, err := deps.Catalog.GetMediaSource(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read media source")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}

		// Fire credits + season analysis in background. CRITICAL: use a
		// detached background context, NOT r.Context() — the request context
		// is cancelled the moment writeJSON returns, which would silently
		// abort the analysis a few hundred ms later (audit caught this).
		// A long-but-bounded timeout caps a runaway analysis without
		// dropping legitimate long runs (season analysis can take minutes
		// for a high-episode-count series).
		analysisCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		go func() {
			defer cancel()
			deps.Chapters.AnalyzeCredits(analysisCtx, id, src.Path, src.DurationSeconds)
		}()

		// Fire season intro analysis if this is a TV episode.
		seasonCtx, seasonCancel := context.WithTimeout(context.Background(), 30*time.Minute)
		go func() {
			defer seasonCancel()
			peers, err := deps.Catalog.GetSeasonPeers(seasonCtx, id)
			if err != nil || len(peers) < 2 {
				return
			}
			eps := make([]chapters.EpisodeInput, 0, len(peers))
			for _, p := range peers {
				eps = append(eps, chapters.EpisodeInput{
					MediaSourceID: p.MediaSourceID,
					Path:          p.Path,
					Duration:      p.Duration,
				})
			}
			deps.Chapters.AnalyzeSeason(seasonCtx, eps)
		}()

		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	}
}

func userPreferencesUpdateHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resolved, ok := auth.ResolvedSessionFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		var body auth.UserPreferencesPatch
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if deps.Auth == nil {
			writeJSON(w, http.StatusOK, body)
			return
		}
		preferences, err := deps.Auth.UpdateUserPreferences(r.Context(), resolved.Principal.ID, body)
		if err != nil {
			if errors.Is(err, auth.ErrInvalidPreferences) {
				writeError(w, http.StatusBadRequest, "posterSize must be S, M, or L")
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to save preferences")
			return
		}
		writeJSON(w, http.StatusOK, preferences)
	}
}
