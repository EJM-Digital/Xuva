package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/api"
	"github.com/EJM-Digital/Xuva/server/internal/auth"
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
	"github.com/EJM-Digital/Xuva/server/internal/metadata"
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
	"github.com/EJM-Digital/Xuva/server/internal/resources"
	runtimestore "github.com/EJM-Digital/Xuva/server/internal/runtime"
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
)

// JobAutoState tracks the live status of one background automation goroutine.
// The goroutine is the sole writer; the /api/jobs handler reads snapshots.
type JobAutoState struct {
	mu          sync.RWMutex
	Status      string // "idle" | "running" | "paused" | "disabled"
	Enabled     bool
	IntervalMin int
	LastRunAt   time.Time
	LastRunErr  string
	NextRunAt   time.Time
}

func (s *JobAutoState) setStatus(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = status
}

func (s *JobAutoState) markRun(err error, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastRunAt = time.Now().UTC()
	s.NextRunAt = next
	s.Status = "idle"
	if err != nil {
		s.LastRunErr = err.Error()
	} else {
		s.LastRunErr = ""
	}
}

func (s *JobAutoState) Snapshot() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]any{
		"status":       s.Status,
		"enabled":      s.Enabled,
		"intervalMins": s.IntervalMin,
	}
	if !s.LastRunAt.IsZero() {
		out["lastRunAt"] = s.LastRunAt.Format(time.RFC3339)
	}
	if s.LastRunErr != "" {
		out["lastRunError"] = s.LastRunErr
	}
	if !s.NextRunAt.IsZero() {
		out["nextRunAt"] = s.NextRunAt.Format(time.RFC3339)
	}
	return out
}

type Application struct {
	Config    config.Config
	StartedAt time.Time
	cancel    context.CancelFunc
	Database  *database.Service
	Catalog   *catalog.Service
	Auth      *auth.Service
	Events    *events.Bus
	Observe   *observability.Service
	Resources *resources.Manager
	Jobs      *jobs.Registry
	Discovery *discovery.Service

	Libraries     *libraries.Service
	Scanner       *scanner.Service
	Scans         *scans.Service
	Probe         *probe.Service
	Probes        *probes.Service
	Media         *media.Service
	Metadata      *metadata.Service
	Movies        *movies.Service
	TV            *tv.Service
	Playback      *playback.Service
	PlayState     *playstate.Service
	Streaming     *streaming.Service
	Transcode     *transcode.Service
	Subtitles     *subtitles.Service
	Devices       *devices.Service
	Sessions      *sessions.Service
	Downloads     *downloads.Service
	Pairing       *pairing.Service
	Migration     *migration.Service
	Trending      *trending.Service
	Trailers      *trailers.Service
	Thumbnails    *thumbnails.Service
	Notifications *notifications.Service
	Chapters      *chapters.Service
	Watchlist     *watchlist.Service

	// Live automation status — written by background goroutines, read by /api/jobs.
	ScanAuto     *JobAutoState
	MetadataAuto *JobAutoState
	ProbeAuto    *JobAutoState
	// liveConfig holds the most-recently-applied config so automation goroutines
	// can pick up hot-reloaded settings without a restart.
	liveConfig atomic.Value // stores config.Config
}

// CurrentConfig returns the most recently applied configuration.
func (a *Application) CurrentConfig() config.Config {
	if v := a.liveConfig.Load(); v != nil {
		return v.(config.Config)
	}
	return a.Config
}

// UpdateLiveConfig stores a new config so automation goroutines pick it up on
// their next tick. Called by the settings handler after a successful save.
func (a *Application) UpdateLiveConfig(cfg config.Config) {
	a.liveConfig.Store(cfg)
}

