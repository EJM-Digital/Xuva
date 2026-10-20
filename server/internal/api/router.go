package api

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/google/uuid"

	"github.com/EJM-Digital/Xuva/server/internal/adaptive"
	"github.com/EJM-Digital/Xuva/server/internal/auth"
	"github.com/EJM-Digital/Xuva/server/internal/buildinfo"
	"github.com/EJM-Digital/Xuva/server/internal/catalog"
	"github.com/EJM-Digital/Xuva/server/internal/chapters"
	"github.com/EJM-Digital/Xuva/server/internal/config"
	"github.com/EJM-Digital/Xuva/server/internal/database"
	"github.com/EJM-Digital/Xuva/server/internal/devices"
	"github.com/EJM-Digital/Xuva/server/internal/discovery"
	"github.com/EJM-Digital/Xuva/server/internal/downloads"
	"github.com/EJM-Digital/Xuva/server/internal/events"
	"github.com/EJM-Digital/Xuva/server/internal/jobs"
	"github.com/EJM-Digital/Xuva/server/internal/libraries"
	"github.com/EJM-Digital/Xuva/server/internal/media"
	metaprovider "github.com/EJM-Digital/Xuva/server/internal/metadata"
	"github.com/EJM-Digital/Xuva/server/internal/metasources"
	"github.com/EJM-Digital/Xuva/server/internal/migration"
	"github.com/EJM-Digital/Xuva/server/internal/movies"
	"github.com/EJM-Digital/Xuva/server/internal/notifications"
	"github.com/EJM-Digital/Xuva/server/internal/observability"
	"github.com/EJM-Digital/Xuva/server/internal/pairing"
	"github.com/EJM-Digital/Xuva/server/internal/playback"
	"github.com/EJM-Digital/Xuva/server/internal/playstate"
	"github.com/EJM-Digital/Xuva/server/internal/probe"
	"github.com/EJM-Digital/Xuva/server/internal/probes"
	"github.com/EJM-Digital/Xuva/server/internal/remote"
	"github.com/EJM-Digital/Xuva/server/internal/resources"
	"github.com/EJM-Digital/Xuva/server/internal/scanner"
	"github.com/EJM-Digital/Xuva/server/internal/scans"
	"github.com/EJM-Digital/Xuva/server/internal/secret"
	"github.com/EJM-Digital/Xuva/server/internal/sessions"
	"github.com/EJM-Digital/Xuva/server/internal/streaming"
	"github.com/EJM-Digital/Xuva/server/internal/subtitles"
	"github.com/EJM-Digital/Xuva/server/internal/systemstats"
	"github.com/EJM-Digital/Xuva/server/internal/thumbnails"
	"github.com/EJM-Digital/Xuva/server/internal/trailers"
	"github.com/EJM-Digital/Xuva/server/internal/transcode"
	"github.com/EJM-Digital/Xuva/server/internal/trending"
	"github.com/EJM-Digital/Xuva/server/internal/tv"
	"github.com/EJM-Digital/Xuva/server/internal/watchlist"
	"github.com/EJM-Digital/Xuva/server/internal/webapp"
	qrcode "github.com/skip2/go-qrcode"
)

type Deps struct {
	Config config.Config
	// Secret seals/opens machine-bound secrets (e.g. SMB share credentials).
	// May be nil if the store failed to initialize; handlers must nil-check.
	Secret        secret.Store
	StartedAt     time.Time
	Database      *database.Service
	Auth          *auth.Service
	Events        *events.Bus
	Observe       *observability.Service
	Resources     *resources.Manager
	Jobs          *jobs.Registry
	Discovery     *discovery.Service
	Libraries     *libraries.Service
	Scanner       *scanner.Service
	Scans         *scans.Service
	Catalog       *catalog.Service
	Media         *media.Service
	Metadata      *metaprovider.Service
	Movies        *movies.Service
	TV            *tv.Service
	Probe         *probe.Service
	Probes        *probes.Service
	Playback      *playback.Service
	PlayState     *playstate.Service
	Streaming     *streaming.Service
	Transcode     *transcode.Service
	Downloads     *downloads.Service
	Devices       *devices.Service
	Sessions      *sessions.Service
	Subtitles     *subtitles.Service
	Pairing       *pairing.Service
	Migration     *migration.Service
	Trending      *trending.Service
	Trailers      *trailers.Service
	Thumbnails    *thumbnails.Service
	Notifications *notifications.Service
	Chapters      *chapters.Service
	Watchlist     *watchlist.Service
	// Automation state — live snapshots from the three background job goroutines.
	ScanAuto     JobAutoStater
	MetadataAuto JobAutoStater
	ProbeAuto    JobAutoStater
}

// JobAutoStater is the read interface used by the /api/jobs handler.
type JobAutoStater interface {
	Snapshot() map[string]any
}

