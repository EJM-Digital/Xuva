package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/auth"
	"github.com/EJM-Digital/Xuva/server/internal/buildinfo"
	"github.com/EJM-Digital/Xuva/server/internal/catalog"
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
	"github.com/EJM-Digital/Xuva/server/internal/migration"
	"github.com/EJM-Digital/Xuva/server/internal/movies"
	"github.com/EJM-Digital/Xuva/server/internal/observability"
	"github.com/EJM-Digital/Xuva/server/internal/pairing"
	"github.com/EJM-Digital/Xuva/server/internal/playback"
	"github.com/EJM-Digital/Xuva/server/internal/playstate"
	"github.com/EJM-Digital/Xuva/server/internal/probe"
	"github.com/EJM-Digital/Xuva/server/internal/probes"
	"github.com/EJM-Digital/Xuva/server/internal/resources"
	"github.com/EJM-Digital/Xuva/server/internal/scanner"
	"github.com/EJM-Digital/Xuva/server/internal/scans"
	"github.com/EJM-Digital/Xuva/server/internal/sessions"
	"github.com/EJM-Digital/Xuva/server/internal/streaming"
	"github.com/EJM-Digital/Xuva/server/internal/subtitles"
	"github.com/EJM-Digital/Xuva/server/internal/transcode"
	"github.com/EJM-Digital/Xuva/server/internal/tv"
)

func TestHealthUsesStableStartedAt(t *testing.T) {
	startedAt := time.Date(2026, 4, 24, 3, 4, 5, 0, time.UTC)
	router := NewRouter(testDeps(t, startedAt))

	first := getJSON(t, router, "/api/health")
	second := getJSON(t, router, "/api/health")

	if first["startedAt"] != startedAt.Format(time.RFC3339) {
		t.Fatalf("expected startedAt %q, got %q", startedAt.Format(time.RFC3339), first["startedAt"])
	}
	if first["startedAt"] != second["startedAt"] {
		t.Fatalf("expected stable startedAt, got %q then %q", first["startedAt"], second["startedAt"])
	}
}

func TestSystemVersionIsPublicAndIncludesSchemaVersion(t *testing.T) {
	startedAt := time.Date(2026, 5, 26, 1, 2, 3, 0, time.UTC)
	router := NewRouter(testDeps(t, startedAt))

	payload := getJSON(t, router, "/api/system/version")

	if payload["product"] != "xuva" || payload["version"] != "dev" {
		t.Fatalf("expected product/version identity, got %#v", payload)
	}
	if sv, _ := payload["schemaVersion"].(string); sv == "" || !strings.HasPrefix(sv, "0") {
		t.Fatalf("expected non-empty schema version, got %#v", payload)
	}
	if payload["startedAt"] != startedAt.Format(time.RFC3339) {
		t.Fatalf("expected stable startedAt, got %#v", payload)
	}
	if _, ok := payload["goVersion"].(string); !ok {
		t.Fatalf("expected go version, got %#v", payload)
	}
}

func TestSystemUpdatesCheckComparesLatestRelease(t *testing.T) {
	previousVersion := buildinfo.Version
	buildinfo.Version = "v0.0.4"
	t.Cleanup(func() { buildinfo.Version = previousVersion })

	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "Xuva/v0.0.4" {
			t.Fatalf("expected Xuva user agent, got %q", r.Header.Get("User-Agent"))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"tag_name":     "v0.0.5",
			"html_url":     "https://github.com/EJM-Digital/Xuva/releases/tag/v0.0.5",
			"published_at": "2026-05-27T04:00:00Z",
			"assets": []map[string]any{
				{
					"name":                 "xuva-v0.0.5-win-x64.exe",
					"browser_download_url": "https://github.com/EJM-Digital/Xuva/releases/download/v0.0.5/xuva-v0.0.5-win-x64.exe",
					"size":                 123,
					"digest":               "sha256:test",
				},
			},
		})
	}))
	defer release.Close()
	t.Setenv("XUVA_UPDATE_RELEASE_API", release.URL)

	router := NewRouter(testDeps(t, time.Now()))
	payload := getJSON(t, router, "/api/system/updates")

	if payload["currentVersion"] != "v0.0.4" || payload["latestVersion"] != "v0.0.5" {
		t.Fatalf("expected current/latest versions, got %#v", payload)
	}
	if payload["updateAvailable"] != true {
		t.Fatalf("expected updateAvailable, got %#v", payload)
	}
	if payload["applySupported"] != false {
		t.Fatalf("expected automatic apply to be unsupported until updater supervisor exists, got %#v", payload)
	}
	assets, ok := payload["assets"].([]any)
	if !ok || len(assets) != 1 {
		t.Fatalf("expected one update asset, got %#v", payload["assets"])
	}
	asset := assets[0].(map[string]any)
	if asset["packageType"] != "windows-installer" {
		t.Fatalf("expected installer asset classification, got %#v", asset)
	}
}

func TestSystemUpdatesApplyStagesVerifiedInstaller(t *testing.T) {
	previousVersion := buildinfo.Version
	buildinfo.Version = "v0.0.4"
	t.Cleanup(func() { buildinfo.Version = previousVersion })

	installerBytes := []byte("fake xuva installer")
	sum := sha256.Sum256(installerBytes)
	sha := hex.EncodeToString(sum[:])
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			writeJSON(w, http.StatusOK, map[string]any{
				"tag_name":     "v0.0.5",
				"html_url":     "https://github.com/EJM-Digital/Xuva/releases/tag/v0.0.5",
				"published_at": "2026-05-27T04:00:00Z",
				"assets": []map[string]any{
					{
						"name":                 "xuva-v0.0.5-win-x64.exe",
						"browser_download_url": releaseURL(r, "/xuva-v0.0.5-win-x64.exe"),
						"size":                 len(installerBytes),
						"digest":               "sha256:" + sha,
					},
				},
			})
		case "/xuva-v0.0.5-win-x64.exe":
			_, _ = w.Write(installerBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer release.Close()
	t.Setenv("XUVA_UPDATE_RELEASE_API", release.URL+"/latest")
	t.Setenv("XUVA_UPDATE_APPLY_SUPPORTED", "1")

	deps := testDeps(t, time.Now())
	deps.Config.RuntimeHome = t.TempDir()
	router := NewRouter(deps)
	response := requestJSON(t, router, http.MethodPost, "/api/system/updates/apply", nil)

	if response["status"] != "staged" || response["version"] != "v0.0.5" {
		t.Fatalf("expected staged update response, got %#v", response)
	}
	pendingPath, _ := response["pendingPath"].(string)
	if pendingPath == "" {
		t.Fatalf("expected pending path, got %#v", response)
	}
	raw, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatalf("read pending update: %v", err)
	}
	var pending map[string]any
	if err := json.Unmarshal(raw, &pending); err != nil {
		t.Fatalf("decode pending update: %v", err)
	}
	if pending["installerSha256"] != sha {
		t.Fatalf("expected staged sha %q, got %#v", sha, pending)
	}
	if _, err := os.Stat(pending["installerPath"].(string)); err != nil {
		t.Fatalf("expected staged installer: %v", err)
	}
}

func releaseURL(r *http.Request, path string) string {
	return "http://" + r.Host + path
}

// When a release publishes both the NSIS .exe and the MSI, the desktop apply
// path stages the MSI — that's the single-Windows-artifact direction in #451.
// The .exe fallback is exercised by TestSystemUpdatesApplyStagesVerifiedInstaller
// above (a release with only the .exe asset).
func TestSystemUpdatesApplyPrefersMSIOverNSIS(t *testing.T) {
	previousVersion := buildinfo.Version
	buildinfo.Version = "v0.0.4"
	t.Cleanup(func() { buildinfo.Version = previousVersion })

	exeBytes := []byte("fake xuva nsis installer")
	msiBytes := []byte("fake xuva msi installer")
	exeSum := sha256.Sum256(exeBytes)
	msiSum := sha256.Sum256(msiBytes)
	exeSHA := hex.EncodeToString(exeSum[:])
	msiSHA := hex.EncodeToString(msiSum[:])

	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			writeJSON(w, http.StatusOK, map[string]any{
				"tag_name":     "v0.0.5",
				"html_url":     "https://github.com/EJM-Digital/Xuva/releases/tag/v0.0.5",
				"published_at": "2026-05-27T04:00:00Z",
				"assets": []map[string]any{
					{
						"name":                 "xuva-v0.0.5-win-x64.exe",
						"browser_download_url": releaseURL(r, "/xuva-v0.0.5-win-x64.exe"),
						"size":                 len(exeBytes),
						"digest":               "sha256:" + exeSHA,
					},
					{
						"name":                 "xuva-server-v0.0.5.msi",
						"browser_download_url": releaseURL(r, "/xuva-server-v0.0.5.msi"),
						"size":                 len(msiBytes),
						"digest":               "sha256:" + msiSHA,
					},
				},
			})
		case "/xuva-v0.0.5-win-x64.exe":
			_, _ = w.Write(exeBytes)
		case "/xuva-server-v0.0.5.msi":
			_, _ = w.Write(msiBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer release.Close()
	t.Setenv("XUVA_UPDATE_RELEASE_API", release.URL+"/latest")
	t.Setenv("XUVA_UPDATE_APPLY_SUPPORTED", "1")

	deps := testDeps(t, time.Now())
	deps.Config.RuntimeHome = t.TempDir()
	router := NewRouter(deps)
	response := requestJSON(t, router, http.MethodPost, "/api/system/updates/apply", nil)

	if response["status"] != "staged" {
		t.Fatalf("expected staged update response, got %#v", response)
	}
	if response["installerSha256"] != msiSHA {
		t.Fatalf("expected MSI to be staged (sha %q), got %#v", msiSHA, response)
	}
	stagedPath, _ := response["installerPath"].(string)
	if !strings.HasSuffix(strings.ToLower(stagedPath), ".msi") {
		t.Fatalf("expected staged installer to be a .msi, got %q", stagedPath)
	}
}

func TestRootServesWebApp(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Contains(body, []byte("__sveltekit_")) || !bytes.Contains(body, []byte("/_app/immutable/")) {
		t.Fatalf("expected svelte root shell, got %s", string(body))
	}
}

func TestRootSupportsHistoryFallback(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	request := httptest.NewRequest(http.MethodGet, "/future-route", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected history fallback 200, got %d", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Fatalf("expected html fallback, got %q", contentType)
	}
}

func TestRootSupportsHistoryFallbackForMigratedRoutes(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	paths := []string{
		"/",
		"/movies",
		"/tv",
		"/movies/movie-route-id",
		"/tv/series-route-id",
		"/settings",
		"/collections",
		"/watchlist",
		"/continue-watching",
		"/recently-added",
	}

	for _, routePath := range paths {
		request := httptest.NewRequest(http.MethodGet, routePath, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("expected %s to return 200, got %d", routePath, response.Code)
		}
		if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "text/html") {
			t.Fatalf("expected %s to return html fallback, got %q", routePath, contentType)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("read body %s: %v", routePath, err)
		}
		if !bytes.Contains(body, []byte("__sveltekit_")) {
			t.Fatalf("expected %s to return svelte bootstrap shell", routePath)
		}
	}
}

func TestRootRedirectsSignedOutHistoryRoutesWhenAuthEnabled(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	paths := []string{"/", "/movies", "/tv/series-route-id", "/settings", "/collections"}

	for _, routePath := range paths {
		request := httptest.NewRequest(http.MethodGet, "http://xuva.test"+routePath, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		if response.Code != http.StatusTemporaryRedirect {
			t.Fatalf("expected %s to redirect to sign-in with 307, got %d", routePath, response.Code)
		}
		if location := response.Header().Get("Location"); location != "/signin" {
			t.Fatalf("expected %s redirect location /signin, got %q", routePath, location)
		}
	}
}

func TestRootAllowsSignInAndAssetsWhenSignedOut(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))

	signInRequest := httptest.NewRequest(http.MethodGet, "http://xuva.test/signin", nil)
	signInResponse := httptest.NewRecorder()
	router.ServeHTTP(signInResponse, signInRequest)
	if signInResponse.Code != http.StatusOK {
		t.Fatalf("expected /signin 200, got %d", signInResponse.Code)
	}

	assetRequest := httptest.NewRequest(http.MethodGet, "http://xuva.test/favicon.svg", nil)
	assetResponse := httptest.NewRecorder()
	router.ServeHTTP(assetResponse, assetRequest)
	if assetResponse.Code != http.StatusOK {
		t.Fatalf("expected /favicon.svg 200, got %d", assetResponse.Code)
	}
}

func TestRootRedirectsToSetupWhenBootstrapPendingAndSignedOut(t *testing.T) {
	router := NewRouter(testDepsWithAuthNoBootstrap(t, time.Now()))
	paths := []string{"/", "/signin", "/movies", "/settings", "/collections"}

	for _, routePath := range paths {
		request := httptest.NewRequest(http.MethodGet, "http://xuva.test"+routePath, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		if response.Code != http.StatusTemporaryRedirect {
			t.Fatalf("expected %s to redirect to setup with 307, got %d", routePath, response.Code)
		}
		if location := response.Header().Get("Location"); location != "/setup" {
			t.Fatalf("expected %s redirect location /setup, got %q", routePath, location)
		}
	}
}

func TestRootAllowsSetupWhenBootstrapPendingAndSignedOut(t *testing.T) {
	router := NewRouter(testDepsWithAuthNoBootstrap(t, time.Now()))

	request := httptest.NewRequest(http.MethodGet, "http://xuva.test/setup", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected /setup 200 while bootstrap is pending, got %d", response.Code)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Contains(body, []byte("__sveltekit_")) {
		t.Fatalf("expected setup wizard route to return svelte shell while bootstrap is pending")
	}
}

func TestRootRedirectsLegacySetupWizardToSetupWhenBootstrapPending(t *testing.T) {
	router := NewRouter(testDepsWithAuthNoBootstrap(t, time.Now()))

	request := httptest.NewRequest(http.MethodGet, "http://xuva.test/setup-wizard", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected /setup-wizard to redirect while bootstrap is pending, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/setup" {
		t.Fatalf("expected /setup-wizard redirect location /setup, got %q", location)
	}
}

func TestRootRedirectsSetupWizardWhenBootstrapIsComplete(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))

	request := httptest.NewRequest(http.MethodGet, "http://xuva.test/setup-wizard", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected /setup-wizard to redirect after bootstrap completion, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/signin" {
		t.Fatalf("expected /setup-wizard redirect location /signin after bootstrap completion, got %q", location)
	}
}

func TestRootRedirectsStandardUserAwayFromSettings(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	adminClient := newAuthTestClient(t)
	loginAs(t, adminClient, router, "admin", "test-password-123!")

	createUser := adminClient.requestJSON(t, router, http.MethodPost, "/api/users", map[string]any{
		"username":    "standard-user",
		"displayName": "Standard User",
		"password":    "test-password-123!",
		"role":        "standard",
	})
	if createUser.status != http.StatusCreated {
		t.Fatalf("expected standard user creation 201, got %d: %s", createUser.status, createUser.body)
	}

	standardClient := newAuthTestClient(t)
	loginAs(t, standardClient, router, "standard-user", "test-password-123!")

	request := httptest.NewRequest(http.MethodGet, "http://xuva.test/settings", nil)
	standardClient.apply(request, false)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected standard user /settings to redirect with 307, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/" {
		t.Fatalf("expected standard user /settings redirect to /, got %q", location)
	}
}

func TestRootDoesNotServeRemovedAdminRoute(t *testing.T) {
	// The dedicated server-side /admin route was removed; admin features now
	// live under /settings. Visiting /admin falls through to the SPA history
	// fallback (no server-side handler exists for it). This test guards
	// against accidentally adding a real /admin handler back to the router.
	router := NewRouter(testDeps(t, time.Now()))
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected /admin to fall through to SPA shell with 200, got %d", response.Code)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Contains(body, []byte("__sveltekit_")) {
		t.Fatalf("expected /admin to return svelte bootstrap shell (no dedicated admin handler), got %q", string(body))
	}
}

func TestUserPreferencesPersistPosterSizePerUser(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	adminClient := newAuthTestClient(t)
	loginAs(t, adminClient, router, "admin", "test-password-123!")

	updated := adminClient.requestJSON(t, router, http.MethodPatch, "/api/users/me/preferences", map[string]any{
		"posterSize": "L",
	})
	if updated.status != http.StatusOK {
		t.Fatalf("expected poster preference update 200, got %d: %s", updated.status, updated.body)
	}
	if updated.payload["posterSize"] != "L" {
		t.Fatalf("expected posterSize L, got %#v", updated.payload)
	}

	updated = adminClient.requestJSON(t, router, http.MethodPatch, "/api/users/me/preferences", map[string]any{
		"autoSkipIntros": true,
	})
	if updated.status != http.StatusOK {
		t.Fatalf("expected auto-skip preference update 200, got %d: %s", updated.status, updated.body)
	}
	if updated.payload["posterSize"] != "L" || updated.payload["autoSkipIntros"] != true {
		t.Fatalf("expected preference patch to preserve posterSize, got %#v", updated.payload)
	}

	session := adminClient.requestJSON(t, router, http.MethodGet, "/api/auth/session", nil)
	preferences := session.payload["preferences"].(map[string]any)
	if preferences["posterSize"] != "L" || preferences["autoSkipIntros"] != true {
		t.Fatalf("expected session preferences to include persisted posterSize, got %#v", preferences)
	}

	createUser := adminClient.requestJSON(t, router, http.MethodPost, "/api/users", map[string]any{
		"username":    "viewer",
		"displayName": "Viewer",
		"password":    "viewer-password-123!",
		"role":        "standard",
	})
	if createUser.status != http.StatusCreated {
		t.Fatalf("expected viewer creation 201, got %d: %s", createUser.status, createUser.body)
	}
	viewerClient := newAuthTestClient(t)
	loginAs(t, viewerClient, router, "viewer", "viewer-password-123!")
	viewerSession := viewerClient.requestJSON(t, router, http.MethodGet, "/api/auth/session", nil)
	viewerPreferences := viewerSession.payload["preferences"].(map[string]any)
	if viewerPreferences["posterSize"] != nil {
		t.Fatalf("expected posterSize to be scoped per user, got %#v", viewerPreferences)
	}
}

func TestUserPreferencesRejectInvalidPosterSize(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")

	response := client.requestJSON(t, router, http.MethodPatch, "/api/users/me/preferences", map[string]any{
		"posterSize": "XL",
	})
	if response.status != http.StatusBadRequest {
		t.Fatalf("expected invalid posterSize 400, got %d: %s", response.status, response.body)
	}
}

func TestRootMissingStaticAssetReturnsNotFound(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	request := httptest.NewRequest(http.MethodGet, "/missing-asset.js", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("expected missing asset 404, got %d", response.Code)
	}
}