func New(ctx context.Context, cfg config.Config) (*Application, error) {
	appCtx, cancel := context.WithCancel(ctx)
	limits := resources.Limits{
		ScanWorkers:      cfg.ScanWorkers,
		ProbeWorkers:     cfg.ProbeWorkers,
		TranscodeWorkers: cfg.TranscodeWorkers,
		GPUWorkers:       cfg.GPUWorkers,
	}
	manager := resources.NewManager(limits)
	bus := events.NewBus(cfg.EventBuffer)
	observe := observability.NewService()
	observe.Subscribe(appCtx, bus)
	databaseService, err := database.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	authService := auth.NewService(databaseService, cfg.AuthDisabled)
	if cfg.AdminPassword != "" {
		if err := authService.Bootstrap(ctx, auth.BootstrapOptions{
			Username: cfg.AdminUsername,
			Password: cfg.AdminPassword,
		}); err != nil {
			return nil, err
		}
	} else if required, err := authService.RequiresBootstrap(ctx); err == nil && required {
		slog.Info("auth bootstrap pending", "reason", "no admin password configured", "next_step", "create first admin from sign-in screen")
	} else if err != nil {
		return nil, err
	}
	catalogService := catalog.NewService(databaseService)
	runtimeStore := runtimestore.NewStore(databaseService)
	libraryService := libraries.NewService()
	if savedLibraries, err := catalogService.ListLibraries(ctx); err == nil {
		for _, library := range savedLibraries {
			libraryService.Set(library)
		}
	}
	if cfg.MovieLibraryPath != "" {
		library, err := catalogService.SaveLibrary(ctx, libraries.Library{
			ID:              "movies",
			Name:            "Movies",
			Path:            cfg.MovieLibraryPath,
			Kind:            libraries.KindMovies,
			MetadataSources: metasources.NormalizeRequestedSourceOrder("movie", cfg.MovieMetadataSources),
			ArtworkSources:  metasources.NormalizeRequestedArtworkOrder("movie", cfg.MovieArtworkSources),
		})
		if err == nil {
			libraryService.Set(library)
		}
	}
	if cfg.TVLibraryPath != "" {
		library, err := catalogService.SaveLibrary(ctx, libraries.Library{
			ID:              "tv",
			Name:            "TV",
			Path:            cfg.TVLibraryPath,
			Kind:            libraries.KindTV,
			MetadataSources: metasources.NormalizeRequestedSourceOrder("series", cfg.SeriesMetadataSources),
			ArtworkSources:  metasources.NormalizeRequestedArtworkOrder("series", cfg.SeriesArtworkSources),
		})
		if err == nil {
			libraryService.Set(library)
		}
	}
	_ = config.SaveFile(cfg.DataDir, cfg)
	_ = ensureRuntimeDirs(cfg)
	_ = catalogService.SaveSettings(ctx, catalog.RuntimeSettings{
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
	})

	jobRegistry := jobs.NewRegistry(manager)
	jobRegistry.Start(appCtx)
	scannerService := scanner.NewService()
	movieService := movies.NewService()
	tvService := tv.NewService()
	metadataService := metadata.NewService(cfg, catalogService, bus)
	scanService := scans.NewService(cfg, bus, jobRegistry.Scan, libraryService, scannerService, catalogService, metadataService, movieService, tvService)
	probeService := probe.NewService(cfg.FFprobePath)
	probesService, err := probes.NewPersistentService(ctx, bus, jobRegistry.Probe, catalogService, probeService, runtimeStore)
	if err != nil {
		return nil, err
	}
	playStateService := playstate.NewService(databaseService, bus)
	transcodeService, err := transcode.NewPersistentService(ctx, bus, jobRegistry.Transcode, cfg.FFmpegPath, cfg.TranscodeDir, runtimeStore)
	if err != nil {
		return nil, err
	}
	// Reap orphaned ffmpeg jobs whose client stopped polling. Check every
	// 30s; cancel anything not touched for 90s. Frontend polls /api/playback/
	// route every 6s while the "Preparing…" panel is shown, so 90s is ~15
	// missed polls — far past any reasonable network blip. Without this,
	// closing the tab on a transcoding file leaves ffmpeg burning CPU.
	transcodeService.RunReaper(ctx, 30*time.Second, 90*time.Second)
	downloadService, err := downloads.NewPersistentService(ctx, bus, jobRegistry.Transcode, cfg.FFmpegPath, cfg.DownloadsDir, runtimeStore)
	if err != nil {
		return nil, err
	}
	sessionService, err := sessions.NewPersistentService(ctx, bus, runtimeStore)
	if err != nil {
		return nil, err
	}
	pairingService := pairing.NewPersistentService(databaseService)
	startRuntimeMaintenance(appCtx, sessionService, transcodeService, probesService, downloadService, pairingService)

	// Per-job automation state objects (written by goroutines, read by /api/jobs).
	scanAuto := &JobAutoState{}
	metaAuto := &JobAutoState{}
	probeAuto := &JobAutoState{}

	// Channel: scan goroutine signals probe goroutine when it finds changed files.
	// Buffered-1 so a fast scan doesn't block waiting for probe to wake up.
	probeSignal := make(chan int, 1)

	// getCfg lets goroutines read the current live config at each tick.
	// We populate liveConfig below after the Application is constructed; the
	// closure is safe because liveConfig is an atomic.Value.
	var liveCfg atomic.Value
	liveCfg.Store(cfg)
	getCfg := func() config.Config {
		if v := liveCfg.Load(); v != nil {
			return v.(config.Config)
		}
		return cfg
	}

	// Start the three independent automation goroutines.
	startScanAutomation(appCtx, getCfg, bus, scanService, sessionService, scanAuto, probeSignal)
	startLibraryWatcher(appCtx, getCfg, bus, scanService, probeSignal)
	startMetadataAutomation(appCtx, getCfg, bus, metadataService, sessionService, metaAuto)
	startProbeAutomation(appCtx, getCfg, bus, probesService, sessionService, probeAuto, probeSignal)
	discoveryService := discovery.NewService(cfg)
	discoveryService.Start(appCtx)
	trendingService := trending.NewService(cfg.TMDBAPIKey, catalogService)
	// Kick off the background trending sampler so /api/client/home never
	// blocks on a TMDB roundtrip (~800 ms cold). Pattern mirrors
	// systemstats.StartSampler — read live config on each tick so a
	// country change in settings flows in without a restart.
	trendingService.StartSampler(appCtx, func() string {
		return getCfg().Country
	})

	// Kick off the background system-stats sampler so /api/system/status
	// returns the latest snapshot in microseconds instead of paying ~750 ms
	// per request (cpuPercent sleeps 120 ms, nvidia-smi exec adds 200-300 ms,
	// per-disk syscalls do the rest). Sampling happens off the request path
	// on a fixed ticker; the handler just reads the cached value. The
	// callback returns the LIVE config so settings changes (data-dir, etc.)
	// flow into subsequent samples without requiring a restart.
	systemstats.StartSampler(appCtx, func() map[string]string {
		live := getCfg()
		return map[string]string{
			"data":      live.DataDir,
			"transcode": live.TranscodeDir,
			"downloads": live.DownloadsDir,
			"metadata":  live.MetadataDir,
			"cache":     live.CacheDir,
			"temp":      live.TempDir,
		}
	})

	// Trailer downloader: spins up a worker pool that yt-dlp's each item's
	// trailer to local MP4 on demand. Plays as a native <video> on the hero —
	// no YouTube embed, no ads, works on a LAN.
	trailersService := trailers.NewService(trailers.Config{
		Enabled:   cfg.TrailersEnabled,
		Dir:       cfg.TrailersDir,
		YTDLPPath: cfg.YTDLPPath,
		Workers:   cfg.TrailerWorkers,
	}, catalogService, bus)
	trailersService.Start(appCtx)
	metadataService.SetTrailers(trailersService)

	// One-time details-json backfill: re-parses raw_json for any TMDB record
	// whose details_json is still '{}' (migration default). Fixes people
	// search on databases created before cast/crew indexing was added.
	// No API calls — purely local re-processing of stored raw_json.
	go func() {
		select {
		case <-appCtx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		if err := metadataService.BackfillDetailsJSON(appCtx); err != nil && appCtx.Err() == nil {
			slog.Warn("details-json backfill failed", "err", err)
		}
	}()

	// People materialization backfill: populates the people/people_credits
	// tables for databases that existed before issue #85 was shipped.
	// Runs 20 s after start so details-json backfill has had a chance to
	// complete first on small databases.
	go func() {
		select {
		case <-appCtx.Done():
			return
		case <-time.After(20 * time.Second):
		}
		if err := catalogService.BackfillPeople(appCtx); err != nil && appCtx.Err() == nil {
			slog.Warn("people backfill failed", "err", err)
		}
	}()

	// Auto-backfill: when TMDB is configured at startup, dispatch a delayed
	// background sweep that fetches TMDB rows for any catalog item missing
	// them (typically wikipedia/filename-only items left over from before
	// the key was added). The 30 s delay lets the rest of the server settle
	// — DB indexes, providers, etc. — before we start hammering TMDB.
	if strings.TrimSpace(cfg.TMDBAPIKey) != "" {
		go func() {
			select {
			case <-appCtx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			missing, err := catalogService.CountItemsMissingProvider(appCtx, "movie", "tmdb")
			if err != nil {
				slog.Debug("auto-backfill skipped (count failed)", "err", err)
				return
			}
			missingSeries, _ := catalogService.CountItemsMissingProvider(appCtx, "series", "tmdb")
			total := missing + missingSeries
			if total == 0 {
				slog.Info("auto-backfill: nothing to do (all items have TMDB metadata)")
				return
			}
			slog.Info("auto-backfill: starting TMDB backfill", "missingItems", total)
			if err := metadataService.StartBackfill(appCtx, "tmdb"); err != nil {
				// ErrBackfillAlreadyRunning is a benign race: the periodic
				// automation goroutine and the boot-time auto-trigger can
				// both fire within the same second on first launch. Demote
				// to Debug so the WARN level stays meaningful for actual
				// failures (DB issues, provider misconfig).
				if errors.Is(err, metadata.ErrBackfillAlreadyRunning) {
					slog.Debug("auto-backfill: already running, skipping concurrent start")
					return
				}
				slog.Warn("auto-backfill failed to start", "err", err)
			}
		}()
	}

	notifService := notifications.NewService(databaseService.DB(), bus)
	notifService.Start(appCtx)

	chaptersService := chapters.NewService(databaseService.DB(), cfg.FFmpegPath, cfg.FpcalcPath)
	watchlistService := watchlist.NewService(databaseService)

	app := &Application{
		Config:        cfg,
		StartedAt:     time.Now().UTC(),
		cancel:        cancel,
		Database:      databaseService,
		Catalog:       catalogService,
		Auth:          authService,
		Events:        bus,
		Observe:       observe,
		Resources:     manager,
		Jobs:          jobRegistry,
		Discovery:     discoveryService,
		Libraries:     libraryService,
		Scanner:       scannerService,
		Scans:         scanService,
		Probe:         probeService,
		Probes:        probesService,
		Media:         media.NewService(),
		Metadata:      metadataService,
		Movies:        movieService,
		TV:            tvService,
		Playback:      playback.NewService(),
		PlayState:     playStateService,
		Streaming:     streaming.NewService(),
		Transcode:     transcodeService,
		Subtitles:     subtitles.NewService(),
		Devices:       devices.NewPersistentService(databaseService),
		Sessions:      sessionService,
		Downloads:     downloadService,
		Pairing:       pairingService,
		Migration:     migration.NewService(databaseService, bus),
		Trending:      trendingService,
		Trailers:      trailersService,
		Thumbnails:    thumbnails.New(cfg.CacheDir, cfg.FFmpegPath, cfg.FFprobePath),
		Notifications: notifService,
		Chapters:      chaptersService,
		Watchlist:     watchlistService,
		ScanAuto:      scanAuto,
		MetadataAuto:  metaAuto,
		ProbeAuto:     probeAuto,
	}
	app.liveConfig.Store(cfg)
	return app, nil
}

func startRuntimeMaintenance(ctx context.Context, sessionService *sessions.Service, transcodeService *transcode.Service, probesService *probes.Service, downloadService *downloads.Service, pairingService *pairing.Service) {
	runRuntimeMaintenance(ctx, sessionService, transcodeService, probesService, downloadService, pairingService)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runRuntimeMaintenance(ctx, sessionService, transcodeService, probesService, downloadService, pairingService)
			}
		}
	}()
}

