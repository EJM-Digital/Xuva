package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/buildinfo"
	"github.com/EJM-Digital/Xuva/server/internal/config"
	"github.com/EJM-Digital/Xuva/server/internal/updater"
)

// This file is the DESKTOP/tray update path: it checks the release feed and
// stages a Windows installer into pending-update.json for the Electron launcher
// to apply. The MSI is preferred (matches the convergence direction in #451 —
// single Windows artifact, tray bundled into the MSI's TrayFeature), with the
// legacy NSIS .exe used as a fallback while older clients still expect it.
// The headless Windows Service self-updates via the `xuva-server.exe update`
// verb (cmd/Xuva), which unconditionally targets the MSI. Both share the
// feed/download/verify primitives in internal/updater.

type updateStatus struct {
	CurrentVersion         string          `json:"currentVersion"`
	LatestVersion          string          `json:"latestVersion,omitempty"`
	UpdateAvailable        bool            `json:"updateAvailable"`
	ReleaseURL             string          `json:"releaseUrl,omitempty"`
	PublishedAt            string          `json:"publishedAt,omitempty"`
	Assets                 []updater.Asset `json:"assets"`
	DockerImage            string          `json:"dockerImage,omitempty"`
	InstallMode            string          `json:"installMode"`
	ApplySupported         bool            `json:"applySupported"`
	ApplyUnsupportedReason string          `json:"applyUnsupportedReason,omitempty"`
	CheckedAt              string          `json:"checkedAt"`
}

type updateApplyResponse struct {
	Status          string `json:"status"`
	Version         string `json:"version"`
	InstallerPath   string `json:"installerPath"`
	InstallerSHA256 string `json:"installerSha256"`
	PendingPath     string `json:"pendingPath"`
	RequiresRestart bool   `json:"requiresRestart"`
	Message         string `json:"message"`
}

type stagedUpdateRequest struct {
	Version         string `json:"version"`
	InstallerPath   string `json:"installerPath"`
	InstallerSHA256 string `json:"installerSha256"`
	InstallerName   string `json:"installerName"`
	ReleaseURL      string `json:"releaseUrl,omitempty"`
	RequestedAt     string `json:"requestedAt"`
}

func updatesCheckHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, err := checkForUpdates(r.Context(), deps.Config)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, status)
	}
}

func updatesApplyHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := stageUpdateApply(r.Context(), deps.Config)
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, result)
	}
}

func checkForUpdates(ctx context.Context, cfg config.Config) (updateStatus, error) {
	current := buildinfo.Current().Version
	status := updateStatus{
		CurrentVersion:         current,
		Assets:                 []updater.Asset{},
		InstallMode:            "download-and-run-installer",
		ApplySupported:         canApplyUpdates(cfg),
		ApplyUnsupportedReason: updateApplyUnsupportedReason(cfg),
		CheckedAt:              time.Now().UTC().Format(time.RFC3339),
	}

	release, err := updater.FetchLatest(ctx)
	if err != nil {
		return status, err
	}
	status.LatestVersion = release.Version
	status.ReleaseURL = release.URL
	status.PublishedAt = release.PublishedAt
	status.Assets = release.Assets
	status.UpdateAvailable = updater.CompareVersions(release.Version, current) > 0
	status.DockerImage = "ghcr.io/ejm-digital/xuva:" + release.Version
	return status, nil
}