func TestRootAssetCacheHeadersAreImmutable(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	request := httptest.NewRequest(http.MethodGet, "/favicon.svg", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	if cacheControl := response.Header().Get("Cache-Control"); !strings.Contains(cacheControl, "immutable") {
		t.Fatalf("expected immutable cache control for static asset, got %q", cacheControl)
	}
}

func TestRootAssetAllowsSameOriginMachineHost(t *testing.T) {
	deps := testDeps(t, time.Now())
	deps.Config.HTTPAddr = "0.0.0.0:8097"
	router := NewRouter(deps)
	request := httptest.NewRequest(http.MethodGet, "http://DESKTOP-7UV0925:8097/favicon.svg", nil)
	request.Header.Set("Origin", "http://DESKTOP-7UV0925:8097")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected same-origin machine-host asset 200, got %d: %s", response.Code, response.Body.String())
	}
}

func TestViteDevAssetsBypassHistoryAuthRedirects(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	router := NewRouter(deps)
	paths := []string{"/@vite/client", "/@fs/Z:/Projects/Xuva/apps/web/svelte/src/app.css", "/src/routes/+layout.svelte"}
	for _, path := range paths {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()

		router.ServeHTTP(response, request)

		if response.Code == http.StatusTemporaryRedirect {
			t.Fatalf("expected Vite dev asset %s to bypass auth history redirect, got redirect to %q", path, response.Header().Get("Location"))
		}
	}
}

func TestRootBuildInfoIsServedNoStore(t *testing.T) {
	// build-info.json was removed from the SvelteKit publish output. The
	// webapp.go handler still applies no-store headers if it ever returns,
	// but the file is not embedded, so the route now 404s. This test guards
	// against accidentally shipping a cached build marker again â€” if a future
	// commit adds build-info.json back, it must serve with no-store headers.
	router := NewRouter(testDeps(t, time.Now()))
	request := httptest.NewRequest(http.MethodGet, "/build-info.json", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code == http.StatusOK {
		if cacheControl := response.Header().Get("Cache-Control"); !strings.Contains(cacheControl, "no-store") {
			t.Fatalf("if build-info.json is served, it must use no-store cache control, got %q", cacheControl)
		}
		return
	}
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected 200 (with no-store) or 404, got %d", response.Code)
	}
}

func TestRetiredRootRoutesReturnNotFound(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	paths := []string{
		"/legacy",
		"/legacy/settings",
		"/next",
		"/next/movies",
		"/next/settings",
	}

	for _, routePath := range paths {
		request := httptest.NewRequest(http.MethodGet, routePath, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		if response.Code != http.StatusNotFound {
			t.Fatalf("expected %s 404, got %d", routePath, response.Code)
		}
	}
}

func TestSecurityHeadersPresentOnAPIAndWebResponses(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	for _, path := range []string{"/", "/api/health"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()

		router.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("expected %s 200, got %d", path, response.Code)
		}
		for header, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := response.Header().Get(header); got != want {
				t.Fatalf("expected %s header %s=%q, got %q", path, header, want, got)
			}
		}
		if got := response.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
			t.Fatalf("expected CSP on %s, got %q", path, got)
		}
	}
}

func TestDisallowedOriginBlockedByCORS(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	request.Header.Set("Origin", "https://evil.example")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("expected disallowed origin 403, got %d", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected no allow-origin header, got %q", got)
	}
}

func TestTrustedOriginAllowedByCORS(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	request := httptest.NewRequest(http.MethodOptions, "/api/health", nil)
	request.Header.Set("Origin", "http://localhost:8097")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("expected preflight 204, got %d", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8097" {
		t.Fatalf("expected local allow-origin, got %q", got)
	}
}

func TestArtworkPathTraversalFailsSafely(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	for _, target := range []string{
		"/api/artwork/movie/example?type=..",
		"/api/artwork/movie/example?type=poster/../../secret",
		"/api/artwork/%2e%2e/example",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()

		router.ServeHTTP(response, request)

		if response.Code != http.StatusBadRequest {
			t.Fatalf("expected traversal request %s to fail with 400, got %d", target, response.Code)
		}
	}
}

func TestArchitectureExposesSeparateQueuesAndWorkloads(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	payload := getJSON(t, router, "/api/architecture")

	queues, ok := payload["queues"].([]any)
	if !ok || len(queues) != 3 {
		t.Fatalf("expected scan, probe, and transcode queues, got %#v", payload["queues"])
	}
	workloads, ok := payload["workloads"].([]any)
	if !ok || len(workloads) != 3 {
		t.Fatalf("expected three workload classes, got %#v", payload["workloads"])
	}
}

func TestRequestCorrelationAndMetrics(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	request.Header.Set("X-Request-ID", "trace-test-1")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected health 200, got %d", response.Code)
	}
	if got := response.Header().Get("X-Request-ID"); got != "trace-test-1" {
		t.Fatalf("expected propagated request id, got %q", got)
	}
	metrics := getJSON(t, router, "/api/metrics")
	requests := metrics["requests"].([]any)
	if len(requests) == 0 {
		t.Fatalf("expected request metrics, got %#v", metrics)
	}
	var found bool
	for _, item := range requests {
		metric := item.(map[string]any)
		if metric["path"] == "/api/health" && metric["lastCorrelationId"] == "trace-test-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected health metric with correlation id, got %#v", requests)
	}
	queues := metrics["queues"].([]any)
	if len(queues) != 3 {
		t.Fatalf("expected queue metrics, got %#v", metrics["queues"])
	}
	firstQueue := queues[0].(map[string]any)
	if _, ok := firstQueue["workerUtilization"]; !ok {
		t.Fatalf("expected worker utilization in queue metric, got %#v", firstQueue)
	}
	if _, ok := firstQueue["maxQueued"]; !ok {
		t.Fatalf("expected maxQueued in queue metric, got %#v", firstQueue)
	}
	if _, ok := metrics["playbackSLO"]; !ok {
		t.Fatalf("expected playbackSLO metric block, got %#v", metrics)
	}
}

func TestClientBootstrapDefaultsToAppleTVContract(t *testing.T) {
	startedAt := time.Date(2026, 4, 30, 4, 5, 6, 0, time.UTC)
	router := NewRouter(testDeps(t, startedAt))
	request := httptest.NewRequest(http.MethodGet, "/api/client/bootstrap", nil)
	request.Host = "xuva.local:8097"
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected bootstrap 200, got %d", response.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	server := payload["server"].(map[string]any)
	if server["name"] != "Xuva" || server["baseUrl"] != "http://xuva.local:8097" || server["startedAt"] != startedAt.Format(time.RFC3339) {
		t.Fatalf("expected server identity, got %#v", server)
	}
	client := payload["client"].(map[string]any)
	profile := client["profile"].(map[string]any)
	if client["requestedProfile"] != "apple-tv" || profile["id"] != "apple-tv" || profile["supportsHls"] != true {
		t.Fatalf("expected apple-tv HLS profile, got %#v", client)
	}
	authPayload := payload["auth"].(map[string]any)
	if authPayload["required"] != false {
		t.Fatalf("expected auth disabled bootstrap to report auth not required, got %#v", authPayload)
	}
	features := payload["features"].(map[string]any)
	if features["vendorRelay"] != false || features["hlsAdaptive"] != true {
		t.Fatalf("expected local-first playback features, got %#v", features)
	}
	endpoints := payload["endpoints"].(map[string]any)
	if endpoints["adaptiveMaster"] != "/api/media-sources/{id}/adaptive/master.m3u8" || endpoints["sessions"] != "/api/sessions" {
		t.Fatalf("expected tv playback endpoints, got %#v", endpoints)
	}
}

func TestClientBootstrapKeepsConfiguredServerName(t *testing.T) {
	deps := testDeps(t, time.Now())
	deps.Config.ServerName = "Family Library"
	router := NewRouter(deps)
	request := httptest.NewRequest(http.MethodGet, "/api/client/bootstrap", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected bootstrap 200, got %d", response.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	server := payload["server"].(map[string]any)
	if server["name"] != "Family Library" {
		t.Fatalf("expected configured server name, got %#v", server)
	}
}

func TestLoopbackWebRequestsCanonicalizeToLocalhost(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8097/signin?next=%2Fsettings", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected redirect, got %d: %s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "http://localhost:8097/signin?next=%2Fsettings" {
		t.Fatalf("expected localhost redirect, got %q", location)
	}
}

func TestLoopbackCanonicalizationDoesNotAffectAPI(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8097/api/health", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected API request to stay on 127.0.0.1, got %d: %s", response.Code, response.Body.String())
	}
}

func TestConfiguredCanonicalWebOriginRedirectsLANBrowserRequests(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	deps.Config.CanonicalWebOrigin = "http://media-server.local:8097"
	router := NewRouter(deps)
	request := httptest.NewRequest(http.MethodGet, "http://10.1.1.99:8097/settings", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected redirect, got %d: %s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "http://media-server.local:8097/settings" {
		t.Fatalf("expected configured canonical web origin redirect, got %q", location)
	}
}

func TestLANBoundServerDoesNotRedirectRawIPToHostname(t *testing.T) {
	// Bare LAN-IP requests must NOT be canonicalized to the OS hostname —
	// that broke Apple TV / mobile clients which resolved the server via
	// mDNS-advertised IP and then chased a redirect to a name they couldn't
	// resolve. Operators who want a forced canonical hostname can still set
	// XUVA_CANONICAL_WEB_ORIGIN explicitly. (Auth-flow redirects to /signin
	// on the same host are fine; we only assert no host-change.)
	deps := testDepsWithAuth(t, time.Now())
	deps.Config.HTTPAddr = "0.0.0.0:8097"
	router := NewRouter(deps)
	request := httptest.NewRequest(http.MethodGet, "http://10.1.1.99:8097/", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	location := response.Header().Get("Location")
	if location == "" {
		return // No redirect at all — also fine.
	}
	// A redirect IS allowed (auth, etc.), but it must stay on the same host.
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse Location %q: %v", location, err)
	}
	if parsed.Host != "" && parsed.Host != "10.1.1.99:8097" {
		t.Fatalf("LAN-IP request should not redirect to a different host; got %q", location)
	}
}

func TestLANBoundServerStillRedirectsLocalhost(t *testing.T) {
	// `http://localhost:8097/` is a cosmetic case — when the server is bound
	// non-loopback and somebody hits localhost, redirect to the OS hostname
	// so the URL bar reads sensibly. Bare IPs (handled by the test above)
	// stay put.
	deps := testDepsWithAuth(t, time.Now())
	deps.Config.HTTPAddr = "0.0.0.0:8097"
	router := NewRouter(deps)
	request := httptest.NewRequest(http.MethodGet, "http://localhost:8097/", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	host := osHostnameForURL()
	if host == "" {
		t.Skip("host name unavailable")
	}
	if response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected redirect for localhost, got %d: %s", response.Code, response.Body.String())
	}
	expected := "http://" + net.JoinHostPort(host, "8097") + "/"
	if location := response.Header().Get("Location"); location != expected {
		t.Fatalf("expected machine-host redirect %q, got %q", expected, location)
	}
}

func TestClientBootstrapReportsCanonicalWebURL(t *testing.T) {
	deps := testDeps(t, time.Now())
	deps.Config.ServerName = "Family Room"
	deps.Config.CanonicalWebOrigin = "http://media-server.local:8097"
	router := NewRouter(deps)
	request := httptest.NewRequest(http.MethodGet, "http://10.1.1.99:8097/api/client/bootstrap", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected bootstrap 200, got %d: %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	server := payload["server"].(map[string]any)
	if server["name"] != "Family Room" || server["displayName"] != "Family Room" {
		t.Fatalf("expected Xuva display name in bootstrap, got %#v", server)
	}
	if server["hostName"] != "media-server.local" {
		t.Fatalf("expected canonical connection host in bootstrap, got %#v", server)
	}
	if server["webUrl"] != "http://media-server.local:8097" || server["canonicalWebOrigin"] != "http://media-server.local:8097" {
		t.Fatalf("expected canonical web URL in bootstrap, got %#v", server)
	}
}

func TestClientBootstrapKeepsReachableRequestURLWhenCanonicalOriginIsBlank(t *testing.T) {
	deps := testDeps(t, time.Now())
	deps.Config.HTTPAddr = "0.0.0.0:8097"
	deps.Config.CanonicalWebOrigin = ""
	router := NewRouter(deps)
	request := httptest.NewRequest(http.MethodGet, "http://10.1.1.99:8097/api/client/bootstrap", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected bootstrap 200, got %d: %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	server := payload["server"].(map[string]any)
	if server["webUrl"] != "http://10.1.1.99:8097" {
		t.Fatalf("expected reachable request URL in bootstrap, got %#v", server)
	}
	if server["hostName"] != "10.1.1.99" {
		t.Fatalf("expected reachable request host in bootstrap, got %#v", server)
	}
}

func TestDiscoveryStatusReturnsSafeFields(t *testing.T) {
	deps := testDeps(t, time.Now())
	deps.Config.ServerName = "Family Library"
	deps.Config.DataDir = filepath.Join(t.TempDir(), "data")
	deps.Config.DiscoveryEnabled = true
	deps.Config.DiscoveryServiceType = "_xuva._tcp"
	deps.Discovery = discovery.NewService(deps.Config)
	router := NewRouter(deps)
	request := httptest.NewRequest(http.MethodGet, "/api/discovery/status", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected discovery status 200, got %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, deps.Config.DataDir) || strings.Contains(strings.ToLower(body), "ffmpeg") {
		t.Fatalf("expected discovery status to avoid paths and runtime secrets, got %s", body)
	}
	payload := decodeBody(t, response.Body.Bytes())
	if payload["serviceName"] != "Family Library" {
		t.Fatalf("expected discovery status service name, got %#v", payload)
	}
	if payload["serviceType"] != "_xuva._tcp.local." {
		t.Fatalf("expected discovery service type, got %#v", payload)
	}
	if payload["hostName"] == "" || payload["hostName"] == "Family Library" {
		t.Fatalf("expected discovery network host name to stay separate from display name, got %#v", payload)
	}
	if payload["webUrl"] == "" {
		t.Fatalf("expected discovery web URL, got %#v", payload)
	}
	if _, ok := payload["txtRecords"]; !ok {
		t.Fatalf("expected txtRecords field, got %#v", payload)
	}
	if _, ok := payload["interfaces"]; !ok {
		t.Fatalf("expected interfaces field, got %#v", payload)
	}
	if _, ok := payload["advertiseIps"]; !ok {
		t.Fatalf("expected advertiseIps field, got %#v", payload)
	}
}

func TestClientBootstrapIsReadableBeforeLogin(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	request := httptest.NewRequest(http.MethodGet, "/api/client/bootstrap?clientProfile=ios", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected unauthenticated bootstrap 200, got %d: %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	authPayload := payload["auth"].(map[string]any)
	if authPayload["required"] != true {
		t.Fatalf("expected auth-enabled bootstrap to report auth required, got %#v", authPayload)
	}
	if authPayload["bootstrapAllowed"] != false {
		t.Fatalf("expected bootstrap to be closed once admin exists, got %#v", authPayload)
	}
	client := payload["client"].(map[string]any)
	profile := client["profile"].(map[string]any)
	if client["requestedProfile"] != "ios" || profile["id"] != "ios" {
		t.Fatalf("expected requested ios profile, got %#v", client)
	}
}

func TestAuthBootstrapCanCreateFirstAdmin(t *testing.T) {
	router := NewRouter(testDepsWithAuthNoBootstrap(t, time.Now()))
	client := newAuthTestClient(t)

	before := client.requestJSON(t, router, http.MethodGet, "/api/client/bootstrap", nil)
	if before.status != http.StatusOK {
		t.Fatalf("expected bootstrap status 200, got %d: %s", before.status, before.body)
	}
	authBefore, _ := before.payload["auth"].(map[string]any)
	if authBefore["bootstrapAllowed"] != true {
		t.Fatalf("expected bootstrapAllowed=true before first account, got %#v", authBefore)
	}

	create := client.requestJSON(t, router, http.MethodPost, "/api/auth/bootstrap", map[string]any{
		"username":    "owner",
		"displayName": "Owner",
		"password":    "owner-password-123!",
	})
	if create.status != http.StatusCreated {
		t.Fatalf("expected bootstrap create 201, got %d: %s", create.status, create.body)
	}
	user, _ := create.payload["user"].(map[string]any)
	if user["username"] != "owner" || user["role"] != "admin" {
		t.Fatalf("expected bootstrap admin account, got %#v", create.payload)
	}

	session := client.requestJSON(t, router, http.MethodGet, "/api/auth/session", nil)
	if session.status != http.StatusOK {
		t.Fatalf("expected authenticated session after bootstrap, got %d: %s", session.status, session.body)
	}

	after := client.requestJSON(t, router, http.MethodGet, "/api/client/bootstrap", nil)
	authAfter, _ := after.payload["auth"].(map[string]any)
	if authAfter["bootstrapAllowed"] != false {
		t.Fatalf("expected bootstrapAllowed=false after first account, got %#v", authAfter)
	}
}

func TestAuthBootstrapRejectedAfterAccountExists(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)

	create := client.requestJSON(t, router, http.MethodPost, "/api/auth/bootstrap", map[string]any{
		"username":    "owner",
		"displayName": "Owner",
		"password":    "owner-password-123!",
	})
	if create.status != http.StatusConflict {
		t.Fatalf("expected bootstrap conflict once account exists, got %d: %s", create.status, create.body)
	}
}

func TestPairingRequestCreateStatusAndAdminApprove(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))

	create := requestJSON(t, router, http.MethodPost, "/api/pairing/requests", map[string]any{
		"deviceName":    "Living Room Apple TV",
		"clientProfile": "apple-tv",
	})
	pairingID, _ := create["id"].(string)
	if pairingID == "" || create["status"] != "pending" || len(create["code"].(string)) != 6 {
		t.Fatalf("expected pending pairing request, got %#v", create)
	}

	status := getJSON(t, router, "/api/pairing/requests/"+pairingID)
	if status["status"] != "pending" || status["code"] == "" {
		t.Fatalf("expected public pending status with code, got %#v", status)
	}

	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")
	approved := client.requestJSON(t, router, http.MethodPost, "/api/pairing/requests/"+pairingID+"/approve", map[string]any{})
	if approved.status != http.StatusOK {
		t.Fatalf("expected approval 200, got %d: %s", approved.status, approved.body)
	}
	if approved.payload["status"] != "approved" || approved.payload["deviceId"] == "" || approved.payload["code"] != nil {
		t.Fatalf("expected approved pairing without code, got %#v", approved.payload)
	}
	authGrant, _ := approved.payload["auth"].(map[string]any)
	token, _ := authGrant["sessionToken"].(string)
	if authGrant["method"] != "header_token" || strings.TrimSpace(token) == "" {
		t.Fatalf("expected approved pairing to include native header token, got %#v", approved.payload)
	}

	polled := getJSON(t, router, "/api/pairing/requests/"+pairingID)
	polledAuth, _ := polled["auth"].(map[string]any)
	if polled["status"] != "approved" || polled["deviceId"] == "" || polled["code"] != nil || strings.TrimSpace(polledAuth["sessionToken"].(string)) == "" {
		t.Fatalf("expected approved polling result with native token and without code, got %#v", polled)
	}

	nativeRequest := httptest.NewRequest(http.MethodGet, "http://xuva.test/api/client/home", nil)
	nativeRequest.Header.Set("X-Auth-Token", token)
	nativeResponse := httptest.NewRecorder()
	router.ServeHTTP(nativeResponse, nativeRequest)
	if nativeResponse.Code != http.StatusOK {
		t.Fatalf("expected native token to access client home, got %d: %s", nativeResponse.Code, nativeResponse.Body.String())
	}

	client = newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")
	devices := client.requestJSON(t, router, http.MethodGet, "/api/devices", nil)
	if devices.status != http.StatusOK {
		t.Fatalf("expected approved devices list 200, got %d: %s", devices.status, devices.body)
	}
	list, ok := devices.payload["devices"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("expected one approved device, got %#v", devices.payload)
	}
	item, _ := list[0].(map[string]any)
	if item["deviceName"] != "Living Room Apple TV" || item["clientProfile"] != "apple-tv" {
		t.Fatalf("expected safe approved device payload, got %#v", item)
	}
	if _, exists := item["deviceId"]; exists {
		t.Fatalf("expected approved device payload to omit raw device id, got %#v", item)
	}
	if strings.Contains(devices.body, "token") || strings.Contains(devices.body, "secret") {
		t.Fatalf("expected approved device payload to avoid auth material, got %s", devices.body)
	}
	pending := client.requestJSON(t, router, http.MethodGet, "/api/pairing/requests", nil)
	if pending.status != http.StatusOK {
		t.Fatalf("expected pairing list 200, got %d: %s", pending.status, pending.body)
	}
	pendingList, _ := pending.payload["requests"].([]any)
	if len(pendingList) != 0 {
		t.Fatalf("expected approved pairing to disappear from pending approvals, got %#v", pending.payload)
	}
	if strings.Contains(pending.body, token) || strings.Contains(pending.body, "sessionToken") {
		t.Fatalf("expected pairing list to avoid native auth token, got %s", pending.body)
	}
}

func TestPairingRequestCreateAcceptsNativeDiscoveryPayload(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))

	create := requestJSON(t, router, http.MethodPost, "/api/pairing/requests", map[string]any{
		"deviceName":    "Bedroom Apple TV",
		"clientProfile": "apple-tv",
		"deviceId":      "apple-tv-bedroom",
	})

	if create["status"] != "pending" || create["deviceId"] != "apple-tv-bedroom" || create["clientProfile"] != "apple-tv" {
		t.Fatalf("expected native pairing request payload to be accepted, got %#v", create)
	}
}

