package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/auth"
)

const (
	roleAdmin    = "admin"
	roleStandard = "standard"
)

// approvalCache caches approved device IDs for a short period to avoid a DB
// round-trip on every protected client request. Devices can only hold a stale
// "approved" entry for up to cacheTTL; revoke invalidates immediately.
var approvalCache = struct {
	mu sync.Mutex
	m  map[string]time.Time
}{m: make(map[string]time.Time)}

const approvalCacheTTL = 2 * time.Minute

// invalidateApprovalCache removes a device from the short-lived approval
// cache. Call this whenever a device is revoked so the change takes effect
// immediately rather than after the TTL expires.
func invalidateApprovalCache(deviceID string) {
	approvalCache.mu.Lock()
	delete(approvalCache.m, deviceID)
	approvalCache.mu.Unlock()
}

type routePolicy struct {
	Pattern string
	Group   string
	Action  string
	Roles   []string
}

func route(pattern string, group string, action string, roles ...string) routePolicy {
	return routePolicy{Pattern: pattern, Group: group, Action: action, Roles: roles}
}

var routePolicies = map[string]routePolicy{
	"GET /api/auth/session":         route("GET /api/auth/session", "auth", "session.read", roleAdmin, roleStandard),
	"POST /api/auth/logout":         route("POST /api/auth/logout", "auth", "session.logout", roleAdmin, roleStandard),
	"GET /api/profiles":             route("GET /api/profiles", "auth", "profiles.list", roleAdmin, roleStandard),
	"POST /api/auth/switch-profile": route("POST /api/auth/switch-profile", "auth", "profile.switch", roleAdmin, roleStandard),
	"GET /api/users":                route("GET /api/users", "auth", "users.list", roleAdmin),
	"POST /api/users":               route("POST /api/users", "auth", "user.create", roleAdmin),
	"PATCH /api/users/{id}":         route("PATCH /api/users/{id}", "auth", "user.update", roleAdmin),
	"DELETE /api/users/{id}":        route("DELETE /api/users/{id}", "auth", "user.delete", roleAdmin),
	"POST /api/users/{id}/password": route("POST /api/users/{id}/password", "auth", "user.password.update", roleAdmin),
	"POST /api/users/{id}/pin":      route("POST /api/users/{id}/pin", "auth", "user.pin.update", roleAdmin),

	"POST /api/libraries":             route("POST /api/libraries", "libraries", "library.save", roleAdmin),
	"DELETE /api/libraries/{id}":      route("DELETE /api/libraries/{id}", "libraries", "library.delete", roleAdmin),
	"POST /api/libraries/{id}/scan":   route("POST /api/libraries/{id}/scan", "libraries", "library.scan", roleAdmin),
	"POST /api/libraries/movies/scan": route("POST /api/libraries/movies/scan", "libraries", "library.scan.movies", roleAdmin),
	"POST /api/libraries/tv/scan":     route("POST /api/libraries/tv/scan", "libraries", "library.scan.tv", roleAdmin),
	"POST /api/libraries/scan":        route("POST /api/libraries/scan", "libraries", "library.scan.all", roleAdmin),

	"PUT /api/metadata/match":                 route("PUT /api/metadata/match", "metadata", "metadata.match", roleAdmin),
	"POST /api/metadata/refresh":              route("POST /api/metadata/refresh", "metadata", "metadata.refresh", roleAdmin),
	"POST /api/metadata/refresh-batch":        route("POST /api/metadata/refresh-batch", "metadata", "metadata.refresh.batch", roleAdmin),
	"GET /api/metadata/candidates":            route("GET /api/metadata/candidates", "metadata", "metadata.candidates", roleAdmin),
	"GET /api/metadata/backfill":              route("GET /api/metadata/backfill", "metadata", "metadata.backfill.read", roleAdmin),
	"POST /api/metadata/backfill":             route("POST /api/metadata/backfill", "metadata", "metadata.backfill.start", roleAdmin),
	"DELETE /api/metadata/backfill":           route("DELETE /api/metadata/backfill", "metadata", "metadata.backfill.stop", roleAdmin),
	"GET /api/migrations/formats":             route("GET /api/migrations/formats", "migration", "migration.formats", roleAdmin),
	"GET /api/migrations/runs":                route("GET /api/migrations/runs", "migration", "migration.runs.list", roleAdmin),
	"GET /api/migrations/runs/{id}":           route("GET /api/migrations/runs/{id}", "migration", "migration.run.read", roleAdmin),
	"POST /api/migrations/dry-run":            route("POST /api/migrations/dry-run", "migration", "migration.dry_run", roleAdmin),
	"POST /api/migrations/import":             route("POST /api/migrations/import", "migration", "migration.import", roleAdmin),
	"POST /api/migrations/runs/{id}/rollback": route("POST /api/migrations/runs/{id}/rollback", "migration", "migration.rollback", roleAdmin),

	"GET /api/client/home":                  route("GET /api/client/home", "client", "client.home", roleAdmin, roleStandard),
	"GET /api/client/movies/{id}":           route("GET /api/client/movies/{id}", "client", "client.movie.detail", roleAdmin, roleStandard),
	"GET /api/client/movies/{id}/similar":   route("GET /api/client/movies/{id}/similar", "client", "client.movie.similar", roleAdmin, roleStandard),
	"GET /api/client/series/{id}":           route("GET /api/client/series/{id}", "client", "client.series.detail", roleAdmin, roleStandard),
	"GET /api/client/series/{id}/similar":   route("GET /api/client/series/{id}/similar", "client", "client.series.similar", roleAdmin, roleStandard),
	"GET /api/client/collections":           route("GET /api/client/collections", "client", "client.collections.list", roleAdmin, roleStandard),
	"GET /api/client/collections/{id}":      route("GET /api/client/collections/{id}", "client", "client.collection.detail", roleAdmin, roleStandard),
	"GET /api/client/people/{name}":         route("GET /api/client/people/{name}", "client", "client.person.detail", roleAdmin, roleStandard),
	"GET /api/client/search":                route("GET /api/client/search", "client", "client.search", roleAdmin, roleStandard),
	"GET /api/client/watchlist":             route("GET /api/client/watchlist", "client", "client.watchlist.list", roleAdmin, roleStandard),
	"POST /api/client/watchlist":            route("POST /api/client/watchlist", "client", "client.watchlist.add", roleAdmin, roleStandard),
	"DELETE /api/client/watchlist/{id}":     route("DELETE /api/client/watchlist/{id}", "client", "client.watchlist.remove", roleAdmin, roleStandard),
	"POST /api/client/playback/start":       route("POST /api/client/playback/start", "client", "client.playback.start", roleAdmin, roleStandard),
	"PATCH /api/client/playback/{id}":       route("PATCH /api/client/playback/{id}", "client", "client.playback.heartbeat", roleAdmin, roleStandard),
	"POST /api/client/playback/{id}/stop":   route("POST /api/client/playback/{id}/stop", "client", "client.playback.stop", roleAdmin, roleStandard),
	"PUT /api/settings":                     route("PUT /api/settings", "settings", "settings.update", roleAdmin),
	"PUT /api/settings/metadata-sources":    route("PUT /api/settings/metadata-sources", "settings", "settings.metadata_sources.update", roleAdmin),
	"GET /api/settings/folders/browse":      route("GET /api/settings/folders/browse", "settings", "settings.folders.browse", roleAdmin),
	"POST /api/settings/folders/browse-smb": route("POST /api/settings/folders/browse-smb", "settings", "settings.folders.browse-smb", roleAdmin),
	"POST /api/settings/hardware/test":      route("POST /api/settings/hardware/test", "settings", "settings.hardware.test", roleAdmin),
	"GET /api/backup/export":                route("GET /api/backup/export", "backup", "backup.export", roleAdmin),
	"POST /api/backup/import":               route("POST /api/backup/import", "backup", "backup.import", roleAdmin),
	"GET /api/notifications":                route("GET /api/notifications", "notifications", "notifications.list", roleAdmin, roleStandard),
	"POST /api/notifications/{id}/dismiss":  route("POST /api/notifications/{id}/dismiss", "notifications", "notifications.dismiss", roleAdmin, roleStandard),
	"POST /api/notifications/dismiss-all":   route("POST /api/notifications/dismiss-all", "notifications", "notifications.dismiss_all", roleAdmin, roleStandard),
	"POST /api/remote/diagnostics":          route("POST /api/remote/diagnostics", "remote", "remote.diagnostics.run", roleAdmin),
	"POST /api/remote/wan":                  route("POST /api/remote/wan", "remote", "remote.wan.lookup", roleAdmin),
	"POST /api/setup/complete":              route("POST /api/setup/complete", "setup", "setup.complete", roleAdmin),

	"GET /api/media-sources/{id}/stream":                     route("GET /api/media-sources/{id}/stream", "media", "media.stream", roleAdmin, roleStandard),
	"GET /api/media-sources/{id}/remux-stream":               route("GET /api/media-sources/{id}/remux-stream", "media", "media.remux.stream", roleAdmin, roleStandard),
	"GET /api/media-sources/{id}/download":                   route("GET /api/media-sources/{id}/download", "media", "media.download", roleAdmin, roleStandard),
	"GET /api/media-sources/{id}/adaptive/master.m3u8":       route("GET /api/media-sources/{id}/adaptive/master.m3u8", "media", "media.adaptive.master", roleAdmin, roleStandard),
	"GET /api/media-sources/{id}/adaptive/{variant}":         route("GET /api/media-sources/{id}/adaptive/{variant}", "media", "media.adaptive.variant", roleAdmin, roleStandard),
	"POST /api/media-sources/{id}/adaptive/session":          route("POST /api/media-sources/{id}/adaptive/session", "media", "media.adaptive.session", roleAdmin, roleStandard),
	"POST /api/adaptive/telemetry":                           route("POST /api/adaptive/telemetry", "media", "media.adaptive.telemetry", roleAdmin, roleStandard),
	"POST /api/media-sources/{id}/stream-token":              route("POST /api/media-sources/{id}/stream-token", "media", "media.stream.token", roleAdmin, roleStandard),
	"GET /api/media-sources/{id}/subtitles/{index}":          route("GET /api/media-sources/{id}/subtitles/{index}", "media", "media.subtitle.stream", roleAdmin, roleStandard),
	"POST /api/media-sources/{id}/subtitles/{index}/convert": route("POST /api/media-sources/{id}/subtitles/{index}/convert", "media", "media.subtitle.convert", roleAdmin, roleStandard),
	"POST /api/media-sources/{id}/probe":                     route("POST /api/media-sources/{id}/probe", "media", "media.probe", roleAdmin),
	"GET /api/media-sources/{id}/thumbnails/status":          route("GET /api/media-sources/{id}/thumbnails/status", "media", "media.thumbnails.status", roleAdmin, roleStandard),
	"POST /api/media-sources/{id}/thumbnails/generate":       route("POST /api/media-sources/{id}/thumbnails/generate", "media", "media.thumbnails.generate", roleAdmin),
	"GET /api/media-sources/{id}/thumbnails/sprite.jpg":      route("GET /api/media-sources/{id}/thumbnails/sprite.jpg", "media", "media.thumbnails.sprite", roleAdmin, roleStandard),
	"GET /api/media-sources/{id}/thumbnails/thumbnails.vtt":  route("GET /api/media-sources/{id}/thumbnails/thumbnails.vtt", "media", "media.thumbnails.vtt", roleAdmin, roleStandard),
	"GET /api/media-sources/{id}/thumbnails/chapters.vtt":    route("GET /api/media-sources/{id}/thumbnails/chapters.vtt", "media", "media.thumbnails.chapters", roleAdmin, roleStandard),
	"POST /api/probes":                                       route("POST /api/probes", "media", "probe.start", roleAdmin),
	"POST /api/work":                                         route("POST /api/work", "work", "work.start", roleAdmin),
	"DELETE /api/work/{id}":                                  route("DELETE /api/work/{id}", "work", "work.cancel", roleAdmin),
	"GET /api/work/{id}/file":                                route("GET /api/work/{id}/file", "work", "work.file", roleAdmin, roleStandard),
	"GET /api/work/{id}/hls/{file}":                          route("GET /api/work/{id}/hls/{file}", "work", "work.hls", roleAdmin, roleStandard),
	"POST /api/downloads":                                    route("POST /api/downloads", "downloads", "download.start", roleAdmin),
	"GET /api/downloads/{id}/file":                           route("GET /api/downloads/{id}/file", "downloads", "download.file", roleAdmin, roleStandard),
	"GET /api/pairing/requests":                              route("GET /api/pairing/requests", "pairing", "pairing.list", roleAdmin),
	"POST /api/pairing/requests/{id}/approve":                route("POST /api/pairing/requests/{id}/approve", "pairing", "pairing.approve", roleAdmin),
	"POST /api/pairing/requests/{id}/deny":                   route("POST /api/pairing/requests/{id}/deny", "pairing", "pairing.deny", roleAdmin),
	"POST /api/pairing/qr":                                   route("POST /api/pairing/qr", "pairing", "pairing.qr.generate", roleAdmin),
	"GET /api/devices":                                       route("GET /api/devices", "devices", "devices.list", roleAdmin),
	"POST /api/devices/{id}/revoke":                          route("POST /api/devices/{id}/revoke", "devices", "devices.revoke", roleAdmin),
	"GET /api/sessions":                                      route("GET /api/sessions", "sessions", "sessions.list", roleAdmin, roleStandard),
	"GET /api/sessions/{id}/inspector":                       route("GET /api/sessions/{id}/inspector", "sessions", "sessions.inspector", roleAdmin, roleStandard),
	"POST /api/sessions":                                     route("POST /api/sessions", "sessions", "session.start", roleAdmin, roleStandard),
	"PATCH /api/sessions/{id}":                               route("PATCH /api/sessions/{id}", "sessions", "session.update", roleAdmin, roleStandard),
	"DELETE /api/sessions/{id}":                              route("DELETE /api/sessions/{id}", "sessions", "session.stop", roleAdmin, roleStandard),
	"PUT /api/playback/state/{id}":                           route("PUT /api/playback/state/{id}", "playback", "playback.state.update", roleAdmin, roleStandard),
	// "GET /play/{id}" intentionally removed — the legacy HTML player
	// handler is no longer registered in router.go. The SPA route
	// /play/[mediaSourceId] now handles the URL via webRootHandler fallback.

	// --- read-only endpoints (issue #55: auth lockdown) ---
	// Admin-only operational / config reads:
	"GET /api/metrics":                   route("GET /api/metrics", "system", "system.metrics", roleAdmin),
	"GET /api/events":                    route("GET /api/events", "system", "system.events", roleAdmin),
	"GET /api/architecture":              route("GET /api/architecture", "system", "system.architecture", roleAdmin),
	"GET /api/system/status":             route("GET /api/system/status", "system", "system.status", roleAdmin),
	"GET /api/system/updates":            route("GET /api/system/updates", "system", "system.updates.read", roleAdmin),
	"POST /api/system/updates/apply":     route("POST /api/system/updates/apply", "system", "system.updates.apply", roleAdmin),
	"GET /api/remote/access":             route("GET /api/remote/access", "remote", "remote.access.read", roleAdmin),
	"GET /api/libraries":                 route("GET /api/libraries", "libraries", "library.list", roleAdmin),
	"GET /api/catalog/summary":           route("GET /api/catalog/summary", "catalog", "catalog.summary", roleAdmin),
	"GET /api/catalog/health":            route("GET /api/catalog/health", "catalog", "catalog.health", roleAdmin),
	"GET /api/catalog/codecs":            route("GET /api/catalog/codecs", "catalog", "catalog.codecs", roleAdmin),
	"GET /api/catalog/playability-audit": route("GET /api/catalog/playability-audit", "catalog", "catalog.playability_audit", roleAdmin),
	"GET /api/review":                    route("GET /api/review", "metadata", "metadata.review", roleAdmin),
	"GET /api/metadata/providers":        route("GET /api/metadata/providers", "metadata", "metadata.providers", roleAdmin),
	"GET /api/metadata/suggestions":      route("GET /api/metadata/suggestions", "metadata", "metadata.suggestions", roleAdmin),
	"GET /api/settings":                  route("GET /api/settings", "settings", "settings.read", roleAdmin),
	"GET /api/settings/performance":      route("GET /api/settings/performance", "settings", "settings.performance.read", roleAdmin),
	"GET /api/probes":                    route("GET /api/probes", "media", "probe.list", roleAdmin),
	"GET /api/probes/{id}":               route("GET /api/probes/{id}", "media", "probe.read", roleAdmin),
	"GET /api/jobs":                      route("GET /api/jobs", "jobs", "jobs.status", roleAdmin),
	"GET /api/work":                      route("GET /api/work", "work", "work.list", roleAdmin),
	"GET /api/downloads":                 route("GET /api/downloads", "downloads", "downloads.list", roleAdmin),
	"GET /api/downloads/{id}":            route("GET /api/downloads/{id}", "downloads", "downloads.read", roleAdmin),
	"GET /api/scans":                     route("GET /api/scans", "libraries", "scans.list", roleAdmin),
	"GET /api/scans/{id}":                route("GET /api/scans/{id}", "libraries", "scans.read", roleAdmin),

	// Both roles can read media-browsing surfaces:
	"GET /api/movies":                       route("GET /api/movies", "catalog", "movies.list", roleAdmin, roleStandard),
	"GET /api/movies/{id}":                  route("GET /api/movies/{id}", "catalog", "movies.read", roleAdmin, roleStandard),
	"GET /api/series":                       route("GET /api/series", "catalog", "series.list", roleAdmin, roleStandard),
	"GET /api/series/{id}":                  route("GET /api/series/{id}", "catalog", "series.read", roleAdmin, roleStandard),
	"GET /api/metadata/{kind}/{id}":         route("GET /api/metadata/{kind}/{id}", "metadata", "metadata.read", roleAdmin, roleStandard),
	"GET /api/artwork/{kind}/{id}":          route("GET /api/artwork/{kind}/{id}", "metadata", "artwork.read", roleAdmin, roleStandard),
	"GET /api/trailers/{tmdbId}":            route("GET /api/trailers/{tmdbId}", "metadata", "trailer.read", roleAdmin, roleStandard),
	"GET /api/versions":                     route("GET /api/versions", "catalog", "versions.read", roleAdmin, roleStandard),
	"GET /api/media-sources":                route("GET /api/media-sources", "media", "media.list", roleAdmin, roleStandard),
	"GET /api/media-sources/{id}":           route("GET /api/media-sources/{id}", "media", "media.read", roleAdmin, roleStandard),
	"DELETE /api/media-sources/{id}":        route("DELETE /api/media-sources/{id}", "media", "media.delete", roleAdmin),
	"GET /api/media-sources/{id}/tracks":    route("GET /api/media-sources/{id}/tracks", "media", "media.tracks", roleAdmin, roleStandard),
	"GET /api/media-sources/{id}/subtitles": route("GET /api/media-sources/{id}/subtitles", "media", "media.subtitles", roleAdmin, roleStandard),
	"GET /api/devices/profiles":             route("GET /api/devices/profiles", "devices", "devices.profiles", roleAdmin, roleStandard),
	"GET /api/playback/recent":              route("GET /api/playback/recent", "playback", "playback.recent", roleAdmin, roleStandard),
	"GET /api/playback/state/{id}":          route("GET /api/playback/state/{id}", "playback", "playback.state.read", roleAdmin, roleStandard),
	"GET /api/playback/decision":            route("GET /api/playback/decision", "playback", "playback.decision", roleAdmin, roleStandard),
	"GET /api/playback/route":               route("GET /api/playback/route", "playback", "playback.route", roleAdmin, roleStandard),

	"GET /api/media-sources/{id}/chapters":          route("GET /api/media-sources/{id}/chapters", "chapters", "chapters.read", roleAdmin, roleStandard),
	"POST /api/media-sources/{id}/chapters/analyze": route("POST /api/media-sources/{id}/chapters/analyze", "chapters", "chapters.analyze", roleAdmin),
	"PATCH /api/users/me/preferences":               route("PATCH /api/users/me/preferences", "auth", "user.preferences.update", roleAdmin, roleStandard),
}

