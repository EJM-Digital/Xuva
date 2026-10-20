package api

import (
	"net/http"

	"github.com/EJM-Digital/Xuva/server/internal/notifications"
)

// notificationsListHandler returns undismissed notifications for the bell drawer.
func notificationsListHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := deps.Notifications.List(r.Context())
		if err != nil {
			http.Error(w, "failed to load notifications", http.StatusInternalServerError)
			return
		}
		if items == nil {
			items = []notifications.Notification{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"notifications": items})
	}
}

// notificationsDismissHandler marks a single notification as dismissed.
func notificationsDismissHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}
		if err := deps.Notifications.Dismiss(r.Context(), id); err != nil {
			http.Error(w, "failed to dismiss notification", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// notificationsDismissAllHandler marks all notifications as dismissed.
func notificationsDismissAllHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := deps.Notifications.DismissAll(r.Context()); err != nil {
			http.Error(w, "failed to dismiss notifications", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
