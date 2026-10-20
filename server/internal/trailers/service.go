// Package trailers implements a self-hosted trailer downloader.
//
// When a metadata refresh produces a YouTube videoKey, the metadata service
// hands it to trailers.Service.Queue(). A small worker pool shells out to
// yt-dlp to fetch the MP4 into <data>/trailers/{tmdb_id}.mp4 and writes the
// resulting path back onto every provider row of the matching item via
// catalog.SetTrailerPath. The hero / detail API then serves the local file
// (with Range support) via /api/trailers/{tmdb_id}.mp4 — no YouTube embed,
// no ads, works on a LAN with no internet, plays as a native <video>.
//
// Design rules:
//   - Idempotent: duplicate Queue() calls for the same item are dropped.
//   - Resilient: yt-dlp failures back off for an hour per-item, so a flaky
//     network can't pin a worker against a single broken trailer.
//   - Cheap to disable: TrailersEnabled=false skips every code path.
//   - No catalog import cycle: we depend on a tiny Catalog interface only.
package trailers

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/events"
)

// Catalog is the subset of catalog.Service the trailer worker needs. Keeping
// it as an interface lets us mock for tests and avoid import cycles.
type Catalog interface {
	SetTrailerPath(ctx context.Context, kind string, itemID string, trailerPath string) error
}

type Config struct {
	Enabled   bool
	Dir       string // <data>/trailers
	YTDLPPath string // "yt-dlp" or absolute path
	Workers   int    // concurrent downloads; default 1
}

type Job struct {
	Kind     string // "movie" | "series"
	ItemID   string // catalog item ID
	TMDBID   string // TMDB external ID — used as the on-disk filename
	VideoKey string // YouTube key
}

type Service struct {
	cfg     Config
	catalog Catalog
	events  *events.Bus

	queue chan Job

	mu       sync.Mutex
	inflight map[string]struct{} // key = kind:itemID
	failures map[string]time.Time
	// unavailable is set at Start when CheckYTDLP fails. Queue() and the
	// worker pool become no-ops so we don't spam logs with one "yt-dlp not
	// found" line per queued job (observed: 1828 identical warnings in a
	// 15-hour session before this guard).
	unavailable bool
}

const (
	// retryBackoff is how long we wait before retrying after a yt-dlp failure
	// for the same item. A network blip shouldn't pin a worker forever.
	retryBackoff = 1 * time.Hour
	// queueCapacity bounds in-memory queued jobs. Library refreshes can burst
	// thousands of metadata updates; we let the rest expire and re-queue on
	// the next refresh rather than spiking memory.
	queueCapacity = 256
)

func NewService(cfg Config, cat Catalog, eventBus *events.Bus) *Service {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	return &Service{
		cfg:      cfg,
		catalog:  cat,
		events:   eventBus,
		queue:    make(chan Job, queueCapacity),
		inflight: make(map[string]struct{}, 64),
		failures: make(map[string]time.Time, 64),
	}
}

// Start spins up the configured number of worker goroutines. No-op when the
// feature is disabled OR when the yt-dlp binary isn't on PATH — in the
// latter case we set s.unavailable so subsequent Queue() calls drop
// silently instead of triggering one warning per item (an empty install
// can otherwise generate thousands of identical "yt-dlp not found" lines).
func (s *Service) Start(ctx context.Context) {
	if !s.cfg.Enabled {
		return
	}
	if err := CheckYTDLP(s.cfg.YTDLPPath); err != nil {
		s.mu.Lock()
		s.unavailable = true
		s.mu.Unlock()
		slog.Warn("trailers: yt-dlp unavailable, disabling trailer downloads",
			"path", s.cfg.YTDLPPath,
			"err", err,
			"hint", "install yt-dlp and put it on PATH (or set XUVA_YTDLP_PATH) to enable local trailer caching",
		)
		return
	}
	if err := os.MkdirAll(s.cfg.Dir, 0o755); err != nil {
		slog.Warn("trailers: could not create cache dir", "dir", s.cfg.Dir, "err", err)
		return
	}
	for i := 0; i < s.cfg.Workers; i++ {
		go s.runWorker(ctx)
	}
	slog.Info("trailers: worker pool started", "workers", s.cfg.Workers, "dir", s.cfg.Dir)
}