func runRuntimeMaintenance(ctx context.Context, sessionService *sessions.Service, transcodeService *transcode.Service, probesService *probes.Service, downloadService *downloads.Service, pairingService *pairing.Service) {
	const sessionTTL = 15 * time.Minute
	const terminalRetention = 24 * time.Hour
	expired, _ := sessionService.Cleanup(ctx, sessionTTL, terminalRetention)
	// Cancel any active ffmpeg/transcode job whose session just expired.
	// This covers the case where a client disconnects without sending a stop
	// request (app killed, network lost, Apple TV standby) — the heartbeat TTL
	// fires here every 5 minutes and cleans up orphaned processes.
	// The explicit stop handler (clientPlaybackStopHandler) already handles the
	// clean-exit path; this is the safety-net for the crash/suspend path.
	if transcodeService != nil {
		for _, s := range expired {
			if s.MediaSourceID != "" {
				transcodeService.CancelActiveForMediaSource(s.MediaSourceID)
			}
		}
	}
	_, _ = transcodeService.Cleanup(ctx, terminalRetention)
	_, _ = probesService.Cleanup(ctx, terminalRetention)
	_, _ = downloadService.Cleanup(ctx, terminalRetention)
	if pairingService != nil {
		_, _ = pairingService.Purge(terminalRetention)
	}
}