func NewRouter(deps Deps) http.Handler {
	mux := http.NewServeMux()

	// === Public endpoints (no auth required) ===
	//
	// Issue #55: every other GET /api/* below MUST go through handleProtected
	// with a routePolicy entry. Only the items in this block may be
	// anonymous. The rationale for each one is documented inline; do not
	// expand this list without a security review.
	//
	mux.HandleFunc("GET /api/health", healthHandler(deps))                               // liveness probe, no PII
	mux.HandleFunc("GET /api/ready", readinessHandler(deps))                             // readiness probe, no PII
	mux.HandleFunc("GET /api/system/version", systemVersionHandler(deps))                // release/upgrade identity, no PII
	mux.HandleFunc("GET /api/client/bootstrap", clientBootstrapHandler(deps))            // pre-login server identity
	mux.HandleFunc("GET /api/discovery/status", discoveryStatusHandler(deps))            // mDNS / local discovery info
	mux.HandleFunc("GET /api/pairing/requests/{id}", pairingStatusHandler(deps))         // unpaired clients poll their own request
	mux.HandleFunc("POST /api/pairing/requests", pairingCreateHandler(deps))             // unpaired clients submit a request
	mux.HandleFunc("DELETE /api/pairing/requests/{id}", pairingCancelHandler(deps))      // client withdraws its own pending request
	mux.HandleFunc("POST /api/auth/bootstrap", authBootstrapHandler(deps))               // first-run admin create
	mux.HandleFunc("POST /api/auth/login", authLoginHandler(deps))                       // sign in
	mux.HandleFunc("GET /api/setup/status", setupStatusHandler(deps))                    // first-run wizard gate, no PII
	mux.HandleFunc("GET /api/pairing/qr/{token}/image.png", pairingQRImageHandler(deps)) // QR PNG — serves to any device pre-auth
	mux.HandleFunc("POST /api/pairing/qr/{token}/claim", pairingQRClaimHandler(deps))    // device claims QR token

	// === Authenticated endpoints ===
	handleProtected(mux, deps, "GET /api/auth/session", authSessionHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/auth/logout", authLogoutHandler(deps))
	handleProtected(mux, deps, "GET /api/profiles", profilesListHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/auth/switch-profile", authSwitchProfileHandler(deps))
	handleProtected(mux, deps, "GET /api/users", usersListHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/users", usersCreateHandler(deps))
	handleProtectedCSRF(mux, deps, "PATCH /api/users/{id}", usersUpdateHandler(deps))
	handleProtectedCSRF(mux, deps, "DELETE /api/users/{id}", usersDeleteHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/users/{id}/password", usersPasswordHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/users/{id}/pin", usersSetPinHandler(deps))
	handleProtected(mux, deps, "GET /api/metrics", metricsHandler(deps))
	handleProtected(mux, deps, "GET /api/events", eventsHandler(deps))
	handleProtected(mux, deps, "GET /api/architecture", architectureHandler(deps))
	handleProtected(mux, deps, "GET /api/libraries", librariesHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/libraries", librarySaveHandler(deps))
	handleProtectedCSRF(mux, deps, "DELETE /api/libraries/{id}", libraryDeleteHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/libraries/{id}/scan", libraryScanByIDHandler(deps))
	handleProtected(mux, deps, "GET /api/catalog/summary", catalogSummaryHandler(deps))
	handleProtected(mux, deps, "GET /api/catalog/health", catalogHealthHandler(deps))
	handleProtected(mux, deps, "GET /api/catalog/codecs", catalogCodecsHandler(deps))
	handleProtected(mux, deps, "GET /api/catalog/playability-audit", catalogPlayabilityAuditHandler(deps))
	handleProtected(mux, deps, "GET /api/migrations/formats", migrationFormatsHandler(deps))
	handleProtected(mux, deps, "GET /api/migrations/runs", migrationRunsHandler(deps))
	handleProtected(mux, deps, "GET /api/migrations/runs/{id}", migrationRunDetailHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/migrations/dry-run", migrationDryRunHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/migrations/import", migrationImportHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/migrations/runs/{id}/rollback", migrationRollbackHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/setup/complete", setupCompleteHandler(deps))
	handleProtected(mux, deps, "GET /api/client/home", clientHomeHandler(deps))
	handleProtected(mux, deps, "GET /api/client/movies/{id}", clientMovieDetailHandler(deps))
	handleProtected(mux, deps, "GET /api/client/movies/{id}/similar", clientSimilarMoviesHandler(deps))
	handleProtected(mux, deps, "GET /api/client/series/{id}", clientSeriesDetailHandler(deps))
	handleProtected(mux, deps, "GET /api/client/series/{id}/similar", clientSimilarSeriesHandler(deps))
	handleProtected(mux, deps, "GET /api/client/collections", clientCollectionsHandler(deps))
	handleProtected(mux, deps, "GET /api/client/collections/{id}", clientCollectionHandler(deps))
	handleProtected(mux, deps, "GET /api/client/people/{name}", clientPersonHandler(deps))
	handleProtected(mux, deps, "GET /api/client/search", clientSearchHandler(deps))
	handleProtected(mux, deps, "GET /api/client/watchlist", clientWatchlistListHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/client/watchlist", clientWatchlistAddHandler(deps))
	handleProtectedCSRF(mux, deps, "DELETE /api/client/watchlist/{id}", clientWatchlistRemoveHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/client/playback/start", clientPlaybackStartHandler(deps))
	handleProtectedCSRF(mux, deps, "PATCH /api/client/playback/{id}", clientPlaybackHeartbeatHandler(deps))
	handleProtected(mux, deps, "POST /api/client/playback/{id}/stop", clientPlaybackStopHandler(deps))
	handleProtected(mux, deps, "GET /api/movies", moviesHandler(deps))
	handleProtected(mux, deps, "GET /api/movies/{id}", movieDetailHandler(deps))
	handleProtected(mux, deps, "GET /api/series", seriesHandler(deps))
	handleProtected(mux, deps, "GET /api/series/{id}", seriesDetailHandler(deps))
	handleProtected(mux, deps, "GET /api/review", reviewHandler(deps))
	handleProtected(mux, deps, "GET /api/metadata/providers", metadataProvidersHandler(deps))
	handleProtected(mux, deps, "GET /api/metadata/suggestions", metadataSuggestionsHandler(deps))
	handleProtected(mux, deps, "GET /api/metadata/{kind}/{id}", metadataRecordsHandler(deps))
	handleProtectedCSRF(mux, deps, "PUT /api/metadata/match", metadataMatchHandler(deps))
	handleProtected(mux, deps, "GET /api/metadata/candidates", metadataCandidatesHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/metadata/refresh", metadataRefreshHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/metadata/refresh-batch", metadataRefreshBatchHandler(deps))
	handleProtected(mux, deps, "GET /api/metadata/backfill", metadataBackfillStatusHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/metadata/backfill", metadataBackfillStartHandler(deps))
	handleProtectedCSRF(mux, deps, "DELETE /api/metadata/backfill", metadataBackfillStopHandler(deps))
	handleProtected(mux, deps, "GET /api/artwork/{kind}/{id}", artworkHandler(deps))
	handleProtected(mux, deps, "GET /api/trailers/{tmdbId}", trailerHandler(deps))
	handleProtected(mux, deps, "GET /api/versions", versionsHandler(deps))
	handleProtected(mux, deps, "GET /api/settings/performance", performanceSettingsHandler(deps))
	handleProtected(mux, deps, "GET /api/settings", settingsHandler(deps))
	handleProtected(mux, deps, "GET /api/settings/folders/browse", settingsFolderBrowseHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/settings/folders/browse-smb", settingsSMBBrowseHandler(deps))
	handleProtectedCSRF(mux, deps, "PUT /api/settings", settingsUpdateHandler(deps))
	handleProtectedCSRF(mux, deps, "PUT /api/settings/metadata-sources", metadataSourceSettingsUpdateHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/settings/hardware/test", hardwareTestHandler(deps))
	handleProtected(mux, deps, "GET /api/system/updates", updatesCheckHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/system/updates/apply", updatesApplyHandler(deps))
	handleProtected(mux, deps, "GET /api/backup/export", backupExportHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/backup/import", backupImportHandler(deps))
	handleProtected(mux, deps, "GET /api/notifications", notificationsListHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/notifications/{id}/dismiss", notificationsDismissHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/notifications/dismiss-all", notificationsDismissAllHandler(deps))
	handleProtected(mux, deps, "GET /api/system/status", systemStatusHandler(deps))
	handleProtected(mux, deps, "GET /api/remote/access", remoteAccessHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/remote/diagnostics", remoteDiagnosticsHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/remote/wan", wanAddressHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources", mediaSourcesHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}", mediaSourceDetailHandler(deps))
	handleProtectedCSRF(mux, deps, "DELETE /api/media-sources/{id}", mediaSourceDeleteHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/adaptive/master.m3u8", adaptiveMasterHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/adaptive/{variant}", adaptiveVariantHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/media-sources/{id}/adaptive/session", adaptiveSessionHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/stream", mediaSourceStreamHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/remux-stream", remuxStreamHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/download", mediaSourceDownloadHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/media-sources/{id}/stream-token", mediaSourceStreamTokenHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/tracks", mediaSourceTracksHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/subtitles", mediaSourceSubtitlesHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/subtitles/{index}", mediaSourceSubtitleStreamHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/media-sources/{id}/subtitles/{index}/convert", mediaSourceSubtitleConvertHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/media-sources/{id}/probe", mediaSourceProbeHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/thumbnails/status", thumbnailStatusHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/media-sources/{id}/thumbnails/generate", thumbnailGenerateHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/thumbnails/sprite.jpg", thumbnailSpriteHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/thumbnails/thumbnails.vtt", thumbnailVTTHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/thumbnails/chapters.vtt", thumbnailChaptersHandler(deps))
	handleProtected(mux, deps, "GET /api/media-sources/{id}/chapters", chaptersGetHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/media-sources/{id}/chapters/analyze", chaptersAnalyzeHandler(deps))
	handleProtectedCSRF(mux, deps, "PATCH /api/users/me/preferences", userPreferencesUpdateHandler(deps))
	handleProtected(mux, deps, "GET /api/probes", probesHandler(deps))
	handleProtected(mux, deps, "GET /api/probes/{id}", probeJobHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/probes", probeStartHandler(deps))
	handleProtected(mux, deps, "GET /api/jobs", jobsStatusHandler(deps))
	handleProtected(mux, deps, "GET /api/work", workHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/work", workStartHandler(deps))
	handleProtectedCSRF(mux, deps, "DELETE /api/work/{id}", workCancelHandler(deps))
	handleProtected(mux, deps, "GET /api/work/{id}/file", workFileHandler(deps))
	handleProtected(mux, deps, "GET /api/work/{id}/hls/{file}", workHLSHandler(deps))
	handleProtected(mux, deps, "GET /api/downloads", downloadsHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/downloads", downloadStartHandler(deps))
	handleProtected(mux, deps, "GET /api/downloads/{id}", downloadJobHandler(deps))
	handleProtected(mux, deps, "GET /api/downloads/{id}/file", downloadFileHandler(deps))
	handleProtected(mux, deps, "GET /api/devices/profiles", deviceProfilesHandler(deps))
	handleProtected(mux, deps, "GET /api/devices", approvedDevicesHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/devices/{id}/revoke", approvedDeviceRevokeHandler(deps))
	handleProtected(mux, deps, "GET /api/pairing/requests", pairingListHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/pairing/requests/{id}/approve", pairingApproveHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/pairing/requests/{id}/deny", pairingDenyHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/pairing/qr", pairingQRGenerateHandler(deps))
	handleProtected(mux, deps, "GET /api/sessions", sessionsHandler(deps))
	handleProtected(mux, deps, "GET /api/sessions/{id}/inspector", sessionInspectorHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/sessions", sessionStartHandler(deps))
	handleProtectedCSRF(mux, deps, "PATCH /api/sessions/{id}", sessionUpdateHandler(deps))
	handleProtectedCSRF(mux, deps, "DELETE /api/sessions/{id}", sessionStopHandler(deps))
	handleProtected(mux, deps, "GET /api/playback/recent", playbackRecentHandler(deps))
	handleProtected(mux, deps, "GET /api/playback/state/{id}", playbackStateGetHandler(deps))
	handleProtectedCSRF(mux, deps, "PUT /api/playback/state/{id}", playbackStateSetHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/adaptive/telemetry", adaptiveTelemetryHandler(deps))
	handleProtected(mux, deps, "GET /api/scans", scansHandler(deps))
	handleProtected(mux, deps, "GET /api/scans/{id}", scanJobHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/libraries/movies/scan", movieScanHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/libraries/tv/scan", tvScanHandler(deps))
	handleProtectedCSRF(mux, deps, "POST /api/libraries/scan", allLibrariesScanHandler(deps))
	handleProtected(mux, deps, "GET /api/playback/decision", playbackDecisionHandler(deps))
	handleProtected(mux, deps, "GET /api/playback/route", playbackRouteHandler(deps))
	// /play/{id} intentionally NOT registered here — it falls through to the
	// SPA root handler, which loads the SvelteKit /play/[mediaSourceId]/+page
	// route with the modern Player.svelte component. Previously a legacy
	// hardcoded HTML player (playerHandler, since deleted) intercepted this
	// route and served a bare <video controls> page — the user (correctly)
	// called it "the old ugly one". The Svelte player handles direct streams
	// AND HLS manifests AND subtitle/audio track switching uniformly.
	mux.Handle("GET /", webRootHandler(deps))
	return withObservability(deps, withCanonicalWebOrigin(deps, withSecurity(deps, withResolvedSession(deps, mux))))
}

func withCanonicalWebOrigin(deps Deps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if !isHistoryRoutePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		origin, ok := canonicalWebOriginForRequest(r, currentConfig(deps))
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		currentOrigin := requestOrigin(r)
		if sameOrigin(currentOrigin, origin) {
			next.ServeHTTP(w, r)
			return
		}
		target := *r.URL
		target.Scheme = origin.Scheme
		target.Host = origin.Host
		http.Redirect(w, r, target.String(), http.StatusTemporaryRedirect)
	})
}

func webRootHandler(deps Deps) http.Handler {
	root := webapp.RootHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() || !isHistoryRoutePath(r.URL.Path) {
			root.ServeHTTP(w, r)
			return
		}

		normalizedPath := normalizeHistoryPath(r.URL.Path)
		if normalizedPath == "" {
			normalizedPath = "/"
		}
		resolved, hasSession := auth.ResolvedSessionFromContext(r.Context())
		isSignedIn := hasSession && strings.TrimSpace(resolved.Principal.ID) != ""
		role := strings.ToLower(strings.TrimSpace(resolved.Principal.Role))
		isAdmin := role == "admin"

		if !isSignedIn {
			needsBootstrap := false
			if deps.Auth != nil {
				if required, err := deps.Auth.RequiresBootstrap(r.Context()); err == nil {
					needsBootstrap = required
				}
			}
			if needsBootstrap {
				if normalizedPath == "/setup" {
					root.ServeHTTP(w, r)
					return
				}
				http.Redirect(w, r, "/setup", http.StatusTemporaryRedirect)
				return
			}
			if normalizedPath == "/signin" {
				root.ServeHTTP(w, r)
				return
			}
			if normalizedPath == "/setup" || normalizedPath == "/setup-wizard" {
				http.Redirect(w, r, "/signin", http.StatusTemporaryRedirect)
				return
			}
			http.Redirect(w, r, "/signin", http.StatusTemporaryRedirect)
			return
		}

		if normalizedPath == "/signin" || normalizedPath == "/setup-wizard" {
			http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
			return
		}
		if normalizedPath == "/settings" || strings.HasPrefix(normalizedPath, "/settings/") {
			if !isAdmin {
				http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
				return
			}
		}

		root.ServeHTTP(w, r)
	})
}

func isHistoryRoutePath(requestPath string) bool {
	if requestPath == "" || requestPath == "/" {
		return true
	}
	if strings.HasPrefix(requestPath, "/api/") ||
		strings.HasPrefix(requestPath, "/_app/") ||
		strings.HasPrefix(requestPath, "/@vite/") ||
		strings.HasPrefix(requestPath, "/@fs/") ||
		strings.HasPrefix(requestPath, "/src/") ||
		strings.HasPrefix(requestPath, "/node_modules/") ||
		strings.HasPrefix(requestPath, "/legacy/") ||
		strings.HasPrefix(requestPath, "/next/") {
		return false
	}
	lastSegment := requestPath
	if slash := strings.LastIndex(lastSegment, "/"); slash >= 0 {
		lastSegment = lastSegment[slash+1:]
	}
	return !strings.Contains(lastSegment, ".")
}

func normalizeHistoryPath(requestPath string) string {
	trimmed := strings.TrimSpace(requestPath)
	if trimmed == "" {
		return "/"
	}
	if trimmed == "/" {
		return "/"
	}
	normalized := "/" + strings.Trim(trimmed, "/")
	return normalized
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func withObservability(deps Deps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		correlationID := observability.NormalizeCorrelationID(r.Header.Get("X-Request-ID"))
		w.Header().Set("X-Request-ID", correlationID)
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r.WithContext(observability.WithCorrelationID(r.Context(), correlationID)))
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		duration := time.Since(started)
		if deps.Observe != nil {
			deps.Observe.ObserveRequest(r.Method, r.URL.Path, status, duration, correlationID)
		}
		slog.Info("api request",
			"correlation_id", correlationID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", float64(duration.Microseconds())/1000,
			"remote_addr", requestRemoteAddr(r),
		)
	})
}

func withResolvedSession(deps Deps, next http.Handler) http.Handler {
	if deps.Auth == nil || deps.Auth.Disabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookieToken := ""
		if cookie, err := r.Cookie(auth.SessionCookieName); err == nil {
			cookieToken = strings.TrimSpace(cookie.Value)
		}
		headerToken := strings.TrimSpace(r.Header.Get("X-Auth-Token"))
		if headerToken == "" {
			authz := strings.TrimSpace(r.Header.Get("Authorization"))
			if authz != "" {
				parts := strings.SplitN(authz, " ", 2)
				if len(parts) == 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "Bearer") {
					headerToken = strings.TrimSpace(parts[1])
				}
			}
		}
		if cookieToken == "" && headerToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		resolve := func(token string) (auth.ResolvedSession, error) {
			return deps.Auth.Resolve(r.Context(), token, requestRemoteAddr(r), r.UserAgent())
		}
		resolved := auth.ResolvedSession{}
		var err error
		// injectProfile attempts to add an active-profile user ID to the context
		// when the client provides a valid X-Profile-Token header.
		injectProfile := func(ctx context.Context, sessionID string) context.Context {
			profileToken := strings.TrimSpace(r.Header.Get("X-Profile-Token"))
			if profileToken == "" {
				return ctx
			}
			profileUserID, err := deps.Auth.ValidateProfileToken(ctx, profileToken, sessionID)
			if err != nil {
				return ctx // expired/invalid — just ignore, don't fail the request
			}
			return auth.ContextWithActiveProfile(ctx, profileUserID)
		}

		if cookieToken != "" {
			resolved, err = resolve(cookieToken)
			if err == nil {
				if resolved.Rotated {
					writeAuthCookies(w, r, resolved)
				}
				ctx := auth.ContextWithResolvedSession(r.Context(), resolved)
				ctx = injectProfile(ctx, resolved.Session.ID)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		if headerToken != "" {
			resolved, err = resolve(headerToken)
			if err == nil {
				if resolved.Rotated {
					writeAuthCookies(w, r, resolved)
				}
				w.Header().Set("X-Auth-Token", resolved.Token)
				ctx := auth.ContextWithResolvedSession(r.Context(), resolved)
				ctx = injectProfile(ctx, resolved.Session.ID)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		// Both candidate tokens failed. Continue unauthenticated and let protected
		// handlers decide response; avoid clearing cookies for transient token races.
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requireAuth(deps Deps, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			next(w, r)
			return
		}
		if _, ok := auth.ResolvedSessionFromContext(r.Context()); !ok {
			// Avoid clearing cookies when a header token is present. That path can be
			// hit by transient resolve failures (for example a momentary DB busy write)
			// and should not force a full sign-out.
			if !hasHeaderAuthToken(r) {
				clearAuthCookies(w)
			}
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}

func requireAuthCSRF(deps Deps, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(deps, func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			next(w, r)
			return
		}
		if hasHeaderAuthToken(r) {
			next(w, r)
			return
		}
		resolved, _ := auth.ResolvedSessionFromContext(r.Context())
		csrfCookie, _ := r.Cookie(auth.CSRFCookieName)
		csrfCookieValue := ""
		if csrfCookie != nil {
			csrfCookieValue = csrfCookie.Value
		}
		if err := deps.Auth.ValidateCSRF(resolved, csrfCookieValue, r.Header.Get("X-CSRF-Token")); err != nil {
			writeError(w, http.StatusForbidden, "valid csrf token required")
			return
		}
		next(w, r)
	})
}

func authLoginHandler(deps Deps) http.HandlerFunc {
	type request struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		TrustDevice bool   `json:"trustDevice,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			writeJSON(w, http.StatusOK, map[string]any{"authDisabled": true})
			return
		}
		var payload request
		if !decodeJSON(w, r, &payload) {
			return
		}
		// TrustDevice=true issues a 90-day session instead of the default 24h.
		// The cookie's Max-Age is derived from session.ExpiresAt in
		// writeAuthCookies, so the cookie + server-side session stay in sync.
		principal, session, token, err := deps.Auth.AuthenticateWithTrust(r.Context(), payload.Username, payload.Password, requestRemoteAddr(r), r.UserAgent(), payload.TrustDevice)
		if err != nil {
			publishAuthAudit(deps, r, payload.Username, "", "", "login", "denied")
			if until, ok := auth.LockoutUntil(err); ok {
				writeJSON(w, http.StatusTooManyRequests, map[string]any{
					"error":       "too many invalid login attempts",
					"lockedUntil": until.Format(time.RFC3339),
				})
				return
			}
			if errors.Is(err, auth.ErrInvalidCredentials) {
				writeError(w, http.StatusUnauthorized, "invalid username or password")
				return
			}
			writeError(w, http.StatusInternalServerError, "login failed")
			return
		}
		publishAuthAudit(deps, r, principal.Username, principal.ID, principal.Role, "login", "allowed")
		writeAuthCookies(w, r, auth.ResolvedSession{Principal: principal, Session: session, Token: token})
		writeJSON(w, http.StatusOK, map[string]any{
			"user": map[string]any{
				"id":          principal.ID,
				"username":    principal.Username,
				"displayName": principal.DisplayName,
				"avatarUrl":   principal.AvatarURL,
				"role":        principal.Role,
			},
			"session": map[string]any{
				"id":        session.ID,
				"expiresAt": session.ExpiresAt.Format(time.RFC3339),
			},
			"sessionToken": token,
		})
	}
}

func authBootstrapHandler(deps Deps) http.HandlerFunc {
	type request struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"displayName"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			writeJSON(w, http.StatusOK, map[string]any{"authDisabled": true})
			return
		}
		var payload request
		if !decodeJSON(w, r, &payload) {
			return
		}
		principal, err := deps.Auth.BootstrapUser(r.Context(), auth.BootstrapOptions{
			Username:    payload.Username,
			Password:    payload.Password,
			DisplayName: payload.DisplayName,
		})
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrBootstrapComplete):
				writeError(w, http.StatusConflict, "an account already exists; sign in")
			case strings.Contains(strings.ToLower(err.Error()), "password"), strings.Contains(strings.ToLower(err.Error()), "required"):
				writeError(w, http.StatusBadRequest, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, "bootstrap failed")
			}
			return
		}
		createdPrincipal, session, token, err := deps.Auth.Authenticate(r.Context(), principal.Username, payload.Password, requestRemoteAddr(r), r.UserAgent())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "bootstrap sign-in failed")
			return
		}
		publishAuthAudit(deps, r, createdPrincipal.Username, createdPrincipal.ID, createdPrincipal.Role, "bootstrap", "allowed")
		writeAuthCookies(w, r, auth.ResolvedSession{Principal: createdPrincipal, Session: session, Token: token})
		writeJSON(w, http.StatusCreated, map[string]any{
			"user": map[string]any{
				"id":          createdPrincipal.ID,
				"username":    createdPrincipal.Username,
				"displayName": createdPrincipal.DisplayName,
				"avatarUrl":   createdPrincipal.AvatarURL,
				"role":        createdPrincipal.Role,
			},
			"session": map[string]any{
				"id":        session.ID,
				"expiresAt": session.ExpiresAt.Format(time.RFC3339),
			},
			"sessionToken": token,
		})
	}
}

func publishAuthAudit(deps Deps, r *http.Request, username string, userID string, role string, action string, result string) {
	if deps.Events == nil {
		return
	}
	deps.Events.Publish("audit.auth", map[string]any{
		"correlationId": observability.CorrelationID(r.Context()),
		"userId":        userID,
		"username":      strings.ToLower(strings.TrimSpace(username)),
		"role":          role,
		"method":        r.Method,
		"path":          r.URL.Path,
		"action":        action,
		"result":        result,
		"createdAt":     time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func publishDomainAudit(deps Deps, r *http.Request, eventType string, action string, result string, fields map[string]any) {
	if deps.Events == nil {
		return
	}
	payload := map[string]any{
		"correlationId": observability.CorrelationID(r.Context()),
		"method":        r.Method,
		"path":          r.URL.Path,
		"action":        action,
		"result":        result,
		"createdAt":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	if resolved, ok := auth.ResolvedSessionFromContext(r.Context()); ok {
		payload["userId"] = resolved.Principal.ID
		payload["username"] = resolved.Principal.Username
		payload["role"] = resolved.Principal.Role
	}
	for key, value := range fields {
		payload[key] = value
	}
	deps.Events.Publish(eventType, payload)
}

func authSessionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			writeJSON(w, http.StatusOK, map[string]any{"authDisabled": true})
			return
		}
		resolved, ok := auth.ResolvedSessionFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		// Re-issue the session cookie on every successful session check. This
		// keeps the cookie + Authorization-header views of auth state in sync
		// even when the client carries a valid bearer token but the cookie has
		// gone stale (browser cleared cookies, cookie TTL elapsed while the
		// localStorage token persisted, etc.). Without this, a client with a
		// valid token but a missing/expired cookie can pass /api/auth/session,
		// then call `window.location.href = '/'` and get 307'd back to /signin
		// by the root handler's `if !isSignedIn` branch — the v0.0.36 redirect
		// flicker loop. Re-stamping the cookie here closes that gap before
		// the full-page navigation fires.
		writeAuthCookies(w, r, resolved)
		payload := map[string]any{
			"user": map[string]any{
				"id":          resolved.Principal.ID,
				"username":    resolved.Principal.Username,
				"displayName": resolved.Principal.DisplayName,
				"avatarUrl":   resolved.Principal.AvatarURL,
				"role":        resolved.Principal.Role,
			},
		}
		payload["session"] = map[string]any{
			"id":        resolved.Session.ID,
			"expiresAt": resolved.Session.ExpiresAt.Format(time.RFC3339),
		}
		payload["csrfToken"] = resolved.Session.CSRFToken
		if prefs, err := deps.Auth.GetUserPreferences(r.Context(), resolved.Principal.ID); err == nil {
			payload["preferences"] = prefs
		}
		// Include active profile if a profile token was presented on this request.
		if profileUserID, ok := auth.ActiveProfileFromContext(r.Context()); ok {
			profiles, _ := deps.Auth.ListProfiles(r.Context())
			for _, p := range profiles {
				if p.ID == profileUserID {
					payload["activeProfile"] = p
					break
				}
			}
		}
		writeJSON(w, http.StatusOK, payload)
	}
}

func authLogoutHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth != nil && !deps.Auth.Disabled() {
			if resolved, ok := auth.ResolvedSessionFromContext(r.Context()); ok {
				_ = deps.Auth.RevokeProfileSessions(r.Context(), resolved.Session.ID)
			}
			if cookie, err := r.Cookie(auth.SessionCookieName); err == nil && cookie.Value != "" {
				_ = deps.Auth.Revoke(r.Context(), cookie.Value)
			}
		}
		clearAuthCookies(w)
		writeJSON(w, http.StatusOK, map[string]any{"status": "logged_out"})
	}
}

func usersListHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			writeError(w, http.StatusServiceUnavailable, "user accounts are not available")
			return
		}
		users, err := deps.Auth.ListUsers(r.Context())
		if err != nil {
			if errors.Is(err, auth.ErrUnauthorized) {
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			writeError(w, http.StatusInternalServerError, "user list failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": users})
	}
}

func usersCreateHandler(deps Deps) http.HandlerFunc {
	type request struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"displayName"`
		Role        string `json:"role"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			writeError(w, http.StatusServiceUnavailable, "user accounts are not available")
			return
		}
		var payload request
		if !decodeJSON(w, r, &payload) {
			return
		}
		principal, err := deps.Auth.CreateUser(r.Context(), payload.Username, payload.Password, payload.DisplayName, payload.Role)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrUnauthorized):
				writeError(w, http.StatusUnauthorized, "authentication required")
			case strings.Contains(strings.ToLower(err.Error()), "users.username"):
				writeError(w, http.StatusConflict, "username already exists")
			case strings.Contains(strings.ToLower(err.Error()), "required"), strings.Contains(strings.ToLower(err.Error()), "password"):
				writeError(w, http.StatusBadRequest, err.Error())
			default:
				writeError(w, http.StatusBadRequest, "user create failed")
			}
			return
		}
		publishDomainAudit(deps, r, "audit.auth", "user.create", "allowed", map[string]any{
			"targetUserId":   principal.ID,
			"targetUsername": principal.Username,
			"targetRole":     principal.Role,
		})
		writeJSON(w, http.StatusCreated, map[string]any{
			"user": map[string]any{
				"id":          principal.ID,
				"username":    principal.Username,
				"displayName": principal.DisplayName,
				"avatarUrl":   principal.AvatarURL,
				"role":        principal.Role,
			},
		})
	}
}

func usersUpdateHandler(deps Deps) http.HandlerFunc {
	type request struct {
		DisplayName  string `json:"displayName"`
		AvatarURL    string `json:"avatarUrl"`
		AvatarPreset string `json:"avatarPreset"`
		AvatarColor  string `json:"avatarColor"`
		IsRestricted *bool  `json:"isRestricted"` // pointer so omitted = unchanged
		MaxRating    string `json:"maxRating"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			writeError(w, http.StatusServiceUnavailable, "user accounts are not available")
			return
		}
		userID := strings.TrimSpace(r.PathValue("id"))
		if userID == "" {
			writeError(w, http.StatusBadRequest, "user id is required")
			return
		}
		var payload request
		if !decodeJSON(w, r, &payload) {
			return
		}
		avatarURL, err := normalizeAvatarURL(payload.AvatarURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// If any profile-specific field is present, use UpdateProfileSettings.
		isRestricted := false
		if payload.IsRestricted != nil {
			isRestricted = *payload.IsRestricted
		}
		account, err := deps.Auth.UpdateProfileSettings(
			r.Context(), userID,
			payload.DisplayName, avatarURL, payload.AvatarPreset,
			payload.AvatarColor, isRestricted, payload.MaxRating,
		)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrUnauthorized):
				writeError(w, http.StatusUnauthorized, "authentication required")
			case errors.Is(err, auth.ErrUserNotFound):
				writeError(w, http.StatusNotFound, "user not found")
			case strings.Contains(strings.ToLower(err.Error()), "display name"):
				writeError(w, http.StatusBadRequest, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, "user update failed")
			}
			return
		}
		publishDomainAudit(deps, r, "audit.auth", "user.update", "allowed", map[string]any{
			"targetUserId": account.ID,
		})
		writeJSON(w, http.StatusOK, map[string]any{"user": account})
	}
}

func usersSetPinHandler(deps Deps) http.HandlerFunc {
	type request struct {
		Pin string `json:"pin"` // empty string = clear pin
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			writeError(w, http.StatusServiceUnavailable, "user accounts are not available")
			return
		}
		userID := strings.TrimSpace(r.PathValue("id"))
		if userID == "" {
			writeError(w, http.StatusBadRequest, "user id is required")
			return
		}
		var payload request
		if !decodeJSON(w, r, &payload) {
			return
		}
		if err := deps.Auth.SetProfilePin(r.Context(), userID, payload.Pin); err != nil {
			switch {
			case errors.Is(err, auth.ErrUserNotFound):
				writeError(w, http.StatusNotFound, "user not found")
			default:
				writeError(w, http.StatusInternalServerError, "pin update failed")
			}
			return
		}
		publishDomainAudit(deps, r, "audit.auth", "user.pin.update", "allowed", map[string]any{
			"targetUserId": userID,
		})
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	}
}

func usersDeleteHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			writeError(w, http.StatusServiceUnavailable, "user accounts are not available")
			return
		}
		userID := strings.TrimSpace(r.PathValue("id"))
		resolved, _ := auth.ResolvedSessionFromContext(r.Context())
		if userID == "" {
			writeError(w, http.StatusBadRequest, "user id is required")
			return
		}
		if userID == resolved.Principal.ID {
			writeError(w, http.StatusBadRequest, "cannot delete the signed-in account")
			return
		}
		if err := deps.Auth.DeleteUser(r.Context(), userID); err != nil {
			switch {
			case errors.Is(err, auth.ErrUnauthorized):
				writeError(w, http.StatusUnauthorized, "authentication required")
			case errors.Is(err, auth.ErrUserNotFound):
				writeError(w, http.StatusNotFound, "user not found")
			case errors.Is(err, auth.ErrLastAdmin):
				writeError(w, http.StatusConflict, "cannot delete the last admin account")
			default:
				writeError(w, http.StatusInternalServerError, "user delete failed")
			}
			return
		}
		publishDomainAudit(deps, r, "audit.auth", "user.delete", "allowed", map[string]any{
			"targetUserId": userID,
		})
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
	}
}

func usersPasswordHandler(deps Deps) http.HandlerFunc {
	type request struct {
		Password string `json:"password"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Auth == nil || deps.Auth.Disabled() {
			writeError(w, http.StatusServiceUnavailable, "user accounts are not available")
			return
		}
		userID := strings.TrimSpace(r.PathValue("id"))
		if userID == "" {
			writeError(w, http.StatusBadRequest, "user id is required")
			return
		}
		var payload request
		if !decodeJSON(w, r, &payload) {
			return
		}
		if err := deps.Auth.SetUserPassword(r.Context(), userID, payload.Password); err != nil {
			switch {
			case errors.Is(err, auth.ErrUnauthorized):
				writeError(w, http.StatusUnauthorized, "authentication required")
			case errors.Is(err, auth.ErrUserNotFound):
				writeError(w, http.StatusNotFound, "user not found")
			case strings.Contains(strings.ToLower(err.Error()), "password"):
				writeError(w, http.StatusBadRequest, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, "password update failed")
			}
			return
		}
		publishDomainAudit(deps, r, "audit.auth", "user.password.update", "allowed", map[string]any{
			"targetUserId": userID,
		})
		responsePayload := map[string]any{"status": "updated"}
		if resolved, ok := auth.ResolvedSessionFromContext(r.Context()); ok && resolved.Principal.ID == userID {
			principal, session, token, err := deps.Auth.Authenticate(
				r.Context(),
				resolved.Principal.Username,
				payload.Password,
				requestRemoteAddr(r),
				r.UserAgent(),
			)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "password updated but session refresh failed")
				return
			}
			refreshed := auth.ResolvedSession{Principal: principal, Session: session, Token: token}
			writeAuthCookies(w, r, refreshed)
			w.Header().Set("X-Auth-Token", token)
			responsePayload["user"] = map[string]any{
				"id":          principal.ID,
				"username":    principal.Username,
				"displayName": principal.DisplayName,
				"avatarUrl":   principal.AvatarURL,
				"role":        principal.Role,
			}
			responsePayload["session"] = map[string]any{
				"id":        session.ID,
				"expiresAt": session.ExpiresAt.Format(time.RFC3339),
			}
			responsePayload["sessionToken"] = token
		}
		writeJSON(w, http.StatusOK, responsePayload)
	}
}

func normalizeAvatarURL(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 1024 {
		return "", errors.New("avatar URL is too long")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("avatar URL must be valid")
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if scheme != "http" && scheme != "https" {
		return "", errors.New("avatar URL must start with http:// or https://")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", errors.New("avatar URL host is required")
	}
	return parsed.String(), nil
}

func writeAuthCookies(w http.ResponseWriter, r *http.Request, resolved auth.ResolvedSession) {
	secure := requestSecure(r)
	maxAge := int(time.Until(resolved.Session.ExpiresAt).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    resolved.Token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CSRFCookieName,
		Value:    resolved.Session.CSRFToken,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

func clearAuthCookies(w http.ResponseWriter) {
	for _, name := range []string{auth.SessionCookieName, auth.CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: name == auth.SessionCookieName,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

func requestSecure(r *http.Request) bool {
	// Do not infer secure-cookie behavior from forwarded headers. In desktop and
	// dev topologies those headers can be present even when the browser origin is
	// plain HTTP localhost, which causes cookies to be dropped.
	return r.TLS != nil
}

func requestOrigin(r *http.Request) url.URL {
	return url.URL{Scheme: requestScheme(r), Host: strings.TrimSpace(r.Host)}
}

func requestScheme(r *http.Request) string {
	if requestSecure(r) {
		return "https"
	}
	return "http"
}

func canonicalWebOriginForRequest(r *http.Request, cfg config.Config) (url.URL, bool) {
	if explicit, err := config.NormalizeWebOrigin(cfg.CanonicalWebOrigin); err == nil && explicit != "" {
		parsed, err := url.Parse(explicit)
		if err == nil && parsed.Scheme != "" && parsed.Host != "" {
			return *parsed, true
		}
	}

	host, port := splitRequestHost(r.Host)
	if port == "" {
		_, port, _ = net.SplitHostPort(strings.TrimSpace(cfg.HTTPAddr))
	}
	if port == "" {
		port = "8097"
	}

	// `localhost` over a non-loopback bind is rewritten to the OS hostname so
	// the URL bar reads sensibly (e.g. http://media-server:8097). Bare-IP
	// requests are intentionally NOT rewritten — a client that resolved the
	// server via mDNS-advertised IP shouldn't be bounced to a hostname it
	// might not be able to resolve. (This was breaking Apple TV discovery.)
	if !config.HTTPAddrLoopbackOnly(cfg.HTTPAddr) && host == "localhost" {
		name := osHostnameForURL()
		if name == "" {
			return url.URL{}, false
		}
		return url.URL{Scheme: requestScheme(r), Host: net.JoinHostPort(name, port)}, true
	}

	// Loopback shortcut: 127.0.0.1 → localhost so dev tools render a friendly
	// URL. Only applied when the bind is loopback-only; otherwise the LAN-IP
	// branch above already handled the visible case.
	if host == "127.0.0.1" || host == "::1" {
		return url.URL{Scheme: requestScheme(r), Host: net.JoinHostPort("localhost", port)}, true
	}

	// Any other request — bare IP, hostname, ::1 already handled — passes
	// through as-is. The canonicalWebOrigin override remains the escape
	// hatch for operators who want a forced canonical hostname.
	return url.URL{}, false
}

func canonicalWebOriginString(r *http.Request, cfg config.Config) string {
	origin, ok := canonicalWebOriginForRequest(r, cfg)
	if !ok {
		return requestBaseURL(r, cfg.HTTPAddr)
	}
	return origin.String()
}

func clientWebOriginString(r *http.Request, cfg config.Config) string {
	if explicit, err := config.NormalizeWebOrigin(cfg.CanonicalWebOrigin); err == nil && explicit != "" {
		return explicit
	}
	return requestBaseURL(r, cfg.HTTPAddr)
}

func clientConnectionHostName(r *http.Request, cfg config.Config) string {
	parsed, err := url.Parse(clientWebOriginString(r, cfg))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Hostname())
}

func sameOrigin(a url.URL, b url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func osHostnameForURL() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	name = strings.TrimSpace(strings.TrimSuffix(name, "."))
	if name == "" || strings.ContainsAny(name, "/\\:?#[]@") {
		return ""
	}
	return name
}

func splitRequestHost(hostport string) (string, string) {
	host := strings.TrimSpace(hostport)
	if host == "" {
		return "", ""
	}
	if parsedHost, port, err := net.SplitHostPort(host); err == nil {
		return strings.ToLower(strings.Trim(strings.TrimSpace(parsedHost), "[]")), port
	}
	return strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]")), ""
}

func hasHeaderAuthToken(r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("X-Auth-Token")) != "" {
		return true
	}
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz == "" {
		return false
	}
	parts := strings.SplitN(authz, " ", 2)
	return len(parts) == 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "Bearer") && strings.TrimSpace(parts[1]) != ""
}

func queueRejectionStatus(err error, fallback int) int {
	if errors.Is(err, jobs.ErrQueueSaturated) {
		return http.StatusTooManyRequests
	}
	return fallback
}

func requestHostIsLoopback(r *http.Request) bool {
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" && r.URL != nil {
		host = strings.TrimSpace(r.URL.Host)
	}
	return isLoopbackHost(host)
}

func isLoopbackHost(hostport string) bool {
	host := strings.TrimSpace(hostport)
	if host == "" {
		return false
	}
	parsedHost := host
	if value, _, err := net.SplitHostPort(host); err == nil {
		parsedHost = value
	}
	parsedHost = strings.TrimSpace(strings.Trim(parsedHost, "[]"))
	if parsedHost == "" {
		return false
	}
	lower := strings.ToLower(parsedHost)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return true
	}
	ip := net.ParseIP(parsedHost)
	return ip != nil && ip.IsLoopback()
}

func requestRemoteAddr(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		return strings.Split(forwarded, ",")[0]
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func healthHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		payload, _ := healthSnapshot(deps)
		writeJSON(w, http.StatusOK, payload)
	}
}

func readinessHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		payload, degraded := healthSnapshot(deps)
		status := http.StatusOK
		if degraded {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, payload)
	}
}

func systemVersionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info := buildinfo.Current()
		schemaVersion := ""
		if deps.Database != nil {
			if value, err := deps.Database.SchemaVersion(r.Context()); err == nil {
				schemaVersion = value
			}
		}
		startedAt := deps.StartedAt
		if startedAt.IsZero() {
			startedAt = time.Now().UTC()
		}
		payload := map[string]any{
			"product":       "xuva",
			"version":       info.Version,
			"commit":        info.Commit,
			"buildDate":     info.Date,
			"schemaVersion": schemaVersion,
			"goVersion":     runtime.Version(),
			"startedAt":     startedAt.UTC().Format(time.RFC3339),
		}
		// Surface the installer receipt that the NSIS customInstall macro
		// writes. A single GET answers "did the installer hook actually
		// fire, did it have admin rights?" — much better than
		// round-tripping through Windows Firewall state, which can't tell
		// "installer added the rule" apart from "user added the rule
		// manually". Missing on non-Windows builds or when the user
		// installed via the zip target — both correctly absent.
		if receipt := readInstallReceipt(); receipt != nil {
			payload["installReceipt"] = receipt
		}
		writeJSON(w, http.StatusOK, payload)
	}
}

// installReceiptPath is the canonical location the NSIS customInstall macro
// writes its JSON receipt to. Hard-coded because (a) ProgramData is the only
// writable location that survives reinstalls and isn't tied to a per-user
// install dir, and (b) the path is locale-invariant on every Windows install
// so we don't need the running process to know where it was installed from
// in order to find the receipt.
const installReceiptPath = `C:\ProgramData\Xuva\install-receipt.json`

func readInstallReceipt() map[string]any {
	data, err := os.ReadFile(installReceiptPath)
	if err != nil {
		return nil
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil
	}
	return receipt
}

func metricsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requests := []observability.RequestMetric{}
		events := []observability.EventMetric{}
		if deps.Observe != nil {
			requests = deps.Observe.Requests()
			events = deps.Observe.Events()
		}
		queues := []map[string]any{}
		timeline := []observability.TimelineEntry{}
		if deps.Jobs != nil {
			queues = deps.Jobs.Snapshot()
		}
		if deps.Observe != nil {
			timeline = deps.Observe.Recent(60)
		}
		playbackSLO := observability.PlaybackSLOMetrics{}
		if deps.Observe != nil {
			playbackSLO = deps.Observe.PlaybackSLO()
		}
		alerts := observability.EvaluateAlerts(queues, requests)
		alerts = append(alerts, observability.EvaluatePlaybackSLOAlerts(playbackSLO)...)
		writeJSON(w, http.StatusOK, map[string]any{
			"requests":    requests,
			"queues":      queues,
			"events":      events,
			"timeline":    timeline,
			"playbackSLO": playbackSLO,
			"outcomes": map[string]any{
				"sessions":  sessionOutcomeCounts(deps),
				"transcode": transcodeOutcomeCounts(deps),
				"downloads": downloadOutcomeCounts(deps),
				"probes":    probeOutcomeCounts(deps),
			},
			"alerts": alerts,
		})
	}
}

func clientBootstrapHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := currentConfig(deps)
		profileID := firstNonEmpty(r.URL.Query().Get("clientProfile"), "apple-tv")
		profile, ok := deps.Devices.GetProfile(profileID)
		if !ok {
			profile, _ = deps.Devices.GetProfile("apple-tv")
		}
		startedAt := deps.StartedAt
		if startedAt.IsZero() {
			startedAt = time.Now().UTC()
		}
		authRequired := deps.Auth != nil && !deps.Auth.Disabled()
		bootstrapAllowed := false
		defaultAdminUsername := strings.TrimSpace(cfg.AdminUsername)
		if defaultAdminUsername == "" {
			defaultAdminUsername = "admin"
		}
		if authRequired {
			if required, err := deps.Auth.RequiresBootstrap(r.Context()); err == nil {
				bootstrapAllowed = required
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"server": map[string]any{
				"product":            "xuva",
				"name":               configDisplayName(cfg.ServerName),
				"displayName":        configDisplayName(cfg.ServerName),
				"hostName":           clientConnectionHostName(r, cfg),
				"baseUrl":            requestBaseURL(r, cfg.HTTPAddr),
				"webUrl":             clientWebOriginString(r, cfg),
				"canonicalWebOrigin": cfg.CanonicalWebOrigin,
				"httpAddr":           cfg.HTTPAddr,
				"lanAddresses":       lanAddresses(cfg.HTTPAddr),
				"startedAt":          startedAt.UTC().Format(time.RFC3339),
			},
			"auth": map[string]any{
				"required":          authRequired,
				"bootstrapAllowed":  bootstrapAllowed,
				"defaultUsername":   defaultAdminUsername,
				"bootstrapEndpoint": "/api/auth/bootstrap",
				"methods":           []string{"session_cookie", "local_pairing_code"},
			},
			"client": map[string]any{
				"requestedProfile": profileID,
				"profile":          profile,
			},
			"profiles": deps.Devices.Profiles(),
			"features": map[string]any{
				"directPlay":        true,
				"hlsAdaptive":       true,
				"resume":            true,
				"watchedState":      true,
				"trackSelection":    true,
				"devicePairing":     "local_code",
				"remoteDiagnostics": true,
				"vendorRelay":       false,
				"trailers":          !cfg.DisableTrailers,
			},
			"endpoints": map[string]string{
				"health":           "/api/health",
				"authSession":      "/api/auth/session",
				"login":            "/api/auth/login",
				"pairingCreate":    "/api/pairing/requests",
				"pairingStatus":    "/api/pairing/requests/{id}",
				"clientHome":       "/api/client/home",
				"deviceProfiles":   "/api/devices/profiles",
				"movies":           "/api/movies",
				"series":           "/api/series",
				"playbackDecision": "/api/playback/decision",
				"playbackRoute":    "/api/playback/route",
				"sessions":         "/api/sessions",
				"playbackState":    "/api/playback/state/{mediaSourceId}",
				"streamToken":      "/api/media-sources/{id}/stream-token",
				"directStream":     "/api/media-sources/{id}/stream",
				"adaptiveMaster":   "/api/media-sources/{id}/adaptive/master.m3u8",
				"adaptiveSession":  "/api/media-sources/{id}/adaptive/session",
				"tracks":           "/api/media-sources/{id}/tracks",
				"subtitles":        "/api/media-sources/{id}/subtitles",
				"remoteAccess":     "/api/remote/access",
			},
		})
	}
}

func discoveryStatusHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := currentConfig(deps)
		status := discovery.Status{
			Enabled:     cfg.DiscoveryEnabled,
			ServiceName: configDisplayName(cfg.ServerName),
			ServiceType: "_xuva._tcp.local.",
			HostName:    clientConnectionHostName(r, cfg),
			WebURL:      clientWebOriginString(r, cfg),
			Note:        "Local discovery is not running.",
		}
		if deps.Discovery != nil {
			status = deps.Discovery.Status()
			if status.HostName == "" {
				status.HostName = clientConnectionHostName(r, cfg)
			}
			if status.WebURL == "" {
				status.WebURL = clientWebOriginString(r, cfg)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":      status.Enabled,
			"running":      status.Running,
			"serviceName":  status.ServiceName,
			"serviceType":  status.ServiceType,
			"hostName":     status.HostName,
			"webUrl":       status.WebURL,
			"port":         status.Port,
			"interfaces":   status.Interfaces,
			"advertiseIps": status.AdvertiseIPs,
			"txtRecords":   status.TXTRecords,
			"lastError":    status.LastError,
			"note":         status.Note,
		})
	}
}

func healthSnapshot(deps Deps) (map[string]any, bool) {
	startedAt := deps.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	checks := map[string]any{}
	degraded := false
	for name, path := range runtimePaths(deps.Config) {
		if path == "" {
			continue
		}
		ok, message := pathReady(path)
		checks["path."+name] = map[string]any{"ok": ok, "message": message}
		if !ok {
			degraded = true
		}
	}
	if deps.Jobs != nil {
		for _, queue := range deps.Jobs.Snapshot() {
			name, _ := queue["name"].(string)
			workers := intFromAny(queue["workers"])
			active := intFromAny(queue["active"])
			queued := intFromAny(queue["queued"])
			ok := workers <= 0 || active < workers || queued == 0
			checks["queue."+name] = map[string]any{
				"ok":      ok,
				"workers": workers,
				"active":  active,
				"queued":  queued,
			}
			if !ok {
				degraded = true
			}
		}
	}
	status := "ok"
	if degraded {
		status = "degraded"
	}
	return map[string]any{
		"status":    status,
		"service":   "xuva-server",
		"startedAt": startedAt.UTC().Format(time.RFC3339),
		"httpAddr":  deps.Config.HTTPAddr,
		"checks":    checks,
		"libraries": map[string]string{
			"movies": deps.Config.MovieLibraryPath,
			"tv":     deps.Config.TVLibraryPath,
		},
	}, degraded
}

func pathReady(path string) (bool, string) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err.Error()
	}
	if !info.IsDir() {
		return false, "path is not a directory"
	}
	testPath, ok := safeChildPath(path, ".xuva-healthcheck")
	if !ok {
		return false, "path cannot be checked safely"
	}
	file, err := os.OpenFile(testPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false, err.Error()
	}
	_ = file.Close()
	_ = os.Remove(testPath)
	return true, "ready"
}

func sessionOutcomeCounts(deps Deps) map[string]int {
	output := map[string]int{}
	if deps.Sessions == nil {
		return output
	}
	for _, session := range deps.Sessions.List() {
		key := strings.TrimSpace(session.Status)
		if key == "" {
			key = "unknown"
		}
		output[key]++
	}
	return output
}

func transcodeOutcomeCounts(deps Deps) map[string]int {
	output := map[string]int{}
	if deps.Transcode == nil {
		return output
	}
	for _, job := range deps.Transcode.List() {
		output[string(job.Status)]++
	}
	return output
}

func downloadOutcomeCounts(deps Deps) map[string]int {
	output := map[string]int{}
	if deps.Downloads == nil {
		return output
	}
	for _, job := range deps.Downloads.List() {
		output[string(job.Status)]++
	}
	return output
}

func probeOutcomeCounts(deps Deps) map[string]int {
	output := map[string]int{}
	if deps.Probes == nil {
		return output
	}
	for _, job := range deps.Probes.List() {
		output[string(job.Status)]++
	}
	return output
}

func intFromAny(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func architectureHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"transports": []string{"HTTP/JSON", "SSE", "WebSocket later for two-way control"},
			"domains":    []string{"libraries", "scanner", "media", "movies", "tv", "playback", "subtitles", "devices", "sessions", "downloads"},
			"workloads":  deps.Resources.Classes(),
			"queues":     deps.Jobs.Snapshot(),
		})
	}
}

func librariesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := currentConfig(deps)
		writeJSON(w, http.StatusOK, map[string]any{
			"libraries":       deps.Libraries.List(),
			"metadataSources": metadataSourceCatalogPayload(r.Context(), cfg, deps.Metadata),
		})
	}
}

func librarySaveHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request libraries.Library
		if !decodeJSON(w, r, &request) {
			return
		}
		if len(request.MetadataSources) == 0 {
			request.MetadataSources = defaultMetadataSourcePreferenceForLibrary(currentConfig(deps), request.Kind)
		}
		if len(request.ArtworkSources) == 0 {
			request.ArtworkSources = defaultArtworkSourcePreferenceForLibrary(currentConfig(deps), request.Kind)
		}
		request.MetadataSources = metasources.NormalizeRequestedSourceOrder(string(request.Kind), request.MetadataSources)
		request.ArtworkSources = metasources.NormalizeRequestedArtworkOrder(string(request.Kind), request.ArtworkSources)
		library, err := deps.Catalog.SaveLibrary(r.Context(), request)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		deps.Libraries.Set(library)
		deps.Events.Publish("library.updated", library)
		publishDomainAudit(deps, r, "audit.library", "library.save", "allowed", map[string]any{
			"libraryId":   library.ID,
			"libraryKind": string(library.Kind),
			"storageType": string(library.StorageType),
		})
		writeJSON(w, http.StatusOK, library)
	}
}

func libraryDeleteHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := deps.Catalog.DeleteLibrary(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, "library delete failed")
			return
		}
		deps.Libraries.Delete(id)
		deps.Events.Publish("library.deleted", map[string]string{"id": id})
		publishDomainAudit(deps, r, "audit.library", "library.delete", "allowed", map[string]any{
			"libraryId": id,
		})
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	}
}

func libraryScanByIDHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		library, ok := deps.Libraries.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "library not found")
			return
		}
		kind := scans.KindMovies
		if library.Kind == libraries.KindTV {
			kind = scans.KindTV
		}
		job, err := deps.Scans.Start(r.Context(), scans.Request{Kind: kind, LibraryID: library.ID, Path: library.Path})
		if err != nil {
			writeError(w, queueRejectionStatus(err, http.StatusBadRequest), err.Error())
			return
		}
		publishDomainAudit(deps, r, "audit.library", "library.scan", "allowed", map[string]any{
			"libraryId":   library.ID,
			"libraryKind": string(library.Kind),
			"jobId":       job.ID,
		})
		writeJSON(w, http.StatusAccepted, job)
	}
}

func catalogSummaryHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		summary, err := deps.Catalog.Summary(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "catalog summary failed")
			return
		}
		writeJSON(w, http.StatusOK, summary)
	}
}

func catalogHealthHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		health, err := deps.Catalog.Health(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "catalog health failed")
			return
		}
		writeJSON(w, http.StatusOK, health)
	}
}

// catalogCodecsHandler returns the per-codec breakdown the dashboard uses
// to show "what's in your library" — see catalog.Codecs() doc.
func catalogCodecsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		breakdown, err := deps.Catalog.Codecs(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "catalog codec breakdown failed")
			return
		}
		writeJSON(w, http.StatusOK, breakdown)
	}
}

// catalogPlayabilityAuditHandler runs playback decisions for every probed
// media source against the three canonical device profiles (web, apple-tv,
// android-tv) and returns aggregated mode counts + top reason codes. The
// results help operators understand what fraction of their library requires
// transcoding and why.
func catalogPlayabilityAuditHandler(deps Deps) http.HandlerFunc {
	type profileBreakdown struct {
		DirectPlay       int            `json:"directPlay"`
		Remux            int            `json:"remux"`
		AudioTranscode   int            `json:"audioTranscode"`
		VideoTranscode   int            `json:"videoTranscode"`
		Other            int            `json:"other"`
		Total            int            `json:"total"`
		VideoActions     map[string]int `json:"videoActions"`     // "direct","copy","transcode","adaptive"
		AudioActions     map[string]int `json:"audioActions"`     // "direct","transcode","copy_or_transcode"
		ContainerActions map[string]int `json:"containerActions"` // "direct","remux","transcode"
	}

	type reasonEntry struct {
		ReasonCode      string `json:"reasonCode"`
		ReasonText      string `json:"reasonText"`
		Profile         string `json:"profile"`
		Count           int    `json:"count"`
		VideoAction     string `json:"videoAction,omitempty"`
		AudioAction     string `json:"audioAction,omitempty"`
		ContainerAction string `json:"containerAction,omitempty"`
		ComponentLabel  string `json:"componentLabel,omitempty"`
	}

	type auditResponse struct {
		TotalProbed int                          `json:"totalProbed"`
		ByProfile   map[string]*profileBreakdown `json:"byProfile"`
		TopReasons  []reasonEntry                `json:"topReasons"`
	}

	profiles := []string{"web", "apple-tv", "android-tv"}

	return func(w http.ResponseWriter, r *http.Request) {
		sources, err := deps.Catalog.ListMediaSources(r.Context(), 0, false)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "catalog listing failed")
			return
		}

		result := &auditResponse{
			ByProfile: make(map[string]*profileBreakdown, len(profiles)),
		}
		for _, p := range profiles {
			result.ByProfile[p] = &profileBreakdown{
				VideoActions:     make(map[string]int),
				AudioActions:     make(map[string]int),
				ContainerActions: make(map[string]int),
			}
		}
		// reason key = profile+"|"+reasonCode
		reasonCounts := map[string]*reasonEntry{}

		for i := range sources {
			src := &sources[i]
			if !src.Probed {
				continue
			}
			result.TotalProbed++
			for _, profile := range profiles {
				decision := deps.Playback.DecideSource(r.Context(), playback.Request{
					MediaSourceID: src.ID,
					ClientProfile: profile,
					RouteType:     "remote",
				}, playback.SourceFacts{
					MediaSourceID: src.ID,
					Container:     src.Container,
					VideoCodec:    src.VideoCodec,
					Width:         src.Width,
					Height:        src.Height,
					Bitrate:       src.Bitrate,
					AudioStreams:  src.AudioStreams,
					Probed:        src.Probed,
				})
				bd := result.ByProfile[profile]
				bd.Total++
				switch decision.Mode {
				case playback.DirectPlay, playback.AdaptiveStream:
					bd.DirectPlay++
				case playback.Remux:
					bd.Remux++
				case playback.AudioTranscode:
					bd.AudioTranscode++
				case playback.VideoTranscode, playback.SubtitleBurn:
					bd.VideoTranscode++
				default:
					bd.Other++
				}
				// Per-component action counts
				if decision.VideoAction != "" {
					bd.VideoActions[decision.VideoAction]++
				}
				if decision.AudioAction != "" {
					bd.AudioActions[decision.AudioAction]++
				}
				if decision.ContainerAction != "" {
					bd.ContainerActions[decision.ContainerAction]++
				}
				// Collect top reasons (only for non-direct decisions)
				if decision.ReasonCode != "" && decision.Mode != playback.DirectPlay && decision.Mode != playback.AdaptiveStream && decision.Mode != playback.DecisionDeferred {
					key := profile + "|" + decision.ReasonCode
					if e, ok := reasonCounts[key]; ok {
						e.Count++
					} else {
						reasonCounts[key] = &reasonEntry{
							ReasonCode:      decision.ReasonCode,
							ReasonText:      decision.ReasonText,
							Profile:         profile,
							Count:           1,
							VideoAction:     decision.VideoAction,
							AudioAction:     decision.AudioAction,
							ContainerAction: decision.ContainerAction,
							ComponentLabel:  auditComponentLabel(decision),
						}
					}
				}
			}
		}

		// Sort reasons by count descending; take top 20.
		reasons := make([]reasonEntry, 0, len(reasonCounts))
		for _, e := range reasonCounts {
			reasons = append(reasons, *e)
		}
		sort.Slice(reasons, func(i, j int) bool {
			return reasons[i].Count > reasons[j].Count
		})
		if len(reasons) > 20 {
			reasons = reasons[:20]
		}
		result.TopReasons = reasons

		writeJSON(w, http.StatusOK, result)
	}
}

// auditComponentLabel returns a short plain-English summary of which
// component(s) need work, e.g. "Audio only", "Container only", "Video + audio".
func auditComponentLabel(d playback.Decision) string {
	videoWork := d.VideoAction == "transcode"
	audioWork := d.AudioAction == "transcode"
	containerWork := d.ContainerAction == "remux" || d.ContainerAction == "transcode" ||
		d.ContainerAction == "transcode_or_remux" || d.ContainerAction == "direct_or_remux"
	switch {
	case videoWork && audioWork:
		return "Video + audio"
	case videoWork:
		return "Video only"
	case audioWork && containerWork:
		return "Audio + container"
	case audioWork:
		return "Audio only"
	case containerWork:
		return "Container only"
	default:
		return "Other"
	}
}

func migrationFormatsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"formats": migration.Formats()})
	}
}

func migrationRunsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Migration == nil {
			writeError(w, http.StatusServiceUnavailable, "migration tooling is not available")
			return
		}
		runs, err := deps.Migration.ListRuns(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "migration runs lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
	}
}

func migrationRunDetailHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Migration == nil {
			writeError(w, http.StatusServiceUnavailable, "migration tooling is not available")
			return
		}
		report, ok, err := deps.Migration.GetRun(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "migration run lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "migration run not found")
			return
		}
		writeJSON(w, http.StatusOK, report)
	}
}

func migrationDryRunHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Migration == nil {
			writeError(w, http.StatusServiceUnavailable, "migration tooling is not available")
			return
		}
		var request migration.Request
		if !decodeJSON(w, r, &request) {
			return
		}
		report, err := deps.Migration.DryRun(r.Context(), request)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, report)
	}
}

func migrationImportHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Migration == nil {
			writeError(w, http.StatusServiceUnavailable, "migration tooling is not available")
			return
		}
		var request migration.Request
		if !decodeJSON(w, r, &request) {
			return
		}
		report, err := deps.Migration.Import(r.Context(), request)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, report)
	}
}

func migrationRollbackHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Migration == nil {
			writeError(w, http.StatusServiceUnavailable, "migration tooling is not available")
			return
		}
		report, err := deps.Migration.Rollback(r.Context(), r.PathValue("id"))
		if err != nil {
			switch err {
			case migration.ErrRunNotFound:
				writeError(w, http.StatusNotFound, "migration run not found")
			case migration.ErrRunRollback:
				writeError(w, http.StatusConflict, "migration run cannot be rolled back")
			default:
				writeError(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		writeJSON(w, http.StatusOK, report)
	}
}

// activeMaxRating returns the max_rating ceiling for the currently active
// profile (if any). Returns "" when no profile is active or the profile has no
// ceiling configured, which means unrestricted access.
func activeMaxRating(ctx context.Context, deps Deps) string {
	if deps.Auth == nil {
		return ""
	}
	profileUserID, ok := auth.ActiveProfileFromContext(ctx)
	if !ok || profileUserID == "" {
		return ""
	}
	rating, _ := deps.Auth.GetProfileMaxRating(ctx, profileUserID)
	return rating
}

func clientHomeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := queryInt(r, "limit", 24)
		maxRating := activeMaxRating(r.Context(), deps)
		recent, err := deps.PlayState.Recent(r.Context(), requestUserID(r), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "recent playback lookup failed")
			return
		}
		movieItems, err := deps.Catalog.ListMovies(r.Context(), limit, maxRating, requestUserID(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "movie list failed")
			return
		}
		seriesItems, err := deps.Catalog.ListSeries(r.Context(), limit, maxRating, requestUserID(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "series list failed")
			return
		}
		// Keep only in-progress items for the continue-watching row.
		var inProgress []playstate.RecentItem
		for _, item := range recent {
			if !item.Watched && item.ProgressSeconds > 0 {
				inProgress = append(inProgress, item)
			}
		}
		// Trending row: built first so it can be inserted before the TV/Movies
		// rows.  Desired order: Trending > New Episodes Dropped > Fresh in Library
		// (issue #222).  Use TMDB regional trending cross-referenced against the
		// local library when available; fall back to a random spotlight.
		var topItems []map[string]any
		if cfg := currentConfig(deps); deps.Trending != nil && cfg.Country != "" {
			if trendingItems, err := deps.Trending.Trending(r.Context(), cfg.Country, 10); err == nil {
				topItems = trendingToTVItems(trendingItems)
			}
		}
		if len(topItems) == 0 {
			topItems = tvSpotlightItems(movieItems, seriesItems, 10)
		}

		rows := []map[string]any{
			{"id": "continue", "title": "Continue Watching", "items": tvRecentItems(r.Context(), deps, inProgress)},
		}
		if len(topItems) > 0 {
			title := "Highest rated"
			eyebrow := "From your library"
			if deps.Trending != nil && currentConfig(deps).Country != "" {
				country := currentConfig(deps).Country
				title = "Trending this week"
				eyebrow = "Your region · " + country
			}
			rows = append(rows, map[string]any{
				"id":      "top",
				"title":   title,
				"eyebrow": eyebrow,
				"items":   topItems,
			})
		}
		rows = append(rows,
			map[string]any{"id": "tv", "title": "TV Shows", "items": tvSeriesItems(seriesItems)},
			map[string]any{"id": "movies", "title": "Movies", "items": tvMovieItems(movieItems)},
			map[string]any{"id": "recently-added", "title": "Recently Added", "items": tvRecentlyAddedItems(movieItems, seriesItems, limit)},
		)
		writeJSON(w, http.StatusOK, map[string]any{
			"profile": firstNonEmpty(r.URL.Query().Get("clientProfile"), "apple-tv"),
			"heroes":  randomHeroItems(movieItems, seriesItems, 5),
			"rows":    rows,
			"actions": map[string]string{
				"movieDetail":  "/api/movies/{id}",
				"seriesDetail": "/api/series/{id}",
				"playback":     "/api/playback/route",
			},
		})
	}
}

// detailContentRating extracts the content rating from a loaded detail's
// metadata pointer (movie or series share the embedded *MetadataRecord).
func metadataContentRating(record *catalog.MetadataRecord) string {
	if record == nil {
		return ""
	}
	return record.ContentRating
}

// ratingBlocked reports whether the active profile's max-rating ceiling forbids
// an item with the given content rating. Returns false when no ceiling is set.
// Centralises the parental-control gate so detail and playback paths enforce the
// same rule the list/search filters already apply.
func ratingBlocked(ctx context.Context, deps Deps, contentRating string) bool {
	ceiling := activeMaxRating(ctx, deps)
	if ceiling == "" {
		return false
	}
	return !catalog.RatingWithinCeiling(contentRating, ceiling)
}

func clientMovieDetailHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		detail, ok, err := deps.Catalog.GetMovie(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "movie detail failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "movie not found")
			return
		}
		// Parental control: a restricted profile must not see detail for an item
		// it isn't allowed to view. 404 (not 403) so the item's existence isn't
		// disclosed, matching how list/search simply omit it.
		if ratingBlocked(r.Context(), deps, metadataContentRating(detail.Metadata)) {
			writeError(w, http.StatusNotFound, "movie not found")
			return
		}
		writeJSON(w, http.StatusOK, clientMovieDetailPayload(r.Context(), deps, r, detail))
	}
}

func clientSeriesDetailHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		detail, ok, err := deps.Catalog.GetSeries(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "series detail failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "series not found")
			return
		}
		if ratingBlocked(r.Context(), deps, metadataContentRating(detail.Metadata)) {
			writeError(w, http.StatusNotFound, "series not found")
			return
		}
		// Collect all episode media source IDs to batch-fetch playback states.
		var sourceIDs []string
		for _, season := range detail.Seasons {
			for _, episode := range season.Episodes {
				for _, v := range episode.Versions {
					if v.MediaSourceID != "" {
						sourceIDs = append(sourceIDs, v.MediaSourceID)
					}
				}
			}
		}
		var playstates map[string]playstate.State
		if len(sourceIDs) > 0 && deps.PlayState != nil {
			playstates, _ = deps.PlayState.GetBatch(r.Context(), requestUserID(r), sourceIDs)
		}
		writeJSON(w, http.StatusOK, clientSeriesDetailPayload(r.Context(), deps, r, detail, playstates))
	}
}

func clientSimilarMoviesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		items, err := deps.Catalog.SimilarMovies(r.Context(), id, 20)
		if err != nil {
			// See clientSimilarSeriesHandler — same rationale. A SimilarMovies
			// SQL failure shouldn't 500 the row; degrade to empty + log so the
			// UI hides MLT instead of surfacing an error tile.
			slog.Warn("similar movies lookup failed", "id", id, "err", err)
			writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func clientSimilarSeriesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		result, err := deps.Catalog.SimilarSeries(r.Context(), id, 20)
		if err != nil {
			// Log but don't 500: a similar-series query failure shouldn't blank
			// the entire detail page's bottom-of-page rail. Return an empty
			// result so the UI hides the row gracefully. Previously this
			// returned 500 and the UI rendered nothing under "Seasons / Cast /
			// Metadata", which read as "MLT broken" on a working detail page.
			slog.Warn("similar series lookup failed", "id", id, "err", err)
			writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
			return
		}
		resp := map[string]any{"items": result.Items}
		if result.Fallback {
			resp["genreFallback"] = true
			resp["fallbackGenre"] = result.FallbackGenre
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func clientCollectionsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		collections, err := deps.Catalog.ListCollections(r.Context(), 0)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "collections lookup failed")
			return
		}
		if collections == nil {
			collections = []catalog.CollectionHit{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"collections": collections,
		})
	}
}

func clientCollectionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		collectionID := strings.TrimSpace(r.PathValue("id"))
		if collectionID == "" {
			writeError(w, http.StatusBadRequest, "collection id is required")
			return
		}
		movies, header, found, err := deps.Catalog.ListMoviesByCollection(r.Context(), collectionID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "collection lookup failed")
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "collection not found")
			return
		}
		movieItems := make([]map[string]any, 0, len(movies))
		for _, movie := range movies {
			entry := map[string]any{
				"id":          movie.ID,
				"kind":        "movie",
				"title":       movie.Title,
				"year":        movie.Year,
				"posterUrl":   metadataPoster(movie.Metadata),
				"backdropUrl": metadataBackdrop(movie.Metadata),
				"logoUrl":     metadataLogo(movie.Metadata),
			}
			if movie.Metadata != nil {
				entry["voteAverage"] = heroRating(movie.Metadata.Ratings)
				entry["genres"] = movie.Metadata.Genres
				entry["overview"] = movie.Metadata.Overview
				if len(movie.Metadata.Directors) > 0 {
					entry["director"] = movie.Metadata.Directors[0]
				}
				if movie.Metadata.RuntimeMinutes > 0 {
					entry["runtimeMinutes"] = movie.Metadata.RuntimeMinutes
				}
			}
			movieItems = append(movieItems, entry)
		}
		collPayload := map[string]any{
			"id":          header.ID,
			"name":        header.Name,
			"posterUrl":   normalizeArtworkSourceURL(header.PosterURL, "poster"),
			"backdropUrl": normalizeArtworkSourceURL(header.BackdropURL, "backdrop"),
			"logoUrl":     normalizeArtworkSourceURL(header.LogoURL, "logo"),
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"collection": collPayload,
			"movies":     movieItems,
		})
	}
}

func clientPersonHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		personName := strings.TrimSpace(r.PathValue("name"))
		if personName == "" {
			writeError(w, http.StatusBadRequest, "person name is required")
			return
		}
		credits, profile, found, err := deps.Catalog.ListItemsByPerson(r.Context(), personName, 100)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "person lookup failed")
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "no credits found for this person")
			return
		}
		creditItems := make([]map[string]any, 0, len(credits))
		for _, credit := range credits {
			entry := map[string]any{
				"id":          credit.ID,
				"kind":        credit.Kind,
				"title":       credit.Title,
				"year":        credit.Year,
				"character":   credit.Character,
				"role":        credit.Role,
				"posterUrl":   metadataPoster(credit.Metadata),
				"backdropUrl": metadataBackdrop(credit.Metadata),
			}
			if credit.Metadata != nil {
				entry["voteAverage"] = heroRating(credit.Metadata.Ratings)
				entry["genres"] = credit.Metadata.Genres
				entry["overview"] = credit.Metadata.Overview
			}
			creditItems = append(creditItems, entry)
		}
		personPayload := map[string]any{
			"name":       profile.Name,
			"profileUrl": normalizeArtworkSourceURL(profile.ProfileURL, "poster"),
			"department": profile.Department,
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"person":  personPayload,
			"credits": creditItems,
		})
	}
}

func clientSearchHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		limit := 8
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil {
				if parsed < 1 {
					parsed = 1
				}
				if parsed > 40 {
					parsed = 40
				}
				limit = parsed
			}
		}
		if query == "" {
			writeJSON(w, http.StatusOK, map[string]any{
				"query":       "",
				"movies":      []any{},
				"series":      []any{},
				"people":      []any{},
				"collections": []any{},
			})
			return
		}
		results, err := deps.Catalog.SearchLibrary(r.Context(), query, limit, activeMaxRating(r.Context(), deps))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "search failed")
			return
		}
		movieItems := make([]map[string]any, 0, len(results.Movies))
		for _, m := range results.Movies {
			entry := map[string]any{
				"id":          m.ID,
				"kind":        "movie",
				"title":       m.Title,
				"year":        m.Year,
				"posterUrl":   metadataPoster(m.Metadata),
				"backdropUrl": metadataBackdrop(m.Metadata),
				"logoUrl":     metadataLogo(m.Metadata),
			}
			if m.Metadata != nil {
				entry["voteAverage"] = heroRating(m.Metadata.Ratings)
				entry["genres"] = m.Metadata.Genres
				entry["overview"] = m.Metadata.Overview
			}
			movieItems = append(movieItems, entry)
		}
		seriesItems := make([]map[string]any, 0, len(results.Series))
		for _, s := range results.Series {
			entry := map[string]any{
				"id":           s.ID,
				"kind":         "series",
				"title":        s.Title,
				"seasonCount":  s.SeasonCount,
				"episodeCount": s.EpisodeCount,
				"posterUrl":    metadataPoster(s.Metadata),
				"backdropUrl":  metadataBackdrop(s.Metadata),
				"logoUrl":      metadataLogo(s.Metadata),
			}
			if s.Metadata != nil {
				entry["voteAverage"] = heroRating(s.Metadata.Ratings)
				entry["genres"] = s.Metadata.Genres
				entry["overview"] = s.Metadata.Overview
				entry["year"] = s.Metadata.Year
			}
			seriesItems = append(seriesItems, entry)
		}
		peopleItems := make([]map[string]any, 0, len(results.People))
		for _, p := range results.People {
			peopleItems = append(peopleItems, map[string]any{
				"kind":        "person",
				"name":        p.Name,
				"profileUrl":  normalizeArtworkSourceURL(p.ProfileURL, "poster"),
				"department":  p.Department,
				"creditCount": p.CreditCount,
			})
		}
		collectionItems := make([]map[string]any, 0, len(results.Collections))
		for _, c := range results.Collections {
			collectionItems = append(collectionItems, map[string]any{
				"kind":        "collection",
				"id":          c.ID,
				"name":        c.Name,
				"posterUrl":   normalizeArtworkSourceURL(c.PosterURL, "poster"),
				"backdropUrl": normalizeArtworkSourceURL(c.BackdropURL, "backdrop"),
				"logoUrl":     normalizeArtworkSourceURL(c.LogoURL, "logo"),
				"movieCount":  c.MovieCount,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"query":       results.Query,
			"movies":      movieItems,
			"series":      seriesItems,
			"people":      peopleItems,
			"collections": collectionItems,
		})
	}
}

func clientWatchlistListHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := deps.Watchlist.List(r.Context(), requestUserID(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "watchlist lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func clientWatchlistAddHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req watchlist.AddRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		item, err := deps.Watchlist.Add(r.Context(), requestUserID(r), req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, item)
	}
}

func clientWatchlistRemoveHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mediaID := r.PathValue("id")
		kind := strings.TrimSpace(r.URL.Query().Get("kind"))
		if mediaID == "" || (kind != "movie" && kind != "series") {
			writeError(w, http.StatusBadRequest, "id path parameter and kind=movie|series query parameter are required")
			return
		}
		if err := deps.Watchlist.Remove(r.Context(), requestUserID(r), mediaID, kind); err != nil {
			writeError(w, http.StatusInternalServerError, "watchlist remove failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func clientPlaybackStartHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var payload clientPlaybackStartRequest
		if !decodeJSON(w, r, &payload) {
			return
		}
		if payload.MediaSourceID == "" {
			writeError(w, http.StatusBadRequest, "media source id is required")
			return
		}
		source, ok, err := deps.Catalog.GetMediaSource(r.Context(), payload.MediaSourceID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		// ── Probe required ────────────────────────────────────────────────────
		// Playback is only permitted after a file has been analysed by the probe
		// job. Return 200 (not 4xx) so that native clients can decode the body
		// and show a specific message rather than hitting a generic HTTP error
		// handler. Clients must check route.status == "deferred".
		if !source.Probed {
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "deferred",
				"error":  "This file has not been analysed yet. Go to Settings → Activity to run the Probe job before playing.",
				"route": map[string]any{
					"route":  "deferred",
					"status": "deferred",
				},
			})
			return
		}
		decision := clientPlaybackDecision(r.Context(), deps, source, clientPlaybackOptions{
			ClientProfile:      payload.ClientProfile,
			RouteType:          payload.RouteType,
			MaxNetworkBitrate:  payload.MaxNetworkBitrate,
			AudioTrackIndex:    payload.AudioTrackIndex,
			SubtitleTrackIndex: payload.SubtitleTrackIndex,
			SubtitleMode:       payload.SubtitleMode,
			SubtitleActive:     payload.SubtitleActive,
			PreferAdaptive:     payload.PreferAdaptive,
			Capabilities:       payload.ClientCapabilities,
		})
		routePayload, status, err := clientPlaybackRoutePayload(deps, r, source, decision, payload)
		if err != nil {
			writeError(w, status, err.Error())
			return
		}
		start := sessions.StartRequest{
			UserID:          requestUserID(r),
			DeviceID:        firstNonEmpty(payload.DeviceID, "apple-tv"),
			MediaSourceID:   source.ID,
			ClientProfile:   firstNonEmpty(payload.ClientProfile, "apple-tv"),
			Route:           clientRouteLabel(decision),
			Mode:            string(decision.Mode),
			ReasonCode:      decision.ReasonCode,
			ReasonText:      decision.ReasonText,
			SelectedTracks:  clientSelectedTracks(payload.AudioTrackIndex, payload.SubtitleTrackIndex),
			ProgressSeconds: 0,
		}
		if err := enrichSessionStartRequest(deps, r.Context(), &start); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		session, err := deps.Sessions.Start(start)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Determine default subtitle behaviour from saved settings (#63 follow-up).
		liveCfg := currentConfig(deps)
		defaultSubs := false
		switch source.Kind {
		case "movie":
			defaultSubs = liveCfg.DefaultSubtitlesMovies
		case "episode":
			defaultSubs = liveCfg.DefaultSubtitlesTV
		}
		// Issue a stream token atomically — the client can skip a separate
		// getStreamToken round-trip when these fields are present in the response.
		streamTokQuery, streamTokURL, streamTokSubBase := "", "", ""
		if deps.Streaming != nil {
			if t, _, tokErr := deps.Streaming.Issue(streaming.Expected{
				MediaSourceID: session.MediaSourceID,
				SessionID:     session.ID,
				UserID:        session.UserID,
				DeviceID:      session.DeviceID,
			}, 0); tokErr == nil {
				q := "?sessionId=" + url.QueryEscape(session.ID) +
					"&deviceId=" + url.QueryEscape(session.DeviceID) +
					"&token=" + url.QueryEscape(t)
				streamTokQuery = q
				streamTokURL = "/api/media-sources/" + session.MediaSourceID + "/stream" + q
				streamTokSubBase = "/api/media-sources/" + session.MediaSourceID + "/subtitles/"
			}
		}
		resp := map[string]any{
			"sessionId":               session.ID,
			"deviceId":                session.DeviceID,
			"mediaSourceId":           session.MediaSourceID,
			"playbackStateUrl":        "/api/playback/state/" + session.MediaSourceID,
			"heartbeatUrl":            "/api/client/playback/" + session.ID,
			"stopUrl":                 "/api/client/playback/" + session.ID + "/stop",
			"heartbeatIntervalMs":     2000,
			"decision":                decision,
			"route":                   routePayload,
			"defaultSubtitlesEnabled": defaultSubs,
		}
		if streamTokQuery != "" {
			resp["streamTokenQuery"] = streamTokQuery
			resp["streamUrl"] = streamTokURL
			resp["subtitleBaseUrl"] = streamTokSubBase
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func clientPlaybackHeartbeatHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request sessions.UpdateRequest
		if !decodeJSON(w, r, &request) {
			return
		}
		session, ok := deps.Sessions.Update(r.PathValue("id"), request)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		_, _ = deps.PlayState.Set(r.Context(), session.MediaSourceID, playstate.Update{
			UserID:          session.UserID,
			ProgressSeconds: session.Progress,
			DurationSeconds: session.Duration,
		})
		writeJSON(w, http.StatusOK, map[string]any{"session": session})
	}
}

func clientPlaybackStopHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request sessions.UpdateRequest
		if !decodeJSON(w, r, &request) {
			return
		}
		if request.Status == "" {
			request.Status = "stopped"
		}
		session, ok := deps.Sessions.Update(r.PathValue("id"), request)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		_, _ = deps.PlayState.Set(r.Context(), session.MediaSourceID, playstate.Update{
			UserID:          session.UserID,
			ProgressSeconds: session.Progress,
			DurationSeconds: session.Duration,
		})
		stopped, _ := deps.Sessions.Stop(r.PathValue("id"))
		// Kill any active transcode for this media source, mirroring the
		// logic in sessionStopHandler.  Only cancel if no other session is
		// still streaming the same source — two clients may share a transcode.
		if deps.Transcode != nil && strings.TrimSpace(stopped.MediaSourceID) != "" {
			otherSessionActive := false
			for _, active := range deps.Sessions.List() {
				if active.MediaSourceID == stopped.MediaSourceID {
					otherSessionActive = true
					break
				}
			}
			if !otherSessionActive {
				deps.Transcode.CancelActiveForMediaSource(stopped.MediaSourceID)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"session": stopped})
	}
}

// clientCapabilities is the shape sent from clients that have performed
// self-detection via canPlayType / matchMedia (issue #64). When present,
// it overrides the corresponding fields from the static device profile so
// the decision tree operates on real supported codec lists rather than
// a curated-but-possibly-wrong profile.
type clientCapabilities struct {
	Containers          []string `json:"containers,omitempty"`
	VideoCodecs         []string `json:"videoCodecs,omitempty"`
	AudioCodecs         []string `json:"audioCodecs,omitempty"`
	SubtitleCodecs      []string `json:"subtitleCodecs,omitempty"`
	MaxVideoBitDepth    int      `json:"maxVideoBitDepth,omitempty"`
	MaxVideoFrameRate   float64  `json:"maxVideoFrameRate,omitempty"`
	SupportsHDR         bool     `json:"supportsHdr,omitempty"`
	SupportsDolbyVision bool     `json:"supportsDolbyVision,omitempty"`
	SupportsHLS         bool     `json:"supportsHls,omitempty"`
}

// applyClientCapabilities merges a client-reported capability set onto a
// Request that has already been seeded from the static device profile.
// Only non-zero / non-empty values override the profile-derived fields.
func applyClientCapabilities(req playback.Request, caps *clientCapabilities) playback.Request {
	if caps == nil {
		return req
	}
	if len(caps.Containers) > 0 {
		req.Containers = caps.Containers
	}
	if len(caps.VideoCodecs) > 0 {
		req.VideoCodecs = caps.VideoCodecs
	}
	if len(caps.AudioCodecs) > 0 {
		req.AudioCodecs = caps.AudioCodecs
	}
	if len(caps.SubtitleCodecs) > 0 {
		req.SubtitleCodecs = caps.SubtitleCodecs
	}
	if caps.MaxVideoBitDepth > 0 {
		req.MaxVideoBitDepth = caps.MaxVideoBitDepth
	}
	if caps.MaxVideoFrameRate > 0 {
		req.MaxFrameRate = caps.MaxVideoFrameRate
	}
	if caps.SupportsHDR {
		req.SupportsHDR = true
	}
	if caps.SupportsDolbyVision {
		req.SupportsDolbyVision = true
	}
	if caps.SupportsHLS {
		req.SupportsAdaptive = true
	}
	return req
}

type clientPlaybackOptions struct {
	ClientProfile      string
	RouteType          string
	MaxNetworkBitrate  int64
	AudioTrackIndex    int
	SubtitleTrackIndex int
	SubtitleMode       string
	SubtitleActive     bool
	PreferAdaptive     bool
	Capabilities       *clientCapabilities
}

type clientPlaybackStartRequest struct {
	MediaSourceID      string `json:"mediaSourceId"`
	DeviceID           string `json:"deviceId"`
	ClientProfile      string `json:"clientProfile"`
	RouteType          string `json:"routeType"`
	MaxNetworkBitrate  int64  `json:"maxNetworkBitrate"`
	AudioTrackIndex    int    `json:"audioTrackIndex"`
	SubtitleTrackIndex int    `json:"subtitleTrackIndex"`
	SubtitleMode       string `json:"subtitleMode"`
	SubtitleActive     bool   `json:"subtitleActive"`
	// ClientCapabilities, when provided, overrides the static device-profile
	// codec / container / HDR whitelist with values measured by the client
	// at runtime (e.g. via MediaSource.isTypeSupported / canPlayType).
	ClientCapabilities *clientCapabilities `json:"clientCapabilities,omitempty"`
	PreferAdaptive     bool                `json:"preferAdaptive,omitempty"`
}

func clientMovieDetailPayload(ctx context.Context, deps Deps, r *http.Request, detail catalog.MovieDetail) map[string]any {
	versions := make([]map[string]any, 0, len(detail.Versions))
	defaultMediaSourceID := ""
	for _, version := range detail.Versions {
		if defaultMediaSourceID == "" {
			defaultMediaSourceID = version.MediaSourceID
		}
		if payload, ok := clientVersionPayload(ctx, deps, r, version.MediaSourceID); ok {
			versions = append(versions, payload)
		}
	}
	itemPayload := map[string]any{
		"id":           detail.ID,
		"kind":         "movie",
		"title":        detail.Title,
		"year":         detail.Year,
		"overview":     metadataOverview(detail.Metadata),
		"posterUrl":    metadataPoster(detail.Metadata),
		"backdropUrl":  metadataBackdrop(detail.Metadata),
		"logoUrl":      metadataLogo(detail.Metadata),
		"thumbnailUrl": metadataThumbnail(detail.Metadata),
		"bannerUrl":    metadataBanner(detail.Metadata),
		"videoKey":     trailerVideoKey(detail.Metadata),
		"trailerUrl":   trailerURL(detail.Metadata),
		"versionCount": detail.VersionCount,
	}
	if detail.Metadata != nil {
		itemPayload["voteAverage"] = heroRating(detail.Metadata.Ratings)
		itemPayload["genres"] = detail.Metadata.Genres
		if detail.Metadata.RuntimeMinutes > 0 {
			itemPayload["runtimeMinutes"] = detail.Metadata.RuntimeMinutes
		}
		if len(detail.Metadata.Directors) > 0 {
			itemPayload["director"] = detail.Metadata.Directors[0]
		}
		// Surface collection art when this movie belongs to a franchise.
		if c := detail.Metadata.Collection; c != nil && c.ID != "" {
			itemPayload["collection"] = map[string]any{
				"id":           c.ID,
				"name":         c.Name,
				"posterUrl":    normalizeArtworkSourceURL(c.PosterURL, "poster"),
				"backdropUrl":  normalizeArtworkSourceURL(c.BackdropURL, "backdrop"),
				"logoUrl":      normalizeArtworkSourceURL(c.LogoURL, "logo"),
				"bannerUrl":    normalizeArtworkSourceURL(c.BannerURL, "banner"),
				"landscapeUrl": normalizeArtworkSourceURL(c.LandscapeURL, "landscape"),
			}
		}
	}
	return map[string]any{
		"item":                 itemPayload,
		"defaultMediaSourceId": defaultMediaSourceID,
		"versions":             versions,
	}
}

func clientSeriesDetailPayload(ctx context.Context, deps Deps, r *http.Request, detail catalog.SeriesDetail, playstates map[string]playstate.State) map[string]any {
	seasons := make([]map[string]any, 0, len(detail.Seasons))
	defaultMediaSourceID := ""
	for _, season := range detail.Seasons {
		episodes := make([]map[string]any, 0, len(season.Episodes))
		for _, episode := range season.Episodes {
			versionPayloads := make([]map[string]any, 0, len(episode.Versions))
			for _, version := range episode.Versions {
				if defaultMediaSourceID == "" {
					defaultMediaSourceID = version.MediaSourceID
				}
				if payload, ok := clientVersionPayload(ctx, deps, r, version.MediaSourceID); ok {
					versionPayloads = append(versionPayloads, payload)
				}
			}
			epEntry := map[string]any{
				"id":            episode.ID,
				"title":         firstNonEmpty(episode.Title, episodeEpisodeLabel(episode)),
				"seasonNumber":  episode.SeasonNumber,
				"episodeNumber": episode.EpisodeNumber,
				"versionCount":  episode.VersionCount,
				"versions":      versionPayloads,
				// Route episode stills through the local artwork proxy so they are
				// cached on disk and served with immutable headers. Direct TMDB URLs
				// bypass the proxy cache and appear black when TMDB is unreachable or
				// the still hasn't been fetched yet. The proxy falls back to an SVG
				// placeholder when no still is cached.
				"thumbnailUrl": "/api/artwork/episode/" + episode.ID + "?type=thumbnail",
			}
			if episode.Metadata != nil {
				epEntry["overview"] = html.UnescapeString(episode.Metadata.Overview)
				epEntry["airDate"] = firstNonEmpty(episode.Metadata.AirDate, episode.Metadata.FirstAirDate)
				if episode.Metadata.RuntimeMinutes > 0 {
					epEntry["runtimeMinutes"] = episode.Metadata.RuntimeMinutes
				}
				epEntry["voteAverage"] = heroRating(episode.Metadata.Ratings)
			}
			// Inject resume position from the first version that has playback state.
			for _, v := range episode.Versions {
				if ps, ok := playstates[v.MediaSourceID]; ok && ps.ProgressSeconds > 0 {
					epEntry["positionSeconds"] = int(ps.ProgressSeconds)
					break
				}
			}
			episodes = append(episodes, epEntry)
		}
		seasonEntry := map[string]any{
			"id":           season.ID,
			"seasonNumber": season.SeasonNumber,
			"episodes":     episodes,
			// Per-season artwork (TMDB /tv/{id}/season/{n} → poster_path).
			"posterUrl":   metadataPoster(season.Metadata),
			"backdropUrl": metadataBackdrop(season.Metadata),
		}
		if season.Metadata != nil {
			seasonEntry["name"] = season.Metadata.Title
			seasonEntry["overview"] = html.UnescapeString(season.Metadata.Overview)
			seasonEntry["airDate"] = season.Metadata.AirDate
		}
		seasons = append(seasons, seasonEntry)
	}
	itemPayload := map[string]any{
		"id":           detail.ID,
		"kind":         "series",
		"title":        detail.Title,
		"overview":     metadataOverview(detail.Metadata),
		"posterUrl":    metadataPoster(detail.Metadata),
		"backdropUrl":  metadataBackdrop(detail.Metadata),
		"logoUrl":      metadataLogo(detail.Metadata),
		"thumbnailUrl": metadataThumbnail(detail.Metadata),
		"bannerUrl":    metadataBanner(detail.Metadata),
		"videoKey":     trailerVideoKey(detail.Metadata),
		"trailerUrl":   trailerURL(detail.Metadata),
		"seasonCount":  detail.SeasonCount,
		"episodeCount": detail.EpisodeCount,
	}
	if detail.Metadata != nil {
		itemPayload["voteAverage"] = heroRating(detail.Metadata.Ratings)
		itemPayload["genres"] = detail.Metadata.Genres
		if detail.Metadata.Year > 0 {
			itemPayload["year"] = detail.Metadata.Year
		}
	}
	return map[string]any{
		"item":                 itemPayload,
		"defaultMediaSourceId": defaultMediaSourceID,
		"seasons":              seasons,
	}
}

func clientVersionPayload(ctx context.Context, deps Deps, r *http.Request, mediaSourceID string) (map[string]any, bool) {
	source, ok, err := deps.Catalog.GetMediaSource(ctx, mediaSourceID)
	if err != nil || !ok {
		return nil, false
	}
	tracks, _, _ := deps.Catalog.GetMediaSourceTracks(ctx, mediaSourceID)
	sidecars := subtitles.DiscoverSidecars(source.Path)
	decision := clientPlaybackDecision(ctx, deps, source, clientPlaybackOptions{
		ClientProfile:     firstNonEmpty(r.URL.Query().Get("clientProfile"), "apple-tv"),
		RouteType:         firstNonEmpty(r.URL.Query().Get("routeType"), "lan"),
		MaxNetworkBitrate: queryInt64(r, "maxNetworkBitrate", 0),
	})
	return map[string]any{
		"mediaSourceId":  source.ID,
		"path":           source.RelPath,
		"qualityLabel":   sessionQualityLabel(source),
		"source":         source,
		"audioTracks":    tracks.AudioTracks,
		"subtitleTracks": tracks.SubtitleTracks,
		"sidecars":       clientSidecarPayloads(deps.Subtitles, sidecars, firstNonEmpty(r.URL.Query().Get("clientProfile"), "apple-tv")),
		"decision":       decision,
	}, true
}

func clientPlaybackDecision(ctx context.Context, deps Deps, source catalog.MediaSourceItem, options clientPlaybackOptions) playback.Decision {
	request := playback.Request{
		MediaSourceID:       source.ID,
		ClientProfile:       firstNonEmpty(options.ClientProfile, "apple-tv"),
		RouteType:           firstNonEmpty(options.RouteType, "lan"),
		MaxNetworkBitrate:   options.MaxNetworkBitrate,
		AudioTrackIndex:     options.AudioTrackIndex,
		SubtitleTrackIndex:  options.SubtitleTrackIndex,
		SubtitleMode:        options.SubtitleMode,
		SubtitleTrackActive: options.SubtitleActive,
		PreferAdaptive:      options.PreferAdaptive,
	}
	request = applyClientProfile(deps, request)
	// Client-reported capabilities override profile fields when present (#64).
	request = applyClientCapabilities(request, options.Capabilities)
	return deps.Playback.DecideSource(ctx, request, playbackSourceFacts(ctx, deps, request, source))
}

func clientPlaybackRoutePayload(deps Deps, r *http.Request, source catalog.MediaSourceItem, decision playback.Decision, payload clientPlaybackStartRequest) (map[string]any, int, error) {
	if decision.Mode == playback.DirectPlay {
		return map[string]any{
			"route":    "direct",
			"status":   "ready",
			"url":      "/api/media-sources/" + source.ID + "/stream",
			"decision": decision,
		}, http.StatusOK, nil
	}
	if !playbackPolicyAllows(deps.Config.PlaybackPolicy, decision) {
		return map[string]any{
			"route":           "blocked",
			"status":          "blocked_by_policy",
			"policy":          playbackPolicyStatus(deps.Config.PlaybackPolicy),
			"decision":        decision,
			"fallbackOptions": playbackPolicyFallbacks(deps.Config.PlaybackPolicy, decision),
		}, http.StatusOK, nil
	}
	if decision.Mode == playback.AdaptiveStream {
		plan := adaptivePlanForSource(deps, source, payload.ClientProfile, firstNonEmpty(payload.RouteType, "remote"), payload.MaxNetworkBitrate)
		if plan.Enabled {
			return map[string]any{
				"route":       "adaptive",
				"status":      "ready",
				"protocol":    plan.Protocol,
				"manifestUrl": "/api/media-sources/" + source.ID + "/adaptive/master.m3u8?clientProfile=" + url.QueryEscape(firstNonEmpty(payload.ClientProfile, "apple-tv")) + "&routeType=" + url.QueryEscape(firstNonEmpty(payload.RouteType, "remote")) + "&maxNetworkBitrate=" + fmt.Sprintf("%d", payload.MaxNetworkBitrate),
				"plan":        plan,
				"decision":    decision,
			}, http.StatusOK, nil
		}
	}
	mode := transcode.ModeTranscode
	if decision.Mode == playback.Remux {
		mode = transcode.ModeRemux
	}
	audioTrackIndex := resolvedAudioTrackIndex(r.Context(), deps, source.ID, payload.AudioTrackIndex)
	if job, ok := deps.Transcode.FindCompleted(source.ID, mode, audioTrackIndex); ok {
		return map[string]any{
			"route":    string(mode),
			"status":   "ready",
			"url":      "/api/work/" + job.ID + "/file",
			"job":      job,
			"decision": decision,
		}, http.StatusOK, nil
	}
	if job, ok := deps.Transcode.FindActive(source.ID, mode, audioTrackIndex); ok {
		return map[string]any{
			"route":    string(mode),
			"status":   string(job.Status),
			"job":      job,
			"decision": decision,
		}, http.StatusAccepted, nil
	}
	request := transcode.Request{MediaSourceID: source.ID, Mode: mode, SourcePath: source.Path, AudioTrackIndex: audioTrackIndex}
	if mode == transcode.ModeTranscode {
		if encoder, ok := selectedHardwareEncoder(r.Context(), deps.Config); ok {
			request.Acceleration = "hardware"
			request.VideoEncoder = encoder
		}
	}
	job, err := deps.Transcode.Start(r.Context(), request)
	if err != nil {
		return nil, queueRejectionStatus(err, http.StatusBadRequest), err
	}
	return map[string]any{
		"route":    string(mode),
		"status":   string(job.Status),
		"job":      job,
		"decision": decision,
	}, http.StatusAccepted, nil
}

func clientRouteLabel(decision playback.Decision) string {
	switch decision.Mode {
	case playback.DirectPlay:
		return "direct"
	case playback.AdaptiveStream:
		return "adaptive"
	case playback.Remux:
		return "remux"
	case playback.AudioTranscode:
		return "audio-transcode"
	case playback.SubtitleBurn:
		return "subtitle-burn"
	case playback.VideoTranscode:
		return "transcode"
	default:
		return "pending"
	}
}

func clientSelectedTracks(audioTrackIndex int, subtitleTrackIndex int) map[string]string {
	selected := map[string]string{}
	if audioTrackIndex > 0 {
		selected["audioTrack"] = fmt.Sprintf("%d", audioTrackIndex)
	}
	if subtitleTrackIndex > 0 {
		selected["subtitleTrack"] = fmt.Sprintf("%d", subtitleTrackIndex)
	}
	if len(selected) == 0 {
		return nil
	}
	return selected
}

func clientSidecarPayloads(service *subtitles.Service, sidecars []subtitles.Sidecar, clientProfile string) []map[string]any {
	if len(sidecars) == 0 {
		return nil
	}
	output := make([]map[string]any, 0, len(sidecars))
	for _, sidecar := range sidecars {
		output = append(output, map[string]any{
			"path":              sidecar.Path,
			"relPath":           sidecar.RelPath,
			"format":            sidecar.Format,
			"language":          sidecar.Language,
			"forced":            sidecar.Forced,
			"hearingImpaired":   sidecar.HearingImpaired,
			"requiresVideoBurn": sidecar.RequiresVideoBurn,
			"conversion":        service.PlanConversion(sidecar, clientProfile),
		})
	}
	return output
}

func metadataOverview(record *catalog.MetadataRecord) string {
	if record == nil {
		return ""
	}
	// Metadata scrapers (TMDB/TVDB) sometimes store HTML entities verbatim
	// (e.g. "&amp;" instead of "&").  Unescape here so every caller — detail
	// handlers, hero carousel, episode lists — gets clean plain text.
	return html.UnescapeString(record.Overview)
}

func episodeEpisodeLabel(episode catalog.EpisodeBrief) string {
	return fmt.Sprintf("S%02d E%02d", episode.SeasonNumber, episode.EpisodeNumber)
}

// tvRecentItems builds the Continue Watching row. For each recent item, we
// look up its parent (movie or series) via media_source_id → artwork id,
// then pull the parent's backdrop + logo so the card can render the Netflix-
// style "resume" tile (16:9 backdrop with clearlogo overlay) instead of the
// plain 2:3 poster.
func tvRecentItems(ctx context.Context, deps Deps, items []playstate.RecentItem) []map[string]any {
	output := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry := map[string]any{
			"id":              item.MediaSourceID,
			"kind":            item.Kind,
			"title":           firstNonEmpty(item.Name, item.RelPath, "Resume playback"),
			"subtitle":        formatResumeSubtitle(item),
			"mediaSourceId":   item.MediaSourceID,
			"progressPercent": item.Percent * 100,
			"lastPlayedAt":    item.LastPlayedAt,
			"route":           "Resume",
		}
		if deps.Catalog != nil {
			if display, ok, err := deps.Catalog.GetMediaSourceDisplay(ctx, item.MediaSourceID); err == nil && ok {
				// Use the display's resolved title (cleaner than the filename).
				if display.Title != "" {
					entry["title"] = display.Title
				}
				// For TV episodes, expose S/E numbers + episode title so the
				// Continue Watching card can render "S1 E3 • Episode Title".
				if display.Kind == "episode" {
					entry["seasonNumber"] = display.SeasonNumber
					entry["episodeNumber"] = display.EpisodeNumber
					if display.EpisodeTitle != "" {
						entry["episodeTitle"] = display.EpisodeTitle
					}
					if display.SeriesID != "" {
						entry["seriesId"] = display.SeriesID
					}
				}
				if display.ArtworkID != "" {
					if record, has, err := deps.Catalog.GetBestMetadata(ctx, display.ArtworkKind, display.ArtworkID); err == nil && has {
						entry["posterUrl"] = metadataPoster(&record)
						entry["backdropUrl"] = metadataBackdrop(&record)
						entry["logoUrl"] = metadataLogo(&record)
						entry["thumbnailUrl"] = metadataThumbnail(&record)
						if record.Year > 0 {
							entry["year"] = record.Year
						}
						entry["voteAverage"] = heroRating(record.Ratings)
						entry["genres"] = record.Genres
						entry["parentId"] = display.ArtworkID
						entry["parentKind"] = display.ArtworkKind
					}
				}
			}
		}
		output = append(output, entry)
	}
	return output
}

func tvMovieItems(items []catalog.MovieListItem) []map[string]any {
	output := make([]map[string]any, 0, len(items))
	for _, item := range items {
		subtitle := "Movie"
		if item.Year > 0 {
			subtitle = fmt.Sprintf("%d", item.Year)
		}
		entry := map[string]any{
			"id":           item.ID,
			"kind":         "movie",
			"title":        item.Title,
			"subtitle":     subtitle,
			"year":         item.Year,
			"posterUrl":    metadataPoster(item.Metadata),
			"backdropUrl":  metadataBackdrop(item.Metadata),
			"logoUrl":      metadataLogo(item.Metadata),
			"thumbnailUrl": metadataThumbnail(item.Metadata),
			"bannerUrl":    metadataBanner(item.Metadata),
			"versionCount": item.VersionCount,
			"route":        "Ready",
		}
		if item.Metadata != nil {
			entry["voteAverage"] = heroRating(item.Metadata.Ratings)
			entry["genres"] = item.Metadata.Genres
		}
		output = append(output, entry)
	}
	return output
}

func tvSeriesItems(items []catalog.SeriesListItem) []map[string]any {
	output := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry := map[string]any{
			"id":           item.ID,
			"kind":         "series",
			"title":        item.Title,
			"subtitle":     fmt.Sprintf("%d season%s / %d episode%s", item.SeasonCount, plural(item.SeasonCount), item.EpisodeCount, plural(item.EpisodeCount)),
			"posterUrl":    metadataPoster(item.Metadata),
			"backdropUrl":  metadataBackdrop(item.Metadata),
			"logoUrl":      metadataLogo(item.Metadata),
			"thumbnailUrl": metadataThumbnail(item.Metadata),
			"bannerUrl":    metadataBanner(item.Metadata),
			"seasonCount":  item.SeasonCount,
			"episodeCount": item.EpisodeCount,
			"route":        "Ready",
		}
		if item.Metadata != nil {
			entry["year"] = item.Metadata.Year
			entry["voteAverage"] = heroRating(item.Metadata.Ratings)
			entry["genres"] = item.Metadata.Genres
		}
		output = append(output, entry)
	}
	return output
}

func tvRecentlyAddedItems(movies []catalog.MovieListItem, series []catalog.SeriesListItem, limit int) []map[string]any {
	output := []map[string]any{}
	output = append(output, tvMovieItems(movies)...)
	output = append(output, tvSeriesItems(series)...)
	if len(output) > limit {
		return output[:limit]
	}
	return output
}

func tvSpotlightItems(movies []catalog.MovieListItem, series []catalog.SeriesListItem, limit int) []map[string]any {
	all := make([]map[string]any, 0, len(movies)+len(series))
	all = append(all, tvMovieItems(movies)...)
	all = append(all, tvSeriesItems(series)...)
	rand.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	if len(all) > limit {
		return all[:limit]
	}
	return all
}

// trendingToTVItems converts TMDB trending items (already cross-referenced
// against the local library) to the same map shape used by tvMovieItems /
// tvSeriesItems so the home screen renders them identically.
func trendingToTVItems(items []trending.Item) []map[string]any {
	const tmdbImageBase = "https://image.tmdb.org/t/p"
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		subtitle := fmt.Sprintf("#%d", item.Rank)
		if item.Year > 0 {
			subtitle = fmt.Sprintf("#%d · %d", item.Rank, item.Year)
		}
		posterURL := ""
		if item.PosterPath != "" {
			posterURL = tmdbImageBase + "/w500" + item.PosterPath
		}
		backdropURL := ""
		if item.BackdropPath != "" {
			backdropURL = tmdbImageBase + "/original" + item.BackdropPath
		}
		out = append(out, map[string]any{
			"id":          item.CatalogID,
			"kind":        item.Kind,
			"title":       item.Title,
			"subtitle":    subtitle,
			"posterUrl":   posterURL,
			"backdropUrl": backdropURL,
			"voteAverage": item.VoteAverage,
			"rank":        item.Rank,
			"route":       "Ready",
		})
	}
	return out
}

// randomHeroItems picks n distinct random items from the full catalog for the
// hero carousel. All available metadata is included so the banner renders as
// richly as possible regardless of how complete the metadata is.
func randomHeroItems(movies []catalog.MovieListItem, series []catalog.SeriesListItem, n int) []map[string]any {
	all := make([]map[string]any, 0, len(movies)+len(series))

	for _, m := range movies {
		item := map[string]any{
			"id":           m.ID,
			"kind":         "movie",
			"title":        m.Title,
			"year":         m.Year,
			"posterUrl":    metadataPoster(m.Metadata),
			"backdropUrl":  metadataBackdrop(m.Metadata),
			"logoUrl":      metadataLogo(m.Metadata),
			"thumbnailUrl": metadataThumbnail(m.Metadata),
			"bannerUrl":    metadataBanner(m.Metadata),
			"videoKey":     trailerVideoKey(m.Metadata),
			"trailerUrl":   trailerURL(m.Metadata),
			"route":        "Ready",
		}
		if m.Metadata != nil {
			item["overview"] = html.UnescapeString(m.Metadata.Overview)
			item["genres"] = m.Metadata.Genres
			item["voteAverage"] = heroRating(m.Metadata.Ratings)
			if len(m.Metadata.Directors) > 0 {
				item["director"] = m.Metadata.Directors[0]
			}
			if m.Metadata.RuntimeMinutes > 0 {
				item["runtime"] = fmt.Sprintf("%d min", m.Metadata.RuntimeMinutes)
			}
		}
		all = append(all, item)
	}

	for _, s := range series {
		item := map[string]any{
			"id":           s.ID,
			"kind":         "series",
			"title":        s.Title,
			"posterUrl":    metadataPoster(s.Metadata),
			"backdropUrl":  metadataBackdrop(s.Metadata),
			"logoUrl":      metadataLogo(s.Metadata),
			"thumbnailUrl": metadataThumbnail(s.Metadata),
			"bannerUrl":    metadataBanner(s.Metadata),
			"videoKey":     trailerVideoKey(s.Metadata),
			"trailerUrl":   trailerURL(s.Metadata),
			"seasonCount":  s.SeasonCount,
			"episodeCount": s.EpisodeCount,
			"route":        "Ready",
		}
		if s.Metadata != nil {
			item["year"] = s.Metadata.Year
			item["overview"] = html.UnescapeString(s.Metadata.Overview)
			item["genres"] = s.Metadata.Genres
			item["voteAverage"] = heroRating(s.Metadata.Ratings)
		}
		all = append(all, item)
	}

	if len(all) == 0 {
		return []map[string]any{{
			"id":       "empty",
			"kind":     "empty",
			"title":    "Add your first library",
			"subtitle": "Open Xuva Settings to add Movies or TV Shows.",
			"route":    "Setup",
		}}
	}
	rand.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	if len(all) > n {
		return all[:n]
	}
	return all
}

// heroRating extracts the highest numeric rating from a Ratings map.
// Handles float64 values and display strings like "8.5/10" or "85%".
func heroRating(ratings catalog.Ratings) float64 {
	best := 0.0
	for _, v := range ratings {
		var f float64
		switch typed := v.(type) {
		case float64:
			f = typed
		case string:
			s := strings.TrimSpace(strings.Split(strings.TrimSuffix(typed, "%"), "/")[0])
			if parsed, err := strconv.ParseFloat(s, 64); err == nil {
				if parsed > 10 {
					parsed /= 10
				}
				f = parsed
			}
		}
		if f > best {
			best = f
		}
	}
	return best
}

func metadataPoster(record *catalog.MetadataRecord) string {
	if record == nil {
		return ""
	}
	return normalizeArtworkSourceURL(record.PosterURL, "poster")
}

func metadataBackdrop(record *catalog.MetadataRecord) string {
	if record == nil {
		return ""
	}
	return normalizeArtworkSourceURL(record.BackdropURL, "backdrop")
}

func metadataThumbnail(record *catalog.MetadataRecord) string {
	if record == nil {
		return ""
	}
	return normalizeArtworkSourceURL(record.ThumbnailURL, "thumbnail")
}

func metadataLogo(record *catalog.MetadataRecord) string {
	if record == nil {
		return ""
	}
	return normalizeArtworkSourceURL(record.LogoURL, "logo")
}

func metadataBanner(record *catalog.MetadataRecord) string {
	if record == nil {
		return ""
	}
	return normalizeArtworkSourceURL(record.BannerURL, "banner")
}

func normalizeArtworkSourceURL(raw string, artType string) string {
	_ = artType
	source := strings.TrimSpace(raw)
	if source == "" {
		return ""
	}
	lower := strings.ToLower(source)
	if !strings.Contains(lower, "image.tmdb.org") {
		return source
	}
	parsed, err := url.Parse(source)
	if err != nil {
		return source
	}
	const prefix = "/t/p/"
	if !strings.HasPrefix(parsed.Path, prefix) {
		return source
	}
	parts := strings.SplitN(strings.TrimPrefix(parsed.Path, prefix), "/", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		return source
	}
	current := parts[0]
	if strings.EqualFold(current, "original") {
		return source
	}
	target := "original"
	if strings.EqualFold(current, target) {
		return source
	}
	parsed.Path = prefix + target + "/" + parts[1]
	return parsed.String()
}

// trailerVideoKey returns the YouTube key for a metadata record, preferring
// the field saved at fetch time. As a backwards-compat path for items whose
// RawJSON was fetched before video parsing landed, it falls back to a
// best-effort scan of the cached TMDB raw response.
func trailerVideoKey(record *catalog.MetadataRecord) string {
	if record == nil {
		return ""
	}
	if key := strings.TrimSpace(record.VideoKey); key != "" {
		return key
	}
	if strings.TrimSpace(record.RawJSON) == "" {
		return ""
	}
	var parsed struct {
		Videos struct {
			Results []struct {
				Key      string `json:"key"`
				Site     string `json:"site"`
				Type     string `json:"type"`
				Official bool   `json:"official"`
			} `json:"results"`
		} `json:"videos"`
	}
	if err := json.Unmarshal([]byte(record.RawJSON), &parsed); err != nil {
		return ""
	}
	var officialTrailer, anyTrailer, teaser, anyYouTube string
	for _, v := range parsed.Videos.Results {
		if !strings.EqualFold(v.Site, "YouTube") || strings.TrimSpace(v.Key) == "" {
			continue
		}
		isTrailer := strings.EqualFold(v.Type, "Trailer")
		isTeaser := strings.EqualFold(v.Type, "Teaser")
		switch {
		case isTrailer && v.Official && officialTrailer == "":
			officialTrailer = v.Key
		case isTrailer && anyTrailer == "":
			anyTrailer = v.Key
		case isTeaser && teaser == "":
			teaser = v.Key
		case anyYouTube == "":
			anyYouTube = v.Key
		}
		if officialTrailer != "" {
			break
		}
	}
	if officialTrailer != "" {
		return officialTrailer
	}
	if anyTrailer != "" {
		return anyTrailer
	}
	if teaser != "" {
		return teaser
	}
	return anyYouTube
}

// trailerURL returns the local /api/trailers/{tmdbId} URL when the MP4 has
// been downloaded, or empty string. The frontend prefers the local URL and
// only falls back to the YouTube key when this is empty.
func trailerURL(record *catalog.MetadataRecord) string {
	if record == nil {
		return ""
	}
	if strings.TrimSpace(record.TrailerPath) == "" {
		return ""
	}
	// Filename is "<tmdbID>.mp4" — use ExternalID (TMDB) when the provider is
	// TMDB. For other providers, the file simply won't exist.
	tmdbID := strings.TrimSpace(record.ExternalID)
	if tmdbID == "" {
		return ""
	}
	if _, err := strconv.Atoi(tmdbID); err != nil {
		return ""
	}
	return "/api/trailers/" + tmdbID + ".mp4"
}

func formatProgress(value float64) string {
	if value <= 0 {
		return "the beginning"
	}
	if value > 1 {
		value = 1
	}
	return fmt.Sprintf("%.0f%%", value*100)
}

func formatResumeSubtitle(item playstate.RecentItem) string {
	if item.Percent > 0 {
		return "Resume from " + formatProgress(item.Percent)
	}
	if item.ProgressSeconds > 0 {
		return "Resume from " + formatSeconds(item.ProgressSeconds)
	}
	return "Resume from the beginning"
}

func formatSeconds(value float64) string {
	seconds := int(value + 0.5)
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	remainingSeconds := seconds % 60
	if minutes < 60 {
		if remainingSeconds == 0 {
			return fmt.Sprintf("%dm", minutes)
		}
		return fmt.Sprintf("%dm %02ds", minutes, remainingSeconds)
	}
	hours := minutes / 60
	remainingMinutes := minutes % 60
	if remainingMinutes == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh %02dm", hours, remainingMinutes)
}

func plural(value int) string {
	if value == 1 {
		return ""
	}
	return "s"
}

func moviesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ListMoviesSummary returns only the ~6 metadata fields the browser
		// grid card needs (poster, genres, contentRating, studios, overview).
		// The full MetadataRecord — cast, crew, ratings, external IDs — is
		// reserved for detail endpoints. This cuts the /api/movies payload
		// from ~77 MB to ~4 MB on a 4000-item library.
		items, err := deps.Catalog.ListMoviesSummary(r.Context(), queryInt(r, "limit", 0), activeMaxRating(r.Context(), deps), requestUserID(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "movie list failed")
			return
		}
		// ETag is content-derived: same library state yields the same hash
		// per user, so SWR's background refresh hits 304 most of the time
		// and skips the JSON.parse on the client.
		writeCachedJSON(w, r, http.StatusOK, map[string]any{"movies": items})
	}
}

func movieDetailHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		detail, ok, err := deps.Catalog.GetMovie(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "movie detail failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "movie not found")
			return
		}
		if ratingBlocked(r.Context(), deps, metadataContentRating(detail.Metadata)) {
			writeError(w, http.StatusNotFound, "movie not found")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	}
}

func seriesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ListSeriesSummary — same slim-metadata approach as moviesHandler.
		items, err := deps.Catalog.ListSeriesSummary(r.Context(), queryInt(r, "limit", 0), activeMaxRating(r.Context(), deps), requestUserID(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "series list failed")
			return
		}
		// See moviesHandler — content-hash ETag so the SWR background refresh
		// hits 304 when the library hasn't changed.
		writeCachedJSON(w, r, http.StatusOK, map[string]any{"series": items})
	}
}

func seriesDetailHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		detail, ok, err := deps.Catalog.GetSeries(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "series detail failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "series not found")
			return
		}
		if ratingBlocked(r.Context(), deps, metadataContentRating(detail.Metadata)) {
			writeError(w, http.StatusNotFound, "series not found")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	}
}

func reviewHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := deps.Catalog.ReviewItems(r.Context(), queryInt(r, "limit", 100))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "review list failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func metadataSuggestionsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := deps.Catalog.ReviewItems(r.Context(), queryInt(r, "limit", 100))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "metadata suggestions failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"suggestions": items,
			"providers":   metadataProviders(r.Context(), deps),
			"strategy":    "Xuva runs strict managed metadata mode: local-first signals and account-free online sources stay active, managed providers auto-run when server credentials are provisioned, and fallback paths continue when limits or provider outages occur.",
		})
	}
}

func metadataRecordsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := r.PathValue("kind")
		id := r.PathValue("id")
		records, err := deps.Catalog.ListMetadataRecords(r.Context(), kind, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "metadata records failed")
			return
		}
		best, ok, err := deps.Catalog.GetBestMetadata(r.Context(), kind, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "metadata records failed")
			return
		}
		var selected any
		if ok {
			selected = best
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"best":      selected,
			"records":   records,
			"providers": metadataProviders(r.Context(), deps),
		})
	}
}

func metadataProvidersHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"managedMode": "strict",
			"providers":   metadataProviders(r.Context(), deps),
		})
	}
}

// metadataCandidatesHandler returns the top TMDB search results for a given
// item so the user can pick the correct match from the disambiguation UI.
// Query params: kind (movie|series), title, year, limit (default 5).
func metadataCandidatesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Metadata == nil {
			writeError(w, http.StatusServiceUnavailable, "metadata providers are not available")
			return
		}
		kind := r.URL.Query().Get("kind")
		if kind != "movie" && kind != "series" {
			writeError(w, http.StatusBadRequest, "kind must be 'movie' or 'series'")
			return
		}
		title := strings.TrimSpace(r.URL.Query().Get("title"))
		if title == "" {
			writeError(w, http.StatusBadRequest, "title is required")
			return
		}
		year := queryInt(r, "year", 0)
		limit := queryInt(r, "limit", 5)
		if limit < 1 || limit > 20 {
			limit = 5
		}
		candidates, err := deps.Metadata.TMDBCandidates(r.Context(), kind, title, year, limit)
		if err != nil {
			writeError(w, http.StatusBadGateway, "TMDB search failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"kind":       kind,
			"title":      title,
			"year":       year,
			"candidates": candidates,
		})
	}
}

func metadataMatchHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request catalog.MetadataUpdate
		if !decodeJSON(w, r, &request) {
			return
		}
		if err := deps.Catalog.ApplyMetadata(r.Context(), request); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		records, err := deps.Catalog.ListMetadataRecords(r.Context(), request.Kind, request.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "metadata records failed")
			return
		}
		deps.Events.Publish("metadata.updated", request)
		writeJSON(w, http.StatusOK, map[string]any{
			"match":   request,
			"records": records,
		})
	}
}

func metadataRefreshHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Metadata == nil {
			writeError(w, http.StatusServiceUnavailable, "metadata providers are not available")
			return
		}
		var request metaprovider.RefreshRequest
		if !decodeJSON(w, r, &request) {
			return
		}
		result, err := deps.Metadata.Refresh(r.Context(), request)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func metadataRefreshBatchHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Metadata == nil {
			writeError(w, http.StatusServiceUnavailable, "metadata providers are not available")
			return
		}
		var request struct {
			Kind  string `json:"kind"`
			Limit int    `json:"limit"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		result, err := deps.Metadata.RefreshBatch(r.Context(), request.Kind, request.Limit)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// metadataBackfillStatusHandler returns a snapshot of the running backfill
// (or last-completed run). Used by the Settings UI's "library health" strip
// to render live progress + by clients to detect whether a backfill is
// already in flight before posting a new one.
func metadataBackfillStatusHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Metadata == nil {
			writeError(w, http.StatusServiceUnavailable, "metadata providers are not available")
			return
		}
		status := deps.Metadata.BackfillStatus()
		// Include live counts even when idle, so the UI can show "x items
		// missing TMDB metadata" without having to poll the catalog
		// directly. We compute these only on the GET handler so the hot
		// path (running backfill) isn't bottlenecked on extra queries.
		missingMovies := 0
		missingSeries := 0
		if deps.Catalog != nil {
			if n, err := deps.Catalog.CountItemsMissingProvider(r.Context(), "movie", "tmdb"); err == nil {
				missingMovies = n
			}
			if n, err := deps.Catalog.CountItemsMissingProvider(r.Context(), "series", "tmdb"); err == nil {
				missingSeries = n
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        status,
			"missingMovies": missingMovies,
			"missingSeries": missingSeries,
			"missingTotal":  missingMovies + missingSeries,
		})
	}
}

func metadataBackfillStartHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Metadata == nil {
			writeError(w, http.StatusServiceUnavailable, "metadata providers are not available")
			return
		}
		var request struct {
			Provider string `json:"provider"`
		}
		_ = decodeJSON(w, r, &request)
		if strings.TrimSpace(request.Provider) == "" {
			request.Provider = "tmdb"
		}
		if err := deps.Metadata.StartBackfill(r.Context(), request.Provider); err != nil {
			if metaprovider.IsBackfillProviderNotConfigured(err) {
				writeJSON(w, http.StatusAccepted, map[string]any{
					"status":   deps.Metadata.BackfillStatus(),
					"accepted": false,
					"reason":   "not_configured",
					"message":  err.Error(),
				})
				return
			}
			if metaprovider.IsBackfillAlreadyRunning(err) {
				writeJSON(w, http.StatusAccepted, map[string]any{
					"status":   deps.Metadata.BackfillStatus(),
					"accepted": false,
					"reason":   "already_running",
					"message":  err.Error(),
				})
				return
			}
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":   deps.Metadata.BackfillStatus(),
			"accepted": true,
		})
	}
}

func metadataBackfillStopHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if deps.Metadata == nil {
			writeError(w, http.StatusServiceUnavailable, "metadata providers are not available")
			return
		}
		deps.Metadata.StopBackfill()
		writeJSON(w, http.StatusOK, map[string]any{
			"status": deps.Metadata.BackfillStatus(),
		})
	}
}

func metadataProviders(ctx context.Context, deps Deps) []map[string]any {
	cfg := currentConfig(deps)
	health := map[string]metaprovider.ProviderHealth{}
	if deps.Metadata != nil {
		health = deps.Metadata.ProviderHealth(ctx)
	}
	providers := []map[string]any{
		{"id": "filename", "name": "Filename and folders", "status": "active", "local": true},
		{"id": "manual", "name": "Manual correction", "status": "active", "local": true},
		{"id": "nfo", "name": "Local NFO", "status": "active", "local": true},
		{"id": "artwork", "name": "Poster and fanart sidecars", "status": "active", "local": true},
		{"id": "tvmaze", "name": "TVMaze", "status": "automatic", "local": false},
		{"id": "wikipedia", "name": "Wikipedia", "status": "automatic", "local": false},
		{"id": "wikidata", "name": "Wikidata", "status": "automatic", "local": false},
		{"id": "tmdb", "name": "TMDB", "status": "managed-ready", "local": false},
		{"id": "tvdb", "name": "TheTVDB", "status": "managed-ready", "local": false},
		{"id": "fanart", "name": "Fanart.tv", "status": "managed-ready", "local": false},
		{"id": "omdb", "name": "OMDb", "status": "managed-ready", "local": false},
	}
	for _, provider := range providers {
		id, _ := provider["id"].(string)
		if id == "" {
			continue
		}
		if id != "tmdb" && id != "tvdb" && id != "fanart" && id != "omdb" {
			continue
		}
		state, ok := health[id]
		if !ok {
			state = metaprovider.ProviderHealth{
				ID:         id,
				Managed:    true,
				Configured: managedProviderConfiguredForConfig(id, cfg),
				Healthy:    managedProviderConfiguredForConfig(id, cfg),
				Status:     "ready",
			}
			if !state.Configured {
				state.Status = "not_provisioned"
				state.Healthy = false
			}
		}
		provider["managedMode"] = "strict"
		provider["configured"] = state.Configured
		provider["healthy"] = state.Healthy
		provider["health"] = state
		provider["status"] = state.Status
	}
	return providers
}

// trailerHandler serves the locally cached MP4 trailer for a given TMDB id.
// The URL is shaped /api/trailers/{tmdbId} — extension can be omitted or be
// ".mp4". `http.ServeFile` handles Range/HEAD/304 automatically, which is
// exactly what <video> needs for instant seek + adaptive buffering.
//
// We deliberately key on TMDB id (not catalog id) because:
//   - It's stable across re-scans
//   - It maps 1:1 with the on-disk filename
//   - Multiple catalog items pointing at the same TMDB title share one file
func trailerHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Trailers == nil || !deps.Trailers.Enabled() {
			writeError(w, http.StatusNotFound, "trailers disabled")
			return
		}
		raw := strings.TrimSuffix(r.PathValue("tmdbId"), ".mp4")
		raw = strings.TrimSpace(raw)
		if raw == "" || !safePathSegment(raw) {
			writeError(w, http.StatusBadRequest, "invalid trailer id")
			return
		}
		// TMDB IDs are integers; reject anything else so we never let a
		// crafted path escape the trailers dir.
		if _, err := strconv.Atoi(raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid trailer id")
			return
		}
		path := deps.Trailers.LocalPath(raw)
		// Double-check the resolved path is inside the trailers dir.
		rootAbs, err := filepath.Abs(deps.Trailers.Dir())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "trailers root unavailable")
			return
		}
		fileAbs, err := filepath.Abs(path)
		if err != nil || !strings.HasPrefix(fileAbs, rootAbs+string(filepath.Separator)) {
			writeError(w, http.StatusBadRequest, "trailer path escape")
			return
		}
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			writeError(w, http.StatusNotFound, "trailer not yet available")
			return
		}
		// Cache aggressively — the contents are immutable once written. The
		// filename includes the TMDB id so a re-fetch would land at the
		// same path (or be a new file via versioned id, in which case the
		// frontend would request a fresh URL).
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeFile(w, r, path)
	}
}

func artworkHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := r.PathValue("kind")
		id := r.PathValue("id")
		title := id
		artType := r.URL.Query().Get("type")
		if artType == "" {
			artType = "poster"
		}
		disableFallback := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("fallback")), "none")
		if !safePathSegment(kind) || !safePathSegment(id) || !safePathSegment(artType) {
			writeError(w, http.StatusBadRequest, "invalid artwork path")
			return
		}
		switch artType {
		case "poster", "backdrop", "thumbnail", "thumb", "logo", "banner":
		default:
			writeError(w, http.StatusBadRequest, "invalid artwork type")
			return
		}
		// Optional ?w=N: serve a resized JPEG variant cached on disk. Width
		// is validated to a sane range; out-of-band values are treated as
		// "no resize" so a request never errors solely on the query string.
		resizeWidth := parseResizeWidth(r.URL.Query().Get("w"))

		if deps.Catalog != nil {
			if record, ok, err := deps.Catalog.GetBestMetadata(r.Context(), kind, id); err == nil && ok {
				if record.Title != "" {
					title = record.Title
				}
			}
			if records, err := deps.Catalog.ListMetadataRecords(r.Context(), kind, id); err == nil {
				candidates := metadataArtworkCandidates(records, artType)
				if path, ok := firstResolvedArtwork(r.Context(), deps.Config.MetadataDir, kind, id, artType, candidates); ok {
					serveArtworkFile(w, r, path, resizeWidth)
					return
				}
			}
		}
		switch kind {
		case "movie":
			if item, ok, err := deps.Catalog.GetMovie(r.Context(), id); err == nil && ok {
				title = item.Title
			}
		case "series":
			if item, ok, err := deps.Catalog.GetSeries(r.Context(), id); err == nil && ok {
				title = item.Title
			}
		}
		if disableFallback {
			writeError(w, http.StatusNotFound, "artwork not found")
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = io.WriteString(w, fallbackArtworkSVG(title, artType))
	}
}

