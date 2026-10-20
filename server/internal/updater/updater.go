// Package updater is the single source of truth for talking to the Xuva release
// feed: discovering the latest release, classifying its assets, downloading one,
// and verifying its SHA-256. Both the desktop update HTTP handlers
// (internal/api) and the headless `xuva-server.exe update` verb (cmd/Xuva, the
// Windows Service self-updater) build on these primitives, so the security-
// sensitive download+verify logic exists in exactly one place.
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/buildinfo"
)

// DefaultReleaseAPI is the GitHub "latest release" endpoint polled when
// XUVA_UPDATE_RELEASE_API is unset.
const DefaultReleaseAPI = "https://api.github.com/repos/EJM-Digital/Xuva/releases/latest"

// MaxDownloadBytes caps a single asset download so a hostile or broken feed
// can't fill the disk. Comfortably larger than the ~108MB MSI.
const MaxDownloadBytes = 700 * 1024 * 1024

// Asset is one downloadable file attached to a release. The JSON tags match the
// shape the desktop /api/system/updates response has always returned.
type Asset struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
	Digest      string `json:"digest,omitempty"`
	PackageType string `json:"packageType"`
}

// Release is the latest published release, with its assets already classified.
type Release struct {
	Version     string // the tag, e.g. "v1.2.3"
	URL         string // human-facing release page
	PublishedAt string
	Assets      []Asset
}

// githubRelease is the subset of the GitHub releases JSON we decode.
type githubRelease struct {
	TagName     string `json:"tag_name"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
		Digest             string `json:"digest"`
	} `json:"assets"`
}

// ReleaseAPIURL is the feed to poll: XUVA_UPDATE_RELEASE_API when set (handy for
// tests and self-hosted mirrors), else DefaultReleaseAPI.
func ReleaseAPIURL() string {
	if v := strings.TrimSpace(os.Getenv("XUVA_UPDATE_RELEASE_API")); v != "" {
		return v
	}
	return DefaultReleaseAPI
}

func userAgent() string {
	return "Xuva/" + buildinfo.Current().Version
}

// FetchLatest queries the release feed and returns the latest release with its
// assets classified by PackageType.
func FetchLatest(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ReleaseAPIURL(), nil)
	if err != nil {
		return Release{}, fmt.Errorf("build update request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent())

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("update check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Release{}, fmt.Errorf("update check failed with HTTP %d", resp.StatusCode)
	}

	var gh githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&gh); err != nil {
		return Release{}, fmt.Errorf("decode update response: %w", err)
	}
	out := Release{
		Version:     strings.TrimSpace(gh.TagName),
		URL:         strings.TrimSpace(gh.HTMLURL),
		PublishedAt: strings.TrimSpace(gh.PublishedAt),
		Assets:      []Asset{},
	}
	for _, a := range gh.Assets {
		name := strings.TrimSpace(a.Name)
		url := strings.TrimSpace(a.BrowserDownloadURL)
		if name == "" || url == "" {
			continue
		}
		out.Assets = append(out.Assets, Asset{
			Name:        name,
			URL:         url,
			Size:        a.Size,
			Digest:      strings.TrimSpace(a.Digest),
			PackageType: ClassifyAsset(name),
		})
	}
	return out, nil
}

// ClassifyAsset maps a release asset filename to a stable package-type key. The
// checksum suffixes must be tested before the bare extensions, or a ".msi.sha256"
// sidecar would be mistaken for the installer itself.
func ClassifyAsset(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.HasSuffix(lower, ".msi.sha256"):
		return "windows-msi-checksum"
	case strings.HasSuffix(lower, ".exe.sha256"):
		return "windows-installer-checksum"
	case strings.HasSuffix(lower, ".zip.sha256"):
		return "windows-portable-checksum"
	case strings.HasSuffix(lower, ".msi"):
		return "windows-msi"
	case strings.HasSuffix(lower, ".exe"):
		return "windows-installer"
	case strings.HasSuffix(lower, ".zip"):
		return "windows-portable"
	default:
		return "asset"
	}
}

// FindAsset returns the first asset of the given package type.
func FindAsset(assets []Asset, packageType string) (Asset, bool) {
	for _, a := range assets {
		if a.PackageType == packageType {
			return a, true
		}
	}
	return Asset{}, false
}

// ExpectedSHA is the asset's published SHA-256 with any "sha256:" prefix removed,
// or "" if the feed didn't carry a digest for it.
func ExpectedSHA(a Asset) string {
	return strings.TrimPrefix(strings.TrimSpace(a.Digest), "sha256:")
}

var releaseVersionPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// CompareVersions returns 1 if a > b, -1 if a < b, 0 if equal OR if either side
// is unparseable. The unparseable-is-zero rule is deliberate: an un-versioned
// "dev" build (or a pre-release tag) is never treated as an upgrade target, so
// the updater can't be tricked into "upgrading" to something it can't order.
func CompareVersions(a, b string) int {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0
	}
	for i := 0; i < len(av); i++ {
		if av[i] > bv[i] {
			return 1
		}
		if av[i] < bv[i] {
			return -1
		}
	}
	return 0
}

func parseVersion(value string) ([3]int, bool) {
	var out [3]int
	m := releaseVersionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(m) != 4 {
		return out, false
	}
	for i := 1; i < len(m); i++ {
		n, err := strconv.Atoi(m[i])
		if err != nil {
			return out, false
		}
		out[i-1] = n
	}
	return out, true
}

// SafePathName turns a version string into a single safe path segment (used to
// namespace a release's download directory).
func SafePathName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, string(filepath.Separator), "-")
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ReplaceAll(value, "\\", "-")
	if value == "" {
		return "unknown"
	}
	return value
}

// Download streams rawURL to destination (atomically, via a .tmp rename),
// refusing anything larger than MaxDownloadBytes.
func Download(ctx context.Context, rawURL string, destination string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent())
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download update asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download update asset failed with HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create update asset directory: %w", err)
	}
	tmp := destination + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create update asset: %w", err)
	}
	_, copyErr := io.Copy(file, io.LimitReader(resp.Body, MaxDownloadBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write update asset: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close update asset: %w", closeErr)
	}
	if info, err := os.Stat(tmp); err == nil && info.Size() > MaxDownloadBytes {
		_ = os.Remove(tmp)
		return fmt.Errorf("update asset exceeds maximum allowed size")
	}
	if err := os.Rename(tmp, destination); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("move update asset into place: %w", err)
	}
	return nil
}

// SHA256File returns the lowercase hex SHA-256 of the file at path.
func SHA256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// ParseSHA256File reads a "<hex>  <filename>" checksum sidecar and returns the
// 64-hex digest, or "" if the file is missing or malformed.
func ParseSHA256File(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return ""
	}
	value := strings.TrimPrefix(strings.TrimSpace(fields[0]), "sha256:")
	if len(value) != sha256.Size*2 {
		return ""
	}
	return value
}