// scanIntervalFor returns the scan interval from config, enforcing a minimum of 5 minutes.
// Falls back to SyncIntervalMins (legacy) if ScanIntervalMins is unset.
func scanIntervalFor(cfg config.Config) time.Duration {
	mins := cfg.ScanIntervalMins
	if mins <= 0 {
		mins = cfg.SyncIntervalMins
	}
	if mins < 5 {
		mins = 15
	}
	return time.Duration(mins) * time.Minute
}

// playbackActive returns true when at least one session is actively playing
// and the config says we should pause jobs during playback.
func playbackActive(cfg config.Config, sessionService *sessions.Service) bool {
	if cfg.DisableJobPause || sessionService == nil {
		return false
	}
	return len(sessionService.List()) > 0
}

// startScanAutomation runs an independent goroutine that triggers library scans
// on a configurable interval. It is fully isolated from the metadata and probe
// goroutines; coordination with probe happens via probeSignal.
func startScanAutomation(
	ctx context.Context,
	getCfg func() config.Config,
	bus *events.Bus,
	scanService *scans.Service,
	sessionService *sessions.Service,
	state *JobAutoState,
	probeSignal chan<- int,
) {
	cfg := getCfg()
	if cfg.DisableScanAuto || cfg.LibrarySyncMode == "manual" {
		state.mu.Lock()
		state.Status = "disabled"
		state.Enabled = false
		state.mu.Unlock()
		return
	}

	interval := scanIntervalFor(cfg)
	state.mu.Lock()
	state.Status = "idle"
	state.Enabled = true
	state.IntervalMin = int(interval.Minutes())
	state.NextRunAt = time.Now().Add(interval)
	state.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cfg = getCfg()

				// Hot-reload: reset ticker if interval changed.
				newInterval := scanIntervalFor(cfg)
				if newInterval != interval {
					interval = newInterval
					ticker.Reset(interval)
				}

				if cfg.DisableScanAuto || cfg.LibrarySyncMode == "manual" {
					state.mu.Lock()
					state.Status = "disabled"
					state.Enabled = false
					state.mu.Unlock()
					return
				}

				// Pause when playback is active.
				if playbackActive(cfg, sessionService) {
					active := len(sessionService.List())
					bus.Publish("automation.scan.skipped", map[string]any{
						"reason":         "active_playback",
						"activeSessions": active,
					})
					slog.Debug("scan automation skipped: active sessions", "count", active)
					state.setStatus("paused")
					state.mu.Lock()
					state.NextRunAt = time.Now().Add(interval)
					state.mu.Unlock()
					continue
				}

				state.setStatus("running")
				state.mu.Lock()
				state.IntervalMin = int(interval.Minutes())
				state.mu.Unlock()
				bus.Publish("automation.scan.started", map[string]any{"intervalMins": int(interval.Minutes())})

				changedFiles, runErr := runScanJob(ctx, scanService, bus)

				next := time.Now().Add(interval)
				state.markRun(runErr, next)

				// Signal probe goroutine. Non-blocking: if it's already queued, skip.
				if runErr == nil && !cfg.DisableProbeAuto {
					select {
					case probeSignal <- changedFiles:
					default:
					}
				}
			}
		}
	}()
}

