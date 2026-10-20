package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/EJM-Digital/Xuva/server/internal/buildinfo"
)

type fakeApplier struct {
	called  bool
	msiPath string
	version string
}

func (f *fakeApplier) apply(_ context.Context, msiPath, version string) error {
	f.called = true
	f.msiPath = msiPath
	f.version = version
	return nil
}

func setBuildVersion(t *testing.T, v string) {
	t.Helper()
	prev := buildinfo.Version
	buildinfo.Version = v
	t.Cleanup(func() { buildinfo.Version = prev })
}

// newReleaseServer serves a GitHub-style "latest release" JSON whose single MSI
// asset downloads msiBody, with the given digest ("" → no digest field).
func newReleaseServer(t *testing.T, version string, msiBody []byte, digest string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/download/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(msiBody)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		asset := map[string]any{
			"name":                 "xuva-server-" + strings.TrimPrefix(version, "v") + ".msi",
			"browser_download_url": srv.URL + "/download/xuva.msi",
			"size":                 len(msiBody),
		}
		if digest != "" {
			asset["digest"] = digest
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": version,
			"html_url": "https://example.test/releases/" + version,
			"assets":   []map[string]any{asset},
		})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestRunUpdateCheckAppliesNewerVerifiedMSI(t *testing.T) {
	body := []byte("PRETEND-MSI-CONTENTS")
	srv := newReleaseServer(t, "v2.0.0", body, sha256Digest(body))
	t.Setenv("XUVA_UPDATE_RELEASE_API", srv.URL)
	t.Setenv("XUVA_RUNTIME_HOME", t.TempDir())
	setBuildVersion(t, "v1.0.0")

	fake := &fakeApplier{}
	if err := runUpdateCheck(context.Background(), fake, false); err != nil {
		t.Fatalf("runUpdateCheck: %v", err)
	}
	if !fake.called {
		t.Fatal("applier was not called for a newer, verified release")
	}
	if fake.version != "v2.0.0" {
		t.Errorf("applied version = %q, want v2.0.0", fake.version)
	}
	got, err := os.ReadFile(fake.msiPath)
	if err != nil {
		t.Fatalf("staged MSI missing: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("staged MSI content mismatch")
	}
}

func TestRunUpdateCheckUpToDate(t *testing.T) {
	body := []byte("x")
	srv := newReleaseServer(t, "v1.0.0", body, sha256Digest(body))
	t.Setenv("XUVA_UPDATE_RELEASE_API", srv.URL)
	t.Setenv("XUVA_RUNTIME_HOME", t.TempDir())
	setBuildVersion(t, "v1.0.0")

	fake := &fakeApplier{}
	if err := runUpdateCheck(context.Background(), fake, false); err != nil {
		t.Fatalf("runUpdateCheck: %v", err)
	}
	if fake.called {
		t.Fatal("applier called even though the installed version is current")
	}
}

func TestRunUpdateCheckRejectsBadChecksum(t *testing.T) {
	body := []byte("real-msi-bytes")
	// Advertise a digest that does not match the body we serve.
	srv := newReleaseServer(t, "v2.0.0", body, "sha256:"+strings.Repeat("0", 64))
	t.Setenv("XUVA_UPDATE_RELEASE_API", srv.URL)
	runtimeHome := t.TempDir()
	t.Setenv("XUVA_RUNTIME_HOME", runtimeHome)
	setBuildVersion(t, "v1.0.0")

	fake := &fakeApplier{}
	err := runUpdateCheck(context.Background(), fake, false)
	if err == nil {
		t.Fatal("expected a checksum-mismatch error")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error = %v, want a checksum mismatch", err)
	}
	if fake.called {
		t.Fatal("applier called despite a checksum mismatch")
	}
}

func TestRunUpdateCheckFailsClosedWithoutChecksum(t *testing.T) {
	body := []byte("unverifiable")
	srv := newReleaseServer(t, "v2.0.0", body, "") // no digest, no sidecar
	t.Setenv("XUVA_UPDATE_RELEASE_API", srv.URL)
	t.Setenv("XUVA_RUNTIME_HOME", t.TempDir())
	setBuildVersion(t, "v1.0.0")

	fake := &fakeApplier{}
	if err := runUpdateCheck(context.Background(), fake, false); err == nil {
		t.Fatal("expected a fail-closed error when no checksum is available")
	}
	if fake.called {
		t.Fatal("applier called for an unverifiable MSI")
	}
}

func TestRunUpdateCheckScheduledRespectsOptOut(t *testing.T) {
	body := []byte("PRETEND-MSI")
	srv := newReleaseServer(t, "v2.0.0", body, sha256Digest(body))
	t.Setenv("XUVA_UPDATE_RELEASE_API", srv.URL)
	t.Setenv("XUVA_RUNTIME_HOME", t.TempDir())
	t.Setenv("XUVA_AUTO_UPDATE", "0") // opted out
	setBuildVersion(t, "v1.0.0")

	fake := &fakeApplier{}
	// scheduled=true honors the opt-out: a newer release exists but is skipped.
	if err := runUpdateCheck(context.Background(), fake, true); err != nil {
		t.Fatalf("runUpdateCheck (scheduled, opted out): %v", err)
	}
	if fake.called {
		t.Fatal("scheduled run applied an update despite XUVA_AUTO_UPDATE=0")
	}
}

func TestRunUpdateCheckManualIgnoresOptOut(t *testing.T) {
	body := []byte("PRETEND-MSI")
	srv := newReleaseServer(t, "v2.0.0", body, sha256Digest(body))
	t.Setenv("XUVA_UPDATE_RELEASE_API", srv.URL)
	t.Setenv("XUVA_RUNTIME_HOME", t.TempDir())
	t.Setenv("XUVA_AUTO_UPDATE", "0") // opted out — but a MANUAL run still proceeds
	setBuildVersion(t, "v1.0.0")

	fake := &fakeApplier{}
	if err := runUpdateCheck(context.Background(), fake, false); err != nil {
		t.Fatalf("runUpdateCheck (manual): %v", err)
	}
	if !fake.called {
		t.Fatal("a manual update run was blocked by the auto-update opt-out")
	}
}

func TestAutoUpdateDisabledByValue(t *testing.T) {
	disabled := []string{"0", "false", "FALSE", "no", "Off", " off "}
	for _, v := range disabled {
		if !autoUpdateDisabledByValue(v) {
			t.Errorf("autoUpdateDisabledByValue(%q) = false, want true", v)
		}
	}
	enabled := []string{"", "1", "true", "yes", "on", "anything"}
	for _, v := range enabled {
		if autoUpdateDisabledByValue(v) {
			t.Errorf("autoUpdateDisabledByValue(%q) = true, want false", v)
		}
	}
}

func TestUpdaterCreateArgs(t *testing.T) {
	exe := `C:\Program Files\Xuva\xuva-server.exe`
	periodic := updaterCreateArgs(updaterTaskName, exe, []string{"/sc", "HOURLY", "/mo", "6"})
	want := []string{
		"/create", "/tn", `Xuva\XuvaUpdater`,
		"/tr", `"C:\Program Files\Xuva\xuva-server.exe" update --scheduled`,
		"/sc", "HOURLY", "/mo", "6",
		"/ru", "SYSTEM", "/rl", "HIGHEST",
		"/f",
	}
	if strings.Join(periodic, "|") != strings.Join(want, "|") {
		t.Errorf("periodic create args =\n  %v\nwant\n  %v", periodic, want)
	}
	// The exe path must be quoted in /tr so the spaced "Program Files" path runs.
	if !strings.Contains(strings.Join(periodic, " "), `"C:\Program Files\Xuva\xuva-server.exe" update --scheduled`) {
		t.Error("the /tr value must wrap the exe path in quotes")
	}

	boot := updaterCreateArgs(updaterBootTaskName, exe, []string{"/sc", "ONSTART"})
	if !strings.Contains(strings.Join(boot, " "), `/tn Xuva\XuvaUpdater-AtBoot`) ||
		!strings.Contains(strings.Join(boot, " "), "/sc ONSTART") {
		t.Errorf("boot create args missing name/schedule: %v", boot)
	}
}

func TestUpdaterDeleteArgs(t *testing.T) {
	if got := strings.Join(updaterDeleteArgs(updaterTaskName), "|"); got != `/delete|/tn|Xuva\XuvaUpdater|/f` {
		t.Errorf("delete args = %q", got)
	}
}

// Both the periodic and the boot task must be in the managed set, or the
// service would leave one of them unregistered / un-cleaned.
func TestUpdaterTasksCoverPeriodicAndBoot(t *testing.T) {
	names := map[string]bool{}
	for _, tk := range updaterTasks {
		names[tk.name] = true
	}
	if !names[updaterTaskName] || !names[updaterBootTaskName] {
		t.Fatalf("updaterTasks must include both %q and %q, got %v", updaterTaskName, updaterBootTaskName, names)
	}
}

func TestRunUpdateCheckNoMSIAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.0.0",
			"assets": []map[string]any{
				{"name": "xuva-v2.0.0-win-x64.exe", "browser_download_url": "https://example.test/x.exe"},
			},
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("XUVA_UPDATE_RELEASE_API", srv.URL)
	t.Setenv("XUVA_RUNTIME_HOME", t.TempDir())
	setBuildVersion(t, "v1.0.0")

	fake := &fakeApplier{}
	if err := runUpdateCheck(context.Background(), fake, false); err == nil {
		t.Fatal("expected an error when the release has no MSI asset")
	}
	if fake.called {
		t.Fatal("applier called when there was no MSI to apply")
	}
}