func TestPairingRequestCreateDoesNotRequireDeviceRegistry(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	deps.Devices = nil
	router := NewRouter(deps)

	create := requestJSON(t, router, http.MethodPost, "/api/pairing/requests", map[string]any{
		"deviceName":    "Bedroom Apple TV",
		"clientProfile": "apple-tv",
		"deviceId":      "apple-tv-bedroom",
	})

	if create["status"] != "pending" || create["id"] == "" {
		t.Fatalf("expected pairing create to survive missing device registry, got %#v", create)
	}
}

func TestPairingDenyDoesNotCreateApprovedDevice(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))

	create := requestJSON(t, router, http.MethodPost, "/api/pairing/requests", map[string]any{
		"deviceName":    "Bedroom Tablet",
		"clientProfile": "ios",
	})
	pairingID, _ := create["id"].(string)
	if pairingID == "" {
		t.Fatalf("expected pairing id, got %#v", create)
	}

	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")
	denied := client.requestJSON(t, router, http.MethodPost, "/api/pairing/requests/"+pairingID+"/deny", map[string]any{})
	if denied.status != http.StatusNoContent {
		t.Fatalf("expected 204 No Content from deny, got %#v", denied)
	}
	devices := client.requestJSON(t, router, http.MethodGet, "/api/devices", nil)
	list, _ := devices.payload["devices"].([]any)
	if len(list) != 0 {
		t.Fatalf("expected denied pairing to avoid approved device record, got %#v", devices.payload)
	}
}

func TestApprovedDeviceCanBeRevoked(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))

	create := requestJSON(t, router, http.MethodPost, "/api/pairing/requests", map[string]any{
		"deviceName":    "Hallway Apple TV",
		"clientProfile": "apple-tv",
	})
	pairingID, _ := create["id"].(string)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")
	approved := client.requestJSON(t, router, http.MethodPost, "/api/pairing/requests/"+pairingID+"/approve", map[string]any{})
	if approved.status != http.StatusOK {
		t.Fatalf("approve pairing: %#v", approved)
	}
	authGrant, _ := approved.payload["auth"].(map[string]any)
	token, _ := authGrant["sessionToken"].(string)
	if strings.TrimSpace(token) == "" {
		t.Fatalf("expected native token in approved pairing, got %#v", approved.payload)
	}
	devices := client.requestJSON(t, router, http.MethodGet, "/api/devices", nil)
	list, _ := devices.payload["devices"].([]any)
	if len(list) != 1 {
		t.Fatalf("expected one approved device, got %#v", devices.payload)
	}
	item, _ := list[0].(map[string]any)
	id, _ := item["id"].(string)
	if id == "" {
		t.Fatalf("expected approved device id, got %#v", item)
	}

	revoked := client.requestJSON(t, router, http.MethodPost, "/api/devices/"+id+"/revoke", map[string]any{})
	if revoked.status != http.StatusOK || revoked.payload["status"] != "revoked" {
		t.Fatalf("expected device revoke success, got %#v", revoked)
	}
	after := client.requestJSON(t, router, http.MethodGet, "/api/devices", nil)
	afterList, _ := after.payload["devices"].([]any)
	if len(afterList) != 0 {
		t.Fatalf("expected revoked device to disappear from approved list, got %#v", after.payload)
	}

	nativeRequest := httptest.NewRequest(http.MethodGet, "http://xuva.test/api/client/home", nil)
	nativeRequest.Header.Set("X-Auth-Token", token)
	nativeResponse := httptest.NewRecorder()
	router.ServeHTTP(nativeResponse, nativeRequest)
	if nativeResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expected revoked native device token to be rejected, got %d: %s", nativeResponse.Code, nativeResponse.Body.String())
	}
}

func TestLegacyNativeSessionWithoutApprovedDeviceIsRejected(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	router := NewRouter(deps)

	_, _, token, err := deps.Auth.IssueSessionForUser(context.Background(), "local", "127.0.0.1", "Legacy Apple TV")
	if err != nil {
		t.Fatalf("issue legacy native token: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://xuva.test/api/client/home", nil)
	request.Header.Set("X-Auth-Token", token)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected unlinked legacy native token to be rejected, got %d: %s", response.Code, response.Body.String())
	}
}

func TestPairingAdminRoutesRequireAdmin(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	if _, err := deps.Auth.CreateUser(context.Background(), "viewer", "viewer-password-123!", "Viewer", "standard"); err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "viewer", "viewer-password-123!")

	list := client.requestJSON(t, router, http.MethodGet, "/api/pairing/requests", nil)
	if list.status != http.StatusForbidden {
		t.Fatalf("expected standard user to get 403 on pairing list, got %d: %s", list.status, list.body)
	}
}

func TestReadinessReflectsDegradedRuntimePath(t *testing.T) {
	deps := testDeps(t, time.Now())
	badPath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(badPath, []byte("file"), 0o600); err != nil {
		t.Fatalf("write bad path: %v", err)
	}
	deps.Config.TranscodeDir = badPath
	router := NewRouter(deps)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ready", nil))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected degraded readiness 503, got %d: %s", response.Code, response.Body.String())
	}
	payload := decodeBody(t, response.Body.Bytes())
	if payload["status"] != "degraded" {
		t.Fatalf("expected degraded status, got %#v", payload)
	}
}

func TestOperationalEventCarriesCorrelationID(t *testing.T) {
	deps := testDeps(t, time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eventsCh, stop := deps.Events.Subscribe(ctx)
	defer stop()
	router := NewRouter(deps)
	request := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(mustJSON(t, map[string]any{
		"mediaSourceId": "source-1",
		"title":         "Example",
	})))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "trace-session-1")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("expected session start 202, got %d: %s", response.Code, response.Body.String())
	}
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-eventsCh:
			if event.Type != "api.session.accepted" {
				continue
			}
			payload := event.Data.(map[string]any)
			if payload["correlationId"] != "trace-session-1" {
				t.Fatalf("expected correlated event, got %#v", payload)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for correlated session event")
		}
	}
}

func TestSystemStatusAndRuntimeFolders(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	status := getJSON(t, router, "/api/system/status")

	cpu := status["cpu"].(map[string]any)
	if cpu["cores"].(float64) < 1 {
		t.Fatalf("expected at least one cpu core, got %#v", cpu)
	}
	if _, ok := status["network"].(map[string]any); !ok {
		t.Fatalf("expected network stats in system status, got %#v", status)
	}
	disks := status["disks"].([]any)
	if len(disks) < 5 {
		t.Fatalf("expected runtime folder disk entries, got %#v", status)
	}
	settings := getJSON(t, router, "/api/settings")
	paths := settings["runtimePaths"].(map[string]any)
	if paths["transcode"] == "" || paths["metadata"] == "" {
		t.Fatalf("expected configurable runtime paths, got %#v", paths)
	}
}

func TestMetricsIncludesTimeline(t *testing.T) {
	deps := testDeps(t, time.Now())
	router := NewRouter(deps)
	deps.Events.Publish("session.route.changed", map[string]any{"toRoute": "adaptive"})
	// The event bus is asynchronous â€” poll until the timeline is populated
	// (up to 2 seconds) to avoid a race between publish and the subscriber goroutine.
	var timeline []any
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		metrics := getJSON(t, router, "/api/metrics")
		if entries, ok := metrics["timeline"].([]any); ok && len(entries) > 0 {
			timeline = entries
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(timeline) == 0 {
		t.Fatal("expected timeline entries after 2s, got none")
	}
}

func TestClientHomeReturnsTVRows(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Movies", "Arrival (2016)", "Arrival.2016.1080p.mkv"))
	writeTestFile(t, filepath.Join(root, "TV", "The Bear", "Season 01", "The.Bear.S01E01.1080p.mkv"))

	router := NewRouter(testDeps(t, time.Now()))
	movieScan := postJSON(t, router, "/api/libraries/movies/scan", map[string]any{
		"path":        filepath.Join(root, "Movies"),
		"sampleLimit": 10,
	})
	waitForScan(t, router, movieScan["id"].(string))
	tvScan := postJSON(t, router, "/api/libraries/tv/scan", map[string]any{
		"path":        filepath.Join(root, "TV"),
		"sampleLimit": 10,
	})
	waitForScan(t, router, tvScan["id"].(string))

	home := getJSON(t, router, "/api/client/home?clientProfile=apple-tv")
	if home["profile"] != "apple-tv" {
		t.Fatalf("expected apple-tv profile, got %#v", home)
	}
	heroes, ok := home["heroes"].([]any)
	if !ok || len(heroes) == 0 {
		t.Fatalf("expected non-empty heroes array, got %#v", home["heroes"])
	}
	for i, raw := range heroes {
		hero, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("expected hero %d to be an object, got %#v", i, raw)
		}
		if hero["title"] == "" || hero["kind"] == "" {
			t.Fatalf("expected hero %d to have title and kind, got %#v", i, hero)
		}
	}
	rows := home["rows"].([]any)
	if len(rows) < 4 {
		t.Fatalf("expected at least four TV rows, got %#v", rows)
	}
	rowItems := map[string]int{}
	for _, raw := range rows {
		row := raw.(map[string]any)
		items := row["items"].([]any)
		rowItems[row["id"].(string)] = len(items)
	}
	if rowItems["movies"] != 1 || rowItems["tv"] != 1 || rowItems["recently-added"] < 2 {
		t.Fatalf("expected movie, tv, and recently added rows, got %#v", rowItems)
	}
}

func TestClientDetailEndpointsReturnNativePayloads(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Movies", "Arrival (2016)", "Arrival.2016.1080p.mkv"))
	writeTestFile(t, filepath.Join(root, "Movies", "Arrival (2016)", "Arrival.2016.1080p.en.ass"))
	writeTestFile(t, filepath.Join(root, "TV", "The Bear", "Season 01", "The.Bear.S01E01.1080p.mkv"))

	router := NewRouter(testDeps(t, time.Now()))
	movieScan := postJSON(t, router, "/api/libraries/movies/scan", map[string]any{
		"path":        filepath.Join(root, "Movies"),
		"sampleLimit": 10,
	})
	waitForScan(t, router, movieScan["id"].(string))
	tvScan := postJSON(t, router, "/api/libraries/tv/scan", map[string]any{
		"path":        filepath.Join(root, "TV"),
		"sampleLimit": 10,
	})
	waitForScan(t, router, tvScan["id"].(string))

	movies := getJSON(t, router, "/api/movies")
	movieID := movies["movies"].([]any)[0].(map[string]any)["id"].(string)
	movieDetail := getJSON(t, router, "/api/client/movies/"+movieID+"?clientProfile=apple-tv")
	if movieDetail["defaultMediaSourceId"] == "" {
		t.Fatalf("expected default movie media source, got %#v", movieDetail)
	}
	movieVersions := movieDetail["versions"].([]any)
	if len(movieVersions) != 1 {
		t.Fatalf("expected one movie version, got %#v", movieDetail)
	}
	movieVersion := movieVersions[0].(map[string]any)
	sidecars := movieVersion["sidecars"].([]any)
	if len(sidecars) != 1 {
		t.Fatalf("expected one sidecar subtitle payload, got %#v", movieVersion)
	}
	conversion := sidecars[0].(map[string]any)["conversion"].(map[string]any)
	if conversion["outputFormat"] != "webvtt" || conversion["reasonCode"] != "subtitle_text_conversion_available" {
		t.Fatalf("expected text subtitle conversion plan, got %#v", conversion)
	}

	series := getJSON(t, router, "/api/series")
	seriesID := series["series"].([]any)[0].(map[string]any)["id"].(string)
	seriesDetail := getJSON(t, router, "/api/client/series/"+seriesID+"?clientProfile=apple-tv")
	if seriesDetail["defaultMediaSourceId"] == "" {
		t.Fatalf("expected default series media source, got %#v", seriesDetail)
	}
	seasons := seriesDetail["seasons"].([]any)
	if len(seasons) != 1 {
		t.Fatalf("expected one season payload, got %#v", seriesDetail)
	}
	episodes := seasons[0].(map[string]any)["episodes"].([]any)
	if len(episodes) != 1 {
		t.Fatalf("expected one episode payload, got %#v", seasons[0])
	}
	episodeVersions := episodes[0].(map[string]any)["versions"].([]any)
	if len(episodeVersions) != 1 {
		t.Fatalf("expected one episode version payload, got %#v", episodes[0])
	}
}

// TestClientSearchHandler_ReturnsShape exercises the happy path: a query that
// matches a movie, a series, a person (via metadata cast), and a collection
// (via metadata $.collection) returns one HTTP 200 payload with all four
// buckets populated and the expected per-kind field shape.
func TestClientSearchHandler_ReturnsShape(t *testing.T) {
	deps := testDeps(t, time.Now())
	router := NewRouter(deps)
	seedSearchFixtures(t, deps.Catalog)

	payload := getJSON(t, router, "/api/client/search?q=spider")
	if payload["query"] != "spider" {
		t.Fatalf("expected query echoed back, got %#v", payload["query"])
	}
	for _, bucket := range []string{"movies", "series", "people", "collections"} {
		arr, ok := payload[bucket].([]any)
		if !ok {
			t.Fatalf("expected %s to be an array, got %#v", bucket, payload[bucket])
		}
		if len(arr) == 0 {
			t.Fatalf("expected %s bucket to contain at least one hit, got empty", bucket)
		}
	}

	movie := payload["movies"].([]any)[0].(map[string]any)
	if movie["kind"] != "movie" || movie["id"] == "" || movie["title"] == "" {
		t.Fatalf("movie hit missing kind/id/title: %#v", movie)
	}
	series := payload["series"].([]any)[0].(map[string]any)
	if series["kind"] != "series" || series["id"] == "" || series["title"] == "" {
		t.Fatalf("series hit missing kind/id/title: %#v", series)
	}
	person := payload["people"].([]any)[0].(map[string]any)
	if person["kind"] != "person" || person["name"] == "" {
		t.Fatalf("person hit missing kind/name: %#v", person)
	}
	if _, ok := person["creditCount"]; !ok {
		t.Fatalf("person hit missing creditCount: %#v", person)
	}
	collection := payload["collections"].([]any)[0].(map[string]any)
	if collection["kind"] != "collection" || collection["id"] == "" || collection["name"] == "" {
		t.Fatalf("collection hit missing kind/id/name: %#v", collection)
	}
	if _, ok := collection["movieCount"]; !ok {
		t.Fatalf("collection hit missing movieCount: %#v", collection)
	}
}

// TestClientSearchHandler_DefaultLimit verifies that omitting the limit param
// applies the documented default of 8 per bucket.
func TestClientSearchHandler_DefaultLimit(t *testing.T) {
	deps := testDeps(t, time.Now())
	router := NewRouter(deps)
	seedManyMoviesForHandler(t, deps.Catalog, "deflim", 12)

	payload := getJSON(t, router, "/api/client/search?q=deflim")
	movies := payload["movies"].([]any)
	if len(movies) != 8 {
		t.Fatalf("expected default limit of 8 movies, got %d", len(movies))
	}
}

// TestClientSearchHandler_LimitParamRespectedAndCapped verifies that an
// explicit limit is honored and that values above 40 are clamped.
func TestClientSearchHandler_LimitParamRespectedAndCapped(t *testing.T) {
	deps := testDeps(t, time.Now())
	router := NewRouter(deps)
	seedManyMoviesForHandler(t, deps.Catalog, "lim", 12)

	payload := getJSON(t, router, "/api/client/search?q=lim&limit=3")
	movies := payload["movies"].([]any)
	if len(movies) != 3 {
		t.Fatalf("expected limit=3 to return 3 movies, got %d", len(movies))
	}

	seedManyMoviesForHandler(t, deps.Catalog, "capcheck", 55)
	capped := getJSON(t, router, "/api/client/search?q=capcheck&limit=100")
	cappedMovies := capped["movies"].([]any)
	if len(cappedMovies) > 40 {
		t.Fatalf("expected limit clamped to 40, got %d", len(cappedMovies))
	}
}