// isUnavailable returns true when CheckYTDLP failed at startup. Threadsafe.
func (s *Service) isUnavailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unavailable
}

// Queue enqueues a trailer-download job. The call is fire-and-forget — it
// never blocks, never returns errors to the caller, and silently drops if
// the queue is full or a download is already in flight for the same item.
// If the MP4 already exists on disk, we just patch the DB and skip work.
func (s *Service) Queue(job Job) {
	if !s.cfg.Enabled || s.isUnavailable() {
		return
	}
	if strings.TrimSpace(job.TMDBID) == "" || strings.TrimSpace(job.VideoKey) == "" {
		return
	}
	if strings.TrimSpace(job.Kind) == "" || strings.TrimSpace(job.ItemID) == "" {
		return
	}

	// Fast path: file already present. Just make sure the DB row knows about
	// it (handles the case where the DB was wiped but the cache wasn't).
	target := s.LocalPath(job.TMDBID)
	if fi, err := os.Stat(target); err == nil && fi.Size() > 0 {
		if err := s.catalog.SetTrailerPath(context.Background(), job.Kind, job.ItemID, target); err != nil {
			slog.Warn("trailers: failed to patch trailer_path (cache hit)", "err", err)
		}
		return
	}

	key := dedupeKey(job)
	s.mu.Lock()
	if _, busy := s.inflight[key]; busy {
		s.mu.Unlock()
		return
	}
	if last, ok := s.failures[key]; ok && time.Since(last) < retryBackoff {
		s.mu.Unlock()
		return
	}
	s.inflight[key] = struct{}{}
	s.mu.Unlock()

	select {
	case s.queue <- job:
		// queued
	default:
		// Queue saturated — release the inflight slot so the next refresh
		// cycle is allowed to retry.
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
		slog.Warn("trailers: queue saturated, dropping job", "videoKey", job.VideoKey)
	}
}

// LocalPath returns the deterministic on-disk path for a given TMDB id.
// Exposed so the HTTP handler can resolve {tmdbID}.mp4 → file.
func (s *Service) LocalPath(tmdbID string) string {
	id := strings.TrimSpace(tmdbID)
	if id == "" {
		return ""
	}
	return filepath.Join(s.cfg.Dir, id+".mp4")
}

// Dir exposes the trailer cache root so callers (e.g. tests, the path
// validator on the HTTP handler) can resolve safely.
func (s *Service) Dir() string { return s.cfg.Dir }

// Enabled reports whether the downloader will accept jobs.
func (s *Service) Enabled() bool { return s.cfg.Enabled }

func (s *Service) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.queue:
			s.process(ctx, job)
		}
	}
}