func handleProtected(mux *http.ServeMux, deps Deps, pattern string, next http.HandlerFunc) {
	mux.HandleFunc(pattern, requireRoutePolicy(deps, routePolicies[pattern], false, next))
}

func handleProtectedCSRF(mux *http.ServeMux, deps Deps, pattern string, next http.HandlerFunc) {
	mux.HandleFunc(pattern, requireRoutePolicy(deps, routePolicies[pattern], true, next))
}

func requireRoutePolicy(deps Deps, policy routePolicy, requireCSRF bool, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(deps, func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			next(w, r)
			return
		}
		resolved, _ := auth.ResolvedSessionFromContext(r.Context())
		if !policyAllows(policy, resolved.Principal.Role) {
			publishAudit(deps, r, policy, resolved, "denied", "role_not_allowed")
			writeError(w, http.StatusForbidden, "role is not allowed for this action")
			return
		}
		if err := requireApprovedNativeDevice(deps, r, policy, resolved); err != nil {
			publishAudit(deps, r, policy, resolved, "denied", "device_not_approved")
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if requireCSRF {
			if resolved.DevBypass {
				publishAudit(deps, r, policy, resolved, "allowed", "dev_auth_bypass")
				next(w, r)
				return
			}
			if hasHeaderAuthToken(r) {
				publishAudit(deps, r, policy, resolved, "allowed", "header_token")
				next(w, r)
				return
			}
			csrfCookie, _ := r.Cookie(auth.CSRFCookieName)
			csrfCookieValue := ""
			if csrfCookie != nil {
				csrfCookieValue = csrfCookie.Value
			}
			if err := deps.Auth.ValidateCSRF(resolved, csrfCookieValue, r.Header.Get("X-CSRF-Token")); err != nil {
				publishAudit(deps, r, policy, resolved, "denied", "csrf_invalid")
				writeError(w, http.StatusForbidden, "valid csrf token required")
				return
			}
		}
		publishAudit(deps, r, policy, resolved, "allowed", "")
		next(w, r)
	})
}