// TestClientSearchHandler_EmptyQueryReturnsEmptyBuckets documents the
// observed behavior: the handler returns 200 with empty buckets for an empty
// query rather than a 400. (Flagged as a behavior choice â€” the original spec
// suggested 400, but the implementation prefers a non-error empty response so
// the client's debounced typeahead can call without special-casing.)
func TestClientSearchHandler_EmptyQueryReturnsEmptyBuckets(t *testing.T) {
	deps := testDeps(t, time.Now())
	router := NewRouter(deps)

	request := httptest.NewRequest(http.MethodGet, "/api/client/search?q=", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty query, got %d: %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload["query"] != "" {
		t.Fatalf("expected query to echo empty string, got %#v", payload["query"])
	}
	for _, bucket := range []string{"movies", "series", "people", "collections"} {
		arr, ok := payload[bucket].([]any)
		if !ok {
			t.Fatalf("expected %s to be an array, got %#v", bucket, payload[bucket])
		}
		if len(arr) != 0 {
			t.Fatalf("expected %s to be empty for empty query, got %#v", bucket, arr)
		}
	}
}

// seedSearchFixtures populates the catalog with cross-bucket fixtures for
// handler-level coverage. The data is independent of the catalog-package
// fixtures so the handler tests don't share state with service tests.
func seedSearchFixtures(t *testing.T, service *catalog.Service) {
	t.Helper()
	ctx := context.Background()

	movieLib := libraries.Library{ID: "movies", Name: "Movies", Path: `X:\Movies`, Kind: libraries.KindMovies}
	tvLib := libraries.Library{ID: "tv", Name: "TV", Path: `X:\TV`, Kind: libraries.KindTV}

	makeMovieFile := func(title string) scanner.FileCandidate {
		rel := filepath.Clean(title + "/" + title + ".1080p.mkv")
		return scanner.FileCandidate{
			Path:       filepath.Clean(`X:\Movies\` + rel),
			RelPath:    rel,
			Name:       title + ".1080p.mkv",
			Extension:  ".mkv",
			Size:       1024,
			ModifiedAt: time.Now().UTC(),
			Changed:    true,
		}
	}

	for i, title := range []string{"Spiderhead", "Spider Sequel"} {
		file := makeMovieFile(title)
		summary := scanner.Summary{
			Kind:         scanner.KindMovies,
			Root:         movieLib.Path,
			StartedAt:    time.Now().UTC(),
			CompletedAt:  time.Now().UTC().Add(time.Millisecond),
			DurationMS:   1,
			TotalFiles:   1,
			MediaFiles:   1,
			ChangedFiles: 1,
			Extensions:   map[string]int{".mkv": 1},
		}
		result := scanner.Result{Summary: summary, Files: []scanner.FileCandidate{file}, SeenRelPaths: []string{file.RelPath}}
		candidates := []movies.Candidate{{Title: title, Year: 2020 + i, Media: file}}
		if _, err := service.SaveMovieScan(ctx, movieLib, result, candidates); err != nil {
			t.Fatalf("seed movie %q: %v", title, err)
		}
	}

	movieList, err := service.ListMovies(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list seeded movies: %v", err)
	}
	for _, item := range movieList {
		if err := service.UpsertMetadataRecord(ctx, catalog.MetadataRecord{
			Kind:       "movie",
			ItemID:     item.ID,
			Provider:   "tmdb",
			Title:      item.Title,
			Year:       item.Year,
			Confidence: 0.9,
			Cast: []catalog.MetadataCredit{
				{Name: "Spider Lee", Character: "Hero", ProfileURL: "https://images.example/spider-lee.jpg"},
			},
			Collection: &catalog.MetadataCollection{
				ID:        "42",
				Name:      "Spider Saga",
				PosterURL: "https://images.example/spider-saga.jpg",
			},
		}); err != nil {
			t.Fatalf("upsert movie metadata for %q: %v", item.Title, err)
		}
	}

	// TV
	tvFile := scanner.FileCandidate{
		Path:       filepath.Clean(`X:\TV\Spider Forest\Season 01\Spider.Forest.S01E01.mkv`),
		RelPath:    filepath.Clean("Spider Forest/Season 01/Spider.Forest.S01E01.mkv"),
		Name:       "Spider.Forest.S01E01.mkv",
		Extension:  ".mkv",
		Size:       1024,
		ModifiedAt: time.Now().UTC(),
		Changed:    true,
	}
	tvSummary := scanner.Summary{
		Kind:         scanner.KindTV,
		Root:         tvLib.Path,
		StartedAt:    time.Now().UTC(),
		CompletedAt:  time.Now().UTC().Add(time.Millisecond),
		DurationMS:   1,
		TotalFiles:   1,
		MediaFiles:   1,
		ChangedFiles: 1,
		Extensions:   map[string]int{".mkv": 1},
	}
	tvResult := scanner.Result{Summary: tvSummary, Files: []scanner.FileCandidate{tvFile}, SeenRelPaths: []string{tvFile.RelPath}}
	tvCandidates := []tv.EpisodeCandidate{{
		SeriesTitle:   "Spider Forest",
		SeasonNumber:  1,
		EpisodeNumber: 1,
		EpisodeTitle:  "Pilot",
		Media:         tvFile,
	}}
	if _, err := service.SaveTVScan(ctx, tvLib, tvResult, tvCandidates); err != nil {
		t.Fatalf("seed tv: %v", err)
	}
	seriesList, err := service.ListSeries(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list seeded series: %v", err)
	}
	for _, item := range seriesList {
		if err := service.UpsertMetadataRecord(ctx, catalog.MetadataRecord{
			Kind:       "series",
			ItemID:     item.ID,
			Provider:   "tmdb",
			Title:      item.Title,
			Confidence: 0.9,
		}); err != nil {
			t.Fatalf("upsert series metadata: %v", err)
		}
	}
}

// seedManyMoviesForHandler creates n movies whose titles all begin with prefix
// so the handler returns a deterministic, scoreable set per limit assertion.
func seedManyMoviesForHandler(t *testing.T, service *catalog.Service, prefix string, n int) {
	t.Helper()
	ctx := context.Background()
	library := libraries.Library{ID: "movies-" + prefix, Name: "Movies-" + prefix, Path: `X:\Movies\` + prefix, Kind: libraries.KindMovies}
	for i := 0; i < n; i++ {
		title := prefix + "-" + padInt(i)
		rel := filepath.Clean(title + "/" + title + ".mkv")
		file := scanner.FileCandidate{
			Path:       filepath.Clean(library.Path + `\` + rel),
			RelPath:    rel,
			Name:       title + ".mkv",
			Extension:  ".mkv",
			Size:       1024,
			ModifiedAt: time.Now().UTC(),
			Changed:    true,
		}
		summary := scanner.Summary{
			Kind:         scanner.KindMovies,
			Root:         library.Path,
			StartedAt:    time.Now().UTC(),
			CompletedAt:  time.Now().UTC().Add(time.Millisecond),
			DurationMS:   1,
			TotalFiles:   1,
			MediaFiles:   1,
			ChangedFiles: 1,
			Extensions:   map[string]int{".mkv": 1},
		}
		result := scanner.Result{Summary: summary, Files: []scanner.FileCandidate{file}, SeenRelPaths: []string{file.RelPath}}
		candidates := []movies.Candidate{{Title: title, Year: 2000 + i, Media: file}}
		if _, err := service.SaveMovieScan(ctx, library, result, candidates); err != nil {
			t.Fatalf("seed movie %q: %v", title, err)
		}
	}
}

func padInt(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return "0" + string(digits[i])
	}
	if i < 100 {
		return string(digits[i/10]) + string(digits[i%10])
	}
	return string(digits[i/100]) + string(digits[(i/10)%10]) + string(digits[i%10])
}

func TestClientPlaybackStartHeartbeatAndStop(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Arrival (2016)", "Arrival.2016.1080p.mkv"))

	deps := testDeps(t, time.Now())
	router := NewRouter(deps)
	payload := postJSON(t, router, "/api/libraries/movies/scan", map[string]any{"path": root})
	waitForScan(t, router, payload["id"].(string))
	sources := getJSON(t, router, "/api/media-sources")
	sourceID := sources["mediaSources"].([]any)[0].(map[string]any)["id"].(string)
	// Probe required before playback — save a native Apple TV result so the
	// decision engine picks DirectPlay rather than blocking on remux.
	if err := deps.Catalog.SaveProbe(context.Background(), sourceID, catalog.ProbeResult{
		Container: "mp4", VideoCodec: "h264", Width: 1920, Height: 1080,
	}); err != nil {
		t.Fatalf("save probe: %v", err)
	}

	started := postJSON(t, router, "/api/client/playback/start", map[string]any{
		"mediaSourceId": sourceID,
		"deviceId":      "apple-tv-living-room",
		"clientProfile": "apple-tv",
		"routeType":     "lan",
	})
	if started["sessionId"] == "" || started["heartbeatUrl"] == "" || started["stopUrl"] == "" {
		t.Fatalf("expected client playback session payload, got %#v", started)
	}
	route := started["route"].(map[string]any)
	if route["route"] != "direct" || route["url"] == "" {
		t.Fatalf("expected direct route payload, got %#v", route)
	}
	sessionID := started["sessionId"].(string)

	heartbeat := requestJSON(t, router, http.MethodPatch, "/api/client/playback/"+sessionID, map[string]any{
		"progressSeconds": 48,
		"durationSeconds": 120,
		"status":          "playing",
	})
	session := heartbeat["session"].(map[string]any)
	if session["progressSeconds"] != float64(48) {
		t.Fatalf("expected heartbeat progress update, got %#v", session)
	}
	state := getJSON(t, router, "/api/playback/state/"+sourceID)
	if state["progressSeconds"] != float64(48) {
		t.Fatalf("expected playback state to follow heartbeat, got %#v", state)
	}

	stopped := postJSON(t, router, "/api/client/playback/"+sessionID+"/stop", map[string]any{
		"progressSeconds": 120,
		"durationSeconds": 120,
		"status":          "stopped",
	})
	stoppedSession := stopped["session"].(map[string]any)
	if stoppedSession["status"] != "stopped" {
		t.Fatalf("expected stopped session, got %#v", stoppedSession)
	}
	active := getJSON(t, router, "/api/sessions")
	if len(active["sessions"].([]any)) != 0 {
		t.Fatalf("expected no active sessions after stop, got %#v", active)
	}
}

func TestClientHomeIncludesUnknownDurationProgressInContinueWatching(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Arrival (2016)", "Arrival.2016.1080p.mkv"))

	deps := testDeps(t, time.Now())
	router := NewRouter(deps)
	payload := postJSON(t, router, "/api/libraries/movies/scan", map[string]any{"path": root})
	waitForScan(t, router, payload["id"].(string))
	sources := getJSON(t, router, "/api/media-sources")
	sourceID := sources["mediaSources"].([]any)[0].(map[string]any)["id"].(string)

	// Probe required before playback — save a native format so the decision
	// engine picks DirectPlay rather than blocking on remux/policy.
	if err := deps.Catalog.SaveProbe(context.Background(), sourceID, catalog.ProbeResult{
		Container: "mp4", VideoCodec: "h264", Width: 1920, Height: 1080,
	}); err != nil {
		t.Fatalf("save probe: %v", err)
	}

	started := postJSON(t, router, "/api/client/playback/start", map[string]any{
		"mediaSourceId": sourceID,
		"deviceId":      "apple-tv-living-room",
		"clientProfile": "apple-tv",
		"routeType":     "lan",
	})
	sessionID := started["sessionId"].(string)
	requestJSON(t, router, http.MethodPatch, "/api/client/playback/"+sessionID, map[string]any{
		"progressSeconds": 48,
		"durationSeconds": 0,
		"status":          "playing",
	})

	home := getJSON(t, router, "/api/client/home?clientProfile=apple-tv")
	rows := home["rows"].([]any)
	var continueRow map[string]any
	for _, row := range rows {
		candidate := row.(map[string]any)
		if candidate["id"] == "continue" {
			continueRow = candidate
			break
		}
	}
	if continueRow == nil {
		t.Fatalf("expected continue row, got %#v", rows)
	}
	items := continueRow["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected unknown-duration progress in continue row, got %#v", items)
	}
	item := items[0].(map[string]any)
	if item["mediaSourceId"] != sourceID {
		t.Fatalf("expected continue item for %q, got %#v", sourceID, item)
	}
	if item["subtitle"] != "Resume from 48s" {
		t.Fatalf("expected progress-based subtitle for unknown duration, got %#v", item)
	}
}

func TestClientPlaybackStartAuthenticatedClientCanStartAdaptiveRoute(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Arrival (2016)", "Arrival.2016.1080p.mkv"))

	deps := testDepsWithAuth(t, time.Now())
	deps.Config.PlaybackPolicy = "full"
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")

	scan := client.requestJSON(t, router, http.MethodPost, "/api/libraries/movies/scan", map[string]any{"path": root})
	if scan.status != http.StatusAccepted {
		t.Fatalf("expected authenticated scan start, got %d: %s", scan.status, scan.body)
	}
	waitForScanAs(t, router, scan.payload["id"].(string), client)
	sources := client.requestJSON(t, router, http.MethodGet, "/api/media-sources", nil)
	sourceID := sources.payload["mediaSources"].([]any)[0].(map[string]any)["id"].(string)
	if err := deps.Catalog.SaveProbe(context.Background(), sourceID, catalog.ProbeResult{
		Container:       "mkv",
		DurationSeconds: 120,
		Bitrate:         24_000_000,
		VideoCodec:      "h264",
		Width:           1920,
		Height:          1080,
		AudioStreams:    1,
		SubtitleStreams: 0,
		RawJSON:         "{}",
	}); err != nil {
		t.Fatalf("save probe: %v", err)
	}

	// Authenticated clients must always be able to start playback for
	// non-direct-play routes — the old guard that blocked these with 409 was wrong.
	// For a high-bitrate remote route, the server picks adaptive HLS.
	started := client.requestJSON(t, router, http.MethodPost, "/api/client/playback/start", map[string]any{
		"mediaSourceId":     sourceID,
		"deviceId":          "apple-tv-living-room",
		"clientProfile":     "apple-tv",
		"routeType":         "remote",
		"maxNetworkBitrate": 8_000_000,
	})
	if started.status != http.StatusOK {
		t.Fatalf("expected authenticated client to start playback, got %d: %s", started.status, started.body)
	}
	route, _ := started.payload["route"].(map[string]any)
	if route == nil {
		t.Fatalf("expected route in response, got %#v", started.payload)
	}
	manifestURL, _ := route["manifestUrl"].(string)
	if manifestURL == "" {
		t.Fatalf("expected adaptive manifestUrl in route, got %#v", route)
	}
}

func TestMigrationDryRunImportAndRollback(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Movies", "Heat (1995)", "Heat.1995.1080p.BluRay.mkv"))
	writeTestFile(t, filepath.Join(root, "TV", "The Bear", "Season 01", "The.Bear.S01E01.1080p.WEB-DL.mkv"))

	deps := testDepsWithAuth(t, time.Now())
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")

	movieScan := client.requestJSON(t, router, http.MethodPost, "/api/libraries/movies/scan", map[string]any{
		"path": filepath.Join(root, "Movies"),
	})
	if movieScan.status != http.StatusAccepted {
		t.Fatalf("movie scan start: %d %s", movieScan.status, movieScan.body)
	}
	waitForScanAs(t, router, movieScan.payload["id"].(string), client)
	tvScan := client.requestJSON(t, router, http.MethodPost, "/api/libraries/tv/scan", map[string]any{
		"path": filepath.Join(root, "TV"),
	})
	if tvScan.status != http.StatusAccepted {
		t.Fatalf("tv scan start: %d %s", tvScan.status, tvScan.body)
	}
	waitForScanAs(t, router, tvScan.payload["id"].(string), client)

	movies := client.requestJSON(t, router, http.MethodGet, "/api/movies", nil)
	series := client.requestJSON(t, router, http.MethodGet, "/api/series", nil)
	movieID := movies.payload["movies"].([]any)[0].(map[string]any)["id"].(string)
	seriesID := series.payload["series"].([]any)[0].(map[string]any)["id"].(string)
	if err := deps.Catalog.UpsertExternalID(context.Background(), catalog.ExternalID{Kind: "movie", ItemID: movieID, Provider: "tmdb", ExternalID: "949"}); err != nil {
		t.Fatalf("seed movie external id: %v", err)
	}
	if err := deps.Catalog.UpsertExternalID(context.Background(), catalog.ExternalID{Kind: "series", ItemID: seriesID, Provider: "tvdb", ExternalID: "411211"}); err != nil {
		t.Fatalf("seed series external id: %v", err)
	}

	fixture, err := os.ReadFile(filepath.Join("..", "migration", "testdata", "plex-watch-history.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	formats := client.requestJSON(t, router, http.MethodGet, "/api/migrations/formats", nil)
	if formats.status != http.StatusOK || len(formats.payload["formats"].([]any)) == 0 {
		t.Fatalf("expected migration formats, got %d %#v", formats.status, formats.payload)
	}

	dryRun := client.requestJSON(t, router, http.MethodPost, "/api/migrations/dry-run", map[string]any{
		"payload": string(fixture),
		"scopes":  []string{migration.ScopePlayback, migration.ScopeMetadata},
		"userId":  "local",
	})
	if dryRun.status != http.StatusOK {
		t.Fatalf("dry run failed: %d %s", dryRun.status, dryRun.body)
	}
	summary := dryRun.payload["summary"].(map[string]any)
	if summary["importable"] != float64(2) || summary["conflicted"] != float64(1) {
		t.Fatalf("expected dry-run conflict classification, got %#v", dryRun.payload)
	}

	imported := client.requestJSON(t, router, http.MethodPost, "/api/migrations/import", map[string]any{
		"payload":            string(fixture),
		"scopes":             []string{migration.ScopePlayback, migration.ScopeMetadata},
		"userId":             "local",
		"selectedImportKeys": []string{"plex-heat", "plex-bear-s1e1"},
	})
	if imported.status != http.StatusOK || imported.payload["runId"] == "" {
		t.Fatalf("expected successful migration import, got %d %#v", imported.status, imported.payload)
	}
	runID := imported.payload["runId"].(string)
	run := client.requestJSON(t, router, http.MethodGet, "/api/migrations/runs/"+runID, nil)
	if run.status != http.StatusOK || run.payload["status"] != "completed" {
		t.Fatalf("expected stored migration run detail, got %d %#v", run.status, run.payload)
	}

	runs := client.requestJSON(t, router, http.MethodGet, "/api/migrations/runs", nil)
	if runs.status != http.StatusOK || len(runs.payload["runs"].([]any)) == 0 {
		t.Fatalf("expected migration runs list, got %d %#v", runs.status, runs.payload)
	}

	rollback := client.requestJSON(t, router, http.MethodPost, "/api/migrations/runs/"+runID+"/rollback", map[string]any{})
	if rollback.status != http.StatusOK || rollback.payload["status"] != "rolled_back" {
		t.Fatalf("expected rollback report, got %d %#v", rollback.status, rollback.payload)
	}
	recent := client.requestJSON(t, router, http.MethodGet, "/api/playback/recent", nil).payload
	if len(recent["recent"].([]any)) != 0 {
		t.Fatalf("expected rollback to clear imported playback state, got %#v", recent)
	}
}

func TestPlaybackDecisionEndpointIsExplicitlyDeferred(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	payload := getJSON(t, router, "/api/playback/decision?clientProfile=android-tv")

	if payload["mode"] != string(playback.DecisionDeferred) {
		t.Fatalf("expected deferred decision mode, got %#v", payload["mode"])
	}
	if payload["clientProfile"] != "android-tv" {
		t.Fatalf("expected client profile to round-trip, got %#v", payload["clientProfile"])
	}
	if payload["reasonCode"] != "source_required" || payload["decisionTraceId"] == "" {
		t.Fatalf("expected v2 decision fields, got %#v", payload)
	}
}

func TestSubtitleConversionEndpointReportsOutputBehavior(t *testing.T) {
	root := t.TempDir()
	mediaPath := filepath.Join(root, "Arrival (2016)", "Arrival.2016.1080p.mkv")
	writeTestFile(t, mediaPath)
	writeTestFile(t, filepath.Join(root, "Arrival (2016)", "Arrival.2016.1080p.en.ass"))

	router := NewRouter(testDeps(t, time.Now()))
	payload := postJSON(t, router, "/api/libraries/movies/scan", map[string]any{
		"path":        root,
		"sampleLimit": 10,
	})
	waitForScan(t, router, payload["id"].(string))
	sources := getJSON(t, router, "/api/media-sources")
	sourceList := sources["mediaSources"].([]any)
	sourceID := sourceList[0].(map[string]any)["id"].(string)

	response := postJSON(t, router, "/api/media-sources/"+sourceID+"/subtitles/0/convert?clientProfile=web", map[string]any{})
	conversion := response["conversion"].(map[string]any)

	if conversion["status"] != "available" || conversion["outputFormat"] != "webvtt" {
		t.Fatalf("expected available WebVTT conversion, got %#v", response)
	}
	if conversion["outputBehavior"] == "" || conversion["serverImpact"] != "low" {
		t.Fatalf("expected clear conversion behavior, got %#v", conversion)
	}
}

func TestMovieScanEndpointUsesMovieClassifier(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Dune Part Two (2024)", "Dune.Part.Two.2024.2160p.REMUX.mkv"))
	writeTestFile(t, filepath.Join(root, "poster.jpg"))

	router := NewRouter(testDeps(t, time.Now()))
	payload := postJSON(t, router, "/api/libraries/movies/scan", map[string]any{
		"path":        root,
		"sampleLimit": 10,
	})
	job := waitForScan(t, router, payload["id"].(string))
	result := job["result"].(map[string]any)
	moviesResult := result["movies"].(map[string]any)

	if moviesResult["moviesFound"] != float64(1) {
		t.Fatalf("expected one movie, got %#v", moviesResult["moviesFound"])
	}
	persisted := moviesResult["persisted"].(map[string]any)
	if persisted["mediaSources"] != float64(1) {
		t.Fatalf("expected one persisted media source, got %#v", persisted["mediaSources"])
	}
}

func TestTVScanEndpointUsesEpisodeClassifier(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "The Bear", "Season 02", "The.Bear.S02E03.Sundae.1080p.WEB-DL.mkv"))
	writeTestFile(t, filepath.Join(root, "The Bear", "Season 02", "notes.txt"))

	router := NewRouter(testDeps(t, time.Now()))
	payload := postJSON(t, router, "/api/libraries/tv/scan", map[string]any{
		"path":        root,
		"sampleLimit": 10,
	})
	job := waitForScan(t, router, payload["id"].(string))
	result := job["result"].(map[string]any)
	tvResult := result["tv"].(map[string]any)

	if tvResult["episodesFound"] != float64(1) {
		t.Fatalf("expected one episode, got %#v", tvResult["episodesFound"])
	}
}

func TestScanEndpointsFallbackToSavedLibraries(t *testing.T) {
	movieRoot := t.TempDir()
	tvRoot := t.TempDir()
	writeTestFile(t, filepath.Join(movieRoot, "Interstellar (2014)", "Interstellar.2014.1080p.BluRay.mkv"))
	writeTestFile(t, filepath.Join(tvRoot, "Andor", "Season 01", "Andor.S01E01.1080p.WEB-DL.mkv"))

	deps := testDeps(t, time.Now())
	deps.Config.MovieLibraryPath = ""
	deps.Config.TVLibraryPath = ""
	router := NewRouter(deps)

	postJSON(t, router, "/api/libraries", map[string]any{
		"kind": "movies",
		"name": "Movies",
		"path": movieRoot,
	})
	postJSON(t, router, "/api/libraries", map[string]any{
		"kind": "tv",
		"name": "TV",
		"path": tvRoot,
	})

	movieScan := postJSON(t, router, "/api/libraries/movies/scan", map[string]any{})
	waitForScan(t, router, movieScan["id"].(string))
	tvScan := postJSON(t, router, "/api/libraries/tv/scan", map[string]any{})
	waitForScan(t, router, tvScan["id"].(string))
	scans := getJSON(t, router, "/api/scans")
	if len(scans["scans"].([]any)) < 2 {
		t.Fatalf("expected fallback scan jobs to be recorded, got %#v", scans)
	}
}

func TestCatalogSummaryUpdatesAfterScans(t *testing.T) {
	movieRoot := t.TempDir()
	tvRoot := t.TempDir()
	writeTestFile(t, filepath.Join(movieRoot, "Heat (1995)", "Heat.1995.1080p.BluRay.mkv"))
	writeTestFile(t, filepath.Join(tvRoot, "The Bear", "Season 02", "The.Bear.S02E03.Sundae.1080p.WEB-DL.mkv"))

	router := NewRouter(testDeps(t, time.Now()))
	payload := postJSON(t, router, "/api/libraries/scan", map[string]any{
		"moviesPath":  movieRoot,
		"tvPath":      tvRoot,
		"sampleLimit": 10,
	})
	waitForScan(t, router, payload["id"].(string))
	summary := getJSON(t, router, "/api/catalog/summary")

	if summary["libraries"] != float64(2) {
		t.Fatalf("expected two libraries, got %#v", summary["libraries"])
	}
	if summary["mediaSources"] != float64(2) {
		t.Fatalf("expected two media sources, got %#v", summary["mediaSources"])
	}
	if summary["movies"] != float64(1) {
		t.Fatalf("expected one movie, got %#v", summary["movies"])
	}
	if summary["episodes"] != float64(1) {
		t.Fatalf("expected one episode, got %#v", summary["episodes"])
	}

	movies := getJSON(t, router, "/api/movies")
	movieList := movies["movies"].([]any)
	if len(movieList) != 1 {
		t.Fatalf("expected one movie in browse API, got %#v", movieList)
	}
	movieID := movieList[0].(map[string]any)["id"].(string)
	movieDetail := getJSON(t, router, "/api/movies/"+movieID)
	if movieDetail["title"] != "Heat" {
		t.Fatalf("expected Heat movie detail, got %#v", movieDetail["title"])
	}
	metadata := getJSON(t, router, "/api/metadata/movie/"+movieID)
	records := metadata["records"].([]any)
	if len(records) == 0 {
		t.Fatalf("expected filename metadata record, got %#v", metadata)
	}
	best := metadata["best"].(map[string]any)
	foundFilename := false
	for _, raw := range records {
		record := raw.(map[string]any)
		if record["provider"] == "filename" {
			foundFilename = true
			break
		}
	}
	if !foundFilename {
		t.Fatalf("expected filename seed metadata to remain available, got %#v", metadata)
	}
	if best["title"] != "Heat" {
		t.Fatalf("expected clean selected title, got %#v", best["title"])
	}
	provenance := best["provenance"].(map[string]any)
	if provenance["fields"].(map[string]any)["title"] == nil {
		t.Fatalf("expected filename title provenance, got %#v", provenance)
	}
	match := requestJSON(t, router, http.MethodPut, "/api/metadata/match", map[string]any{
		"kind":     "movie",
		"id":       movieID,
		"title":    "Heat",
		"year":     1995,
		"provider": "manual",
		"overview": "A professional thief weighs one last score.",
	})
	if len(match["records"].([]any)) < 2 {
		t.Fatalf("expected manual and filename metadata records, got %#v", match)
	}
	foundManual := false
	foundFilename = false
	for _, raw := range match["records"].([]any) {
		record := raw.(map[string]any)
		if record["provider"] == "manual" {
			foundManual = true
		}
		if record["provider"] == "filename" {
			foundFilename = true
		}
	}
	if !foundManual || !foundFilename {
		t.Fatalf("expected manual and filename metadata records, got %#v", match)
	}

	series := getJSON(t, router, "/api/series")
	seriesList := series["series"].([]any)
	if len(seriesList) != 1 {
		t.Fatalf("expected one series in browse API, got %#v", seriesList)
	}
	seriesID := seriesList[0].(map[string]any)["id"].(string)
	seriesDetail := getJSON(t, router, "/api/series/"+seriesID)
	if seriesDetail["title"] != "The Bear" {
		t.Fatalf("expected The Bear series detail, got %#v", seriesDetail["title"])
	}

	sources := getJSON(t, router, "/api/media-sources?unprobed=true")
	sourceList := sources["mediaSources"].([]any)
	if len(sourceList) != 2 {
		t.Fatalf("expected two unprobed media sources, got %#v", sourceList)
	}
	sourceID := sourceList[0].(map[string]any)["id"].(string)
	sourceDetail := getJSON(t, router, "/api/media-sources/"+sourceID)
	if sourceDetail["id"] != sourceID {
		t.Fatalf("expected media source detail, got %#v", sourceDetail)
	}
	streamRequest := httptest.NewRequest(http.MethodGet, "/api/media-sources/"+sourceID+"/stream", nil)
	streamResponse := httptest.NewRecorder()
	router.ServeHTTP(streamResponse, streamRequest)
	if streamResponse.Code != http.StatusOK {
		t.Fatalf("expected stream 200, got %d: %s", streamResponse.Code, streamResponse.Body.String())
	}
	decision := getJSON(t, router, "/api/playback/decision?mediaSourceId="+sourceID+"&clientProfile=web")
	if decision["mode"] != string(playback.DecisionDeferred) {
		t.Fatalf("expected unprobed source to defer playback decision, got %#v", decision["mode"])
	}
	if decision["reasonCode"] != "probe_required" || decision["decisionTraceId"] == "" {
		t.Fatalf("expected v2 probe decision fields, got %#v", decision)
	}
}

func TestMetadataProvidersEndpointUsesStrictManagedModeHealth(t *testing.T) {
	t.Setenv("XUVA_MANAGED_TMDB_API_KEY", "managed-tmdb-key")
	deps := testDeps(t, time.Now())
	router := NewRouter(deps)

	payload := getJSON(t, router, "/api/metadata/providers")
	if payload["managedMode"] != "strict" {
		t.Fatalf("expected strict managed mode, got %#v", payload["managedMode"])
	}
	items, _ := payload["providers"].([]any)
	if len(items) == 0 {
		t.Fatalf("expected metadata providers payload, got %#v", payload)
	}

	lookup := map[string]map[string]any{}
	for _, raw := range items {
		row, _ := raw.(map[string]any)
		id, _ := row["id"].(string)
		if id != "" {
			lookup[id] = row
		}
	}

	tmdb := lookup["tmdb"]
	if tmdb == nil {
		t.Fatalf("expected tmdb provider row, got %#v", payload)
	}
	if tmdb["managedMode"] != "strict" || tmdb["configured"] != true || tmdb["healthy"] != true {
		t.Fatalf("expected tmdb to be configured and healthy from managed credentials, got %#v", tmdb)
	}

	tvdb := lookup["tvdb"]
	if tvdb == nil {
		t.Fatalf("expected tvdb provider row, got %#v", payload)
	}
	if tvdb["configured"] != false || tvdb["healthy"] != false {
		t.Fatalf("expected tvdb to remain unconfigured in this test, got %#v", tvdb)
	}
}

func TestMetadataBackfillStartReturnsAcceptedWhenProviderNotConfigured(t *testing.T) {
	origDefault := config.DefaultTMDBAPIKey
	config.DefaultTMDBAPIKey = ""
	t.Cleanup(func() {
		config.DefaultTMDBAPIKey = origDefault
	})
	t.Setenv("XUVA_MANAGED_TMDB_API_KEY", "")
	t.Setenv("XUVA_TMDB_API_KEY", "")

	router := NewRouter(testDeps(t, time.Now()))
	payload := requestJSON(t, router, http.MethodPost, "/api/metadata/backfill", map[string]any{
		"provider": "tmdb",
	})

	if payload["accepted"] != false {
		t.Fatalf("expected accepted=false when tmdb key missing, got %#v", payload)
	}
	if payload["reason"] != "not_configured" {
		t.Fatalf("expected reason=not_configured, got %#v", payload)
	}
	status, _ := payload["status"].(map[string]any)
	if status == nil || status["running"] != false {
		t.Fatalf("expected idle backfill status, got %#v", payload)
	}
}

// TestPlayerPageIncludesDirectStreamRecoveryFlow used to assert that the
// legacy hardcoded HTML player (playerHandler) included specific JS fragments
// for stream-recovery flow. That handler has been deleted — /play/{id} now
// falls through to the SPA root which loads the SvelteKit Player.svelte
// component, so this test is no longer meaningful. The recovery flow lives
// in apps/web/svelte/src/lib/components/player/Player.svelte and is covered
// by frontend tests.

func TestPlaybackStateAndSessions(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Heat (1995)", "Heat.1995.1080p.BluRay.mkv"))

	router := NewRouter(testDeps(t, time.Now()))
	payload := postJSON(t, router, "/api/libraries/movies/scan", map[string]any{"path": root})
	waitForScan(t, router, payload["id"].(string))
	sources := getJSON(t, router, "/api/media-sources")
	sourceID := sources["mediaSources"].([]any)[0].(map[string]any)["id"].(string)

	state := requestJSON(t, router, http.MethodPut, "/api/playback/state/"+sourceID, map[string]any{
		"progressSeconds": 50,
		"durationSeconds": 100,
	})
	if state["watched"] != false {
		t.Fatalf("expected 50 percent progress to not be marked watched, got %#v", state)
	}
	recent := getJSON(t, router, "/api/playback/recent")
	if len(recent["recent"].([]any)) != 1 {
		t.Fatalf("expected recent playback item, got %#v", recent)
	}

	session := postJSON(t, router, "/api/sessions", map[string]any{"mediaSourceId": sourceID, "deviceId": "web"})
	sessionID := session["id"].(string)
	updated := requestJSON(t, router, http.MethodPatch, "/api/sessions/"+sessionID, map[string]any{
		"progressSeconds": 12,
		"durationSeconds": 100,
	})
	if updated["progressSeconds"] != float64(12) {
		t.Fatalf("expected session progress update, got %#v", updated)
	}
	active := getJSON(t, router, "/api/sessions")
	if len(active["sessions"].([]any)) != 1 {
		t.Fatalf("expected one active session, got %#v", active)
	}

	download := postJSON(t, router, "/api/downloads", map[string]any{"mediaSourceId": sourceID, "targetProfile": "original"})
	if download["status"] != string(downloads.StatusCompleted) {
		t.Fatalf("expected original download to complete immediately, got %#v", download)
	}
	downloadsPayload := getJSON(t, router, "/api/downloads")
	if len(downloadsPayload["downloads"].([]any)) != 1 {
		t.Fatalf("expected one download job, got %#v", downloadsPayload)
	}
}

func TestSessionInspectorEndpointExposesPlaybackFields(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	started := postJSON(t, router, "/api/sessions", map[string]any{
		"mediaSourceId":  "source-1",
		"deviceId":       "web",
		"clientProfile":  "web",
		"route":          "direct",
		"mode":           "Direct Play",
		"reasonCode":     "direct_play_supported",
		"reasonText":     "Direct playback is available.",
		"serverImpact":   "Low impact",
		"selectedTracks": map[string]string{"audio": "Default", "subtitles": "Off"},
	})
	id := started["id"].(string)
	updated := requestJSON(t, router, http.MethodPatch, "/api/sessions/"+id, map[string]any{
		"route":          "transcode",
		"mode":           "Video Transcode",
		"reasonCode":     "video_conversion_required",
		"reasonText":     "Video conversion required.",
		"serverImpact":   "High server load",
		"selectedTracks": map[string]string{"audio": "Default", "subtitles": "English PGS"},
	})
	if updated["route"] != "transcode" {
		t.Fatalf("expected route update, got %#v", updated)
	}

	inspector := getJSON(t, router, "/api/sessions/"+id+"/inspector")
	if inspector["route"] != "transcode" || inspector["reasonCode"] != "video_conversion_required" {
		t.Fatalf("expected inspector route/reason, got %#v", inspector)
	}
	tracks := inspector["selectedTracks"].(map[string]any)
	if tracks["subtitles"] != "English PGS" {
		t.Fatalf("expected selected subtitle track, got %#v", tracks)
	}
	history := inspector["routeHistory"].([]any)
	if len(history) != 1 {
		t.Fatalf("expected route history, got %#v", inspector)
	}
}

func TestMetadataMatchResolvesReview(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Unclear.File.Name.mkv"))

	router := NewRouter(testDeps(t, time.Now()))
	payload := postJSON(t, router, "/api/libraries/movies/scan", map[string]any{"path": root})
	waitForScan(t, router, payload["id"].(string))
	review := getJSON(t, router, "/api/review")
	items := review["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("expected review item")
	}
	item := items[0].(map[string]any)
	requestJSON(t, router, http.MethodPut, "/api/metadata/match", map[string]any{
		"kind":  item["kind"],
		"id":    item["id"],
		"title": "Fixed Title",
		"year":  2026,
	})

	review = getJSON(t, router, "/api/review")
	if len(review["items"].([]any)) != 0 {
		t.Fatalf("expected review queue to be resolved, got %#v", review)
	}
}

func TestLibraryManagementRemoteAndArtworkEndpoints(t *testing.T) {
	root := t.TempDir()
	router := NewRouter(testDeps(t, time.Now()))

	library := postJSON(t, router, "/api/libraries", map[string]any{
		"kind":            "movies",
		"name":            "Archive Movies",
		"path":            root,
		"metadataSources": []string{"wikipedia", "nfo", "filename"},
	})
	if library["name"] != "Archive Movies" {
		t.Fatalf("expected saved library, got %#v", library)
	}
	if got := library["metadataSources"].([]any); len(got) != 3 || got[0] != "wikipedia" || got[1] != "nfo" || got[2] != "filename" {
		t.Fatalf("expected saved metadata source order, got %#v", library)
	}
	libraryID := library["id"].(string)
	librariesPayload := getJSON(t, router, "/api/libraries")
	if len(librariesPayload["libraries"].([]any)) != 1 {
		t.Fatalf("expected one saved library, got %#v", librariesPayload)
	}
	if librariesPayload["metadataSources"] == nil {
		t.Fatalf("expected metadata source catalog, got %#v", librariesPayload)
	}
	scan := postJSON(t, router, "/api/libraries/"+libraryID+"/scan", map[string]any{})
	waitForScan(t, router, scan["id"].(string))

	remote := getJSON(t, router, "/api/remote/access")
	if remote["wanLookup"] != "available_on_request" {
		t.Fatalf("expected explicit wan lookup flag, got %#v", remote)
	}
	if remote["diagnostics"] != "available" {
		t.Fatalf("expected diagnostics to be advertised, got %#v", remote)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/artwork/movie/example", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("expected svg artwork response, got %d %q", response.Code, response.Header().Get("Content-Type"))
	}
}

func TestArtworkQualityGateThresholds(t *testing.T) {
	posterHiRes := testJPEG(t, 900, 1350)
	posterLowRes := testJPEG(t, 400, 600)
	backdropHiRes := testJPEG(t, 1920, 1080)
	backdropLowRes := testJPEG(t, 960, 540)

	if !artworkPassesQualityGateBytes(posterHiRes, "poster", "https://example.com/poster.jpg", true) {
		t.Fatalf("expected hi-res poster to pass quality gate")
	}
	if artworkPassesQualityGateBytes(posterLowRes, "poster", "https://example.com/poster.jpg", true) {
		t.Fatalf("expected low-res poster to fail quality gate")
	}
	if !artworkPassesQualityGateBytes(backdropHiRes, "backdrop", "https://example.com/backdrop.jpg", true) {
		t.Fatalf("expected hi-res backdrop to pass quality gate")
	}
	if artworkPassesQualityGateBytes(backdropLowRes, "backdrop", "https://example.com/backdrop.jpg", true) {
		t.Fatalf("expected low-res backdrop to fail quality gate")
	}
	if !artworkPassesQualityGateBytes(posterLowRes, "poster", "https://example.com/poster.jpg", false) {
		t.Fatalf("expected low-res poster to pass relaxed quality gate fallback")
	}
	if !artworkPassesQualityGateBytes(backdropLowRes, "backdrop", "https://example.com/backdrop.jpg", false) {
		t.Fatalf("expected low-res backdrop to pass relaxed quality gate fallback")
	}
}

func TestMetadataArtworkCandidatesBackdropFallsBackToPoster(t *testing.T) {
	records := []catalog.MetadataRecord{
		{Provider: "tvmaze", PosterURL: "https://img.example/poster-a.jpg"},
		{Provider: "tmdb", BackdropURL: "https://img.example/backdrop-b.jpg", PosterURL: "https://img.example/poster-b.jpg"},
	}
	candidates := metadataArtworkCandidates(records, "backdrop")
	if len(candidates) < 2 {
		t.Fatalf("expected backdrop candidates with poster fallback, got %#v", candidates)
	}
	if candidates[0] != "https://img.example/backdrop-b.jpg" {
		t.Fatalf("expected backdrop candidate first, got %#v", candidates)
	}
	if candidates[1] != "https://img.example/poster-a.jpg" {
		t.Fatalf("expected poster fallback after backdrop, got %#v", candidates)
	}
}

func testJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	fill := color.RGBA{R: 42, G: 92, B: 146, A: 255}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			canvas.SetRGBA(x, y, fill)
		}
	}
	var output bytes.Buffer
	if err := jpeg.Encode(&output, canvas, &jpeg.Options{Quality: 88}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return output.Bytes()
}

func TestRemoteDiagnosticsEndpointRejectsSensitiveURLParts(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))
	result := postJSON(t, router, "/api/remote/diagnostics", map[string]any{
		"publicUrl": "https://media.example.com/watch?token=secret",
	})
	if result["failureClass"] != "invalid_input" {
		t.Fatalf("expected invalid input without leaking url details, got %#v", result)
	}
	target := result["target"].(map[string]any)
	if len(target) != 0 {
		t.Fatalf("expected empty sanitized target for invalid input, got %#v", target)
	}
}

func TestAdaptiveStreamingPlanManifestAndTelemetry(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Big Movie (2026)", "Big.Movie.2026.2160p.mkv"))

	deps := testDeps(t, time.Now())
	deps.Config.PlaybackPolicy = "full"
	router := NewRouter(deps)
	payload := postJSON(t, router, "/api/libraries/movies/scan", map[string]any{"path": root})
	waitForScan(t, router, payload["id"].(string))
	sources := getJSON(t, router, "/api/media-sources")
	sourceID := sources["mediaSources"].([]any)[0].(map[string]any)["id"].(string)
	if err := deps.Catalog.SaveProbe(context.Background(), sourceID, catalog.ProbeResult{
		Container:       "matroska",
		DurationSeconds: 7200,
		Bitrate:         61_000_000,
		VideoCodec:      "hevc",
		Width:           3840,
		Height:          2160,
		AudioStreams:    1,
		RawJSON:         `{"streams":[]}`,
	}); err != nil {
		t.Fatalf("save probe: %v", err)
	}

	decision := getJSON(t, router, "/api/playback/decision?mediaSourceId="+sourceID+"&clientProfile=web&routeType=remote&maxNetworkBitrate=10000000")
	if decision["mode"] != string(playback.AdaptiveStream) {
		t.Fatalf("expected adaptive decision, got %#v", decision)
	}
	route := getJSON(t, router, "/api/playback/route?mediaSourceId="+sourceID+"&clientProfile=web&routeType=remote&maxNetworkBitrate=10000000")
	if route["route"] != "adaptive" || route["manifestUrl"] == "" {
		t.Fatalf("expected adaptive route, got %#v", route)
	}
	session := postJSON(t, router, "/api/media-sources/"+sourceID+"/adaptive/session", map[string]any{
		"clientProfile":     "web",
		"routeType":         "remote",
		"maxNetworkBitrate": 10_000_000,
	})
	plan := session["plan"].(map[string]any)
	if plan["enabled"] != true {
		t.Fatalf("expected enabled adaptive plan, got %#v", session)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/media-sources/"+sourceID+"/adaptive/master.m3u8?clientProfile=web&routeType=remote&maxNetworkBitrate=10000000", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "#EXT-X-STREAM-INF") {
		t.Fatalf("expected hls master playlist, got %d: %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/media-sources/"+sourceID+"/adaptive/variant-720p.m3u8?clientProfile=web&routeType=remote&maxNetworkBitrate=10000000", nil)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "#EXT-X-TARGETDURATION") {
		t.Fatalf("expected hls variant playlist, got %d: %s", response.Code, response.Body.String())
	}

	telemetry := requestJSON(t, router, http.MethodPost, "/api/adaptive/telemetry", map[string]any{
		"sessionId":       "sess_1",
		"mediaSourceId":   sourceID,
		"clientProfile":   "web",
		"event":           "stall",
		"variantId":       "720p",
		"bufferSeconds":   0.4,
		"stallMs":         1200,
		"observedBitrate": 3_800_000,
	})
	if telemetry["event"] != "adaptive.stall" || telemetry["correlationId"] == "" {
		t.Fatalf("expected correlated adaptive telemetry, got %#v", telemetry)
	}
}

func TestPlaybackRouteForcePlayableBypassesPolicyBlock(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Force Play Test (2026)", "Force.Play.Test.2026.Remux.2160p.mkv"))

	deps := testDeps(t, time.Now())
	deps.Config.PlaybackPolicy = "original_only"
	router := NewRouter(deps)
	payload := postJSON(t, router, "/api/libraries/movies/scan", map[string]any{"path": root})
	waitForScan(t, router, payload["id"].(string))
	sources := getJSON(t, router, "/api/media-sources")
	sourceID := sources["mediaSources"].([]any)[0].(map[string]any)["id"].(string)
	if err := deps.Catalog.SaveProbe(context.Background(), sourceID, catalog.ProbeResult{
		Container:       "matroska",
		DurationSeconds: 5400,
		Bitrate:         54_000_000,
		VideoCodec:      "hevc",
		Width:           3840,
		Height:          2160,
		AudioStreams:    1,
		RawJSON:         `{"streams":[]}`,
	}); err != nil {
		t.Fatalf("save probe: %v", err)
	}

	blocked := getJSON(t, router, "/api/playback/route?mediaSourceId="+sourceID+"&clientProfile=web&routeType=lan&supportsAdaptive=true")
	if blocked["status"] != "blocked_by_policy" {
		t.Fatalf("expected blocked_by_policy without forcePlayable, got %#v", blocked)
	}

	forced := getJSON(t, router, "/api/playback/route?mediaSourceId="+sourceID+"&clientProfile=web&routeType=lan&supportsAdaptive=true&forcePlayable=true")
	if forced["status"] == "blocked_by_policy" {
		t.Fatalf("expected forcePlayable to bypass policy block, got %#v", forced)
	}
}

func TestTrackByIndexPrefersDefaultTrackWhenRequestedIndexMissing(t *testing.T) {
	tracks := []probe.Track{
		{Index: 1, Codec: "aac", Default: false},
		{Index: 4, Codec: "ac3", Default: true},
	}
	selected := trackByIndex(tracks, 99)
	if selected.Index != 4 {
		t.Fatalf("expected default track index 4, got %#v", selected)
	}
}

func TestAuthProtectedRouteRequiresSession(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	request := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", response.Code, response.Body.String())
	}
}

func TestProtectedSettingsRequireAuthByDefault(t *testing.T) {
	router := NewRouter(testDepsWithAuthNoBootstrap(t, time.Now()))
	request := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:8097/api/settings", bytes.NewReader(mustJSON(t, map[string]any{
		"serverName": "Family Room",
	})))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for protected settings update without auth, got %d: %s", response.Code, response.Body.String())
	}
}

func TestAuthLoginAndProtectedRouteAccess(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)

	login := client.requestJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "admin",
		"password": "test-password-123!",
	})
	if login.status != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", login.status, login.body)
	}
	if login.payload["user"].(map[string]any)["username"] != "admin" {
		t.Fatalf("expected admin user, got %#v", login.payload)
	}
	if client.csrfToken == "" {
		t.Fatalf("expected csrf token to be captured")
	}

	protected := client.requestJSON(t, router, http.MethodGet, "/api/sessions", nil)
	if protected.status != http.StatusOK {
		t.Fatalf("expected protected route 200, got %d: %s", protected.status, protected.body)
	}

	session := client.requestJSON(t, router, http.MethodGet, "/api/auth/session", nil)
	if session.status != http.StatusOK {
		t.Fatalf("expected session route 200, got %d: %s", session.status, session.body)
	}
	if session.payload["csrfToken"] == "" {
		t.Fatalf("expected csrf token in session payload, got %#v", session.payload)
	}
}

func TestAuthSwitchProfileIssuesProfileToken(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")

	profiles := client.requestJSON(t, router, http.MethodGet, "/api/profiles", nil)
	if profiles.status != http.StatusOK {
		t.Fatalf("expected profiles 200, got %d: %s", profiles.status, profiles.body)
	}
	profileList, ok := profiles.payload["profiles"].([]any)
	if !ok || len(profileList) == 0 {
		t.Fatalf("expected at least one profile, got %#v", profiles.payload)
	}
	adminProfile, ok := profileList[0].(map[string]any)
	if !ok || adminProfile["id"] == "" {
		t.Fatalf("expected profile id, got %#v", profileList[0])
	}

	switched := client.requestJSON(t, router, http.MethodPost, "/api/auth/switch-profile", map[string]any{
		"profileUserId": adminProfile["id"],
	})
	if switched.status != http.StatusOK {
		t.Fatalf("expected switch-profile 200, got %d: %s", switched.status, switched.body)
	}
	if switched.payload["profileToken"] == "" {
		t.Fatalf("expected profile token, got %#v", switched.payload)
	}
}

func TestAuthLoginLoopbackWithForwardedHTTPSDoesNotSetSecureCookie(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	body := mustJSON(t, map[string]any{
		"username": "admin",
		"password": "test-password-123!",
	})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8097/api/auth/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "127.0.0.1:8097")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", response.Code, response.Body.String())
	}
	for _, cookie := range response.Result().Cookies() {
		if (cookie.Name == auth.SessionCookieName || cookie.Name == auth.CSRFCookieName) && cookie.Secure {
			t.Fatalf("expected non-secure auth cookies on loopback host, got secure cookie %q", cookie.Name)
		}
	}
}

func TestAuthLoginForwardedHTTPSOnNonLoopbackDoesNotSetSecureCookieWithoutTLS(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	body := mustJSON(t, map[string]any{
		"username": "admin",
		"password": "test-password-123!",
	})
	request := httptest.NewRequest(http.MethodPost, "http://xuva.test/api/auth/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "xuva.example")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", response.Code, response.Body.String())
	}
	for _, cookie := range response.Result().Cookies() {
		if (cookie.Name == auth.SessionCookieName || cookie.Name == auth.CSRFCookieName) && cookie.Secure {
			t.Fatalf("expected non-secure auth cookies without TLS transport, got secure cookie %q", cookie.Name)
		}
	}
}

func TestAuthLoginHTTPSRequestSetsSecureCookie(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	body := mustJSON(t, map[string]any{
		"username": "admin",
		"password": "test-password-123!",
	})
	request := httptest.NewRequest(http.MethodPost, "https://xuva.test/api/auth/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", response.Code, response.Body.String())
	}
	secureCookies := 0
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == auth.SessionCookieName || cookie.Name == auth.CSRFCookieName {
			if cookie.Secure {
				secureCookies++
			}
		}
	}
	if secureCookies < 2 {
		t.Fatalf("expected secure auth cookies on TLS request, got %d secure cookies", secureCookies)
	}
}

func TestAuthRevokedSessionDenied(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)

	login := client.requestJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "admin",
		"password": "test-password-123!",
	})
	if login.status != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", login.status, login.body)
	}
	logout := client.requestJSON(t, router, http.MethodPost, "/api/auth/logout", map[string]any{})
	if logout.status != http.StatusOK {
		t.Fatalf("expected logout 200, got %d: %s", logout.status, logout.body)
	}

	protected := client.requestJSON(t, router, http.MethodGet, "/api/sessions", nil)
	if protected.status != http.StatusUnauthorized {
		t.Fatalf("expected revoked session to get 401, got %d: %s", protected.status, protected.body)
	}
}

func TestAuthInvalidLoginsTriggerLockout(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)

	for attempt := 0; attempt < 4; attempt++ {
		response := client.requestJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
			"username": "admin",
			"password": "wrong-password",
		})
		if response.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d expected 401, got %d: %s", attempt+1, response.status, response.body)
		}
	}

	locked := client.requestJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "admin",
		"password": "wrong-password",
	})
	if locked.status != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on lockout, got %d: %s", locked.status, locked.body)
	}
	if locked.payload["lockedUntil"] == "" {
		t.Fatalf("expected lockedUntil in response, got %#v", locked.payload)
	}
}

func TestAuthMutationRejectsMissingCSRF(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)

	login := client.requestJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "admin",
		"password": "test-password-123!",
	})
	if login.status != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", login.status, login.body)
	}

	request := httptest.NewRequest(http.MethodPut, "http://xuva.test/api/settings", bytes.NewReader(mustJSON(t, map[string]any{
		"httpAddr": "127.0.0.1:8097",
	})))
	request.Header.Set("Content-Type", "application/json")
	client.apply(request, false)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without csrf header, got %d: %s", response.Code, response.Body.String())
	}
}

func TestMigrationMutationRejectsMissingCSRF(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)

	login := client.requestJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "admin",
		"password": "test-password-123!",
	})
	if login.status != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", login.status, login.body)
	}

	request := httptest.NewRequest(http.MethodPost, "http://xuva.test/api/migrations/dry-run", bytes.NewReader(mustJSON(t, map[string]any{
		"payload": `{"schema":"xuva.migration.v1","source":"generic","items":[{"id":"item-001","kind":"movie","title":"Test"}]}`,
	})))
	request.Header.Set("Content-Type", "application/json")
	client.apply(request, false)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("expected migration dry-run without csrf to return 403, got %d: %s", response.Code, response.Body.String())
	}
}

func TestMigrationRoutesRejectStandardUser(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	adminClient := newAuthTestClient(t)
	loginAs(t, adminClient, router, "admin", "test-password-123!")

	createUser := adminClient.requestJSON(t, router, http.MethodPost, "/api/users", map[string]any{
		"username":    "standard-user",
		"displayName": "Standard User",
		"password":    "test-password-123!",
		"role":        "standard",
	})
	if createUser.status != http.StatusCreated {
		t.Fatalf("expected standard user creation 201, got %d: %s", createUser.status, createUser.body)
	}

	standardClient := newAuthTestClient(t)
	loginAs(t, standardClient, router, "standard-user", "test-password-123!")

	formats := standardClient.requestJSON(t, router, http.MethodGet, "/api/migrations/formats", nil)
	if formats.status != http.StatusForbidden {
		t.Fatalf("expected standard user migration formats 403, got %d: %s", formats.status, formats.body)
	}
}

func TestAuthHeaderTokenAllowsProtectedRouteWithoutCookies(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)

	login := client.requestJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "admin",
		"password": "test-password-123!",
	})
	if login.status != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", login.status, login.body)
	}
	token, _ := login.payload["sessionToken"].(string)
	if strings.TrimSpace(token) == "" {
		t.Fatalf("expected sessionToken in login payload, got %#v", login.payload)
	}

	request := httptest.NewRequest(http.MethodGet, "http://xuva.test/api/users", nil)
	request.Header.Set("X-Auth-Token", token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected header-token users list 200, got %d: %s", response.Code, response.Body.String())
	}
}

func TestAuthHeaderTokenMutationBypassesCSRFRequirement(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)

	login := client.requestJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "admin",
		"password": "test-password-123!",
	})
	if login.status != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", login.status, login.body)
	}
	token, _ := login.payload["sessionToken"].(string)
	if strings.TrimSpace(token) == "" {
		t.Fatalf("expected sessionToken in login payload, got %#v", login.payload)
	}

	request := httptest.NewRequest(http.MethodPost, "http://xuva.test/api/users", bytes.NewReader(mustJSON(t, map[string]any{
		"username":    "viewer-hdr",
		"displayName": "Viewer Header",
		"password":    "viewer-password-123!",
		"role":        "standard",
	})))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Token", token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("expected header-token create user 201, got %d: %s", response.Code, response.Body.String())
	}
}

func TestAuthHeaderTokenOverridesStaleCookie(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)

	login := client.requestJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "admin",
		"password": "test-password-123!",
	})
	if login.status != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", login.status, login.body)
	}
	token, _ := login.payload["sessionToken"].(string)
	if strings.TrimSpace(token) == "" {
		t.Fatalf("expected sessionToken in login payload, got %#v", login.payload)
	}

	req := httptest.NewRequest(http.MethodGet, "http://xuva.test/api/users", nil)
	req.Header.Set("X-Auth-Token", token)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "stale.invalid.token"})
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected header token to authenticate even with stale cookie, got %d: %s", res.Code, res.Body.String())
	}
}

func TestAuthzStandardCannotCallAdminSettingsRoutes(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	if _, err := deps.Auth.CreateUser(context.Background(), "viewer", "viewer-password-123!", "Viewer", "standard"); err != nil {
		t.Fatalf("create standard user: %v", err)
	}
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "viewer", "viewer-password-123!")

	response := client.requestJSON(t, router, http.MethodPut, "/api/settings", map[string]any{
		"httpAddr": "127.0.0.1:8097",
	})

	if response.status != http.StatusForbidden {
		t.Fatalf("expected standard user to get 403 on settings, got %d: %s", response.status, response.body)
	}

	browse := client.requestJSON(t, router, http.MethodGet, "/api/settings/folders/browse?path="+url.QueryEscape(t.TempDir()), nil)
	if browse.status != http.StatusForbidden {
		t.Fatalf("expected standard user to get 403 on folder browse, got %d: %s", browse.status, browse.body)
	}
}

func TestAuthzAdminCanCallAdminSettingsRoutes(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")

	response := client.requestJSON(t, router, http.MethodPut, "/api/settings", map[string]any{
		"httpAddr": "127.0.0.1:8097",
	})

	if response.status != http.StatusOK {
		t.Fatalf("expected admin settings update 200, got %d: %s", response.status, response.body)
	}
}

func TestAuthzAdminCanManageUsers(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")

	list := client.requestJSON(t, router, http.MethodGet, "/api/users", nil)
	if list.status != http.StatusOK {
		t.Fatalf("expected admin users list 200, got %d: %s", list.status, list.body)
	}
	if len(list.payload["users"].([]any)) == 0 {
		t.Fatalf("expected at least one user in list, got %#v", list.payload)
	}

	created := client.requestJSON(t, router, http.MethodPost, "/api/users", map[string]any{
		"username":    "viewer",
		"displayName": "Viewer",
		"password":    "viewer-password-123!",
		"role":        "standard",
	})
	if created.status != http.StatusCreated {
		t.Fatalf("expected create user 201, got %d: %s", created.status, created.body)
	}
	user := created.payload["user"].(map[string]any)
	userID, _ := user["id"].(string)
	if userID == "" {
		t.Fatalf("expected created user id, got %#v", created.payload)
	}

	password := client.requestJSON(t, router, http.MethodPost, "/api/users/"+userID+"/password", map[string]any{
		"password": "viewer-password-456!",
	})
	if password.status != http.StatusOK {
		t.Fatalf("expected password update 200, got %d: %s", password.status, password.body)
	}
	profile := client.requestJSON(t, router, http.MethodPatch, "/api/users/"+userID, map[string]any{
		"displayName": "Viewer Profile",
		"avatarUrl":   "https://cdn.example.com/viewer.jpg",
	})
	if profile.status != http.StatusOK {
		t.Fatalf("expected profile update 200, got %d: %s", profile.status, profile.body)
	}
	updated := profile.payload["user"].(map[string]any)
	if updated["avatarUrl"] != "https://cdn.example.com/viewer.jpg" {
		t.Fatalf("expected avatarUrl in profile update response, got %#v", profile.payload)
	}

	deleteResp := client.requestJSON(t, router, http.MethodDelete, "/api/users/"+userID, map[string]any{})
	if deleteResp.status != http.StatusOK {
		t.Fatalf("expected delete user 200, got %d: %s", deleteResp.status, deleteResp.body)
	}
}

func TestAuthUserUpdateRejectsMissingCSRF(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	created, err := deps.Auth.CreateUser(context.Background(), "viewer", "viewer-password-123!", "Viewer", "standard")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")

	req := httptest.NewRequest(http.MethodPatch, "http://xuva.test/api/users/"+created.ID, bytes.NewReader(mustJSON(t, map[string]any{
		"displayName": "Viewer Updated",
		"avatarUrl":   "https://cdn.example.com/viewer.png",
	})))
	req.Header.Set("Content-Type", "application/json")
	client.apply(req, false)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without csrf header, got %d: %s", res.Code, res.Body.String())
	}
}

func TestAuthPasswordUpdateRefreshesCurrentSession(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")

	password := client.requestJSON(t, router, http.MethodPost, "/api/users/admin/password", map[string]any{
		"password": "test-password-456!",
	})
	if password.status != http.StatusOK {
		t.Fatalf("expected password update 200, got %d: %s", password.status, password.body)
	}
	if password.payload["sessionToken"] == "" {
		t.Fatalf("expected refreshed sessionToken in password update response, got %#v", password.payload)
	}

	session := client.requestJSON(t, router, http.MethodGet, "/api/auth/session", nil)
	if session.status != http.StatusOK {
		t.Fatalf("expected refreshed session 200, got %d: %s", session.status, session.body)
	}

	logout := client.requestJSON(t, router, http.MethodPost, "/api/auth/logout", map[string]any{})
	if logout.status != http.StatusOK {
		t.Fatalf("expected logout 200 after password refresh, got %d: %s", logout.status, logout.body)
	}

	relogin := client.requestJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "admin",
		"password": "test-password-456!",
	})
	if relogin.status != http.StatusOK {
		t.Fatalf("expected relogin with updated password 200, got %d: %s", relogin.status, relogin.body)
	}
}

func TestAuthzStandardCannotManageUsers(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	if _, err := deps.Auth.CreateUser(context.Background(), "viewer", "viewer-password-123!", "Viewer", "standard"); err != nil {
		t.Fatalf("create standard user: %v", err)
	}
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "viewer", "viewer-password-123!")

	list := client.requestJSON(t, router, http.MethodGet, "/api/users", nil)
	if list.status != http.StatusForbidden {
		t.Fatalf("expected standard user users list 403, got %d: %s", list.status, list.body)
	}
	create := client.requestJSON(t, router, http.MethodPost, "/api/users", map[string]any{
		"username":    "viewer2",
		"displayName": "Viewer Two",
		"password":    "viewer-password-123!",
		"role":        "standard",
	})
	if create.status != http.StatusForbidden {
		t.Fatalf("expected standard user create 403, got %d: %s", create.status, create.body)
	}
	password := client.requestJSON(t, router, http.MethodPost, "/api/users/admin/password", map[string]any{
		"password": "new-password-123!",
	})
	if password.status != http.StatusForbidden {
		t.Fatalf("expected standard user password update 403, got %d: %s", password.status, password.body)
	}
	profile := client.requestJSON(t, router, http.MethodPatch, "/api/users/admin", map[string]any{
		"displayName": "Admin Updated",
		"avatarUrl":   "https://cdn.example.com/admin.png",
	})
	if profile.status != http.StatusForbidden {
		t.Fatalf("expected standard user profile update 403, got %d: %s", profile.status, profile.body)
	}
	deleteResp := client.requestJSON(t, router, http.MethodDelete, "/api/users/admin", map[string]any{})
	if deleteResp.status != http.StatusForbidden {
		t.Fatalf("expected standard user delete 403, got %d: %s", deleteResp.status, deleteResp.body)
	}
}

func TestAuthzAdminCannotDeleteLastAdmin(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")

	response := client.requestJSON(t, router, http.MethodDelete, "/api/users/admin", map[string]any{})
	if response.status != http.StatusBadRequest && response.status != http.StatusConflict {
		t.Fatalf("expected delete self guard or last-admin guard, got %d: %s", response.status, response.body)
	}
}

func TestSettingsFolderBrowseListsDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Movies"), 0o755); err != nil {
		t.Fatalf("mkdir movies: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte("not a folder"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	router := NewRouter(testDeps(t, time.Now()))

	response := requestJSON(t, router, http.MethodGet, "/api/settings/folders/browse?path="+url.QueryEscape(root), nil)
	if response["path"] == "" || response["parent"] == "" {
		t.Fatalf("expected current path and parent, got %#v", response)
	}
	if response["currentPath"] != response["path"] || response["parentPath"] != response["parent"] {
		t.Fatalf("expected modern and legacy path fields to match, got %#v", response)
	}
	entries := response["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["name"] != "Movies" {
		t.Fatalf("expected only child folders, got %#v", response)
	}
	if entries[0].(map[string]any)["isDir"] != true {
		t.Fatalf("expected entries to be marked as directories, got %#v", response)
	}
	if response["writable"] != true {
		t.Fatalf("expected writable temp folder, got %#v", response)
	}
}

func TestSettingsFolderBrowseWithoutPathReturnsRoots(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))

	response := requestJSON(t, router, http.MethodGet, "/api/settings/folders/browse", nil)
	if response["currentPath"] != "" {
		t.Fatalf("expected root browser to have empty current path, got %#v", response)
	}
	entries := response["entries"].([]any)
	if len(entries) == 0 {
		t.Fatalf("expected root entries, got %#v", response)
	}
	if response["message"] == "" {
		t.Fatalf("expected server-visible path guidance, got %#v", response)
	}
}

func TestSettingsRuntimePathsReflectSavedValuesBeforeRestart(t *testing.T) {
	// Note: DataDir is intentionally json:"-" â€” it's resolved at startup from
	// XUVA_DATA_DIR and can't be modified via the settings API (settings.json
	// itself lives inside DataDir). This test exercises a settable runtime
	// path (transcodeDir) to verify saved values surface in /api/settings,
	// /api/system/status, and the runtimePaths block before a restart.
	deps := testDeps(t, time.Now())
	router := NewRouter(deps)
	originalData := deps.Config.DataDir
	nextTranscode := filepath.Join(t.TempDir(), "transcode")

	update := requestJSON(t, router, http.MethodPut, "/api/settings", map[string]any{
		"transcodeDir": nextTranscode,
	})
	updatedPaths := update["runtimePaths"].(map[string]any)
	if updatedPaths["transcode"] != nextTranscode {
		t.Fatalf("expected update response to include saved transcode path, got %#v", update)
	}

	reloaded := getJSON(t, router, "/api/settings")
	reloadedPaths := reloaded["runtimePaths"].(map[string]any)
	if reloadedPaths["transcode"] != nextTranscode {
		t.Fatalf("expected settings reload to keep saved transcode path before restart, got %#v", reloaded)
	}
	status := getJSON(t, router, "/api/system/status")
	disks := status["disks"].([]any)
	found := false
	for _, item := range disks {
		disk := item.(map[string]any)
		if disk["name"] == "data" && disk["path"] == originalData {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected system status to include data dir entry, got %#v", status)
	}
}

func TestSettingsServerNameIsUserFacingServerIdentity(t *testing.T) {
	deps := testDeps(t, time.Now())
	router := NewRouter(deps)

	update := requestJSON(t, router, http.MethodPut, "/api/settings", map[string]any{
		"serverName": "  Living Room Xuva  ",
	})
	configPayload, _ := update["config"].(map[string]any)
	if configPayload["serverName"] != "Living Room Xuva" {
		t.Fatalf("expected trimmed server name, got %#v", configPayload)
	}

	reloaded := getJSON(t, router, "/api/settings")
	reloadedConfig, _ := reloaded["config"].(map[string]any)
	if reloadedConfig["serverName"] != "Living Room Xuva" {
		t.Fatalf("expected saved server name after reload, got %#v", reloadedConfig)
	}
}

func TestClientBootstrapUsesSavedServerName(t *testing.T) {
	deps := testDeps(t, time.Now())
	router := NewRouter(deps)

	requestJSON(t, router, http.MethodPut, "/api/settings", map[string]any{
		"serverName": "Family Library",
	})

	bootstrap := getJSON(t, router, "/api/client/bootstrap")
	serverPayload, _ := bootstrap["server"].(map[string]any)
	if serverPayload["name"] != "Family Library" {
		t.Fatalf("expected client bootstrap to use saved server name, got %#v", serverPayload)
	}
}

func TestSettingsServerNameValidation(t *testing.T) {
	router := NewRouter(testDeps(t, time.Now()))

	empty := httptest.NewRecorder()
	router.ServeHTTP(empty, httptest.NewRequest(http.MethodPut, "http://xuva.test/api/settings", bytes.NewReader(mustJSON(t, map[string]any{
		"serverName": "   ",
	}))))
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("expected empty server name to fail, got %d: %s", empty.Code, empty.Body.String())
	}

	longName := strings.Repeat("L", 51)
	tooLong := httptest.NewRecorder()
	router.ServeHTTP(tooLong, httptest.NewRequest(http.MethodPut, "http://xuva.test/api/settings", bytes.NewReader(mustJSON(t, map[string]any{
		"serverName": longName,
	}))))
	if tooLong.Code != http.StatusBadRequest {
		t.Fatalf("expected long server name to fail, got %d: %s", tooLong.Code, tooLong.Body.String())
	}
}

func TestSettingsServerNameMigratesLegacyDefault(t *testing.T) {
	deps := testDeps(t, time.Now())
	deps.Config.ServerName = "My Server"
	router := NewRouter(deps)

	settings := getJSON(t, router, "/api/settings")
	configPayload, _ := settings["config"].(map[string]any)
	if configPayload["serverName"] != "Xuva" {
		t.Fatalf("expected legacy default server name to display as Xuva, got %#v", configPayload)
	}
}

func TestSettingsIgnoreManagedProviderKeysInUserSettings(t *testing.T) {
	deps := testDeps(t, time.Now())
	router := NewRouter(deps)

	requestJSON(t, router, http.MethodPut, "/api/settings", map[string]any{
		"tmdbApiKey":     "tmdb-test-key",
		"fanartTvApiKey": "fanart-test-key",
		"omdbApiKey":     "omdb-test-key",
	})

	reloaded := getJSON(t, router, "/api/settings")
	reloadedConfig, _ := reloaded["config"].(map[string]any)
	if _, ok := reloadedConfig["metadataProviders"]; ok {
		t.Fatalf("expected settings payload to keep managed providers out of user settings, got %#v", reloadedConfig["metadataProviders"])
	}

	saved, err := config.LoadFile(deps.Config.DataDir)
	if err != nil {
		t.Fatalf("load saved settings file: %v", err)
	}
	if saved.TMDBAPIKey != "" || saved.FanartTVAPIKey != "" || saved.OMDbAPIKey != "" {
		t.Fatalf("expected settings API to ignore managed provider keys, got %#v", saved)
	}
}

func TestSettingsMetadataSourcePreferencesExposeDefaultsAndPersist(t *testing.T) {
	deps := testDeps(t, time.Now())
	router := NewRouter(deps)

	initial := getJSON(t, router, "/api/settings")
	preferences, _ := initial["metadataSourcePreferences"].(map[string]any)
	movie, _ := preferences["movie"].([]any)
	series, _ := preferences["series"].([]any)
	movieArtwork, _ := preferences["movieArtwork"].([]any)
	seriesArtwork, _ := preferences["seriesArtwork"].([]any)
	if len(movie) == 0 || len(series) == 0 || len(movieArtwork) == 0 || len(seriesArtwork) == 0 {
		t.Fatalf("expected default metadata source preferences, got %#v", initial)
	}

	update := requestJSON(t, router, http.MethodPut, "/api/settings/metadata-sources", map[string]any{
		"movie":         []string{"wikipedia", "tmdb", "filename"},
		"series":        []string{"tvmaze", "wikidata", "filename"},
		"movieArtwork":  []string{"artwork", "fanart", "tmdb"},
		"seriesArtwork": []string{"artwork", "fanart", "wikipedia"},
	})
	if update["restartRequired"] != false {
		t.Fatalf("expected metadata source updates to avoid restart, got %#v", update)
	}

	updatedPreferences, _ := update["metadataSourcePreferences"].(map[string]any)
	updatedMovie, _ := updatedPreferences["movie"].([]any)
	updatedSeries, _ := updatedPreferences["series"].([]any)
	updatedMovieArtwork, _ := updatedPreferences["movieArtwork"].([]any)
	updatedSeriesArtwork, _ := updatedPreferences["seriesArtwork"].([]any)
	if len(updatedMovie) != 3 || updatedMovie[0] != "wikipedia" || updatedMovie[1] != "tmdb" || updatedMovie[2] != "filename" {
		t.Fatalf("expected movie metadata source order to persist, got %#v", updatedPreferences)
	}
	if len(updatedSeries) != 3 || updatedSeries[0] != "tvmaze" || updatedSeries[1] != "wikidata" || updatedSeries[2] != "filename" {
		t.Fatalf("expected series metadata source order to persist, got %#v", updatedPreferences)
	}
	if len(updatedMovieArtwork) != 3 || updatedMovieArtwork[0] != "artwork" || updatedMovieArtwork[1] != "fanart" || updatedMovieArtwork[2] != "tmdb" {
		t.Fatalf("expected movie artwork source order to persist, got %#v", updatedPreferences)
	}
	if len(updatedSeriesArtwork) != 3 || updatedSeriesArtwork[0] != "artwork" || updatedSeriesArtwork[1] != "fanart" || updatedSeriesArtwork[2] != "wikipedia" {
		t.Fatalf("expected series artwork source order to persist, got %#v", updatedPreferences)
	}

	saved, err := config.LoadFile(deps.Config.DataDir)
	if err != nil {
		t.Fatalf("load saved settings file: %v", err)
	}
	if len(saved.MovieMetadataSources) != 3 || saved.MovieMetadataSources[0] != "wikipedia" || saved.MovieMetadataSources[1] != "tmdb" || saved.MovieMetadataSources[2] != "filename" {
		t.Fatalf("expected saved movie metadata source order, got %#v", saved.MovieMetadataSources)
	}
	if len(saved.SeriesMetadataSources) != 3 || saved.SeriesMetadataSources[0] != "tvmaze" || saved.SeriesMetadataSources[1] != "wikidata" || saved.SeriesMetadataSources[2] != "filename" {
		t.Fatalf("expected saved series metadata source order, got %#v", saved.SeriesMetadataSources)
	}
	if len(saved.MovieArtworkSources) != 3 || saved.MovieArtworkSources[0] != "artwork" || saved.MovieArtworkSources[1] != "fanart" || saved.MovieArtworkSources[2] != "tmdb" {
		t.Fatalf("expected saved movie artwork source order, got %#v", saved.MovieArtworkSources)
	}
	if len(saved.SeriesArtworkSources) != 3 || saved.SeriesArtworkSources[0] != "artwork" || saved.SeriesArtworkSources[1] != "fanart" || saved.SeriesArtworkSources[2] != "wikipedia" {
		t.Fatalf("expected saved series artwork source order, got %#v", saved.SeriesArtworkSources)
	}
}

func TestLibrariesInheritMetadataSourcePreferencesFromSettings(t *testing.T) {
	root := t.TempDir()
	deps := testDeps(t, time.Now())
	router := NewRouter(deps)

	requestJSON(t, router, http.MethodPut, "/api/settings/metadata-sources", map[string]any{
		"movie":         []string{"wikipedia", "tmdb", "filename"},
		"series":        []string{"tvmaze", "wikidata", "filename"},
		"movieArtwork":  []string{"artwork", "fanart", "tmdb"},
		"seriesArtwork": []string{"artwork", "fanart", "wikipedia"},
	})

	moviesLibrary := postJSON(t, router, "/api/libraries", map[string]any{
		"kind": "movies",
		"name": "Archive Movies",
		"path": root,
	})
	if got := moviesLibrary["metadataSources"].([]any); len(got) != 3 || got[0] != "wikipedia" || got[1] != "tmdb" || got[2] != "filename" {
		t.Fatalf("expected movies library to inherit metadata source order, got %#v", moviesLibrary)
	}
	if got := moviesLibrary["artworkSources"].([]any); len(got) != 3 || got[0] != "artwork" || got[1] != "fanart" || got[2] != "tmdb" {
		t.Fatalf("expected movies library to inherit artwork source order, got %#v", moviesLibrary)
	}

	tvLibrary := postJSON(t, router, "/api/libraries", map[string]any{
		"kind": "tv",
		"name": "Archive TV",
		"path": filepath.Join(root, "tv"),
	})
	if got := tvLibrary["metadataSources"].([]any); len(got) != 3 || got[0] != "tvmaze" || got[1] != "wikidata" || got[2] != "filename" {
		t.Fatalf("expected TV library to inherit metadata source order, got %#v", tvLibrary)
	}
	if got := tvLibrary["artworkSources"].([]any); len(got) != 3 || got[0] != "artwork" || got[1] != "fanart" || got[2] != "wikipedia" {
		t.Fatalf("expected TV library to inherit artwork source order, got %#v", tvLibrary)
	}
}

func TestAuthzProtectedMediaAndDownloadRoutes(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	if _, err := deps.Auth.CreateUser(context.Background(), "viewer", "viewer-password-123!", "Viewer", "standard"); err != nil {
		t.Fatalf("create standard user: %v", err)
	}
	router := NewRouter(deps)

	unauth := httptest.NewRequest(http.MethodGet, "/api/media-sources/missing/stream", nil)
	unauthResponse := httptest.NewRecorder()
	router.ServeHTTP(unauthResponse, unauth)
	if unauthResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthenticated stream 401, got %d: %s", unauthResponse.Code, unauthResponse.Body.String())
	}

	viewer := newAuthTestClient(t)
	loginAs(t, viewer, router, "viewer", "viewer-password-123!")
	sessionID := startPlaybackSession(t, viewer, router, "missing", "web")
	token := viewer.requestJSON(t, router, http.MethodPost, "/api/media-sources/missing/stream-token", map[string]any{
		"sessionId": sessionID,
		"deviceId":  "web",
	})
	if token.status != http.StatusOK {
		t.Fatalf("expected standard stream token 200, got %d: %s", token.status, token.body)
	}
	streamURL, _ := token.payload["streamUrl"].(string)
	stream := viewer.requestJSON(t, router, http.MethodGet, streamURL, nil)
	if stream.status != http.StatusNotFound {
		t.Fatalf("expected standard stream request to reach handler and return 404, got %d: %s", stream.status, stream.body)
	}
	download := viewer.requestJSON(t, router, http.MethodPost, "/api/downloads", map[string]any{"mediaSourceId": "missing", "targetProfile": "original"})
	if download.status != http.StatusForbidden {
		t.Fatalf("expected standard download creation 403, got %d: %s", download.status, download.body)
	}
}

func TestAuthzAuditEventsIncludeActorAndAction(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	eventsCh, stop := deps.Events.Subscribe(ctx)
	t.Cleanup(func() {
		stop()
		cancel()
	})
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")

	response := client.requestJSON(t, router, http.MethodPut, "/api/settings", map[string]any{
		"httpAddr": "127.0.0.1:8097",
	})
	if response.status != http.StatusOK {
		t.Fatalf("expected settings update 200, got %d: %s", response.status, response.body)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-eventsCh:
			if event.Type != "audit.route" {
				continue
			}
			data := event.Data.(map[string]any)
			if data["userId"] == "" || data["action"] != "settings.update" || data["result"] != "allowed" {
				t.Fatalf("unexpected audit data: %#v", data)
			}
			return
		case <-deadline:
			t.Fatalf("timed out waiting for audit route event")
		}
	}
}

func TestSensitiveActionsEmitDomainAuditEvents(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	eventsCh, stop := deps.Events.Subscribe(ctx)
	t.Cleanup(func() {
		stop()
		cancel()
	})
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")

	settingsResponse := client.requestJSON(t, router, http.MethodPut, "/api/settings", map[string]any{
		"httpAddr": "127.0.0.1:8097",
	})
	if settingsResponse.status != http.StatusOK {
		t.Fatalf("expected settings update 200, got %d: %s", settingsResponse.status, settingsResponse.body)
	}
	libraryResponse := client.requestJSON(t, router, http.MethodPost, "/api/libraries", map[string]any{
		"name":        "Movies",
		"kind":        "movies",
		"path":        t.TempDir(),
		"storageType": "local",
	})
	if libraryResponse.status != http.StatusOK {
		t.Fatalf("expected library save 200, got %d: %s", libraryResponse.status, libraryResponse.body)
	}

	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 3 {
		select {
		case event := <-eventsCh:
			data, _ := event.Data.(map[string]any)
			switch event.Type {
			case "audit.auth":
				if data["action"] == "login" && data["result"] == "allowed" && data["userId"] != "" {
					seen[event.Type] = true
				}
			case "audit.settings":
				if data["action"] == "settings.update" && data["result"] == "allowed" && data["userId"] != "" {
					seen[event.Type] = true
				}
			case "audit.library":
				if data["action"] == "library.save" && data["result"] == "allowed" && data["libraryId"] != "" {
					seen[event.Type] = true
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for domain audit events, saw %#v", seen)
		}
	}
}

func TestSignedStreamWithoutTokenIsDenied(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")
	sessionID := startPlaybackSession(t, client, router, "source_1", "web")

	response := client.requestJSON(t, router, http.MethodGet, "/api/media-sources/source_1/stream?sessionId="+sessionID+"&deviceId=web", nil)

	if response.status != http.StatusUnauthorized {
		t.Fatalf("expected missing stream token 401, got %d: %s", response.status, response.body)
	}
}

func TestSignedStreamExpiredTokenIsDenied(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")
	sessionID := startPlaybackSession(t, client, router, "source_1", "web")
	token, _, err := deps.Streaming.Issue(streaming.Expected{MediaSourceID: "source_1", SessionID: sessionID, UserID: "admin", DeviceID: "web"}, -time.Minute)
	if err != nil {
		t.Fatalf("issue expired token: %v", err)
	}

	response := client.requestJSON(t, router, http.MethodGet, "/api/media-sources/source_1/stream?sessionId="+sessionID+"&deviceId=web&token="+url.QueryEscape(token), nil)

	if response.status != http.StatusForbidden || !strings.Contains(response.body, "expired") {
		t.Fatalf("expected expired stream token 403, got %d: %s", response.status, response.body)
	}
}

func TestSignedStreamForgedTokenIsDenied(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")
	sessionID := startPlaybackSession(t, client, router, "source_1", "web")
	token, _, err := deps.Streaming.Issue(streaming.Expected{MediaSourceID: "source_1", SessionID: sessionID, UserID: "admin", DeviceID: "web"}, 0)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	response := client.requestJSON(t, router, http.MethodGet, "/api/media-sources/source_1/stream?sessionId="+sessionID+"&deviceId=web&token="+url.QueryEscape(token+"x"), nil)

	if response.status != http.StatusForbidden || !strings.Contains(response.body, "invalid") {
		t.Fatalf("expected forged stream token 403, got %d: %s", response.status, response.body)
	}
}

func TestSignedStreamTokenCannotMoveAcrossSessions(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")
	firstSessionID := startPlaybackSession(t, client, router, "source_1", "web")
	secondSessionID := startPlaybackSession(t, client, router, "source_1", "web")
	token, _, err := deps.Streaming.Issue(streaming.Expected{MediaSourceID: "source_1", SessionID: firstSessionID, UserID: "admin", DeviceID: "web"}, 0)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	response := client.requestJSON(t, router, http.MethodGet, "/api/media-sources/source_1/stream?sessionId="+secondSessionID+"&deviceId=web&token="+url.QueryEscape(token), nil)

	if response.status != http.StatusForbidden || !strings.Contains(response.body, "session") {
		t.Fatalf("expected cross-session token 403, got %d: %s", response.status, response.body)
	}
}

func TestSignedStreamLimitBlocksExcessStreams(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	deps.Streaming.SetLimits(1, 1)
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")
	sessionID := startPlaybackSession(t, client, router, "source_1", "web")
	expected := streaming.Expected{MediaSourceID: "source_1", SessionID: sessionID, UserID: "admin", DeviceID: "web"}
	firstToken, _, err := deps.Streaming.Issue(expected, 0)
	if err != nil {
		t.Fatalf("issue first token: %v", err)
	}
	_, release, err := deps.Streaming.Validate(firstToken, expected)
	if err != nil {
		t.Fatalf("hold first stream: %v", err)
	}
	defer release()
	secondToken, _, err := deps.Streaming.Issue(expected, 0)
	if err != nil {
		t.Fatalf("issue second token: %v", err)
	}

	response := client.requestJSON(t, router, http.MethodGet, "/api/media-sources/source_1/stream?sessionId="+sessionID+"&deviceId=web&token="+url.QueryEscape(secondToken), nil)

	if response.status != http.StatusTooManyRequests {
		t.Fatalf("expected stream limit 429, got %d: %s", response.status, response.body)
	}
}

func TestSignedStreamTokenEndpointReturnsBoundURL(t *testing.T) {
	router := NewRouter(testDepsWithAuth(t, time.Now()))
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")
	sessionID := startPlaybackSession(t, client, router, "source_1", "web")

	token := client.requestJSON(t, router, http.MethodPost, "/api/media-sources/source_1/stream-token", map[string]any{
		"sessionId": sessionID,
		"deviceId":  "web",
	})

	if token.status != http.StatusOK || token.payload["streamUrl"] == "" || token.payload["token"] == "" {
		t.Fatalf("expected bound stream token response, got %d: %#v", token.status, token.payload)
	}
}

func TestProbeStartReturns429WhenQueueSaturated(t *testing.T) {
	deps := testDepsWithAuth(t, time.Now())
	router := NewRouter(deps)
	client := newAuthTestClient(t)
	loginAs(t, client, router, "admin", "test-password-123!")

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	total := deps.Jobs.Probe.Workers + deps.Jobs.Probe.MaxQueued
	for i := 0; i < total; i++ {
		if err := deps.Jobs.Probe.Submit(context.Background(), func(context.Context) { <-block }); err != nil {
			t.Fatalf("saturate probe queue failed at %d: %v", i, err)
		}
	}

	response := client.requestJSON(t, router, http.MethodPost, "/api/probes", map[string]any{
		"limit": 5,
	})
	if response.status != http.StatusTooManyRequests {
		t.Fatalf("expected probe start 429 when queue saturated, got %d: %s", response.status, response.body)
	}
}

func testDeps(t *testing.T, startedAt time.Time) Deps {
	t.Helper()

	dataDir := t.TempDir()
	cfg := config.Config{
		HTTPAddr:         "127.0.0.1:8097",
		DataDir:          dataDir,
		AuthDisabled:     true,
		TranscodeDir:     filepath.Join(dataDir, "transcode"),
		DownloadsDir:     filepath.Join(dataDir, "downloads"),
		MetadataDir:      filepath.Join(dataDir, "metadata"),
		CacheDir:         filepath.Join(dataDir, "cache"),
		TempDir:          filepath.Join(dataDir, "temp"),
		EventBuffer:      64,
		ScanWorkers:      1,
		ProbeWorkers:     2,
		TranscodeWorkers: 1,
		GPUWorkers:       1,
	}
	for _, dir := range []string{cfg.TranscodeDir, cfg.DownloadsDir, cfg.MetadataDir, cfg.CacheDir, cfg.TempDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create runtime dir: %v", err)
		}
	}
	manager := resources.NewManager(resources.Limits{
		ScanWorkers:      cfg.ScanWorkers,
		ProbeWorkers:     cfg.ProbeWorkers,
		TranscodeWorkers: cfg.TranscodeWorkers,
		GPUWorkers:       cfg.GPUWorkers,
	})
	registry := jobs.NewRegistry(manager)
	ctx, cancel := context.WithCancel(context.Background())
	registry.Start(ctx)
	t.Cleanup(cancel)
	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
	})
	libraryService := libraries.NewService()
	scannerService := scanner.NewService()
	catalogService := catalog.NewService(db)
	movieService := movies.NewService()
	tvService := tv.NewService()
	eventBus := events.NewBus(cfg.EventBuffer)
	observe := observability.NewService()
	observe.Subscribe(ctx, eventBus)
	return Deps{
		Config:    cfg,
		StartedAt: startedAt,
		Database:  db,
		Events:    eventBus,
		Observe:   observe,
		Resources: manager,
		Jobs:      registry,
		Libraries: libraryService,
		Scanner:   scannerService,
		Scans:     scans.NewService(cfg, eventBus, registry.Scan, libraryService, scannerService, catalogService, metaprovider.NewService(cfg, catalogService, eventBus), movieService, tvService),
		Catalog:   catalogService,
		Media:     media.NewService(),
		Metadata:  metaprovider.NewService(cfg, catalogService, eventBus),
		Movies:    movieService,
		TV:        tvService,
		Probe:     probe.NewService("ffprobe"),
		Probes:    probes.NewService(eventBus, registry.Probe, catalogService, probe.NewService("ffprobe")),
		Playback:  playback.NewService(),
		PlayState: playstate.NewService(db, eventBus),
		Streaming: streaming.NewServiceWithKey("test", []byte("01234567890123456789012345678901")),
		Transcode: transcode.NewService(eventBus, registry.Transcode, "ffmpeg", filepath.Join(t.TempDir(), "transcode")),
		Downloads: downloads.NewService(eventBus, registry.Transcode, "ffmpeg", filepath.Join(t.TempDir(), "downloads")),
		Devices:   devices.NewPersistentService(db),
		Sessions:  sessions.NewService(eventBus),
		Subtitles: subtitles.NewService(),
		Pairing:   pairing.NewService(),
		Migration: migration.NewService(db, eventBus),
	}
}

func testDepsWithAuth(t *testing.T, startedAt time.Time) Deps {
	t.Helper()

	deps := testDeps(t, startedAt)
	deps.Config.AuthDisabled = false
	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open auth database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close auth database: %v", err)
		}
	})
	deps.Catalog = catalog.NewService(db)
	deps.Database = db
	deps.Metadata = metaprovider.NewService(deps.Config, deps.Catalog, deps.Events)
	deps.Scans = scans.NewService(deps.Config, deps.Events, deps.Jobs.Scan, deps.Libraries, deps.Scanner, deps.Catalog, deps.Metadata, deps.Movies, deps.TV)
	deps.Probes = probes.NewService(deps.Events, deps.Jobs.Probe, deps.Catalog, probe.NewService("ffprobe"))
	deps.PlayState = playstate.NewService(db, deps.Events)
	deps.Migration = migration.NewService(db, deps.Events)
	deps.Auth = auth.NewService(db, false)
	deps.Devices = devices.NewPersistentService(db)
	if err := deps.Auth.Bootstrap(context.Background(), auth.BootstrapOptions{
		Username: "admin",
		Password: "test-password-123!",
	}); err != nil {
		t.Fatalf("bootstrap auth: %v", err)
	}
	return deps
}

func testDepsWithAuthNoBootstrap(t *testing.T, startedAt time.Time) Deps {
	t.Helper()

	deps := testDeps(t, startedAt)
	deps.Config.AuthDisabled = false
	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open auth database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close auth database: %v", err)
		}
	})
	deps.Catalog = catalog.NewService(db)
	deps.Database = db
	deps.Metadata = metaprovider.NewService(deps.Config, deps.Catalog, deps.Events)
	deps.Scans = scans.NewService(deps.Config, deps.Events, deps.Jobs.Scan, deps.Libraries, deps.Scanner, deps.Catalog, deps.Metadata, deps.Movies, deps.TV)
	deps.Probes = probes.NewService(deps.Events, deps.Jobs.Probe, deps.Catalog, probe.NewService("ffprobe"))
	deps.PlayState = playstate.NewService(db, deps.Events)
	deps.Migration = migration.NewService(db, deps.Events)
	deps.Auth = auth.NewService(db, false)
	deps.Devices = devices.NewPersistentService(db)
	return deps
}

type authTestClient struct {
	jar       http.CookieJar
	csrfToken string
}

type authJSONResponse struct {
	status  int
	payload map[string]any
	body    string
}

func newAuthTestClient(t *testing.T) *authTestClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	return &authTestClient{jar: jar}
}

func (c *authTestClient) requestJSON(t *testing.T, router http.Handler, method string, path string, body any) authJSONResponse {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(mustJSON(t, body))
	}
	request := httptest.NewRequest(method, "http://xuva.test"+path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	c.apply(request, method != http.MethodGet && method != http.MethodHead)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	c.store(request, response)

	result := authJSONResponse{status: response.Code, body: response.Body.String()}
	if response.Body.Len() > 0 {
		_ = json.Unmarshal(response.Body.Bytes(), &result.payload)
	}
	return result
}

func (c *authTestClient) apply(request *http.Request, includeCSRF bool) {
	for _, cookie := range c.jar.Cookies(request.URL) {
		request.AddCookie(cookie)
	}
	if includeCSRF && c.csrfToken != "" {
		request.Header.Set("X-CSRF-Token", c.csrfToken)
	}
}

func (c *authTestClient) store(request *http.Request, response *httptest.ResponseRecorder) {
	c.jar.SetCookies(request.URL, response.Result().Cookies())
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == auth.CSRFCookieName {
			c.csrfToken = cookie.Value
		}
	}
}

func loginAs(t *testing.T, client *authTestClient, router http.Handler, username string, password string) {
	t.Helper()
	response := client.requestJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"username": username,
		"password": password,
	})
	if response.status != http.StatusOK {
		t.Fatalf("login %s failed with %d: %s", username, response.status, response.body)
	}
}

func startPlaybackSession(t *testing.T, client *authTestClient, router http.Handler, mediaSourceID string, deviceID string) string {
	t.Helper()
	response := client.requestJSON(t, router, http.MethodPost, "/api/sessions", map[string]any{
		"mediaSourceId": mediaSourceID,
		"deviceId":      deviceID,
		"clientProfile": "web",
		"mode":          "direct",
		"route":         "direct",
	})
	if response.status != http.StatusAccepted {
		t.Fatalf("start session failed with %d: %s", response.status, response.body)
	}
	sessionID, _ := response.payload["id"].(string)
	if sessionID == "" {
		t.Fatalf("expected session id, got %#v", response.payload)
	}
	return sessionID
}

func getJSON(t *testing.T, router http.Handler, path string) map[string]any {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK && response.Code != http.StatusAccepted {
		t.Fatalf("expected 200 or 202, got %d: %s", response.Code, response.Body.String())
	}

	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return payload
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return payload
}

func waitForScan(t *testing.T, router http.Handler, id string) map[string]any {
	return waitForScanAs(t, router, id, nil)
}

// waitForScanAs is the auth-aware variant. Pass a logged-in client when the
// router was built with auth enabled (testDepsWithAuth); the polling GETs on
// /api/scans/{id} now require a resolved session (issue #55).
func waitForScanAs(t *testing.T, router http.Handler, id string, client *authTestClient) map[string]any {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var job map[string]any
		if client != nil {
			job = client.requestJSON(t, router, http.MethodGet, "/api/scans/"+id, nil).payload
		} else {
			job = getJSON(t, router, "/api/scans/"+id)
		}
		switch job["status"] {
		case string(scans.StatusCompleted):
			return job
		case string(scans.StatusFailed):
			t.Fatalf("scan failed: %#v", job)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("scan %s did not complete", id)
	return nil
}

func postJSON(t *testing.T, router http.Handler, path string, body any) map[string]any {
	t.Helper()
	return requestJSON(t, router, http.MethodPost, path, body)
}

func requestJSON(t *testing.T, router http.Handler, method string, path string, body any) map[string]any {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK && response.Code != http.StatusAccepted {
		t.Fatalf("expected 200 or 202, got %d: %s", response.Code, response.Body.String())
	}

	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return payload
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return raw
}

func anyToString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if value == nil {
		return ""
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}