// runScanJob executes a full library scan and waits for it to complete.
// Returns the number of changed files found, or an error.
func runScanJob(ctx context.Context, scanService *scans.Service, bus *events.Bus) (int, error) {
	job, err := scanService.Start(ctx, scans.Request{Kind: scans.KindAll})
	if err != nil {
		bus.Publish("automation.scan.failed", map[string]any{"error": err.Error()})
		slog.Debug("scan automation: scan start failed", "error", err)
		return 0, err
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
			current, ok := scanService.Get(job.ID)
			if !ok {
				return 0, nil
			}
			switch current.Status {
			case scans.StatusCompleted:
				bus.Publish("automation.scan.completed", map[string]any{
					"scanId":       job.ID,
					"mediaFiles":   current.MediaFiles,
					"changedFiles": current.ChangedFiles,
				})
				return current.ChangedFiles, nil
			case scans.StatusFailed:
				bus.Publish("automation.scan.failed", map[string]any{"scanId": job.ID, "error": current.Error})
				return 0, errors.New(current.Error)
			}
		}
	}
}

// startMetadataAutomation runs an independent goroutine that triggers metadata
// backfill on its own configurable interval. Completely isolated from scan/probe.
func startMetadataAutomation(
	ctx context.Context,
	getCfg func() config.Config,
	bus *events.Bus,
	metadataService *metadata.Service,
	sessionService *sessions.Service,
	state *JobAutoState,
) {
	cfg := getCfg()
	if cfg.DisableMetadataAuto {
		state.mu.Lock()
		state.Status = "disabled"
		state.Enabled = false
		state.mu.Unlock()
		return
	}

	mins := cfg.MetadataIntervalMins
	if mins < 5 {
		mins = 360
	}
	interval := time.Duration(mins) * time.Minute

	state.mu.Lock()
	state.Status = "idle"
	state.Enabled = true
	state.IntervalMin = mins
	state.NextRunAt = time.Now().Add(interval)
	state.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cfg = getCfg()

				// Hot-reload interval.
				newMins := cfg.MetadataIntervalMins
				if newMins < 5 {
					newMins = 360
				}
				newInterval := time.Duration(newMins) * time.Minute
				if newInterval != interval {
					interval = newInterval
					ticker.Reset(interval)
				}

				if cfg.DisableMetadataAuto {
					state.mu.Lock()
					state.Status = "disabled"
					state.Enabled = false
					state.mu.Unlock()
					return
				}

				if playbackActive(cfg, sessionService) {
					active := len(sessionService.List())
					bus.Publish("automation.metadata.skipped", map[string]any{
						"reason":         "active_playback",
						"activeSessions": active,
					})
					slog.Debug("metadata automation skipped: active sessions", "count", active)
					state.setStatus("paused")
					state.mu.Lock()
					state.NextRunAt = time.Now().Add(interval)
					state.mu.Unlock()
					continue
				}

				state.setStatus("running")
				bus.Publish("automation.metadata.started", nil)

				err := metadataService.StartBackfill(ctx, "tmdb")
				if err != nil {
					if metadata.IsBackfillAlreadyRunning(err) {
						slog.Debug("metadata automation: backfill already running")
						bus.Publish("automation.metadata.skipped", map[string]any{"reason": "backfill already running"})
						err = nil
					} else if metadata.IsBackfillProviderNotConfigured(err) {
						slog.Warn("metadata automation: TMDB provider not configured; metadata run skipped")
						bus.Publish("automation.metadata.skipped", map[string]any{"reason": "provider tmdb is not configured"})
						err = nil
					} else {
						slog.Debug("metadata automation: backfill skipped", "reason", err)
						bus.Publish("automation.metadata.skipped", map[string]any{"reason": err.Error()})
					}
				} else {
					bus.Publish("automation.metadata.started", nil)
					slog.Debug("metadata automation: backfill started")
				}

				next := time.Now().Add(interval)
				state.markRun(err, next)
			}
		}
	}()
}