func stageUpdateApply(ctx context.Context, cfg config.Config) (updateApplyResponse, error) {
	status, err := checkForUpdates(ctx, cfg)
	if err != nil {
		return updateApplyResponse{}, err
	}
	if !status.UpdateAvailable {
		return updateApplyResponse{}, fmt.Errorf("no newer Xuva release is available")
	}
	if !status.ApplySupported {
		return updateApplyResponse{}, errors.New(status.ApplyUnsupportedReason)
	}
	// Prefer the MSI (convergence direction — single Windows artifact); fall
	// back to the legacy NSIS .exe if the release omits the MSI. The desktop
	// launcher branches on the staged file's extension to choose msiexec vs
	// NSIS /S, so a release that publishes both will deliver the MSI.
	installer, ok := updater.FindAsset(status.Assets, "windows-msi")
	checksum, _ := updater.FindAsset(status.Assets, "windows-msi-checksum")
	if !ok {
		installer, ok = updater.FindAsset(status.Assets, "windows-installer")
		checksum, _ = updater.FindAsset(status.Assets, "windows-installer-checksum")
	}
	if !ok {
		return updateApplyResponse{}, fmt.Errorf("latest release does not include a Windows installer")
	}

	root := updateRoot(cfg)
	updateDir := filepath.Join(root, "updates", updater.SafePathName(status.LatestVersion))
	if err := os.MkdirAll(updateDir, 0o755); err != nil {
		return updateApplyResponse{}, fmt.Errorf("create update directory: %w", err)
	}
	installerPath := filepath.Join(updateDir, installer.Name)
	if err := updater.Download(ctx, installer.URL, installerPath); err != nil {
		return updateApplyResponse{}, err
	}
	expectedSHA := updater.ExpectedSHA(installer)
	if expectedSHA == "" && checksum.URL != "" {
		checksumPath := filepath.Join(updateDir, checksum.Name)
		if err := updater.Download(ctx, checksum.URL, checksumPath); err != nil {
			return updateApplyResponse{}, err
		}
		expectedSHA = updater.ParseSHA256File(checksumPath)
	}
	// An installer we cannot verify must never be staged. If neither the GitHub
	// asset digest nor a published .exe.sha256 sidecar gave us an expected hash,
	// fail closed rather than handing an unverified binary to the elevated
	// launcher.
	if expectedSHA == "" {
		_ = os.Remove(installerPath)
		return updateApplyResponse{}, fmt.Errorf("release is missing a checksum for the installer; refusing to stage an unverified update")
	}
	actualSHA, err := updater.SHA256File(installerPath)
	if err != nil {
		return updateApplyResponse{}, err
	}
	if !strings.EqualFold(expectedSHA, actualSHA) {
		_ = os.Remove(installerPath)
		return updateApplyResponse{}, fmt.Errorf("installer checksum mismatch")
	}

	request := stagedUpdateRequest{
		Version:         status.LatestVersion,
		InstallerPath:   installerPath,
		InstallerSHA256: actualSHA,
		InstallerName:   installer.Name,
		ReleaseURL:      status.ReleaseURL,
		RequestedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	pendingPath := filepath.Join(root, "updates", "pending-update.json")
	if err := writeJSONFileAtomic(pendingPath, request); err != nil {
		return updateApplyResponse{}, fmt.Errorf("stage update request: %w", err)
	}
	return updateApplyResponse{
		Status:          "staged",
		Version:         request.Version,
		InstallerPath:   request.InstallerPath,
		InstallerSHA256: request.InstallerSHA256,
		PendingPath:     pendingPath,
		RequiresRestart: true,
		Message:         "Update staged. The Windows launcher will stop Xuva, run the installer, and restart the web app.",
	}, nil
}

func canApplyUpdates(cfg config.Config) bool {
	if os.Getenv("XUVA_UPDATE_APPLY_SUPPORTED") == "1" {
		return strings.TrimSpace(updateRoot(cfg)) != ""
	}
	return runtime.GOOS == "windows" && strings.TrimSpace(cfg.RuntimeHome) != ""
}

func updateApplyUnsupportedReason(cfg config.Config) string {
	if canApplyUpdates(cfg) {
		return ""
	}
	if runtime.GOOS != "windows" && os.Getenv("XUVA_UPDATE_APPLY_SUPPORTED") != "1" {
		return "Automatic apply is only supported by the Windows tray launcher. Docker and Linux installs should upgrade by replacing the image or package."
	}
	return "Automatic apply requires a writable Xuva runtime home managed by the Windows launcher."
}

func updateRoot(cfg config.Config) string {
	if root := strings.TrimSpace(cfg.RuntimeHome); root != "" {
		return root
	}
	if dataDir := strings.TrimSpace(cfg.DataDir); dataDir != "" {
		return filepath.Dir(dataDir)
	}
	return ""
}

func writeJSONFileAtomic(path string, payload any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(payload)
	closeErr := file.Close()
	if encodeErr != nil {
		_ = os.Remove(tmp)
		return encodeErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