// firstResolvedArtwork walks candidate URLs in priority order, trying the
// strict quality gate first and falling back to the lenient gate, returning
// the first on-disk path that matches. This lets the handler do one decision
// pass over candidates instead of two.
func firstResolvedArtwork(ctx context.Context, metadataDir, kind, id, artType string, candidates []string) (string, bool) {
	for _, candidate := range candidates {
		if path, ok := resolveArtwork(ctx, metadataDir, kind, id, artType, candidate, true); ok {
			return path, true
		}
	}
	for _, candidate := range candidates {
		if path, ok := resolveArtwork(ctx, metadataDir, kind, id, artType, candidate, false); ok {
			return path, true
		}
	}
	return "", false
}

// serveArtworkFile streams an original or resized artwork file with an
// immutable Cache-Control header. When width > 0 and the source is wider than
// the target, the file is downscaled to a .wNNN.{jpg,webp} sibling on first
// request and reused thereafter. The output format is picked from the
// request's Accept header (webp for modern browsers, jpg otherwise — see
// resolveServeFormat in image_resize.go) so each variant gets its own cache
// entry side-by-side; a JPEG and a WebP of the same source coexist as
// poster.w200.jpg and poster.w200.webp.
//
// A resize/encode failure falls back to serving the original rather than
// erroring out — the user still sees a poster. The Vary: Accept header is
// emitted whenever resizing is in play so caches downstream don't serve a
// WebP body to a JPEG-only client (and vice versa).
func serveArtworkFile(w http.ResponseWriter, r *http.Request, originalPath string, width int) {
	servePath := originalPath
	if width > 0 {
		format := resolveServeFormat(r.Header.Get("Accept"))
		sized := sizedArtworkPath(originalPath, width, format)
		if _, err := os.Stat(sized); err != nil {
			if rerr := resizeImageWidth(originalPath, sized, width); rerr == nil {
				servePath = sized
			}
		} else {
			servePath = sized
		}
		// Tell downstream caches (and the browser cache) that the response
		// body depends on Accept — without this, a CDN could serve a WebP
		// payload to a curl client that asked for image/* and the result
		// would be a broken image.
		w.Header().Set("Vary", "Accept")
	}
	// 1 year, immutable. Hashes/source URL changes invalidate by switching
	// to a different sized variant filename, not by client revalidation.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, servePath)
}