// startProbeAutomation runs an independent goroutine that triggers ffprobe jobs.
// It fires when the scan goroutine signals via probeSignal (event-driven, no own
// timer). It can also be triggered on-demand via POST /api/probes.
func startProbeAutomation(
	ctx context.Context,
	getCfg func() config.Config,
	bus *events.Bus,
	probesService *probes.Service,
	sessionService *sessions.Service,
	state *JobAutoState,
	probeSignal <-chan int,
) {
	cfg := getCfg()
	enabled := !cfg.DisableProbeAuto
	state.mu.Lock()
	if enabled {
		state.Status = "idle"
	} else {
		state.Status = "disabled"
	}
	state.Enabled = enabled
	state.mu.Unlock()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case changedFiles := <-probeSignal:
				cfg = getCfg()

				if cfg.DisableProbeAuto {
					state.mu.Lock()
					state.Status = "disabled"
					state.Enabled = false
					state.mu.Unlock()
					continue
				}

				if changedFiles == 0 {
					// Scan found no new/changed files; no probe needed.
					slog.Debug("probe automation: skipped (no changed files from scan)")
					continue
				}

				if playbackActive(cfg, sessionService) {
					active := len(sessionService.List())
					bus.Publish("automation.probe.skipped", map[string]any{
						"reason":         "active_playback",
						"activeSessions": active,
					})
					slog.Debug("probe automation skipped: active sessions", "count", active)
					state.setStatus("paused")
					continue
				}

				state.setStatus("running")
				bus.Publish("automation.probe.started", map[string]any{"changedFiles": changedFiles})

				_, err := probesService.Start(ctx, probes.Request{Limit: cfg.ProbeBatchLimit})
				if err != nil {
					slog.Debug("probe automation: probe start failed", "error", err)
					bus.Publish("automation.probe.failed", map[string]any{"error": err.Error()})
					state.markRun(err, time.Time{})
				} else {
					bus.Publish("automation.probe.queued", map[string]any{"batchLimit": cfg.ProbeBatchLimit})
					state.markRun(nil, time.Time{})
				}
			}
		}
	}()
}