func (s *Service) process(ctx context.Context, job Job) {
	key := dedupeKey(job)
	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
	}()

	target := s.LocalPath(job.TMDBID)
	// Race-window catch: another worker (or a previous run) may have finished
	// between Queue() and here.
	if fi, err := os.Stat(target); err == nil && fi.Size() > 0 {
		_ = s.catalog.SetTrailerPath(ctx, job.Kind, job.ItemID, target)
		return
	}

	tempTarget := target + ".part"
	_ = os.Remove(tempTarget)

	args := []string{
		"--no-warnings",
		"--no-progress",
		"--no-playlist",
		"--no-call-home",
		"--retries", "2",
		"--socket-timeout", "30",
		"--no-mtime",
		"--restrict-filenames",
		// Prefer ≤1080p MP4 (H.264 + AAC) for max browser compat. yt-dlp will
		// remux on the fly if it has to pick separate streams.
		"--format", "bv*[height<=1080][ext=mp4]+ba[ext=m4a]/bv*[height<=1080]+ba/b[height<=1080]/b",
		"--merge-output-format", "mp4",
		"--output", tempTarget,
		"https://www.youtube.com/watch?v=" + job.VideoKey,
	}

	cmd := exec.CommandContext(ctx, s.cfg.YTDLPPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.recordFailure(key)
		slog.Warn("trailers: yt-dlp failed",
			"kind", job.Kind,
			"itemID", job.ItemID,
			"tmdbID", job.TMDBID,
			"videoKey", job.VideoKey,
			"err", err,
			"output", strings.TrimSpace(string(out)),
		)
		_ = os.Remove(tempTarget)
		if s.events != nil {
			s.events.Publish("trailer.failed", map[string]any{
				"kind":     job.Kind,
				"itemID":   job.ItemID,
				"videoKey": job.VideoKey,
				"error":    err.Error(),
			})
		}
		return
	}

	// yt-dlp can append extension variations depending on remux outcome.
	// Resolve the actual produced file before renaming.
	produced, ok := resolveProduced(tempTarget, target)
	if !ok {
		slog.Warn("trailers: produced file not found", "tempTarget", tempTarget, "target", target)
		s.recordFailure(key)
		return
	}
	if produced != target {
		if err := os.Rename(produced, target); err != nil {
			slog.Warn("trailers: rename failed", "from", produced, "to", target, "err", err)
			s.recordFailure(key)
			return
		}
	}

	if err := s.catalog.SetTrailerPath(ctx, job.Kind, job.ItemID, target); err != nil {
		slog.Warn("trailers: failed to patch trailer_path", "err", err)
		return
	}

	slog.Info("trailers: downloaded",
		"kind", job.Kind,
		"itemID", job.ItemID,
		"tmdbID", job.TMDBID,
		"videoKey", job.VideoKey,
		"path", target,
	)
	if s.events != nil {
		s.events.Publish("trailer.downloaded", map[string]any{
			"kind":     job.Kind,
			"itemID":   job.ItemID,
			"tmdbID":   job.TMDBID,
			"videoKey": job.VideoKey,
			"path":     target,
		})
	}
}

func (s *Service) recordFailure(key string) {
	s.mu.Lock()
	s.failures[key] = time.Now()
	s.mu.Unlock()
}

func dedupeKey(j Job) string { return j.Kind + ":" + j.ItemID }

// resolveProduced finds the file yt-dlp actually wrote. Normally it's
// tempTarget, but if --merge-output-format had to remux it could have
// landed at the final target name or with an alternative extension.
func resolveProduced(tempTarget string, target string) (string, bool) {
	if fi, err := os.Stat(tempTarget); err == nil && fi.Size() > 0 {
		return tempTarget, true
	}
	if fi, err := os.Stat(target); err == nil && fi.Size() > 0 {
		return target, true
	}
	// As a last resort, scan the directory for "<tmdbID>.*" — yt-dlp will
	// sometimes write straight to e.g. .webm if remux fails silently.
	dir := filepath.Dir(target)
	base := strings.TrimSuffix(filepath.Base(target), ".mp4")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if strings.HasPrefix(name, base+".") && !strings.HasSuffix(name, ".part") {
			return filepath.Join(dir, name), true
		}
	}
	return "", false
}

// PickBestTrailer extracts the best YouTube key from a list of TMDB video
// records. Preference order: official trailer > any trailer > teaser > any
// YouTube video. Empty string when none match.
//
// Exported here (rather than in router.go) so the metadata service can
// stash the key on MetadataRecord at fetch time, avoiding a second JSON
// parse later in the request hot path.
type VideoCandidate struct {
	Key      string
	Site     string
	Type     string
	Official bool
}

func PickBestTrailer(videos []VideoCandidate) string {
	var officialTrailer, anyTrailer, teaser, anyYouTube string
	for _, v := range videos {
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

// CheckYTDLP verifies yt-dlp is callable. Returns nil if it works.
// Useful for startup diagnostics.
func CheckYTDLP(binaryPath string) error {
	if strings.TrimSpace(binaryPath) == "" {
		return errors.New("yt-dlp path not configured")
	}
	cmd := exec.Command(binaryPath, "--version")
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}