func requireApprovedNativeDevice(deps Deps, r *http.Request, policy routePolicy, resolved auth.ResolvedSession) error {
	if policy.Group != "client" {
		return nil
	}
	if deps.Auth == nil || deps.Auth.Disabled() {
		return nil
	}
	if resolved.Principal.ID != "local" && strings.TrimSpace(resolved.Session.DeviceID) == "" {
		return nil
	}
	if deps.Devices == nil {
		return fmt.Errorf("native device approval is required")
	}
	deviceID := strings.TrimSpace(resolved.Session.DeviceID)
	if deviceID == "" {
		return fmt.Errorf("native device approval is required")
	}
	// Check cache first to avoid a DB round-trip on every client request.
	approvalCache.mu.Lock()
	exp, cached := approvalCache.m[deviceID]
	approvalCache.mu.Unlock()
	if cached && time.Now().Before(exp) {
		return nil // device is approved (cached)
	}

	approved, err := deps.Devices.IsApproved(r.Context(), deviceID)
	if err != nil {
		slog.Warn("native device approval check failed", "deviceId", deviceID, "err", err)
		return fmt.Errorf("native device approval could not be verified")
	}
	if !approved {
		return fmt.Errorf("native device approval is required")
	}
	// Cache the approval for a short period.
	approvalCache.mu.Lock()
	approvalCache.m[deviceID] = time.Now().Add(approvalCacheTTL)
	approvalCache.mu.Unlock()
	return nil
}

func policyAllows(policy routePolicy, role string) bool {
	role = strings.ToLower(strings.TrimSpace(role))
	for _, allowed := range policy.Roles {
		if role == allowed {
			return true
		}
	}
	return false
}

func publishAudit(deps Deps, r *http.Request, policy routePolicy, resolved auth.ResolvedSession, result string, reason string) {
	if deps.Events == nil {
		return
	}
	deps.Events.Publish("audit.route", map[string]any{
		"userId":    resolved.Principal.ID,
		"username":  resolved.Principal.Username,
		"role":      resolved.Principal.Role,
		"method":    r.Method,
		"path":      r.URL.Path,
		"pattern":   policy.Pattern,
		"group":     policy.Group,
		"action":    policy.Action,
		"result":    result,
		"reason":    reason,
		"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
	})
}