func ensureRuntimeDirs(cfg config.Config) error {
	for _, dir := range []string{cfg.DataDir, cfg.LogDir, cfg.TranscodeDir, cfg.DownloadsDir, cfg.MetadataDir, cfg.CacheDir, cfg.TempDir, cfg.TrailersDir} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (a *Application) Router() http.Handler {
	// Machine-bound secret store for sealing SMB share credentials. A failure
	// here is non-fatal: credential storage is simply unavailable (handlers
	// nil-check), but the rest of the server runs normally.
	secretStore, err := secret.New(a.Config.DataDir)
	if err != nil {
		slog.Error("secret store init failed; network credential storage disabled", "error", err)
	}
	return api.NewRouter(api.Deps{
		Config:        a.Config,
		Secret:        secretStore,
		StartedAt:     a.StartedAt,
		Database:      a.Database,
		Auth:          a.Auth,
		Events:        a.Events,
		Observe:       a.Observe,
		Resources:     a.Resources,
		Jobs:          a.Jobs,
		Discovery:     a.Discovery,
		Libraries:     a.Libraries,
		Scanner:       a.Scanner,
		Scans:         a.Scans,
		Catalog:       a.Catalog,
		Media:         a.Media,
		Metadata:      a.Metadata,
		Movies:        a.Movies,
		TV:            a.TV,
		Probe:         a.Probe,
		Probes:        a.Probes,
		Playback:      a.Playback,
		PlayState:     a.PlayState,
		Streaming:     a.Streaming,
		Transcode:     a.Transcode,
		Downloads:     a.Downloads,
		Devices:       a.Devices,
		Sessions:      a.Sessions,
		Subtitles:     a.Subtitles,
		Pairing:       a.Pairing,
		Trending:      a.Trending,
		Trailers:      a.Trailers,
		Thumbnails:    a.Thumbnails,
		Migration:     a.Migration,
		Notifications: a.Notifications,
		Chapters:      a.Chapters,
		Watchlist:     a.Watchlist,
		ScanAuto:      a.ScanAuto,
		MetadataAuto:  a.MetadataAuto,
		ProbeAuto:     a.ProbeAuto,
	})
}

func (a *Application) Shutdown(context.Context) {
	a.cancel()
	if a.Discovery != nil {
		a.Discovery.Shutdown()
	}
	a.Events.Close()
	_ = a.Database.Close()
}