func metadataArtworkCandidates(records []catalog.MetadataRecord, artType string) []string {
	output := []string{}
	seen := map[string]struct{}{}
	push := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		output = append(output, value)
	}
	switch strings.ToLower(strings.TrimSpace(artType)) {
	case "backdrop":
		for _, record := range records {
			push(metadataBackdrop(&record))
		}
		for _, record := range records {
			push(metadataPoster(&record))
		}
		return output
	case "thumbnail", "thumb":
		for _, record := range records {
			push(metadataThumbnail(&record))
		}
		for _, record := range records {
			push(metadataBackdrop(&record))
		}
		for _, record := range records {
			push(metadataPoster(&record))
		}
		return output
	case "logo":
		for _, record := range records {
			push(metadataLogo(&record))
		}
		return output
	case "banner":
		for _, record := range records {
			push(metadataBanner(&record))
		}
		for _, record := range records {
			push(metadataBackdrop(&record))
		}
		return output
	}
	for _, record := range records {
		push(metadataPoster(&record))
	}
	for _, record := range records {
		push(metadataBackdrop(&record))
	}
	return output
}

func fallbackArtworkSVG(title string, artType string) string {
	safeTitle := html.EscapeString(truncate(strings.TrimSpace(title), 20))
	if safeTitle == "" {
		safeTitle = "Xuva"
	}
	if strings.EqualFold(artType, "backdrop") {
		return `<svg xmlns="http://www.w3.org/2000/svg" width="1280" height="720" viewBox="0 0 1280 720">
<defs>
  <linearGradient id="bg" x1="0" x2="1" y1="0" y2="1">
    <stop stop-color="#0a1522"/>
    <stop offset="0.52" stop-color="#11233a"/>
    <stop offset="1" stop-color="#0d1a2c"/>
  </linearGradient>
  <radialGradient id="glow" cx="0.82" cy="0.14" r="0.6">
    <stop stop-color="#5bc2d6" stop-opacity="0.2"/>
    <stop offset="1" stop-color="#5bc2d6" stop-opacity="0"/>
  </radialGradient>
  <linearGradient id="logo" x1="0" y1="0" x2="1" y2="1">
    <stop stop-color="#A78BFA"/>
    <stop offset="0.55" stop-color="#7C3AED"/>
    <stop offset="1" stop-color="#DB2777"/>
  </linearGradient>
</defs>
<rect width="1280" height="720" fill="url(#bg)"/>
<rect width="1280" height="720" fill="url(#glow)"/>
<g transform="translate(546 266)">
  <rect x="0" y="0" width="188" height="188" rx="28" fill="#0f1f34" fill-opacity="0.78" stroke="#d7ebf8" stroke-opacity="0.18" stroke-width="2"/>
  <path d="M46 40 L78 40 L146 94 L100 94 Z" fill="url(#logo)"/>
  <path d="M46 148 L78 148 L146 94 L100 94 Z" fill="url(#logo)"/>
</g>
</svg>`
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="600" height="900" viewBox="0 0 600 900">
<defs>
  <linearGradient id="g" x1="0" x2="1" y1="0" y2="1">
    <stop stop-color="#101f33"/>
    <stop offset="1" stop-color="#0a1522"/>
  </linearGradient>
  <radialGradient id="glow" cx="0.78" cy="0.16" r="0.55">
    <stop stop-color="#61d0e2" stop-opacity="0.24"/>
    <stop offset="1" stop-color="#61d0e2" stop-opacity="0"/>
  </radialGradient>
  <linearGradient id="logo" x1="0" y1="0" x2="1" y2="1">
    <stop stop-color="#A78BFA"/>
    <stop offset="0.55" stop-color="#7C3AED"/>
    <stop offset="1" stop-color="#DB2777"/>
  </linearGradient>
</defs>
<rect width="600" height="900" fill="url(#g)"/>
<rect width="600" height="900" fill="url(#glow)"/>
<rect x="26" y="26" width="548" height="848" rx="22" fill="none" stroke="#d7ebf8" stroke-opacity="0.12" stroke-width="2"/>
<g transform="translate(176 298)">
  <rect x="0" y="0" width="248" height="248" rx="34" fill="#0f1f34" fill-opacity="0.78" stroke="#d7ebf8" stroke-opacity="0.18" stroke-width="2"/>
  <path d="M60 54 L102 54 L192 126 L128 126 Z" fill="url(#logo)"/>
  <path d="M60 194 L102 194 L192 126 L128 126 Z" fill="url(#logo)"/>
</g>
<text x="72" y="702" fill="#ecf5fc" fill-opacity="0.9" font-family="Inter,Segoe UI,sans-serif" font-size="40" font-weight="760">%s</text>
</svg>`, safeTitle)
}

// resolveArtwork returns the on-disk path for an artwork candidate, either a
// local library file or a remote URL fetched into the metadata-dir cache.
// Returns ("", false) when neither succeeds. Callers then decide whether to
// serve the file as-is or pass it through the resize pipeline.
func resolveArtwork(ctx context.Context, metadataDir string, kind string, id string, artType string, source string, strict bool) (string, bool) {
	if path, ok := resolveLocalArtwork(source, artType, strict); ok {
		return path, true
	}
	return resolveCachedArtwork(ctx, metadataDir, kind, id, artType, source, strict)
}

func resolveLocalArtwork(path string, artType string, strict bool) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false
	}
	lower := strings.ToLower(path)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		if !artworkPassesQualityGatePath(path, artType, strict) {
			return "", false
		}
		return path, true
	default:
		return "", false
	}
}

func resolveCachedArtwork(ctx context.Context, metadataDir string, kind string, id string, artType string, sourceURL string, strict bool) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(sourceURL), "https://") && !strings.HasPrefix(strings.ToLower(sourceURL), "http://") {
		return "", false
	}
	if metadataDir == "" {
		return "", false
	}
	dir, ok := safeChildPath(metadataDir, "artwork", kind, id)
	if !ok {
		return "", false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false
	}
	normalizedSource := strings.TrimSpace(sourceURL)
	sourceMarkerPath, ok := safeChildPath(dir, artType+".source")
	if !ok {
		return "", false
	}
	cachedSource := readCachedArtworkSource(sourceMarkerPath)
	refreshBecauseSourceChanged := cachedSource != "" && !strings.EqualFold(cachedSource, normalizedSource)
	refreshTMDBCacheWithoutMarker := cachedSource == "" && strings.Contains(strings.ToLower(normalizedSource), "image.tmdb.org/t/p/")
	for _, ext := range []string{".jpg", ".png", ".webp"} {
		path, ok := safeChildPath(dir, artType+ext)
		if !ok {
			return "", false
		}
		if _, err := os.Stat(path); err == nil {
			if refreshBecauseSourceChanged || refreshTMDBCacheWithoutMarker {
				_ = os.Remove(path)
				continue
			}
			if !artworkPassesQualityGatePath(path, artType, strict) {
				return "", false
			}
			return path, true
		}
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", false
	}
	request.Header.Set("User-Agent", "Xuva/0.1 (+https://github.com/xuvahq/xuva)")
	request.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", false
	}
	ext := artworkExtension(response.Header.Get("Content-Type"), sourceURL)
	path, ok := safeChildPath(dir, artType+ext)
	if !ok {
		return "", false
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, 32*1024*1024))
	if err != nil {
		return "", false
	}
	if !artworkPassesQualityGateBytes(payload, artType, sourceURL, strict) {
		return "", false
	}
	file, err := os.Create(path)
	if err != nil {
		return "", false
	}
	if _, err := io.Copy(file, bytes.NewReader(payload)); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", false
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", false
	}
	writeCachedArtworkSource(sourceMarkerPath, normalizedSource)
	return path, true
}

func readCachedArtworkSource(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func writeCachedArtworkSource(path string, source string) {
	_ = os.WriteFile(path, []byte(strings.TrimSpace(source)), 0o644)
}

func artworkExtension(contentType string, sourceURL string) string {
	contentType = strings.ToLower(contentType)
	switch {
	case strings.Contains(contentType, "png"):
		return ".png"
	case strings.Contains(contentType, "webp"):
		return ".webp"
	case strings.Contains(contentType, "jpeg"), strings.Contains(contentType, "jpg"):
		return ".jpg"
	}
	ext := strings.ToLower(filepath.Ext(strings.Split(sourceURL, "?")[0]))
	if ext == ".png" || ext == ".webp" || ext == ".jpg" || ext == ".jpeg" {
		if ext == ".jpeg" {
			return ".jpg"
		}
		return ext
	}
	return ".jpg"
}

func artworkPassesQualityGatePath(path string, artType string, strict bool) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return artworkPassesQualityGateBytes(data, artType, path, strict)
}

func artworkPassesQualityGateBytes(payload []byte, artType string, sourceHint string, strict bool) bool {
	minWidth, minHeight := artworkQualityThresholds(artType, strict)
	config, _, err := image.DecodeConfig(bytes.NewReader(payload))
	if err == nil {
		return config.Width >= minWidth && config.Height >= minHeight
	}
	widthHint, heightHint := artworkDimensionHints(sourceHint, artType)
	if widthHint <= 0 || heightHint <= 0 {
		return false
	}
	return widthHint >= minWidth && heightHint >= minHeight
}

func artworkQualityThresholds(artType string, strict bool) (minWidth int, minHeight int) {
	switch strings.ToLower(strings.TrimSpace(artType)) {
	case "logo":
		if strict {
			return 400, 120
		}
		return 160, 48
	case "banner":
		if strict {
			return 1000, 180
		}
		return 360, 64
	case "thumbnail", "thumb":
		if strict {
			return 640, 360
		}
		return 240, 135
	}
	if !strict {
		if strings.EqualFold(artType, "backdrop") {
			return 480, 270
		}
		return 180, 260
	}
	if strings.EqualFold(artType, "backdrop") {
		return 1280, 720
	}
	return 600, 900
}

func artworkDimensionHints(source string, artType string) (int, int) {
	value := strings.ToLower(strings.TrimSpace(source))
	if value == "" {
		return 0, 0
	}
	if strings.Contains(value, "image.tmdb.org/t/p/") {
		return tmdbArtworkHint(value, artType)
	}
	if strings.Contains(value, "fanart.tv/fanart") {
		switch strings.ToLower(strings.TrimSpace(artType)) {
		case "logo":
			return 800, 310
		case "banner":
			return 1000, 185
		case "thumbnail", "thumb":
			return 1000, 562
		case "backdrop":
			return 1920, 1080
		default:
			return 1000, 1500
		}
	}
	if strings.Contains(value, "tvmaze.com") {
		if strings.Contains(value, "/original") || strings.Contains(value, "original_") {
			return 1280, 720
		}
		if strings.Contains(value, "/medium") || strings.Contains(value, "medium_") {
			if strings.EqualFold(artType, "backdrop") {
				return 0, 0
			}
			return 210, 295
		}
	}
	if strings.Contains(value, "/thumb/") {
		if width := wikipediaThumbWidth(value); width > 0 {
			if strings.EqualFold(artType, "backdrop") {
				return width, int(float64(width) * 9.0 / 16.0)
			}
			return width, int(float64(width) * 1.5)
		}
	}
	return 0, 0
}

func tmdbArtworkHint(source string, artType string) (int, int) {
	const prefix = "/t/p/"
	parsed, err := url.Parse(source)
	if err != nil {
		return 0, 0
	}
	if !strings.HasPrefix(parsed.Path, prefix) {
		return 0, 0
	}
	parts := strings.SplitN(strings.TrimPrefix(parsed.Path, prefix), "/", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	size := strings.TrimSpace(parts[0])
	switch strings.ToLower(size) {
	case "original":
		if strings.EqualFold(artType, "backdrop") {
			return 1920, 1080
		}
		return 1000, 1500
	}
	if !strings.HasPrefix(strings.ToLower(size), "w") {
		return 0, 0
	}
	width, err := strconv.Atoi(strings.TrimPrefix(strings.ToLower(size), "w"))
	if err != nil || width <= 0 {
		return 0, 0
	}
	if strings.EqualFold(artType, "backdrop") {
		return width, int(float64(width) * 9.0 / 16.0)
	}
	return width, int(float64(width) * 1.5)
}

func wikipediaThumbWidth(source string) int {
	const marker = "px-"
	index := strings.Index(source, marker)
	if index <= 0 {
		return 0
	}
	start := index - 1
	for start >= 0 {
		char := source[start]
		if char < '0' || char > '9' {
			break
		}
		start--
	}
	width, err := strconv.Atoi(source[start+1 : index])
	if err != nil || width <= 0 {
		return 0
	}
	return width
}

func versionsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := deps.Catalog.VersionGroups(r.Context(), queryInt(r, "limit", 100))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "version groups failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"versions": items})
	}
}

func performanceSettingsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		libraryList := deps.Libraries.List()
		profile := "local"
		for _, library := range libraryList {
			if library.StorageType == libraries.StorageNetwork || library.StorageType == libraries.StorageRemovable || library.StorageType == libraries.StorageMounted {
				profile = "conservative"
				break
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"profile":              profile,
			"playbackPolicy":       playbackPolicyStatus(deps.Config.PlaybackPolicy),
			"limits":               deps.Resources.Limits(),
			"queues":               deps.Jobs.Snapshot(),
			"libraries":            libraryList,
			"storageDefaults":      storageDefaults(libraryList),
			"hardwareAcceleration": hardwareAccelerationStatus(deps.Config),
			"recommendations": []string{
				"Keep scan concurrency low for network/removable storage",
				"Probe jobs are isolated from scan and transcode queues",
				"Playback-critical work is separated from background jobs",
			},
		})
	}
}

func storageDefaults(libraryList []libraries.Library) map[string]libraries.WorkerDefaults {
	output := map[string]libraries.WorkerDefaults{}
	for _, library := range libraryList {
		output[library.ID] = libraries.DefaultsForStorage(library.StorageType)
	}
	return output
}

func metadataSourceCatalogPayload(ctx context.Context, cfg config.Config, service *metaprovider.Service) map[string][]map[string]any {
	catalog := metasources.SourceCatalogByKind(cfg)
	health := map[string]metaprovider.ProviderHealth{}
	if service != nil {
		health = service.ProviderHealth(ctx)
	}
	output := map[string][]map[string]any{
		"movie":  {},
		"series": {},
	}
	for kind, definitions := range catalog {
		rows := make([]map[string]any, 0, len(definitions))
		for _, definition := range definitions {
			item := map[string]any{
				"id":               definition.ID,
				"name":             definition.Name,
				"description":      definition.Description,
				"coverage":         definition.Coverage,
				"note":             definition.Note,
				"kinds":            definition.Kinds,
				"local":            definition.Local,
				"managed":          definition.Managed,
				"requiresConfig":   definition.RequiresConfig,
				"available":        definition.Available,
				"supportsMetadata": definition.SupportsMetadata,
				"supportsArtwork":  definition.SupportsArtwork,
			}
			if definition.Managed {
				state, ok := health[definition.ID]
				if !ok {
					state = metaprovider.ProviderHealth{
						ID:         definition.ID,
						Managed:    true,
						Configured: managedProviderConfiguredForConfig(definition.ID, cfg),
						Healthy:    definition.Available,
						Status:     "ready",
					}
					if !state.Configured {
						state.Status = "not_provisioned"
						state.Healthy = false
					}
				}
				item["available"] = state.Configured
				item["providerHealth"] = state
				item["runtimeReady"] = state.Healthy
				item["status"] = state.Status
			}
			rows = append(rows, item)
		}
		output[kind] = rows
	}
	return output
}

func metadataSourcePreferencesPayload(cfg config.Config) map[string][]string {
	return map[string][]string{
		"movie":         configuredMetadataSourceOrder(cfg, "movie"),
		"series":        configuredMetadataSourceOrder(cfg, "series"),
		"movieArtwork":  configuredArtworkSourceOrder(cfg, "movie"),
		"seriesArtwork": configuredArtworkSourceOrder(cfg, "series"),
	}
}

func configuredMetadataSourceOrder(cfg config.Config, kind string) []string {
	switch metasources.NormalizeKind(kind) {
	case "series":
		return metasources.NormalizeRequestedSourceOrder("series", cfg.SeriesMetadataSources)
	default:
		return metasources.NormalizeRequestedSourceOrder("movie", cfg.MovieMetadataSources)
	}
}

func configuredArtworkSourceOrder(cfg config.Config, kind string) []string {
	switch metasources.NormalizeKind(kind) {
	case "series":
		return metasources.NormalizeRequestedArtworkOrder("series", cfg.SeriesArtworkSources)
	default:
		return metasources.NormalizeRequestedArtworkOrder("movie", cfg.MovieArtworkSources)
	}
}

func defaultMetadataSourcePreferenceForLibrary(cfg config.Config, kind libraries.Kind) []string {
	switch kind {
	case libraries.KindTV:
		return configuredMetadataSourceOrder(cfg, "series")
	default:
		return configuredMetadataSourceOrder(cfg, "movie")
	}
}

func defaultArtworkSourcePreferenceForLibrary(cfg config.Config, kind libraries.Kind) []string {
	switch kind {
	case libraries.KindTV:
		return configuredArtworkSourceOrder(cfg, "series")
	default:
		return configuredArtworkSourceOrder(cfg, "movie")
	}
}

func managedProviderConfiguredForConfig(provider string, cfg config.Config) bool {
	return metaprovider.ManagedProviderConfigured(provider, cfg)
}

func settingsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := currentConfig(deps)
		health := map[string]metaprovider.ProviderHealth{}
		if deps.Metadata != nil {
			health = deps.Metadata.ProviderHealth(r.Context())
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"config":                    settingsPayload(cfg),
			"runtimePaths":              runtimePaths(cfg),
			"libraries":                 deps.Libraries.List(),
			"metadataSources":           metadataSourceCatalogPayload(r.Context(), cfg, deps.Metadata),
			"metadataSourcePreferences": metadataSourcePreferencesPayload(cfg),
			"metadataHealth":            health,
			"managedMode":               "strict",
		})
	}
}

func settingsFolderBrowseHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimSpace(r.URL.Query().Get("path"))
		if path == "" {
			payload := browseRootPayload(currentConfig(deps).DataDir)
			writeJSON(w, http.StatusOK, payload)
			return
		}
		payload, status := browseFolderPayload(path)
		writeJSON(w, status, payload)
	}
}

func settingsUpdateHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request config.Config
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &request); err != nil {
				writeError(w, http.StatusBadRequest, "invalid json body")
				return
			}
		}
		fields := map[string]json.RawMessage{}
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &fields); err != nil {
				writeError(w, http.StatusBadRequest, "invalid json body")
				return
			}
		}
		updated := currentConfig(deps)
		if value, ok := fields["serverName"]; ok {
			var serverName string
			if err := json.Unmarshal(value, &serverName); err != nil {
				writeError(w, http.StatusBadRequest, "server name must be text")
				return
			}
			normalized, err := config.NormalizeServerName(serverName)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			updated.ServerName = normalized
		}
		if value, ok := fields["canonicalWebOrigin"]; ok {
			var origin string
			if err := json.Unmarshal(value, &origin); err != nil {
				writeError(w, http.StatusBadRequest, "canonical web origin must be text")
				return
			}
			normalized, err := config.NormalizeWebOrigin(origin)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			updated.CanonicalWebOrigin = normalized
		}
		mergeString(&updated.HTTPAddr, request.HTTPAddr)
		mergeString(&updated.DataDir, request.DataDir)
		mergeString(&updated.TranscodeDir, request.TranscodeDir)
		mergeString(&updated.DownloadsDir, request.DownloadsDir)
		mergeString(&updated.MetadataDir, request.MetadataDir)
		mergeString(&updated.CacheDir, request.CacheDir)
		mergeString(&updated.TempDir, request.TempDir)
		mergeString(&updated.FFmpegPath, request.FFmpegPath)
		mergeString(&updated.FFprobePath, request.FFprobePath)
		mergeInt(&updated.EventBuffer, request.EventBuffer)
		mergeInt(&updated.ScanWorkers, request.ScanWorkers)
		mergeInt(&updated.ProbeWorkers, request.ProbeWorkers)
		mergeInt(&updated.TranscodeWorkers, request.TranscodeWorkers)
		mergeInt(&updated.GPUWorkers, request.GPUWorkers)
		if value, ok := fields["hardwareUnlocked"]; ok {
			var hardwareUnlocked bool
			if err := json.Unmarshal(value, &hardwareUnlocked); err != nil {
				writeError(w, http.StatusBadRequest, "hardware unlocked must be true or false")
				return
			}
			updated.HardwareUnlocked = hardwareUnlocked
		}
		mergeString(&updated.LibrarySyncMode, request.LibrarySyncMode)
		mergeString(&updated.PlaybackPolicy, request.PlaybackPolicy)
		updated.PlaybackPolicy = normalizedPlaybackPolicy(updated.PlaybackPolicy)
		mergeInt(&updated.SyncIntervalMins, request.SyncIntervalMins)
		mergeInt(&updated.WatchDebounceSecs, request.WatchDebounceSecs)
		mergeInt(&updated.ProbeBatchLimit, request.ProbeBatchLimit)
		if request.AllowedOrigins != nil {
			updated.AllowedOrigins = request.AllowedOrigins
		}
		// country / timezone may be set or explicitly cleared via the settings form
		if value, ok := fields["country"]; ok {
			var country string
			if err := json.Unmarshal(value, &country); err != nil {
				writeError(w, http.StatusBadRequest, "country must be text")
				return
			}
			updated.Country = strings.ToUpper(strings.TrimSpace(country))
		}
		if value, ok := fields["timezone"]; ok {
			var timezone string
			if err := json.Unmarshal(value, &timezone); err != nil {
				writeError(w, http.StatusBadRequest, "timezone must be text")
				return
			}
			updated.Timezone = strings.TrimSpace(timezone)
		}
		if value, ok := fields["metadataLanguage"]; ok {
			var lang string
			if err := json.Unmarshal(value, &lang); err != nil {
				writeError(w, http.StatusBadRequest, "metadataLanguage must be text")
				return
			}
			lang = strings.TrimSpace(lang)
			if lang == "" {
				lang = "en-US"
			}
			updated.MetadataLanguage = lang
		}
		if value, ok := fields["preferTextSubtitles"]; ok {
			var v bool
			if err := json.Unmarshal(value, &v); err != nil {
				writeError(w, http.StatusBadRequest, "preferTextSubtitles must be true or false")
				return
			}
			updated.PreferTextSubtitles = v
		}
		if value, ok := fields["originalQualityOnly"]; ok {
			var v bool
			if err := json.Unmarshal(value, &v); err != nil {
				writeError(w, http.StatusBadRequest, "originalQualityOnly must be true or false")
				return
			}
			updated.OriginalQualityOnly = v
		}
		if value, ok := fields["defaultSubtitlesMovies"]; ok {
			var v bool
			if err := json.Unmarshal(value, &v); err != nil {
				writeError(w, http.StatusBadRequest, "defaultSubtitlesMovies must be true or false")
				return
			}
			updated.DefaultSubtitlesMovies = v
		}
		if value, ok := fields["defaultSubtitlesTV"]; ok {
			var v bool
			if err := json.Unmarshal(value, &v); err != nil {
				writeError(w, http.StatusBadRequest, "defaultSubtitlesTV must be true or false")
				return
			}
			updated.DefaultSubtitlesTV = v
		}
		if value, ok := fields["disableTrailers"]; ok {
			var v bool
			if err := json.Unmarshal(value, &v); err != nil {
				writeError(w, http.StatusBadRequest, "disableTrailers must be true or false")
				return
			}
			updated.DisableTrailers = v
		}
		if err := config.SaveFile(deps.Config.DataDir, updated); err != nil {
			writeError(w, http.StatusInternalServerError, "settings save failed")
			return
		}
		if err := deps.Catalog.SaveSettings(r.Context(), catalog.RuntimeSettings{
			HTTPAddr:         updated.HTTPAddr,
			DataDir:          updated.DataDir,
			TranscodeDir:     updated.TranscodeDir,
			DownloadsDir:     updated.DownloadsDir,
			MetadataDir:      updated.MetadataDir,
			CacheDir:         updated.CacheDir,
			TempDir:          updated.TempDir,
			FFmpegPath:       updated.FFmpegPath,
			FFprobePath:      updated.FFprobePath,
			EventBuffer:      updated.EventBuffer,
			ScanWorkers:      updated.ScanWorkers,
			ProbeWorkers:     updated.ProbeWorkers,
			TranscodeWorkers: updated.TranscodeWorkers,
			GPUWorkers:       updated.GPUWorkers,
			Country:          updated.Country,
			Timezone:         updated.Timezone,
			SetupComplete:    updated.SetupComplete,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "settings save failed")
			return
		}
		deps.Events.Publish("settings.updated", settingsPayload(updated))
		publishDomainAudit(deps, r, "audit.settings", "settings.update", "allowed", map[string]any{
			"restartRequired": true,
			"playbackPolicy":  updated.PlaybackPolicy,
			"syncInterval":    updated.SyncIntervalMins,
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"config":          settingsPayload(updated),
			"runtimePaths":    runtimePaths(updated),
			"restartRequired": true,
		})
	}
}

func metadataSourceSettingsUpdateHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Movie         []string `json:"movie"`
			Series        []string `json:"series"`
			MovieArtwork  []string `json:"movieArtwork"`
			SeriesArtwork []string `json:"seriesArtwork"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}

		movie := metasources.NormalizeRequestedSourceOrder("movie", request.Movie)
		series := metasources.NormalizeRequestedSourceOrder("series", request.Series)
		movieArtwork := metasources.NormalizeRequestedArtworkOrder("movie", request.MovieArtwork)
		seriesArtwork := metasources.NormalizeRequestedArtworkOrder("series", request.SeriesArtwork)
		if len(movie) == 0 {
			writeError(w, http.StatusBadRequest, "choose at least one movie metadata source")
			return
		}
		if len(series) == 0 {
			writeError(w, http.StatusBadRequest, "choose at least one TV metadata source")
			return
		}
		if len(movieArtwork) == 0 {
			writeError(w, http.StatusBadRequest, "choose at least one movie artwork source")
			return
		}
		if len(seriesArtwork) == 0 {
			writeError(w, http.StatusBadRequest, "choose at least one TV artwork source")
			return
		}

		updated := currentConfig(deps)
		updated.MovieMetadataSources = append([]string(nil), movie...)
		updated.SeriesMetadataSources = append([]string(nil), series...)
		updated.MovieArtworkSources = append([]string(nil), movieArtwork...)
		updated.SeriesArtworkSources = append([]string(nil), seriesArtwork...)
		if err := config.SaveFile(deps.Config.DataDir, updated); err != nil {
			writeError(w, http.StatusInternalServerError, "metadata source settings save failed")
			return
		}

		librariesList, err := deps.Catalog.ListLibraries(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "metadata source settings save failed")
			return
		}
		for _, library := range librariesList {
			switch library.Kind {
			case libraries.KindMovies:
				library.MetadataSources = append([]string(nil), movie...)
				library.ArtworkSources = append([]string(nil), movieArtwork...)
			case libraries.KindTV:
				library.MetadataSources = append([]string(nil), series...)
				library.ArtworkSources = append([]string(nil), seriesArtwork...)
			default:
				continue
			}
			saved, saveErr := deps.Catalog.SaveLibrary(r.Context(), library)
			if saveErr != nil {
				writeError(w, http.StatusInternalServerError, "metadata source settings save failed")
				return
			}
			deps.Libraries.Set(saved)
			deps.Events.Publish("library.updated", saved)
		}

		payload := metadataSourcePreferencesPayload(updated)
		deps.Events.Publish("settings.updated", map[string]any{"metadataSourcePreferences": payload})
		publishDomainAudit(deps, r, "audit.settings", "settings.metadata_sources.update", "allowed", map[string]any{
			"movieSources":         payload["movie"],
			"seriesSources":        payload["series"],
			"movieArtworkSources":  payload["movieArtwork"],
			"seriesArtworkSources": payload["seriesArtwork"],
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"metadataSourcePreferences": payload,
			"metadataSources":           metadataSourceCatalogPayload(r.Context(), updated, deps.Metadata),
			"restartRequired":           false,
		})
	}
}

