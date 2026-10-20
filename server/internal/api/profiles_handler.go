package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/EJM-Digital/Xuva/server/internal/auth"
)

// profilesListHandler — GET /api/profiles
//
// Returns the public-facing profile cards for all real users, suitable for
// rendering the "Who's Watching?" picker screen.  No credential or PIN data
// is included in the response; only display metadata and boolean flags
// indicating whether a PIN is required to enter/exit each profile.
func profilesListHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			writeError(w, http.StatusServiceUnavailable, "user accounts are not available")
			return
		}
		profiles, err := deps.Auth.ListProfiles(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not list profiles")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"profiles": profiles})
	}
}

// authSwitchProfileHandler — POST /api/auth/switch-profile
//
// Validates the necessary PINs and issues a profile session token that the
// client stores (e.g. in localStorage) and sends back on every subsequent
// request as the X-Profile-Token header.
//
// Request body:
//
//	{
//	  "profileUserId":     "user_abc",   // required
//	  "currentProfilePin": "1234",       // required only when leaving a restricted profile with a PIN
//	  "targetProfilePin":  "5678"        // required only when entering a non-restricted profile with an entry PIN
//	}
//
// Response (200):
//
//	{
//	  "profileToken": "<token>",
//	  "profile": { ...ProfileCard fields... }
//	}
func authSwitchProfileHandler(deps Deps) http.HandlerFunc {
	type request struct {
		ProfileUserID     string `json:"profileUserId"`
		CurrentProfilePin string `json:"currentProfilePin"`
		TargetProfilePin  string `json:"targetProfilePin"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			writeError(w, http.StatusServiceUnavailable, "user accounts are not available")
			return
		}
		resolved, ok := auth.ResolvedSessionFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}

		var req request
		if !decodeJSON(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.ProfileUserID) == "" {
			writeError(w, http.StatusBadRequest, "profileUserId is required")
			return
		}

		// Current profile token (used to validate exit PIN for restricted profiles).
		currentProfileToken := strings.TrimSpace(r.Header.Get("X-Profile-Token"))

		token, card, err := deps.Auth.SwitchProfile(
			r.Context(),
			resolved.Session.ID,
			req.ProfileUserID,
			currentProfileToken,
			req.CurrentProfilePin,
			req.TargetProfilePin,
		)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrInvalidPin):
				writeError(w, http.StatusUnauthorized, "incorrect pin")
			case errors.Is(err, auth.ErrUserNotFound):
				writeError(w, http.StatusNotFound, "profile not found")
			default:
				writeError(w, http.StatusInternalServerError, "profile switch failed")
			}
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"profileToken": token,
			"profile":      card,
		})
	}
}