func browseFolderPayload(path string) (map[string]any, int) {
	resolved, err := resolveBrowsePath(path)
	if err != nil {
		return map[string]any{
			"path":        path,
			"currentPath": path,
			"roots":       browseRoots(),
			"entries":     browseRootEntries(),
			"error":       err.Error(),
		}, http.StatusBadRequest
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return map[string]any{
			"path":        resolved,
			"currentPath": resolved,
			"parent":      parentFolder(resolved),
			"parentPath":  parentFolder(resolved),
			"roots":       browseRoots(),
			"error":       err.Error(),
		}, http.StatusOK
	}
	if !info.IsDir() {
		return map[string]any{
			"path":        resolved,
			"currentPath": resolved,
			"parent":      parentFolder(resolved),
			"parentPath":  parentFolder(resolved),
			"roots":       browseRoots(),
			"error":       "path is not a folder",
		}, http.StatusOK
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return map[string]any{
			"path":        resolved,
			"currentPath": resolved,
			"parent":      parentFolder(resolved),
			"parentPath":  parentFolder(resolved),
			"roots":       browseRoots(),
			"writable":    false,
			"error":       err.Error(),
		}, http.StatusOK
	}
	folders := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		folders = append(folders, map[string]any{
			"name":  name,
			"path":  filepath.Join(resolved, name),
			"isDir": true,
		})
	}
	sort.Slice(folders, func(i, j int) bool {
		return strings.ToLower(folders[i]["name"].(string)) < strings.ToLower(folders[j]["name"].(string))
	})
	writable, message := pathReady(resolved)
	return map[string]any{
		"path":        resolved,
		"currentPath": resolved,
		"parent":      parentFolder(resolved),
		"parentPath":  parentFolder(resolved),
		"roots":       browseRoots(),
		"entries":     folders,
		"writable":    writable,
		"message":     message,
	}, http.StatusOK
}

func browseRootPayload(dataDir string) map[string]any {
	roots := browseRoots()
	return map[string]any{
		"path":        "",
		"currentPath": "",
		"parent":      nil,
		"parentPath":  nil,
		"roots":       roots,
		"entries":     browseRootEntries(),
		"writable":    false,
		"message":     "Choose a server-visible folder. For NAS paths, use a mapped drive or type a UNC path such as \\\\server\\share.",
		"dataDir":     dataDir,
	}
}

func resolveBrowsePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return filepath.Abs(".")
	}
	cleaned := filepath.Clean(os.ExpandEnv(path))
	if filepath.IsAbs(cleaned) {
		return cleaned, nil
	}
	return filepath.Abs(cleaned)
}

func parentFolder(path string) string {
	parent := filepath.Dir(path)
	if parent == path || parent == "." {
		return ""
	}
	return parent
}

func browseRoots() []map[string]any {
	roots := []map[string]any{}
	if runtime.GOOS == "windows" {
		for drive := 'A'; drive <= 'Z'; drive++ {
			root := string(drive) + `:\`
			if info, err := os.Stat(root); err == nil && info.IsDir() {
				roots = append(roots, map[string]any{"name": strings.TrimSuffix(root, `\`), "path": root, "isDir": true})
			}
		}
		return roots
	}
	roots = append(roots, map[string]any{"name": "Root", "path": string(filepath.Separator), "isDir": true})
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, map[string]any{"name": "Home", "path": home, "isDir": true})
	}
	return roots
}

func browseRootEntries() []map[string]any {
	roots := browseRoots()
	entries := make([]map[string]any, 0, len(roots))
	for _, root := range roots {
		entries = append(entries, map[string]any{
			"name":  root["name"],
			"path":  root["path"],
			"isDir": true,
		})
	}
	return entries
}

func hardwareAccelerationStatus(cfg config.Config) map[string]any {
	encoders, err := detectHardwareEncoders(cfg.FFmpegPath)
	available := len(encoders) > 0
	unlockState := "locked"
	if cfg.HardwareUnlocked {
		unlockState = "unlocked"
	}
	status := "not_detected"
	if err != nil {
		status = "unknown"
	}
	if available {
		status = "available"
	}
	result := map[string]any{
		"status":         status,
		"available":      available,
		"unlockState":    unlockState,
		"gpuWorkers":     cfg.GPUWorkers,
		"encoders":       encoders,
		"recommendation": hardwareAccelerationRecommendation(available, cfg.GPUWorkers),
	}
	if err != nil {
		result["error"] = err.Error()
	}
	// Include last-run test cache and the best working encoder (no re-test).
	if cache := loadHWTestCache(cfg.DataDir); cache != nil {
		result["lastTest"] = map[string]any{
			"status":   cache.Status,
			"working":  cache.Working,
			"tested":   cache.Tested,
			"tests":    cache.Tests,
			"testedAt": cache.TestedAt,
		}
	}
	if encID, encLabel := bestCachedEncoder(cfg.DataDir); encID != "" {
		result["selectedEncoder"] = map[string]any{"id": encID, "label": encLabel}
	}
	return result
}

func hardwareAccelerationRecommendation(available bool, gpuWorkers int) string {
	if !available {
		return "No FFmpeg hardware encoder support was detected. Video conversion will fall back to CPU until a compatible GPU driver and FFmpeg build are available."
	}
	if gpuWorkers <= 0 {
		return "FFmpeg exposes hardware encoder support, but GPU worker slots are disabled. Enable one or more slots to reserve GPU conversion capacity."
	}
	return "FFmpeg exposes hardware encoder support. Once licensed and runtime-tested, Xuva can use GPU conversion for heavy video routes and subtitle burn-in."
}

func playbackPolicyStatus(policy string) map[string]any {
	policy = normalizedPlaybackPolicy(policy)
	labels := map[string]string{
		"original_only": "Original Only",
		"light":         "Light Compatibility",
		"full":          "Full Compatibility",
		"cinema":        "Cinema Server",
	}
	descriptions := map[string]string{
		"original_only": "Xuva plays the original file only. If this device cannot play it as-is, Xuva shows fallback options instead of converting automatically.",
		"light":         "Xuva may repackage while playing or convert audio. Video stays untouched, so quality is preserved.",
		"full":          "Xuva may convert video while playing when a device needs it. Work is temporary unless the user creates an optimized version.",
		"cinema":        "Xuva allows heavier live conversion and future automated optimization controls for power users.",
	}
	return map[string]any{
		"id":          policy,
		"label":       labels[policy],
		"description": descriptions[policy],
	}
}

func normalizedPlaybackPolicy(policy string) string {
	switch policy {
	case "light", "full", "cinema":
		return policy
	default:
		return "original_only"
	}
}

func playbackPolicyAllows(policy string, decision playback.Decision) bool {
	switch decision.Mode {
	case playback.DirectPlay:
		return true
	}
	switch normalizedPlaybackPolicy(policy) {
	case "light":
		return decision.Mode == playback.Remux || decision.Mode == playback.AudioTranscode
	case "full", "cinema":
		return decision.Mode == playback.Remux || decision.Mode == playback.AudioTranscode || decision.Mode == playback.AdaptiveStream || decision.Mode == playback.VideoTranscode || decision.Mode == playback.SubtitleBurn
	default:
		return false
	}
}

func playbackPolicyFallbacks(policy string, decision playback.Decision) []map[string]string {
	mode := string(decision.Mode)
	fallbacks := []map[string]string{
		{"label": "Play on a compatible device", "detail": "Use a player that supports this file as-is so Xuva does not need to convert anything."},
		{"label": "Allow this session to adapt", "detail": "Switch to a compatibility policy that permits the required playback work: " + mode + "."},
	}
	if decision.Mode == playback.Remux {
		fallbacks = append(fallbacks, map[string]string{"label": "Allow live repackage", "detail": "Repackage while playing. No video quality loss and no permanent copy."})
	}
	if decision.Mode == playback.AudioTranscode {
		fallbacks = append(fallbacks, map[string]string{"label": "Allow audio conversion", "detail": "Convert audio while playing. Video remains untouched and temporary work is discarded."})
	}
	if decision.Mode == playback.VideoTranscode || decision.Mode == playback.SubtitleBurn {
		fallbacks = append(fallbacks, map[string]string{"label": "Allow live video conversion", "detail": "Convert video only while playing. This may use high CPU or GPU if hardware acceleration is unlocked and working."})
	}
	fallbacks = append(fallbacks, map[string]string{"label": "Create optimized version", "detail": "Optional stored version for easier future playback. Xuva should show size, quality, and storage impact first."})
	return fallbacks
}

type hwTestCache struct {
	Status   string           `json:"status"`
	Working  int              `json:"working"`
	Tested   int              `json:"tested"`
	Tests    []map[string]any `json:"tests"`
	TestedAt string           `json:"testedAt"`
}

func hwTestCachePath(dataDir string) string {
	return filepath.Join(dataDir, "hardware_test.json")
}

func loadHWTestCache(dataDir string) *hwTestCache {
	raw, err := os.ReadFile(hwTestCachePath(dataDir))
	if err != nil {
		return nil
	}
	var cache hwTestCache
	if err := json.Unmarshal(raw, &cache); err != nil {
		return nil
	}
	return &cache
}

func saveHWTestCache(dataDir string, cache hwTestCache) {
	raw, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(hwTestCachePath(dataDir), raw, 0o644)
}

// encoderDisplayLabel maps an ffmpeg encoder ID to a user-facing vendor label.
func encoderDisplayLabel(encoderID string) string {
	switch {
	case strings.Contains(encoderID, "_nvenc"):
		return "NVIDIA NVENC"
	case strings.Contains(encoderID, "_qsv"):
		return "Intel QSV"
	case strings.Contains(encoderID, "_amf"):
		return "AMD AMF"
	case strings.Contains(encoderID, "_vaapi"):
		return "VAAPI"
	case strings.Contains(encoderID, "_videotoolbox"):
		return "Apple VideoToolbox"
	case encoderID == "":
		return "CPU"
	}
	return encoderID
}

// bestCachedEncoder reads the hardware test cache and returns the encoder ID and
// display label for the best working encoder in the preferred order, without
// re-running any tests.
func bestCachedEncoder(dataDir string) (id, label string) {
	cache := loadHWTestCache(dataDir)
	if cache == nil || cache.Status != "passed" {
		return "", "CPU"
	}
	working := make(map[string]bool, len(cache.Tests))
	for _, test := range cache.Tests {
		tid, _ := test["id"].(string)
		ok, _ := test["ok"].(bool)
		if ok {
			working[tid] = true
		}
	}
	preferred := []string{"h264_qsv", "h264_nvenc", "h264_amf", "h264_vaapi", "h264_videotoolbox"}
	for _, pid := range preferred {
		if working[pid] {
			return pid, encoderDisplayLabel(pid)
		}
	}
	return "", "CPU"
}

func hardwareTestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		encoders, err := detectHardwareEncoders(deps.Config.FFmpegPath)
		if err != nil {
			result := map[string]any{
				"status": "failed",
				"error":  err.Error(),
				"tests":  []map[string]any{},
			}
			saveHWTestCache(deps.Config.DataDir, hwTestCache{
				Status:   "failed",
				Tests:    []map[string]any{},
				TestedAt: time.Now().UTC().Format(time.RFC3339),
			})
			writeJSON(w, http.StatusOK, result)
			return
		}
		tests := make([]map[string]any, 0, len(encoders))
		working := 0
		for _, encoder := range encoders {
			id := encoder["id"]
			result := testHardwareEncoder(r.Context(), deps.Config.FFmpegPath, id)
			if ok, _ := result["ok"].(bool); ok {
				working++
			}
			for key, value := range encoder {
				result[key] = value
			}
			tests = append(tests, result)
		}
		status := "failed"
		if working > 0 {
			status = "passed"
		}
		testedAt := time.Now().UTC().Format(time.RFC3339)
		cache := hwTestCache{
			Status:   status,
			Working:  working,
			Tested:   len(tests),
			Tests:    tests,
			TestedAt: testedAt,
		}
		saveHWTestCache(deps.Config.DataDir, cache)
		writeJSON(w, http.StatusOK, map[string]any{
			"status":   status,
			"working":  working,
			"tested":   len(tests),
			"tests":    tests,
			"testedAt": testedAt,
		})
	}
}

func testHardwareEncoder(parent context.Context, ffmpegPath string, encoder string) map[string]any {
	if strings.TrimSpace(ffmpegPath) == "" {
		ffmpegPath = "ffmpeg"
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	// 320x240 is safely above all encoder minimums (NVENC HEVC requires ≥128px
	// each dimension; 128x72 triggered "Frame Dimension less than minimum").
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=1",
		"-frames:v", "1", "-an",
		"-c:v", encoder,
	}
	// QSV on Windows requires an explicit quality mode; without it the MFX
	// session initialises but rejects the encode request.
	if strings.HasSuffix(encoder, "_qsv") {
		args = append(args, "-global_quality", "25")
	}
	args = append(args, "-f", "null", os.DevNull)
	output, err := exec.CommandContext(ctx, ffmpegPath, args...).CombinedOutput()
	result := map[string]any{
		"ok":         err == nil && ctx.Err() == nil,
		"durationMs": time.Since(started).Milliseconds(),
	}
	if ctx.Err() != nil {
		result["error"] = ctx.Err().Error()
	} else if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		if len(message) > 500 {
			message = message[:500]
		}
		result["error"] = message
	}
	return result
}

func detectHardwareEncoders(ffmpegPath string) ([]map[string]string, error) {
	if strings.TrimSpace(ffmpegPath) == "" {
		ffmpegPath = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-encoders").CombinedOutput()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("ffmpeg encoder scan failed: %w", err)
	}
	text := strings.ToLower(string(output))
	candidates := []struct {
		id     string
		label  string
		vendor string
		codec  string
	}{
		{"h264_qsv", "H.264 Intel Quick Sync", "Intel Quick Sync", "H.264"},
		{"hevc_qsv", "HEVC Intel Quick Sync", "Intel Quick Sync", "HEVC"},
		{"av1_qsv", "AV1 Intel Quick Sync", "Intel Quick Sync", "AV1"},
		{"h264_nvenc", "H.264 NVIDIA NVENC", "NVIDIA NVENC", "H.264"},
		{"hevc_nvenc", "HEVC NVIDIA NVENC", "NVIDIA NVENC", "HEVC"},
		{"av1_nvenc", "AV1 NVIDIA NVENC", "NVIDIA NVENC", "AV1"},
		{"h264_amf", "H.264 AMD AMF", "AMD AMF", "H.264"},
		{"hevc_amf", "HEVC AMD AMF", "AMD AMF", "HEVC"},
		{"av1_amf", "AV1 AMD AMF", "AMD AMF", "AV1"},
		{"h264_vaapi", "H.264 VAAPI", "VAAPI", "H.264"},
		{"hevc_vaapi", "HEVC VAAPI", "VAAPI", "HEVC"},
		{"av1_vaapi", "AV1 VAAPI", "VAAPI", "AV1"},
		{"h264_videotoolbox", "H.264 VideoToolbox", "Apple VideoToolbox", "H.264"},
		{"hevc_videotoolbox", "HEVC VideoToolbox", "Apple VideoToolbox", "HEVC"},
	}
	isWindows := runtime.GOOS == "windows"
	isDarwin := runtime.GOOS == "darwin"
	encoders := make([]map[string]string, 0)
	for _, candidate := range candidates {
		// VAAPI requires the Linux DRM/KMS subsystem — never works on Windows.
		if strings.HasSuffix(candidate.id, "_vaapi") && isWindows {
			continue
		}
		// VideoToolbox is macOS-only.
		if strings.HasSuffix(candidate.id, "_videotoolbox") && !isDarwin {
			continue
		}
		if strings.Contains(text, candidate.id) {
			encoders = append(encoders, map[string]string{
				"id":     candidate.id,
				"label":  candidate.label,
				"vendor": candidate.vendor,
				"codec":  candidate.codec,
			})
		}
	}
	return encoders, nil
}

func selectedHardwareEncoder(ctx context.Context, cfg config.Config) (string, bool) {
	if !cfg.HardwareUnlocked || cfg.GPUWorkers <= 0 {
		return "", false
	}
	encoders, err := detectHardwareEncoders(cfg.FFmpegPath)
	if err != nil {
		return "", false
	}
	preferred := []string{"h264_qsv", "h264_nvenc", "h264_amf", "h264_vaapi", "h264_videotoolbox"}
	for _, id := range preferred {
		for _, encoder := range encoders {
			if encoder["id"] == id {
				result := testHardwareEncoder(ctx, cfg.FFmpegPath, id)
				if ok, _ := result["ok"].(bool); ok {
					return id, true
				}
			}
		}
	}
	return "", false
}

func systemStatusHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Collect base stats from the systemstats package and graft on
		// serverStartedAt so the dashboard can compute uptime client-side
		// without a separate /api/about round-trip.
		snap := systemstats.Collect(runtimePaths(currentConfig(deps)))
		payload := map[string]any{
			"collectedAt": snap.CollectedAt,
			"cpu":         snap.CPU,
			"memory":      snap.Memory,
			"process":     snap.Process,
			"network":     snap.Network,
			"disks":       snap.Disks,
		}
		if snap.GPU != nil {
			payload["gpu"] = snap.GPU
		}
		if !deps.StartedAt.IsZero() {
			payload["serverStartedAt"] = deps.StartedAt.Format(time.RFC3339)
		}
		writeJSON(w, http.StatusOK, payload)
	}
}

func currentConfig(deps Deps) config.Config {
	if saved, err := config.LoadFile(deps.Config.DataDir); err == nil {
		return config.Merge(deps.Config, saved)
	}
	return deps.Config
}

func settingsPayload(cfg config.Config) map[string]any {
	return map[string]any{
		"serverName":             configDisplayName(cfg.ServerName),
		"httpAddr":               cfg.HTTPAddr,
		"canonicalWebOrigin":     cfg.CanonicalWebOrigin,
		"dataDir":                cfg.DataDir,
		"transcodeDir":           cfg.TranscodeDir,
		"downloadsDir":           cfg.DownloadsDir,
		"metadataDir":            cfg.MetadataDir,
		"cacheDir":               cfg.CacheDir,
		"tempDir":                cfg.TempDir,
		"ffmpegPath":             cfg.FFmpegPath,
		"ffprobePath":            cfg.FFprobePath,
		"eventBuffer":            cfg.EventBuffer,
		"scanWorkers":            cfg.ScanWorkers,
		"probeWorkers":           cfg.ProbeWorkers,
		"transcodeWorkers":       cfg.TranscodeWorkers,
		"gpuWorkers":             cfg.GPUWorkers,
		"hardwareUnlocked":       cfg.HardwareUnlocked,
		"playbackPolicy":         cfg.PlaybackPolicy,
		"librarySyncMode":        cfg.LibrarySyncMode,
		"syncIntervalMins":       cfg.SyncIntervalMins,
		"watchDebounceSecs":      cfg.WatchDebounceSecs,
		"probeBatchLimit":        cfg.ProbeBatchLimit,
		"allowedOrigins":         cfg.AllowedOrigins,
		"country":                cfg.Country,
		"timezone":               cfg.Timezone,
		"metadataLanguage":       cfg.MetadataLanguage,
		"preferTextSubtitles":    cfg.PreferTextSubtitles,
		"originalQualityOnly":    cfg.OriginalQualityOnly,
		"defaultSubtitlesMovies": cfg.DefaultSubtitlesMovies,
		"defaultSubtitlesTV":     cfg.DefaultSubtitlesTV,
	}
}

func configDisplayName(value string) string {
	if strings.TrimSpace(value) == "My Server" {
		return "Xuva"
	}
	normalized, err := config.NormalizeServerName(value)
	if err != nil {
		return "Xuva"
	}
	return normalized
}

func runtimePaths(cfg config.Config) map[string]string {
	return map[string]string{
		"data":      cfg.DataDir,
		"transcode": cfg.TranscodeDir,
		"downloads": cfg.DownloadsDir,
		"metadata":  cfg.MetadataDir,
		"cache":     cfg.CacheDir,
		"temp":      cfg.TempDir,
	}
}

func mergeString(target *string, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		*target = value
	}
}

func mergeInt(target *int, value int) {
	if value > 0 {
		*target = value
	}
}

func remoteAccessHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"httpAddr":       deps.Config.HTTPAddr,
			"lanAddresses":   lanAddresses(deps.Config.HTTPAddr),
			"wanAddress":     "",
			"wanLookup":      "available_on_request",
			"diagnostics":    "available",
			"failureClasses": []string{remote.ClassNotConfigured, remote.ClassPrivateRoute, remote.ClassDNS, remote.ClassNATFirewall, remote.ClassCertificate, remote.ClassThroughput},
			"recommendation": "Use your own VPN, reverse proxy, or port-forwarding setup. Xuva does not require hosted relay servers.",
		})
	}
}

func remoteDiagnosticsHandler(deps Deps) http.HandlerFunc {
	checker := remote.NewChecker()
	return func(w http.ResponseWriter, r *http.Request) {
		var payload remote.Request
		if !decodeJSON(w, r, &payload) {
			return
		}
		result := checker.Diagnose(r.Context(), payload, lanAddresses(deps.Config.HTTPAddr))
		writeJSON(w, http.StatusOK, result)
	}
}

func wanAddressHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client := http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("https://api.ipify.org?format=json")
		if err != nil {
			writeError(w, http.StatusBadGateway, "wan lookup failed")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			writeError(w, http.StatusBadGateway, "wan lookup failed")
			return
		}
		var payload map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			writeError(w, http.StatusBadGateway, "wan lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"publicIp": payload["ip"],
			"source":   "https://api.ipify.org",
			"note":     "WAN address was requested explicitly and discovered using an external IP check service.",
		})
	}
}

func mediaSourcesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := deps.Catalog.ListMediaSources(r.Context(), queryInt(r, "limit", 100), r.URL.Query().Get("unprobed") == "true")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source list failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mediaSources": items})
	}
}

func mediaSourceDetailHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		item, ok, err := deps.Catalog.GetMediaSource(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source detail failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		writeJSON(w, http.StatusOK, item)
	}
}

func mediaSourceDeleteHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := deps.Catalog.DeleteMediaSource(r.Context(), id); err != nil {
			if strings.Contains(err.Error(), "not found") {
				writeError(w, http.StatusNotFound, "media source not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "delete failed: "+err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func mediaSourceTracksHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tracks, ok, err := deps.Catalog.GetMediaSourceTracks(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source tracks failed")
			return
		}
		if !ok {
			writeJSON(w, http.StatusOK, catalog.MediaSourceTracks{})
			return
		}
		writeJSON(w, http.StatusOK, tracks)
	}
}

func mediaSourceProbeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		item, ok, err := deps.Catalog.GetMediaSource(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		result, err := deps.Probe.Probe(r.Context(), item.Path)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "ffprobe failed")
			return
		}
		if err := deps.Catalog.SaveProbe(r.Context(), item.ID, catalog.ProbeResult{
			Container:       result.Container,
			DurationSeconds: result.DurationSeconds,
			Bitrate:         result.Bitrate,
			VideoCodec:      result.VideoCodec,
			VideoProfile:    result.VideoProfile,
			VideoLevel:      result.VideoLevel,
			VideoBitDepth:   result.VideoBitDepth,
			VideoFrameRate:  result.VideoFrameRate,
			PixelFormat:     result.PixelFormat,
			ColorPrimaries:  result.ColorPrimaries,
			ColorTransfer:   result.ColorTransfer,
			ColorSpace:      result.ColorSpace,
			HDRFormat:       result.HDRFormat,
			Width:           result.Width,
			Height:          result.Height,
			AudioStreams:    result.AudioStreams,
			SubtitleStreams: result.SubtitleStreams,
			RawJSON:         result.RawJSON,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "probe persistence failed")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func probesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"probes": deps.Probes.List()})
	}
}

// jobsStatusHandler returns a unified snapshot of all three automation jobs
// (scan, metadata, probe) plus the current probe job list. Used by /settings/activity.
func jobsStatusHandler(deps Deps) http.HandlerFunc {
	snapshotOrNil := func(s JobAutoStater) map[string]any {
		if s == nil {
			return map[string]any{"status": "disabled", "enabled": false}
		}
		return s.Snapshot()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		scanSnap := snapshotOrNil(deps.ScanAuto)
		metaSnap := snapshotOrNil(deps.MetadataAuto)
		probeSnap := snapshotOrNil(deps.ProbeAuto)

		// Enrich the probe snapshot with the live probe job list so the UI can
		// show per-file progress without a separate /api/probes call.
		if deps.Probes != nil {
			probeSnap["activeJobs"] = deps.Probes.List()
		}

		// Enrich metadata snapshot with backfill status from the service.
		if deps.Metadata != nil {
			metaSnap["backfill"] = deps.Metadata.BackfillStatus()
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"scan":     scanSnap,
			"metadata": metaSnap,
			"probe":    probeSnap,
		})
	}
}

// ── Thumbnail sprite handlers (issue #65) ────────────────────────────────────

func thumbnailStatusHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if deps.Thumbnails == nil {
			writeJSON(w, http.StatusOK, map[string]any{"generated": false, "error": "thumbnail service unavailable"})
			return
		}
		status := deps.Thumbnails.GetStatus(id)
		writeJSON(w, http.StatusOK, status)
	}
}

func thumbnailGenerateHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if deps.Thumbnails == nil {
			writeError(w, http.StatusServiceUnavailable, "thumbnail service unavailable")
			return
		}
		source, ok, err := deps.Catalog.GetMediaSource(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		if err := deps.Thumbnails.Generate(r.Context(), source.ID, source.Path, source.DurationSeconds); err != nil {
			writeError(w, http.StatusInternalServerError, "thumbnail generation failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "generating", "mediaSourceId": id})
	}
}

func thumbnailSpriteHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if deps.Thumbnails == nil {
			writeError(w, http.StatusServiceUnavailable, "thumbnail service unavailable")
			return
		}
		path, err := deps.Thumbnails.SpritePath(id)
		if err != nil {
			writeError(w, http.StatusNotFound, "sprite not available — generate thumbnails first")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, path)
	}
}

func thumbnailVTTHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if deps.Thumbnails == nil {
			writeError(w, http.StatusServiceUnavailable, "thumbnail service unavailable")
			return
		}
		path, err := deps.Thumbnails.VTTPath(id)
		if err != nil {
			writeError(w, http.StatusNotFound, "thumbnail VTT not available — generate thumbnails first")
			return
		}
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, path)
	}
}

func thumbnailChaptersHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if deps.Thumbnails == nil {
			writeError(w, http.StatusServiceUnavailable, "thumbnail service unavailable")
			return
		}
		path, err := deps.Thumbnails.ChaptersPath(id)
		if err != nil {
			writeError(w, http.StatusNotFound, "chapter track not available")
			return
		}
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, path)
	}
}

func probeJobHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, ok := deps.Probes.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "probe job not found")
			return
		}
		writeJSON(w, http.StatusOK, job)
	}
}

func probeStartHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request probes.Request
		if !decodeJSON(w, r, &request) {
			return
		}
		job, err := deps.Probes.Start(r.Context(), request)
		if err != nil {
			writeError(w, queueRejectionStatus(err, http.StatusBadRequest), err.Error())
			return
		}
		publishOperationalEvent(deps, r, "api.probe.accepted", map[string]any{
			"jobId":         job.ID,
			"mediaSourceId": job.MediaSourceID,
			"limit":         job.Limit,
		})
		writeJSON(w, http.StatusAccepted, job)
	}
}

func ensureMediaFileAccessible(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return os.ErrNotExist
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.ErrInvalid
	}
	return nil
}

// remuxStreamHandler pipes FFmpeg output directly to the HTTP response as
// fragmented MP4, enabling instant playback of MKV files without waiting for
// the HLS pre-processing job to complete. The audio is either copied as-is
// (audioMode=copy) or re-encoded to AAC (audioMode=transcode) for browsers
// that cannot decode the source codec (e.g. DTS, TrueHD).
func remuxStreamHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, ok := authorizeStreamRequest(deps, w, r, r.PathValue("id"))
		if !ok {
			return
		}
		defer release()
		item, ok, err := deps.Catalog.GetMediaSource(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		if err := ensureMediaFileAccessible(item.Path); err != nil {
			writeError(w, http.StatusNotFound, "media file is unavailable from the configured library path")
			return
		}

		startTime := queryFloat(r, "startTime", 0)
		audioMode := r.URL.Query().Get("audioMode")
		if audioMode != "transcode" {
			audioMode = "copy"
		}

		ffmpegPath := deps.Config.FFmpegPath
		if strings.TrimSpace(ffmpegPath) == "" {
			ffmpegPath = "ffmpeg"
		}

		var args []string
		// Use fast input seek only when a non-zero start time is requested.
		// Placing -ss before -i enables fast seek (keyframe seek); placing it
		// after would require decoding every frame up to the target -- very slow.
		if startTime > 0 {
			args = append(args, "-ss", fmt.Sprintf("%.3f", startTime))
		}
		args = append(args, "-i", item.Path)
		if audioMode == "transcode" {
			args = append(args, "-c:v", "copy", "-c:a", "aac", "-b:a", "192k")
		} else {
			args = append(args, "-c", "copy")
		}
		args = append(args, "-f", "mp4", "-movflags", "frag_keyframe+empty_moov+dash", "-")

		// CommandContext ensures the FFmpeg process is killed when the HTTP
		// connection closes (context cancelled) so we do not leak child processes.
		cmd := exec.CommandContext(r.Context(), ffmpegPath, args...)

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create ffmpeg pipe")
			return
		}
		cmd.Stderr = nil

		if err := cmd.Start(); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to start ffmpeg: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Accept-Ranges", "none")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)

		// Pipe FFmpeg stdout to the response. io.Copy returns when the client
		// disconnects or FFmpeg exits -- whichever happens first.
		_, _ = io.Copy(w, stdout)

		// Wait for the process to exit so we can reclaim its resources.
		// A broken-pipe exit (client disconnected) is normal and expected.
		_ = cmd.Wait()
	}
}

func mediaSourceStreamHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, ok := authorizeStreamRequest(deps, w, r, r.PathValue("id"))
		if !ok {
			return
		}
		defer release()
		item, ok, err := deps.Catalog.GetMediaSource(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		if err := ensureMediaFileAccessible(item.Path); err != nil {
			writeError(w, http.StatusNotFound, "media file is unavailable from the configured library path")
			return
		}
		http.ServeFile(w, r, item.Path)
	}
}

// mediaSourceDownloadHandler serves the media file as a download attachment.
// It sets Content-Disposition: attachment so browsers and iOS save-to-files
// flows treat the response as a file download rather than a stream.
func mediaSourceDownloadHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, ok := authorizeStreamRequest(deps, w, r, r.PathValue("id"))
		if !ok {
			return
		}
		defer release()
		item, ok, err := deps.Catalog.GetMediaSource(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		if err := ensureMediaFileAccessible(item.Path); err != nil {
			writeError(w, http.StatusNotFound, "media file is unavailable from the configured library path")
			return
		}
		filename := item.Name
		if item.Extension != "" {
			filename = item.Name + "." + item.Extension
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, strings.ReplaceAll(filename, `"`, `\"`)))
		http.ServeFile(w, r, item.Path)
	}
}

func mediaSourceStreamTokenHandler(deps Deps) http.HandlerFunc {
	type request struct {
		SessionID string `json:"sessionId"`
		DeviceID  string `json:"deviceId"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Streaming == nil {
			writeError(w, http.StatusServiceUnavailable, "stream signing is not available")
			return
		}
		resolved, ok := auth.ResolvedSessionFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		var payload request
		if !decodeJSON(w, r, &payload) {
			return
		}
		session, ok := deps.Sessions.Get(payload.SessionID)
		if !ok {
			writeError(w, http.StatusForbidden, "playback session is not active")
			return
		}
		if session.MediaSourceID != r.PathValue("id") || session.UserID != resolved.Principal.ID {
			writeError(w, http.StatusForbidden, "playback session does not match this media source")
			return
		}
		deviceID := firstNonEmpty(payload.DeviceID, session.DeviceID)
		if deviceID == "" || deviceID != session.DeviceID {
			writeError(w, http.StatusForbidden, "playback device does not match this session")
			return
		}
		token, claims, err := deps.Streaming.Issue(streaming.Expected{
			MediaSourceID: session.MediaSourceID,
			SessionID:     session.ID,
			UserID:        session.UserID,
			DeviceID:      session.DeviceID,
		}, 0)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "stream token issue failed")
			return
		}
		query := "?sessionId=" + url.QueryEscape(session.ID) + "&deviceId=" + url.QueryEscape(session.DeviceID) + "&token=" + url.QueryEscape(token)
		writeJSON(w, http.StatusOK, map[string]any{
			"token":           token,
			"expiresAt":       time.Unix(claims.ExpiresAt, 0).UTC().Format(time.RFC3339),
			"streamUrl":       "/api/media-sources/" + session.MediaSourceID + "/stream" + query,
			"subtitleBaseUrl": "/api/media-sources/" + session.MediaSourceID + "/subtitles/",
			"query":           query,
		})
	}
}

func mediaSourceSubtitlesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		item, ok, err := deps.Catalog.GetMediaSource(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sidecars": subtitles.DiscoverSidecars(item.Path)})
	}
}

func mediaSourceSubtitleStreamHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, ok := authorizeStreamRequest(deps, w, r, r.PathValue("id"))
		if !ok {
			return
		}
		defer release()
		item, ok, err := deps.Catalog.GetMediaSource(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		index := parsePathInt(r.PathValue("index"), -1)
		sidecars := subtitles.DiscoverSidecars(item.Path)
		if index < 0 || index >= len(sidecars) {
			writeError(w, http.StatusNotFound, "subtitle not found")
			return
		}
		if sidecars[index].Format == "vtt" {
			w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		http.ServeFile(w, r, sidecars[index].Path)
	}
}

func mediaSourceSubtitleConvertHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		item, ok, err := deps.Catalog.GetMediaSource(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		index := parsePathInt(r.PathValue("index"), -1)
		sidecars := subtitles.DiscoverSidecars(item.Path)
		if index < 0 || index >= len(sidecars) {
			writeError(w, http.StatusNotFound, "subtitle not found")
			return
		}
		service := deps.Subtitles
		if service == nil {
			service = subtitles.NewService()
		}
		plan := service.PlanConversion(sidecars[index], firstNonEmpty(r.URL.Query().Get("clientProfile"), "web"))
		writeJSON(w, http.StatusAccepted, map[string]any{
			"mediaSourceId": item.ID,
			"subtitleIndex": index,
			"sidecar":       sidecars[index],
			"conversion":    plan,
		})
	}
}

func authorizeStreamRequest(deps Deps, w http.ResponseWriter, r *http.Request, mediaSourceID string) (func(), bool) {
	if deps.Auth == nil || deps.Auth.Disabled() {
		return func() {}, true
	}
	if deps.Streaming == nil {
		writeError(w, http.StatusServiceUnavailable, "stream signing is not available")
		return nil, false
	}
	resolved, ok := auth.ResolvedSessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	sessionID := r.URL.Query().Get("sessionId")
	deviceID := r.URL.Query().Get("deviceId")
	session, ok := deps.Sessions.Get(sessionID)
	if !ok {
		publishStreamDenied(deps, r, resolved, mediaSourceID, sessionID, "session_inactive")
		writeError(w, http.StatusForbidden, "playback session is not active")
		return nil, false
	}
	if session.MediaSourceID != mediaSourceID || session.UserID != resolved.Principal.ID || session.DeviceID != deviceID {
		publishStreamDenied(deps, r, resolved, mediaSourceID, sessionID, "session_mismatch")
		writeError(w, http.StatusForbidden, "playback session does not match this stream")
		return nil, false
	}
	// Parental control: even with a valid session+token, a restricted profile
	// must not be able to stream/remux/download an item whose rating exceeds
	// its ceiling. This is the chokepoint for all byte-serving handlers, so it
	// closes the IDOR where a kids profile streamed any item by media-source ID.
	if blocked, reason := streamRatingBlocked(r.Context(), deps, mediaSourceID); blocked {
		publishStreamDenied(deps, r, resolved, mediaSourceID, sessionID, reason)
		writeError(w, http.StatusForbidden, "this title is not available for the active profile")
		return nil, false
	}

	_, release, err := deps.Streaming.Validate(r.URL.Query().Get("token"), streaming.Expected{
		MediaSourceID: mediaSourceID,
		SessionID:     session.ID,
		UserID:        session.UserID,
		DeviceID:      session.DeviceID,
	})
	if err != nil {
		status, reason := streamTokenError(err)
		publishStreamDenied(deps, r, resolved, mediaSourceID, sessionID, reason)
		writeError(w, status, reason)
		return nil, false
	}
	return release, true
}

// streamRatingBlocked reports whether the active profile's max-rating ceiling
// forbids playback of the media source. Returns (false, "") when no ceiling is
// set or the rating is allowed. A lookup error fails open with a logged reason
// rather than blocking legitimate playback — the ceiling is a content filter,
// not the primary access-control boundary (that's the session+token check).
func streamRatingBlocked(ctx context.Context, deps Deps, mediaSourceID string) (bool, string) {
	ceiling := activeMaxRating(ctx, deps)
	if ceiling == "" {
		return false, ""
	}
	rating, ok, err := deps.Catalog.MediaSourceContentRating(ctx, mediaSourceID)
	if err != nil {
		slog.Warn("stream rating lookup failed", "mediaSourceId", mediaSourceID, "err", err)
		return false, ""
	}
	if !ok {
		return false, ""
	}
	if catalog.RatingWithinCeiling(rating, ceiling) {
		return false, ""
	}
	return true, "rating_ceiling"
}

func streamTokenError(err error) (int, string) {
	switch {
	case errors.Is(err, streaming.ErrMissingToken):
		return http.StatusUnauthorized, "stream token is required"
	case errors.Is(err, streaming.ErrExpiredToken):
		return http.StatusForbidden, "stream token is expired"
	case errors.Is(err, streaming.ErrInvalidSignature), errors.Is(err, streaming.ErrMalformedToken), errors.Is(err, streaming.ErrSigningKeyMissing):
		return http.StatusForbidden, "stream token is invalid"
	case errors.Is(err, streaming.ErrTokenMismatch):
		return http.StatusForbidden, "stream token does not match playback session"
	case errors.Is(err, streaming.ErrStreamLimit):
		return http.StatusTooManyRequests, "stream limit exceeded"
	default:
		return http.StatusForbidden, "stream token rejected"
	}
}

func publishStreamDenied(deps Deps, r *http.Request, resolved auth.ResolvedSession, mediaSourceID string, sessionID string, reason string) {
	if deps.Events == nil {
		return
	}
	deps.Events.Publish("audit.stream.denied", map[string]any{
		"correlationId": observability.CorrelationID(r.Context()),
		"userId":        resolved.Principal.ID,
		"username":      resolved.Principal.Username,
		"role":          resolved.Principal.Role,
		"method":        r.Method,
		"path":          r.URL.Path,
		"mediaSourceId": mediaSourceID,
		"sessionId":     sessionID,
		"reason":        reason,
		"createdAt":     time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func publishOperationalEvent(deps Deps, r *http.Request, eventType string, fields map[string]any) {
	if deps.Events == nil {
		return
	}
	payload := map[string]any{
		"correlationId": observability.CorrelationID(r.Context()),
		"method":        r.Method,
		"path":          r.URL.Path,
		"createdAt":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	for key, value := range fields {
		payload[key] = value
	}
	deps.Events.Publish(eventType, payload)
}

func workHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"work": deps.Transcode.List()})
	}
}

func workStartHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request transcode.Request
		if !decodeJSON(w, r, &request) {
			return
		}
		if request.MediaSourceID != "" && request.SourcePath == "" {
			source, ok, err := deps.Catalog.GetMediaSource(r.Context(), request.MediaSourceID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "media source lookup failed")
				return
			}
			if !ok {
				writeError(w, http.StatusNotFound, "media source not found")
				return
			}
			request.SourcePath = source.Path
		}
		if request.Mode == transcode.ModeTranscode && request.VideoEncoder == "" {
			if encoder, ok := selectedHardwareEncoder(r.Context(), deps.Config); ok {
				request.Acceleration = "hardware"
				request.VideoEncoder = encoder
			}
		}
		job, err := deps.Transcode.Start(r.Context(), request)
		if err != nil {
			writeError(w, queueRejectionStatus(err, http.StatusBadRequest), err.Error())
			return
		}
		publishOperationalEvent(deps, r, "api.work.accepted", map[string]any{
			"jobId":         job.ID,
			"mediaSourceId": job.MediaSourceID,
			"mode":          string(job.Mode),
		})
		writeJSON(w, http.StatusAccepted, job)
	}
}

func workCancelHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, ok := deps.Transcode.Cancel(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "work job not found")
			return
		}
		writeJSON(w, http.StatusOK, job)
	}
}

func workFileHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, ok := deps.Transcode.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "work job not found")
			return
		}
		if job.Status != transcode.StatusCompleted || job.OutputPath == "" {
			writeError(w, http.StatusConflict, "work job is not ready")
			return
		}
		http.ServeFile(w, r, job.OutputPath)
	}
}

func workHLSHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, ok := deps.Transcode.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "work job not found")
			return
		}
		if job.OutputDir == "" {
			writeError(w, http.StatusConflict, "job is not in HLS mode")
			return
		}
		if job.Status != transcode.StatusStreaming && job.Status != transcode.StatusCompleted {
			writeError(w, http.StatusConflict, "HLS segments not yet available")
			return
		}
		file := r.PathValue("file")
		if file == "" || strings.ContainsAny(file, "/\\") || strings.Contains(file, "..") {
			writeError(w, http.StatusBadRequest, "invalid segment filename")
			return
		}
		http.ServeFile(w, r, filepath.Join(job.OutputDir, file))
	}
}

func downloadsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"downloads": deps.Downloads.List()})
	}
}

func downloadJobHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, ok := deps.Downloads.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "download job not found")
			return
		}
		writeJSON(w, http.StatusOK, job)
	}
}

func downloadStartHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request downloads.Request
		if !decodeJSON(w, r, &request) {
			return
		}
		if request.MediaSourceID != "" && request.SourcePath == "" {
			source, ok, err := deps.Catalog.GetMediaSource(r.Context(), request.MediaSourceID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "media source lookup failed")
				return
			}
			if !ok {
				writeError(w, http.StatusNotFound, "media source not found")
				return
			}
			request.SourcePath = source.Path
			request.SourceName = source.Name
		}
		if request.TargetProfile != downloads.ProfileOriginal && request.VideoEncoder == "" {
			if encoder, ok := selectedHardwareEncoder(r.Context(), deps.Config); ok {
				request.Acceleration = "hardware"
				request.VideoEncoder = encoder
			}
		}
		job, err := deps.Downloads.Start(r.Context(), request)
		if err != nil {
			writeError(w, queueRejectionStatus(err, http.StatusBadRequest), err.Error())
			return
		}
		publishOperationalEvent(deps, r, "api.download.accepted", map[string]any{
			"jobId":         job.ID,
			"mediaSourceId": job.MediaSourceID,
			"profile":       job.TargetProfile,
		})
		writeJSON(w, http.StatusAccepted, job)
	}
}

func downloadFileHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, ok := deps.Downloads.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "download job not found")
			return
		}
		if job.Status != downloads.StatusCompleted {
			writeError(w, http.StatusConflict, "download is not ready")
			return
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(job.OutputPath)))
		http.ServeFile(w, r, job.OutputPath)
	}
}

func deviceProfilesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"profiles": deps.Devices.Profiles()})
	}
}

func approvedDevicesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Devices == nil {
			writeJSON(w, http.StatusOK, map[string]any{"devices": []map[string]any{}})
			return
		}
		items, err := deps.Devices.ListApproved(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "approved devices lookup failed")
			return
		}
		payload := make([]map[string]any, 0, len(items))
		for _, item := range items {
			payload = append(payload, approvedDevicePayload(item))
		}
		writeJSON(w, http.StatusOK, map[string]any{"devices": payload})
	}
}

func approvedDeviceRevokeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Devices == nil {
			writeError(w, http.StatusServiceUnavailable, "approved device registry is not available")
			return
		}
		item, err := deps.Devices.Revoke(r.Context(), r.PathValue("id"))
		if err != nil {
			switch err {
			case devices.ErrNotFound:
				writeError(w, http.StatusNotFound, "approved device not found")
			case devices.ErrRegistryUnavailable:
				writeError(w, http.StatusServiceUnavailable, "approved device registry is not available")
			default:
				writeError(w, http.StatusInternalServerError, "approved device update failed")
			}
			return
		}
		// Immediately remove from approval cache so the revoke takes effect
		// without waiting for the TTL to expire.
		invalidateApprovalCache(item.DeviceID)
		// Kill the auth session that was issued at pairing time so the
		// revoked device's cached X-Auth-Token immediately stops working.
		// Without this the device status flips to revoked but the token
		// still validates because the auth layer checks the session row.
		if deps.Auth != nil && !deps.Auth.Disabled() && strings.TrimSpace(item.AuthSessionID) != "" {
			if err := deps.Auth.RevokeSessionID(r.Context(), item.AuthSessionID); err != nil {
				slog.Warn("device revoke could not invalidate session", "deviceId", item.DeviceID, "sessionId", item.AuthSessionID, "err", err)
			}
		}
		if deps.Auth != nil && !deps.Auth.Disabled() {
			if err := deps.Auth.RevokeDeviceSessions(r.Context(), item.DeviceID); err != nil {
				slog.Warn("device revoke could not invalidate device sessions", "deviceId", item.DeviceID, "err", err)
			}
		}
		publishOperationalEvent(deps, r, "device.revoked", map[string]any{
			"deviceId":      item.DeviceID,
			"displayName":   item.DisplayName,
			"clientProfile": item.ClientProfile,
		})
		writeJSON(w, http.StatusOK, approvedDevicePayload(item))
	}
}

func pairingCreateHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Pairing == nil {
			writeError(w, http.StatusServiceUnavailable, "pairing is not available")
			return
		}
		var request pairing.CreateRequest
		if !decodeJSON(w, r, &request) {
			return
		}
		clientProfile := firstNonEmpty(request.ClientProfile, "apple-tv")
		request.ClientProfile = clientProfile
		if deps.Devices != nil {
			if _, ok := deps.Devices.GetProfile(clientProfile); !ok {
				writeError(w, http.StatusBadRequest, "unknown client profile")
				return
			}
		}
		item, err := deps.Pairing.Create(request)
		if err != nil {
			slog.Warn("pairing request create failed", "clientProfile", request.ClientProfile, "deviceName", request.DeviceName, "err", err)
			writeError(w, http.StatusInternalServerError, "pairing request failed")
			return
		}
		publishOperationalEvent(deps, r, "pairing.request.created", map[string]any{
			"pairingId":     item.ID,
			"clientProfile": item.ClientProfile,
			"deviceName":    item.DeviceName,
			"expiresAt":     item.ExpiresAt,
		})
		writeJSON(w, http.StatusAccepted, item)
	}
}

func pairingStatusHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Pairing == nil {
			writeError(w, http.StatusServiceUnavailable, "pairing is not available")
			return
		}
		item, ok := deps.Pairing.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "pairing request not found")
			return
		}
		writeJSON(w, http.StatusOK, item)
	}
}

func pairingListHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Pairing == nil {
			writeError(w, http.StatusServiceUnavailable, "pairing is not available")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"requests": deps.Pairing.List()})
	}
}

// pairingCancelHandler lets an unpaired client withdraw its own pending
// pairing request when the user resets the flow. Authorization is by
// device identity (the deviceId from the original Create) — no admin
// session required because the client legitimately owns the row.
func pairingCancelHandler(deps Deps) http.HandlerFunc {
	type request struct {
		DeviceID string `json:"deviceId"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Pairing == nil {
			writeError(w, http.StatusServiceUnavailable, "pairing is not available")
			return
		}
		id := r.PathValue("id")
		// Accept deviceId from either JSON body or query parameter so AVPlayer-
		// style clients without a body can still call it.
		var payload request
		_ = decodeJSON(w, r, &payload)
		deviceID := strings.TrimSpace(firstNonEmpty(payload.DeviceID, r.URL.Query().Get("deviceId")))
		if deviceID == "" {
			writeError(w, http.StatusBadRequest, "deviceId required")
			return
		}
		if err := deps.Pairing.Cancel(id, deviceID); err != nil {
			switch err {
			case pairing.ErrNotFound:
				writeError(w, http.StatusNotFound, "pairing request not found")
			case pairing.ErrClosed:
				writeError(w, http.StatusConflict, "pairing request is already approved")
			default:
				writeError(w, http.StatusInternalServerError, "cancel failed")
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func pairingApproveHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		closePairingRequest(w, r, deps, true)
	}
}

func pairingDenyHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		closePairingRequest(w, r, deps, false)
	}
}

func closePairingRequest(w http.ResponseWriter, r *http.Request, deps Deps, approve bool) {
	if deps.Pairing == nil {
		writeError(w, http.StatusServiceUnavailable, "pairing is not available")
		return
	}
	actor := "local-admin"
	if resolved, ok := auth.ResolvedSessionFromContext(r.Context()); ok {
		actor = firstNonEmpty(resolved.Principal.Username, resolved.Principal.ID, actor)
	}
	var (
		item pairing.Request
		err  error
	)
	if approve {
		item, err = deps.Pairing.Approve(r.PathValue("id"), actor)
	} else {
		item, err = deps.Pairing.Deny(r.PathValue("id"), actor)
	}
	if err != nil {
		switch err {
		case pairing.ErrNotFound:
			writeError(w, http.StatusNotFound, "pairing request not found")
		case pairing.ErrClosed:
			writeError(w, http.StatusConflict, "pairing request is already closed")
		default:
			writeError(w, http.StatusInternalServerError, "pairing request failed")
		}
		return
	}
	if approve && deps.Devices != nil {
		if _, registerErr := deps.Devices.Approve(r.Context(), devices.ApproveInput{
			DeviceID:      item.DeviceID,
			DeviceName:    item.DeviceName,
			ClientProfile: item.ClientProfile,
			ApprovedBy:    actor,
		}); registerErr != nil {
			writeError(w, http.StatusInternalServerError, "approved device registry update failed")
			return
		}
		publishOperationalEvent(deps, r, "device.approved", map[string]any{
			"deviceId":      item.DeviceID,
			"deviceName":    item.DeviceName,
			"clientProfile": item.ClientProfile,
		})
	}
	if approve && deps.Auth != nil && !deps.Auth.Disabled() {
		_, session, token, sessionErr := deps.Auth.IssueDeviceSessionForUser(r.Context(), "local", item.DeviceID, requestRemoteAddr(r), item.DeviceName)
		if sessionErr != nil {
			writeError(w, http.StatusInternalServerError, "native device credential issue failed")
			return
		}
		credentialed, grantErr := deps.Pairing.AttachAuthGrant(item.ID, pairing.AuthGrant{
			Method:       "header_token",
			SessionToken: token,
			ExpiresAt:    session.ExpiresAt,
		})
		if grantErr != nil {
			writeError(w, http.StatusInternalServerError, "pairing credential update failed")
			return
		}
		// Link the freshly-issued session to the approved-device row so a
		// later Revoke can invalidate the token (not just flip the device
		// status, which leaves the cached X-Auth-Token still working).
		if deps.Devices != nil {
			_ = deps.Devices.AttachSession(r.Context(), item.DeviceID, session.ID)
		}
		item = credentialed
	}
	eventStatus := item.Status
	if !approve {
		eventStatus = "denied"
	}
	publishOperationalEvent(deps, r, "pairing.request."+eventStatus, map[string]any{
		"pairingId":     item.ID,
		"clientProfile": item.ClientProfile,
		"deviceName":    item.DeviceName,
		"deviceId":      item.DeviceID,
	})
	if !approve {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// ── QR pair tokens ────────────────────────────────────────────────────────────

const pairTokenTTL = 90 * time.Second

func pairingQRGenerateHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Database == nil {
			writeError(w, http.StatusServiceUnavailable, "database not available")
			return
		}
		token, err := randomPairToken()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "token generation failed")
			return
		}
		actor := "local-admin"
		if resolved, ok := auth.ResolvedSessionFromContext(r.Context()); ok {
			actor = firstNonEmpty(resolved.Principal.Username, resolved.Principal.ID, actor)
		}
		now := time.Now().UTC()
		expiresAt := now.Add(pairTokenTTL)
		_, err = deps.Database.DB().ExecContext(r.Context(), `
			INSERT INTO pair_tokens(token, created_by, expires_at, created_at) VALUES(?, ?, ?, ?)
		`, token, actor, expiresAt.Format(time.RFC3339), now.Format(time.RFC3339))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "token storage failed")
			return
		}
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		host := r.Host
		claimURL := scheme + "://" + host + "/api/pairing/qr/" + token + "/claim"
		writeJSON(w, http.StatusOK, map[string]any{
			"token":     token,
			"claimUrl":  claimURL,
			"imageUrl":  "/api/pairing/qr/" + token + "/image.png",
			"expiresAt": expiresAt.Format(time.RFC3339),
		})
	}
}

func pairingQRImageHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(r.PathValue("token"))
		if token == "" || deps.Database == nil {
			writeError(w, http.StatusBadRequest, "invalid token")
			return
		}
		// Check token still exists and is not expired (lightweight guard)
		var expiresAt string
		err := deps.Database.DB().QueryRowContext(r.Context(), `
			SELECT expires_at FROM pair_tokens WHERE token = ? AND claimed_at = ''
		`, token).Scan(&expiresAt)
		if err != nil {
			http.Error(w, "token not found or already claimed", http.StatusNotFound)
			return
		}
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		host := r.Host
		content := scheme + "://" + host + "/api/pairing/qr/" + token + "/claim"
		png, err := qrcode.Encode(content, qrcode.Medium, 256)
		if err != nil {
			http.Error(w, "qr generation failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(png)
	}
}

type pairingQRClaimRequest struct {
	DeviceName    string `json:"deviceName"`
	ClientProfile string `json:"clientProfile"`
	DeviceID      string `json:"deviceId"`
}

func pairingQRClaimHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Database == nil {
			writeError(w, http.StatusServiceUnavailable, "database not available")
			return
		}
		token := strings.TrimSpace(r.PathValue("token"))
		if token == "" {
			writeError(w, http.StatusBadRequest, "token is required")
			return
		}
		var req pairingQRClaimRequest
		decodeJSON(w, r, &req)
		deviceName := strings.TrimSpace(req.DeviceName)
		if deviceName == "" {
			deviceName = "Xuva Device"
		}
		clientProfile := strings.TrimSpace(req.ClientProfile)
		if clientProfile == "" {
			clientProfile = "apple-tv"
		}
		deviceID := strings.TrimSpace(req.DeviceID)
		if deviceID == "" {
			deviceID = "qr-" + uuid.NewString()
		}

		// Atomically claim the token (not expired, not yet claimed)
		now := time.Now().UTC()
		result, err := deps.Database.DB().ExecContext(r.Context(), `
			UPDATE pair_tokens
			SET claimed_by = ?, claimed_at = ?
			WHERE token = ? AND claimed_at = '' AND expires_at > ?
		`, deviceID, now.Format(time.RFC3339), token, now.Format(time.RFC3339))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "token claim failed")
			return
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			writeError(w, http.StatusGone, "pair token is expired or already used")
			return
		}

		// Register the device as approved
		if deps.Devices != nil {
			actor := "qr-pair"
			if _, regErr := deps.Devices.Approve(r.Context(), devices.ApproveInput{
				DeviceID:      deviceID,
				DeviceName:    deviceName,
				ClientProfile: clientProfile,
				ApprovedBy:    actor,
			}); regErr != nil {
				writeError(w, http.StatusInternalServerError, "device registration failed")
				return
			}
		}

		// Issue auth credentials
		if deps.Auth != nil && !deps.Auth.Disabled() {
			_, session, authToken, sessErr := deps.Auth.IssueDeviceSessionForUser(r.Context(), "local", deviceID, requestRemoteAddr(r), deviceName)
			if sessErr != nil {
				writeError(w, http.StatusInternalServerError, "credential issue failed")
				return
			}
			if deps.Devices != nil {
				_ = deps.Devices.AttachSession(r.Context(), deviceID, session.ID)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"deviceId":  deviceID,
				"authToken": authToken,
				"expiresAt": session.ExpiresAt.Format(time.RFC3339),
			})
			return
		}
		// Auth disabled — just confirm the claim
		writeJSON(w, http.StatusOK, map[string]any{"deviceId": deviceID})
	}
}

func randomPairToken() (string, error) {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	if _, err := cryptorand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 8)
	for i, v := range b {
		out[i] = chars[int(v)%len(chars)]
	}
	return string(out), nil
}

func approvedDevicePayload(item devices.ApprovedDevice) map[string]any {
	return map[string]any{
		"id":            item.ID,
		"deviceName":    item.DeviceName,
		"clientProfile": item.ClientProfile,
		"displayName":   item.DisplayName,
		"status":        item.Status,
		"approvedAt":    item.ApprovedAt,
		"approvedBy":    item.ApprovedBy,
		"createdAt":     item.CreatedAt,
		"updatedAt":     item.UpdatedAt,
	}
}

func sessionsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// SECURITY: standard users only see their own sessions; admins see
		// every session. Previously the full list (every user's session
		// IDs, MediaSourceIDs, UserIDs) was returned to any caller, which
		// was the enumeration substrate for cross-user attacks against
		// PATCH/DELETE /api/sessions/{id} and the inspector endpoint.
		all := deps.Sessions.List()
		if requestIsAdmin(r) {
			writeJSON(w, http.StatusOK, map[string]any{"sessions": all})
			return
		}
		callerID := sessionOwnerID(r)
		filtered := make([]sessions.Session, 0, len(all))
		for _, s := range all {
			if s.UserID == callerID {
				filtered = append(filtered, s)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": filtered})
	}
}

func sessionInspectorHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// SECURITY: load the session first so we can ownership-check before
		// returning its inspector payload (title, bitrate, device, route
		// history). Without this, a standard user can read any session by
		// ID — combined with the predictable session ID format, trivial.
		session, ok := deps.Sessions.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		if !requestIsAdmin(r) && session.UserID != sessionOwnerID(r) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		inspector, ok := deps.Sessions.Inspector(id)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeJSON(w, http.StatusOK, inspector)
	}
}

func sessionStartHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request sessions.StartRequest
		if !decodeJSON(w, r, &request) {
			return
		}
		// SECURITY: unconditionally bind UserID to the authenticated caller.
		// Same root cause as playbackStateSetHandler — accepting a
		// client-supplied UserID let standard users attribute sessions to
		// other users (corrupting playback history, watch state, transcode
		// accounting).
		request.UserID = requestUserID(r)
		if err := enrichSessionStartRequest(deps, r.Context(), &request); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		session, err := deps.Sessions.Start(request)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		publishOperationalEvent(deps, r, "api.session.accepted", map[string]any{
			"sessionId":     session.ID,
			"mediaSourceId": session.MediaSourceID,
			"deviceId":      session.DeviceID,
			"mode":          session.Mode,
		})
		writeJSON(w, http.StatusAccepted, session)
	}
}

func enrichSessionStartRequest(deps Deps, ctx context.Context, request *sessions.StartRequest) error {
	if request == nil || request.MediaSourceID == "" || deps.Catalog == nil {
		return nil
	}
	source, ok, err := deps.Catalog.GetMediaSource(ctx, request.MediaSourceID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if request.SourceName == "" {
		request.SourceName = source.Name
	}
	if request.Container == "" {
		request.Container = source.Container
	}
	if request.VideoCodec == "" {
		request.VideoCodec = source.VideoCodec
	}
	if request.Bitrate == 0 {
		request.Bitrate = source.Bitrate
	}
	if request.DurationSeconds == 0 {
		request.DurationSeconds = source.DurationSeconds
	}
	display, displayOK, err := deps.Catalog.GetMediaSourceDisplay(ctx, request.MediaSourceID)
	if err != nil {
		return err
	}
	if displayOK {
		if request.Title == "" {
			request.Title = display.Title
		}
		if request.QualityLabel == "" {
			request.QualityLabel = display.QualityLabel
		}
		if request.ArtworkURL == "" && display.ArtworkKind != "" && display.ArtworkID != "" {
			request.ArtworkURL = "/api/artwork/" + display.ArtworkKind + "/" + display.ArtworkID
		}
	}
	if request.Title == "" {
		request.Title = source.Name
	}
	if request.QualityLabel == "" {
		request.QualityLabel = sessionQualityLabel(source)
	}
	if request.ClientProfile == "" {
		request.ClientProfile = request.DeviceID
	}
	if request.ClientProfile == "" {
		request.ClientProfile = "web"
	}
	if request.Route == "" {
		request.Route = request.Mode
	}
	if request.ServerImpact == "" {
		request.ServerImpact = sessionServerImpact(request.Route, request.Mode)
	}
	// Populate reason code/text and per-component action fields from the
	// playback decision. These power the granular "Video · Audio · Container"
	// breakdown shown in NowPlayingCard and session detail views.
	if request.ReasonCode == "" || request.ReasonText == "" ||
		request.VideoAction == "" || request.AudioAction == "" || request.ContainerAction == "" {
		pbReq := playback.Request{
			MediaSourceID: request.MediaSourceID,
			ClientProfile: request.ClientProfile,
		}
		facts := playbackSourceFacts(ctx, deps, pbReq, source)
		decision := deps.Playback.DecideSource(ctx, pbReq, facts)
		if request.ReasonCode == "" {
			request.ReasonCode = decision.ReasonCode
		}
		if request.ReasonText == "" {
			request.ReasonText = decision.ReasonText
		}
		if request.VideoAction == "" {
			request.VideoAction = decision.VideoAction
		}
		if request.AudioAction == "" {
			request.AudioAction = decision.AudioAction
		}
		if request.ContainerAction == "" {
			request.ContainerAction = decision.ContainerAction
		}
		// Carry the source audio codec into the session so NowPlayingCard
		// can show "DTS → AAC" rather than just "Converting".
		if request.AudioCodec == "" {
			request.AudioCodec = facts.AudioCodec
		}
	}
	// Tag the session with the encoder being used so Now Playing can show
	// "NVENC" / "Intel QSV" / "CPU" alongside the Transcoding badge.
	if request.EncoderLabel == "" && request.Mode == "transcode" && deps.Transcode != nil {
		if job, ok := deps.Transcode.FindAnyActiveTranscode(request.MediaSourceID); ok {
			request.EncoderLabel = encoderDisplayLabel(job.VideoEncoder)
		}
		// If no active job yet (session start racing job creation), fall back to
		// the best encoder from the last hardware test cache.
		if request.EncoderLabel == "" {
			_, request.EncoderLabel = bestCachedEncoder(deps.Config.DataDir)
		}
	}
	return nil
}

func sessionQualityLabel(source catalog.MediaSourceItem) string {
	parts := []string{}
	if source.Width >= 3800 || source.Height >= 2000 {
		parts = append(parts, "4K")
	} else if source.Width >= 1900 || source.Height >= 1000 {
		parts = append(parts, "1080p")
	} else if source.Height > 0 {
		parts = append(parts, fmt.Sprintf("%dp", source.Height))
	}
	if source.VideoCodec != "" {
		parts = append(parts, strings.ToUpper(source.VideoCodec))
	}
	if source.Container != "" {
		parts = append(parts, strings.ToUpper(source.Container))
	}
	return strings.Join(parts, " ")
}

func sessionServerImpact(route string, mode string) string {
	value := strings.ToLower(route + " " + mode)
	switch {
	case strings.Contains(value, "transcode"):
		return "Converting while playing"
	case strings.Contains(value, "remux"):
		return "Repackaging while playing"
	case strings.Contains(value, "preparing"):
		return "Selecting playback route"
	case strings.Contains(value, "direct"):
		return "Low impact"
	default:
		return "Route pending"
	}
}

func sessionUpdateHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request sessions.UpdateRequest
		if !decodeJSON(w, r, &request) {
			return
		}
		// SECURITY: ownership-check before mutating someone else's session.
		// Without this, any standard user could PATCH any session by ID
		// (combined with predictable session IDs from sessions.nextSessionID,
		// trivial to script). Return 404 (not 403) on mismatch so we don't
		// leak existence of the target session.
		id := r.PathValue("id")
		existing, ok := deps.Sessions.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		if !requestIsAdmin(r) && existing.UserID != sessionOwnerID(r) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		session, ok := deps.Sessions.Update(id, request)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		_, _ = deps.PlayState.Set(r.Context(), session.MediaSourceID, playstate.Update{
			UserID:          session.UserID,
			ProgressSeconds: session.Progress,
			DurationSeconds: session.Duration,
		})
		writeJSON(w, http.StatusOK, session)
	}
}

func sessionStopHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// SECURITY: ownership-check before stopping someone else's playback.
		// See sessionUpdateHandler for the same rationale.
		existing, ok := deps.Sessions.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		if !requestIsAdmin(r) && existing.UserID != sessionOwnerID(r) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		session, ok := deps.Sessions.Stop(id)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		if deps.Transcode != nil && strings.TrimSpace(session.MediaSourceID) != "" {
			otherSessionActive := false
			for _, active := range deps.Sessions.List() {
				if active.MediaSourceID == session.MediaSourceID {
					otherSessionActive = true
					break
				}
			}
			if !otherSessionActive {
				deps.Transcode.CancelActiveForMediaSource(session.MediaSourceID)
			}
		}
		writeJSON(w, http.StatusOK, session)
	}
}

func playbackRecentHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := deps.PlayState.Recent(r.Context(), requestUserID(r), queryInt(r, "limit", 24))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "recent playback lookup failed")
			return
		}
		if deps.Catalog != nil {
			for i := range items {
				if display, ok, err := deps.Catalog.GetMediaSourceDisplay(r.Context(), items[i].MediaSourceID); err == nil && ok && display.Title != "" {
					items[i].Name = display.Title
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"recent": items})
	}
}

func playbackStateGetHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, _, err := deps.PlayState.Get(r.Context(), requestUserID(r), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "playback state lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, state)
	}
}

func playbackStateSetHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request playstate.Update
		if !decodeJSON(w, r, &request) {
			return
		}
		// SECURITY: unconditionally bind UserID to the authenticated caller —
		// never trust a client-supplied UserID. The previous
		// `if request.UserID == ""` fallback let any standard user write
		// playback state on behalf of another user (mark watched, set
		// progress) just by including a different userId in the body.
		request.UserID = requestUserID(r)
		state, err := deps.PlayState.Set(r.Context(), r.PathValue("id"), request)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, state)
	}
}

func requestUserID(r *http.Request) string {
	if resolved, ok := auth.ResolvedSessionFromContext(r.Context()); ok && strings.TrimSpace(resolved.Principal.ID) != "" {
		return resolved.Principal.ID
	}
	return r.URL.Query().Get("userId")
}

// requestIsAdmin reports whether the request's resolved session is an admin
// principal. Used to gate cross-user visibility on endpoints (Sessions list /
// inspector / update / stop) — admins see and act on every session; standard
// users only see and act on their own.
func requestIsAdmin(r *http.Request) bool {
	resolved, ok := auth.ResolvedSessionFromContext(r.Context())
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(resolved.Principal.Role), roleAdmin)
}

// sessionOwnerID returns the caller's session-owner identity for ownership
// checks. Matches sessions.Service.Start's default of "local" when no auth
// context is resolved so unauthenticated callers (XUVA_AUTH_DISABLED loopback
// installs, dev/tests) can still act on the sessions they themselves created.
func sessionOwnerID(r *http.Request) string {
	if id := requestUserID(r); id != "" {
		return id
	}
	return "local"
}

func movieScanHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request libraryScanRequest
		if !decodeJSON(w, r, &request) {
			return
		}
		path := firstNonEmpty(
			request.Path,
			request.MoviesPath,
			deps.Config.MovieLibraryPath,
			firstLibraryPathByKind(deps.Libraries, libraries.KindMovies),
		)
		if path == "" {
			writeError(w, http.StatusBadRequest, "Add a Movies library folder before scanning movies.")
			return
		}
		job, err := deps.Scans.Start(r.Context(), scans.Request{Kind: scans.KindMovies, Path: path, SampleLimit: request.SampleLimit})
		if err != nil {
			writeError(w, queueRejectionStatus(err, http.StatusBadRequest), err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	}
}

func tvScanHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request libraryScanRequest
		if !decodeJSON(w, r, &request) {
			return
		}
		path := firstNonEmpty(
			request.Path,
			request.TVPath,
			deps.Config.TVLibraryPath,
			firstLibraryPathByKind(deps.Libraries, libraries.KindTV),
		)
		if path == "" {
			writeError(w, http.StatusBadRequest, "Add a TV library folder before scanning TV.")
			return
		}
		job, err := deps.Scans.Start(r.Context(), scans.Request{Kind: scans.KindTV, Path: path, SampleLimit: request.SampleLimit})
		if err != nil {
			writeError(w, queueRejectionStatus(err, http.StatusBadRequest), err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	}
}

func allLibrariesScanHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request libraryScanRequest
		if !decodeJSON(w, r, &request) {
			return
		}

		moviesPath := firstNonEmpty(
			request.MoviesPath,
			deps.Config.MovieLibraryPath,
			firstLibraryPathByKind(deps.Libraries, libraries.KindMovies),
		)
		tvPath := firstNonEmpty(
			request.TVPath,
			deps.Config.TVLibraryPath,
			firstLibraryPathByKind(deps.Libraries, libraries.KindTV),
		)
		if moviesPath == "" && tvPath == "" {
			writeError(w, http.StatusBadRequest, "Add a Movies or TV library folder before starting a library scan.")
			return
		}

		job, err := deps.Scans.Start(r.Context(), scans.Request{Kind: scans.KindAll, MoviesPath: moviesPath, TVPath: tvPath, SampleLimit: request.SampleLimit})
		if err != nil {
			writeError(w, queueRejectionStatus(err, http.StatusBadRequest), err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	}
}

func scansHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"scans": deps.Scans.List()})
	}
}

func scanJobHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, ok := deps.Scans.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "scan job not found")
			return
		}
		writeJSON(w, http.StatusOK, job)
	}
}

func adaptiveSessionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source, ok, err := deps.Catalog.GetMediaSource(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		var payload struct {
			ClientProfile     string `json:"clientProfile"`
			RouteType         string `json:"routeType"`
			MaxNetworkBitrate int64  `json:"maxNetworkBitrate"`
		}
		if !decodeJSON(w, r, &payload) {
			return
		}
		plan := adaptivePlanForSource(deps, source, payload.ClientProfile, payload.RouteType, payload.MaxNetworkBitrate)
		writeJSON(w, http.StatusOK, map[string]any{
			"plan":        plan,
			"manifestUrl": "/api/media-sources/" + source.ID + "/adaptive/master.m3u8?clientProfile=" + url.QueryEscape(firstNonEmpty(payload.ClientProfile, "web")) + "&routeType=" + url.QueryEscape(payload.RouteType) + "&maxNetworkBitrate=" + fmt.Sprintf("%d", payload.MaxNetworkBitrate),
		})
	}
}

func adaptiveMasterHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source, ok, err := deps.Catalog.GetMediaSource(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		plan := adaptivePlanForSource(deps, source, r.URL.Query().Get("clientProfile"), r.URL.Query().Get("routeType"), queryInt64(r, "maxNetworkBitrate", 0))
		if !plan.Enabled {
			writeError(w, http.StatusPreconditionFailed, plan.Reason)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(adaptive.MasterPlaylist(plan)))
	}
}

func adaptiveVariantHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source, ok, err := deps.Catalog.GetMediaSource(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		variantID, validVariant := adaptiveVariantID(r.PathValue("variant"))
		if !validVariant {
			writeError(w, http.StatusNotFound, "adaptive variant not found")
			return
		}
		plan := adaptivePlanForSource(deps, source, r.URL.Query().Get("clientProfile"), r.URL.Query().Get("routeType"), queryInt64(r, "maxNetworkBitrate", 0))
		playlist, ok := adaptive.MediaPlaylist(plan, variantID)
		if !plan.Enabled || !ok {
			writeError(w, http.StatusPreconditionFailed, "adaptive variant is not available")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(playlist))
	}
}

func adaptiveTelemetryHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var payload adaptive.Telemetry
		if !decodeJSON(w, r, &payload) {
			return
		}
		event := adaptive.NormalizeTelemetry(payload, observability.CorrelationID(r.Context()))
		if deps.Events != nil {
			deps.Events.Publish(event.Event, map[string]any{
				"sessionId":         event.SessionID,
				"mediaSourceId":     event.MediaSourceID,
				"clientProfile":     event.ClientProfile,
				"variantId":         event.VariantID,
				"previousVariantId": event.PreviousVariant,
				"bufferSeconds":     event.BufferSeconds,
				"stallMs":           event.StallMS,
				"observedBitrate":   event.ObservedBitrate,
				"correlationId":     event.CorrelationID,
				"createdAt":         event.CreatedAt,
			})
		}
		writeJSON(w, http.StatusAccepted, event)
	}
}

func playbackDecisionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request := playback.Request{
			MediaSourceID:       r.URL.Query().Get("mediaSourceId"),
			ClientProfile:       r.URL.Query().Get("clientProfile"),
			Policy:              r.URL.Query().Get("policy"),
			RouteType:           r.URL.Query().Get("routeType"),
			MaxNetworkBitrate:   queryInt64(r, "maxNetworkBitrate", 0),
			AudioTrackIndex:     queryInt(r, "audioTrackIndex", 0),
			AudioCodec:          r.URL.Query().Get("audioCodec"),
			AudioChannels:       queryInt(r, "audioChannels", 0),
			SubtitleTrackIndex:  queryInt(r, "subtitleTrackIndex", 0),
			SubtitleCodec:       r.URL.Query().Get("subtitleCodec"),
			SubtitleMode:        r.URL.Query().Get("subtitleMode"),
			SubtitleTrackActive: r.URL.Query().Get("subtitleTrackActive") == "true",
			SupportsAdaptive:    r.URL.Query().Get("supportsAdaptive") == "true",
		}
		request = applyClientProfile(deps, request)
		decision := deps.Playback.Decide(r.Context(), request)
		if request.MediaSourceID != "" {
			source, ok, err := deps.Catalog.GetMediaSource(r.Context(), request.MediaSourceID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "media source lookup failed")
				return
			}
			if !ok {
				writeError(w, http.StatusNotFound, "media source not found")
				return
			}
			tracks, _, _ := deps.Catalog.GetMediaSourceTracks(r.Context(), request.MediaSourceID)
			if request.AudioCodec == "" && len(tracks.AudioTracks) > 0 {
				audio := trackByIndex(tracks.AudioTracks, request.AudioTrackIndex)
				request.AudioTrackIndex = audio.Index
				request.AudioCodec = audio.Codec
				request.AudioChannels = audio.Channels
			}
			if request.SubtitleTrackActive && request.SubtitleCodec == "" && len(tracks.SubtitleTracks) > 0 {
				subtitle := trackByIndex(tracks.SubtitleTracks, request.SubtitleTrackIndex)
				request.SubtitleTrackIndex = subtitle.Index
				request.SubtitleCodec = subtitle.Codec
			}
			decision = deps.Playback.DecideSource(r.Context(), request, playback.SourceFacts{
				MediaSourceID:    source.ID,
				Container:        source.Container,
				VideoCodec:       source.VideoCodec,
				Width:            source.Width,
				Height:           source.Height,
				AudioStreams:     source.AudioStreams,
				SubtitleStreams:  source.SubtitleStreams,
				SidecarSubtitles: len(subtitles.DiscoverSidecars(source.Path)),
				Bitrate:          source.Bitrate,
				Probed:           source.Probed,
				AudioCodec:       request.AudioCodec,
				AudioChannels:    request.AudioChannels,
				SubtitleCodec:    request.SubtitleCodec,
				SubtitleActive:   request.SubtitleTrackActive,
			})
		}
		writeJSON(w, http.StatusOK, decision)
	}
}

func playbackRouteHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mediaSourceID := r.URL.Query().Get("mediaSourceId")
		if mediaSourceID == "" {
			writeError(w, http.StatusBadRequest, "media source id is required")
			return
		}
		item, ok, err := deps.Catalog.GetMediaSource(r.Context(), mediaSourceID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "media source lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "media source not found")
			return
		}
		if err := ensureMediaFileAccessible(item.Path); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"route":  "blocked",
				"status": "source_unavailable",
				"decision": map[string]any{
					"mode":       "Blocked",
					"reasonCode": "source_unavailable",
					"reasonText": "Media file is unavailable from the configured library path.",
				},
			})
			return
		}
		decision := playbackDecisionForSource(r.Context(), deps, r, item)
		slog.Debug("playback route decision",
			"mediaSourceId", mediaSourceID,
			"clientProfile", r.URL.Query().Get("clientProfile"),
			"mode", string(decision.Mode),
			"reasonCode", decision.ReasonCode,
			"videoAction", decision.VideoAction,
			"audioAction", decision.AudioAction,
			"containerAction", decision.ContainerAction,
			"estimatedCpuCost", decision.EstimatedCPUCost,
			"correlationId", observability.CorrelationID(r.Context()),
		)
		if decision.Mode == playback.DecisionDeferred {
			writeJSON(w, http.StatusOK, map[string]any{
				"route":    "deferred",
				"status":   "deferred",
				"decision": decision,
			})
			return
		}
		if decision.Mode == playback.DirectPlay {
			writeJSON(w, http.StatusOK, map[string]any{
				"route":    "direct",
				"status":   "ready",
				"url":      "/api/media-sources/" + mediaSourceID + "/stream",
				"decision": decision,
			})
			return
		}
		forcePlayable := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("forcePlayable")), "true")
		if !forcePlayable && !playbackPolicyAllows(deps.Config.PlaybackPolicy, decision) {
			writeJSON(w, http.StatusOK, map[string]any{
				"route":           "blocked",
				"status":          "blocked_by_policy",
				"policy":          playbackPolicyStatus(deps.Config.PlaybackPolicy),
				"decision":        decision,
				"fallbackOptions": playbackPolicyFallbacks(deps.Config.PlaybackPolicy, decision),
			})
			return
		}
		if decision.Mode == playback.AdaptiveStream {
			plan := adaptivePlanForSource(deps, item, r.URL.Query().Get("clientProfile"), firstNonEmpty(r.URL.Query().Get("routeType"), "remote"), queryInt64(r, "maxNetworkBitrate", 0))
			if plan.Enabled {
				writeJSON(w, http.StatusOK, map[string]any{
					"route":       "adaptive",
					"status":      "ready",
					"protocol":    plan.Protocol,
					"manifestUrl": "/api/media-sources/" + mediaSourceID + "/adaptive/master.m3u8?clientProfile=" + url.QueryEscape(firstNonEmpty(r.URL.Query().Get("clientProfile"), "web")) + "&routeType=" + url.QueryEscape(firstNonEmpty(r.URL.Query().Get("routeType"), "remote")) + "&maxNetworkBitrate=" + fmt.Sprintf("%d", queryInt64(r, "maxNetworkBitrate", 0)),
					"plan":        plan,
					"decision":    decision,
				})
				return
			}
		}
		// Remux (container-only repack) and AudioTranscode (video copy + audio
		// re-encode) both use the live-pipe remux endpoint -- no pre-processing
		// required, playback starts within a second matching Plex/Jellyfin/Emby
		// direct-stream behaviour.
		if decision.Mode == playback.Remux || decision.Mode == playback.AudioTranscode {
			audioMode := "copy"
			if decision.Mode == playback.AudioTranscode {
				audioMode = "transcode"
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"route":    "remux",
				"status":   "ready",
				"url":      "/api/media-sources/" + mediaSourceID + "/remux-stream?audioMode=" + audioMode,
				"decision": decision,
			})
			return
		}
		// Map the playback decision mode to the transcode pipeline for full
		// transcodes (video re-encoding needed).
		mode := transcode.ModeTranscode
		audioTrackIndex := resolvedAudioTrackIndex(r.Context(), deps, mediaSourceID, queryInt(r, "audioTrackIndex", 0))
		if job, ok := deps.Transcode.FindCompleted(mediaSourceID, mode, audioTrackIndex); ok {
			if job.OutputFormat == "hls" {
				writeJSON(w, http.StatusOK, map[string]any{
					"route":       string(mode),
					"status":      "ready",
					"protocol":    "hls",
					"manifestUrl": "/api/work/" + job.ID + "/hls/index.m3u8",
					"job":         job,
					"decision":    decision,
				})
			} else {
				writeJSON(w, http.StatusOK, map[string]any{
					"route":    string(mode),
					"status":   "ready",
					"url":      "/api/work/" + job.ID + "/file",
					"job":      job,
					"decision": decision,
				})
			}
			return
		}
		if job, ok := deps.Transcode.FindActive(mediaSourceID, mode, audioTrackIndex); ok {
			if job.Status == transcode.StatusStreaming {
				writeJSON(w, http.StatusOK, map[string]any{
					"route":       string(mode),
					"status":      "ready",
					"protocol":    "hls",
					"manifestUrl": "/api/work/" + job.ID + "/hls/index.m3u8",
					"job":         job,
					"decision":    decision,
				})
			} else {
				writeJSON(w, http.StatusAccepted, map[string]any{
					"route":    string(mode),
					"status":   string(job.Status),
					"job":      job,
					"decision": decision,
				})
			}
			return
		}
		request := transcode.Request{MediaSourceID: mediaSourceID, Mode: mode, SourcePath: item.Path, AudioTrackIndex: audioTrackIndex}
		if mode == transcode.ModeTranscode {
			request.OutputFormat = "hls"
			if encoder, ok := selectedHardwareEncoder(r.Context(), deps.Config); ok {
				request.Acceleration = "hardware"
				request.VideoEncoder = encoder
			}
		}
		job, err := deps.Transcode.Start(r.Context(), request)
		if err != nil {
			writeError(w, queueRejectionStatus(err, http.StatusBadRequest), err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"route":    string(mode),
			"status":   string(job.Status),
			"job":      job,
			"decision": decision,
		})
	}
}

func playbackDecisionForSource(ctx context.Context, deps Deps, r *http.Request, source catalog.MediaSourceItem) playback.Decision {
	request := playback.Request{
		MediaSourceID:       source.ID,
		ClientProfile:       firstNonEmpty(r.URL.Query().Get("clientProfile"), "web"),
		Policy:              r.URL.Query().Get("policy"),
		RouteType:           r.URL.Query().Get("routeType"),
		MaxNetworkBitrate:   queryInt64(r, "maxNetworkBitrate", 0),
		AudioTrackIndex:     queryInt(r, "audioTrackIndex", 0),
		AudioCodec:          r.URL.Query().Get("audioCodec"),
		AudioChannels:       queryInt(r, "audioChannels", 0),
		SubtitleTrackIndex:  queryInt(r, "subtitleTrackIndex", 0),
		SubtitleCodec:       r.URL.Query().Get("subtitleCodec"),
		SubtitleMode:        r.URL.Query().Get("subtitleMode"),
		SubtitleTrackActive: r.URL.Query().Get("subtitleTrackActive") == "true",
		SupportsAdaptive:    r.URL.Query().Get("supportsAdaptive") == "true",
	}
	request = applyClientProfile(deps, request)
	// Apply any client-reported capability flags passed as query params (#64).
	// The route endpoint is GET so we use sparse flags rather than a JSON body.
	var caps *clientCapabilities
	if r.URL.Query().Has("supportsHdr") || r.URL.Query().Has("maxBitDepth") ||
		r.URL.Query().Has("videoCodecs") || r.URL.Query().Has("audioCodecs") {
		caps = &clientCapabilities{
			SupportsHDR:      r.URL.Query().Get("supportsHdr") == "true",
			MaxVideoBitDepth: queryInt(r, "maxBitDepth", 0),
		}
		if vc := r.URL.Query().Get("videoCodecs"); vc != "" {
			for _, c := range strings.Split(vc, ",") {
				if s := strings.TrimSpace(c); s != "" {
					caps.VideoCodecs = append(caps.VideoCodecs, s)
				}
			}
		}
		if ac := r.URL.Query().Get("audioCodecs"); ac != "" {
			for _, c := range strings.Split(ac, ",") {
				if s := strings.TrimSpace(c); s != "" {
					caps.AudioCodecs = append(caps.AudioCodecs, s)
				}
			}
		}
	}
	request = applyClientCapabilities(request, caps)
	return deps.Playback.DecideSource(ctx, request, playbackSourceFacts(ctx, deps, request, source))
}

func adaptivePlanForSource(deps Deps, source catalog.MediaSourceItem, clientProfile string, routeType string, maxNetworkBitrate int64) adaptive.Plan {
	profileID := firstNonEmpty(clientProfile, "web")
	profile, ok := deps.Devices.GetProfile(profileID)
	supportsHLS := ok && profile.SupportsHLS
	return adaptive.BuildPlan(adaptive.Request{
		MediaSourceID:     source.ID,
		ClientProfile:     profileID,
		RouteType:         routeType,
		SourceBitrate:     source.Bitrate,
		MaxNetworkBitrate: maxNetworkBitrate,
		Width:             source.Width,
		Height:            source.Height,
		SupportsHLS:       supportsHLS,
	})
}

func adaptiveVariantID(segment string) (string, bool) {
	if !strings.HasPrefix(segment, "variant-") || !strings.HasSuffix(segment, ".m3u8") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(segment, "variant-"), ".m3u8")
	if id == "" || strings.ContainsAny(id, `/\`) {
		return "", false
	}
	return id, true
}

func applyClientProfile(deps Deps, request playback.Request) playback.Request {
	profile, ok := deps.Devices.GetProfile(request.ClientProfile)
	if !ok {
		return request
	}
	request.ClientProfile = profile.ID
	request.Containers = profile.Containers
	request.VideoCodecs = profile.VideoCodecs
	request.AudioCodecs = profile.AudioCodecs
	request.SubtitleCodecs = profile.SubtitleCodecs
	request.MaxVideoBitDepth = profile.MaxVideoBitDepth
	request.MaxFrameRate = profile.MaxVideoFrameRate
	request.SupportsHDR = profile.SupportsHDR
	request.SupportsAdaptive = request.SupportsAdaptive || profile.SupportsHLS
	return request
}

func playbackSourceFacts(ctx context.Context, deps Deps, request playback.Request, source catalog.MediaSourceItem) playback.SourceFacts {
	tracks, _, _ := deps.Catalog.GetMediaSourceTracks(ctx, request.MediaSourceID)
	if request.AudioCodec == "" && len(tracks.AudioTracks) > 0 {
		audio := trackByIndex(tracks.AudioTracks, request.AudioTrackIndex)
		request.AudioTrackIndex = audio.Index
		request.AudioCodec = audio.Codec
		request.AudioChannels = audio.Channels
	}
	if request.SubtitleTrackActive && request.SubtitleCodec == "" && len(tracks.SubtitleTracks) > 0 {
		subtitle := trackByIndex(tracks.SubtitleTracks, request.SubtitleTrackIndex)
		request.SubtitleTrackIndex = subtitle.Index
		request.SubtitleCodec = subtitle.Codec
	}
	return playback.SourceFacts{
		MediaSourceID:    source.ID,
		Container:        source.Container,
		VideoCodec:       source.VideoCodec,
		VideoProfile:     source.VideoProfile,
		VideoLevel:       source.VideoLevel,
		VideoBitDepth:    source.VideoBitDepth,
		HDR:              source.HDRFormat,
		DoviProfile:      source.DoviProfile,
		FrameRate:        source.VideoFrameRate,
		Width:            source.Width,
		Height:           source.Height,
		AudioStreams:     source.AudioStreams,
		SubtitleStreams:  source.SubtitleStreams,
		SidecarSubtitles: len(subtitles.DiscoverSidecars(source.Path)),
		Bitrate:          source.Bitrate,
		Probed:           source.Probed,
		AudioCodec:       request.AudioCodec,
		AudioChannels:    request.AudioChannels,
		SubtitleCodec:    request.SubtitleCodec,
		SubtitleActive:   request.SubtitleTrackActive,
	}
}

func trackByIndex(tracks []probe.Track, index int) probe.Track {
	for _, track := range tracks {
		if track.Index == index {
			return track
		}
	}
	for _, track := range tracks {
		if track.Default {
			return track
		}
	}
	return tracks[0]
}

func resolvedAudioTrackIndex(ctx context.Context, deps Deps, mediaSourceID string, requestedIndex int) int {
	tracks, ok, err := deps.Catalog.GetMediaSourceTracks(ctx, mediaSourceID)
	if err != nil || !ok || len(tracks.AudioTracks) == 0 {
		return 0
	}
	return trackByIndex(tracks.AudioTracks, requestedIndex).Index
}

type libraryScanRequest struct {
	Path        string `json:"path"`
	MoviesPath  string `json:"moviesPath"`
	TVPath      string `json:"tvPath"`
	SampleLimit int    `json:"sampleLimit"`
}

type movieScanResponse struct {
	Kind        scanner.LibraryKind    `json:"kind"`
	Path        string                 `json:"path"`
	Summary     scanner.Summary        `json:"summary"`
	Persisted   catalog.PersistSummary `json:"persisted"`
	MoviesFound int                    `json:"moviesFound"`
	Movies      []movies.Candidate     `json:"movies"`
}

type tvScanResponse struct {
	Kind          scanner.LibraryKind    `json:"kind"`
	Path          string                 `json:"path"`
	Summary       scanner.Summary        `json:"summary"`
	Persisted     catalog.PersistSummary `json:"persisted"`
	EpisodesFound int                    `json:"episodesFound"`
	Episodes      []tv.EpisodeCandidate  `json:"episodes"`
}

type combinedScanResponse struct {
	Movies movieScanResponse `json:"movies,omitempty"`
	TV     tvScanResponse    `json:"tv,omitempty"`
}

func scanMovies(w http.ResponseWriter, r *http.Request, deps Deps, path string, sampleLimit int) (movieScanResponse, bool) {
	deps.Events.Publish("scan.started", map[string]string{"kind": string(scanner.KindMovies), "path": path})
	result, err := deps.Scanner.Scan(r.Context(), scanner.Request{Kind: scanner.KindMovies, Root: path})
	if err != nil {
		deps.Events.Publish("scan.failed", map[string]string{"kind": string(scanner.KindMovies), "path": path, "error": err.Error()})
		writeScanError(w, err)
		return movieScanResponse{}, false
	}
	candidates := deps.Movies.Classify(result.Files)
	library := libraries.Library{
		ID:   "movies",
		Name: "Movies",
		Path: result.Root,
		Kind: libraries.KindMovies,
	}
	deps.Libraries.Set(library)
	persisted, err := deps.Catalog.SaveMovieScan(r.Context(), library, result, candidates)
	if err != nil {
		deps.Events.Publish("scan.failed", map[string]string{"kind": string(scanner.KindMovies), "path": result.Root, "error": err.Error()})
		writeError(w, http.StatusInternalServerError, "movie catalog update failed")
		return movieScanResponse{}, false
	}
	response := movieScanResponse{
		Kind:        scanner.KindMovies,
		Path:        result.Root,
		Summary:     result.Summary,
		Persisted:   persisted,
		MoviesFound: len(candidates),
		Movies:      limitMovies(candidates, sampleLimit),
	}
	deps.Events.Publish("scan.completed", map[string]any{"kind": string(scanner.KindMovies), "path": result.Root, "mediaFiles": result.MediaFiles})
	return response, true
}

func scanTV(w http.ResponseWriter, r *http.Request, deps Deps, path string, sampleLimit int) (tvScanResponse, bool) {
	deps.Events.Publish("scan.started", map[string]string{"kind": string(scanner.KindTV), "path": path})
	result, err := deps.Scanner.Scan(r.Context(), scanner.Request{Kind: scanner.KindTV, Root: path})
	if err != nil {
		deps.Events.Publish("scan.failed", map[string]string{"kind": string(scanner.KindTV), "path": path, "error": err.Error()})
		writeScanError(w, err)
		return tvScanResponse{}, false
	}
	candidates := deps.TV.Classify(result.Files)
	library := libraries.Library{
		ID:   "tv",
		Name: "TV",
		Path: result.Root,
		Kind: libraries.KindTV,
	}
	deps.Libraries.Set(library)
	persisted, err := deps.Catalog.SaveTVScan(r.Context(), library, result, candidates)
	if err != nil {
		deps.Events.Publish("scan.failed", map[string]string{"kind": string(scanner.KindTV), "path": result.Root, "error": err.Error()})
		writeError(w, http.StatusInternalServerError, "tv catalog update failed")
		return tvScanResponse{}, false
	}
	response := tvScanResponse{
		Kind:          scanner.KindTV,
		Path:          result.Root,
		Summary:       result.Summary,
		Persisted:     persisted,
		EpisodesFound: len(candidates),
		Episodes:      limitEpisodes(candidates, sampleLimit),
	}
	deps.Events.Publish("scan.completed", map[string]any{"kind": string(scanner.KindTV), "path": result.Root, "mediaFiles": result.MediaFiles})
	return response, true
}

// maxJSONBodyBytes caps the size of a JSON control-plane request body. These
// payloads are small (settings, IDs, track selections); 1 MiB is generous while
// preventing an unbounded body from exhausting memory. Bulk endpoints that
// legitimately accept large bodies (e.g. backup upload) set their own, larger
// MaxBytesReader before calling their decoder and do not use decodeJSON.
const maxJSONBodyBytes = 1 << 20 // 1 MiB

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(destination); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid json body")
		return false
	}
	return true
}

func writeScanError(w http.ResponseWriter, err error) {
	if errors.Is(err, scanner.ErrMissingRoot) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func firstLibraryPathByKind(service *libraries.Service, kind libraries.Kind) string {
	if service == nil {
		return ""
	}
	for _, library := range service.List() {
		if library.Kind == kind {
			return strings.TrimSpace(library.Path)
		}
	}
	return ""
}

func lanAddresses(httpAddr string) []string {
	_, port, err := net.SplitHostPort(httpAddr)
	if err != nil || port == "" {
		port = "8097"
	}
	output := []string{}
	interfaces, err := net.Interfaces()
	if err != nil {
		return output
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ipv4 := ip.To4(); ipv4 != nil {
				output = append(output, "http://"+ipv4.String()+":"+port)
			}
		}
	}
	return output
}

func requestBaseURL(r *http.Request, fallbackAddr string) string {
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		host = fallbackAddr
	}
	scheme := "http"
	if requestSecure(r) {
		scheme = "https"
	}
	return scheme + "://" + host
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func queryInt(r *http.Request, key string, fallback int) int {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback
	}
	var output int
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return fallback
		}
		output = output*10 + int(ch-'0')
	}
	if output == 0 {
		return fallback
	}
	return output
}

func queryInt64(r *http.Request, key string, fallback int64) int64 {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback
	}
	var output int64
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return fallback
		}
		output = output*10 + int64(ch-'0')
	}
	if output == 0 {
		return fallback
	}
	return output
}

func queryFloat(r *http.Request, key string, fallback float64) float64 {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback
	}
	output, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return output
}

func parsePathInt(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	output := 0
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return fallback
		}
		output = output*10 + int(ch-'0')
	}
	return output
}

func limitMovies(candidates []movies.Candidate, limit int) []movies.Candidate {
	if limit <= 0 {
		limit = 100
	}
	if len(candidates) <= limit {
		return candidates
	}
	return candidates[:limit]
}

func limitEpisodes(candidates []tv.EpisodeCandidate, limit int) []tv.EpisodeCandidate {
	if limit <= 0 {
		limit = 100
	}
	if len(candidates) <= limit {
		return candidates
	}
	return candidates[:limit]
}

func eventsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch, cancel := deps.Events.Subscribe(r.Context())
		defer cancel()

		fmt.Fprintf(w, "event: ready\ndata: {\"status\":\"connected\"}\n\n")
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case event, ok := <-ch:
				if !ok {
					return
				}
				payload, err := json.Marshal(event)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", payload)
				flusher.Flush()
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// setupStatusHandler returns whether the first-run wizard should be shown.
// It is intentionally public (no auth required) so the frontend can gate
// navigation before the user has created an account.
func setupStatusHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := currentConfig(deps)
		requiresBootstrap := false
		if deps.Auth != nil && !deps.Auth.Disabled() {
			if required, err := deps.Auth.RequiresBootstrap(r.Context()); err == nil {
				requiresBootstrap = required
			}
		}
		hasLibraries := len(deps.Libraries.List()) > 0
		// Show the wizard when there is no admin account yet, or when the server
		// has never been explicitly finished (no libraries means nothing useful
		// can be streamed yet, so nudge the user through setup).
		requiresSetup := requiresBootstrap || (!cfg.SetupComplete && !hasLibraries)
		writeJSON(w, http.StatusOK, map[string]any{
			"requiresSetup": requiresSetup,
			"steps": map[string]bool{
				"account":   !requiresBootstrap,
				"region":    cfg.Country != "",
				"libraries": hasLibraries,
			},
		})
	}
}

// setupCompleteHandler saves the region/timezone chosen in the setup wizard
// and marks setup as complete. Requires an authenticated session so it runs
// after the account-creation step.
func setupCompleteHandler(deps Deps) http.HandlerFunc {
	type request struct {
		Country  string `json:"country"`
		Timezone string `json:"timezone"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		var req request
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid json body")
				return
			}
		}
		cfg := currentConfig(deps)
		if v := strings.ToUpper(strings.TrimSpace(req.Country)); v != "" {
			cfg.Country = v
		}
		if v := strings.TrimSpace(req.Timezone); v != "" {
			cfg.Timezone = v
		}
		cfg.SetupComplete = true
		if err := config.SaveFile(deps.Config.DataDir, cfg); err != nil {
			writeError(w, http.StatusInternalServerError, "setup save failed")
			return
		}
		_ = deps.Catalog.SaveSettings(r.Context(), catalog.RuntimeSettings{
			HTTPAddr:          cfg.HTTPAddr,
			DataDir:           cfg.DataDir,
			TranscodeDir:      cfg.TranscodeDir,
			DownloadsDir:      cfg.DownloadsDir,
			MetadataDir:       cfg.MetadataDir,
			CacheDir:          cfg.CacheDir,
			TempDir:           cfg.TempDir,
			FFmpegPath:        cfg.FFmpegPath,
			FFprobePath:       cfg.FFprobePath,
			ScanWorkers:       cfg.ScanWorkers,
			ProbeWorkers:      cfg.ProbeWorkers,
			TranscodeWorkers:  cfg.TranscodeWorkers,
			GPUWorkers:        cfg.GPUWorkers,
			LibrarySyncMode:   cfg.LibrarySyncMode,
			SyncIntervalMins:  cfg.SyncIntervalMins,
			WatchDebounceSecs: cfg.WatchDebounceSecs,
			ProbeBatchLimit:   cfg.ProbeBatchLimit,
			Country:           cfg.Country,
			Timezone:          cfg.Timezone,
			SetupComplete:     cfg.SetupComplete,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
