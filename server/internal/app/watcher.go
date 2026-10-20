package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/EJM-Digital/Xuva/server/internal/config"
	"github.com/EJM-Digital/Xuva/server/internal/events"
	"github.com/EJM-Digital/Xuva/server/internal/scans"
)

const watchDebounce = 30 * time.Second

// startLibraryWatcher watches MovieLibraryPath and TVLibraryPath for filesystem
// events and triggers an incremental scan after a 30-second debounce. It runs
// alongside the periodic scan automation — the two are additive, not exclusive.
//
// Debounce reasoning: large file copies emit continuous Write events; waiting
// 30 s after the last event means the scan usually fires on a complete file.
// Create/Rename/Remove always reset the timer; Write events only do so for
// recognised media file extensions.
func startLibraryWatcher(
	ctx context.Context,
	getCfg func() config.Config,
	bus *events.Bus,
	scanService *scans.Service,
	probeSignal chan<- int,
) {
	cfg := getCfg()
	var dirs []string
	if cfg.MovieLibraryPath != "" {
		dirs = append(dirs, cfg.MovieLibraryPath)
	}
	if cfg.TVLibraryPath != "" && cfg.TVLibraryPath != cfg.MovieLibraryPath {
		dirs = append(dirs, cfg.TVLibraryPath)
	}
	if len(dirs) == 0 {
		return
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("library watcher: could not create watcher", "error", err)
		return
	}

	watching := 0
	for _, dir := range dirs {
		if err := w.Add(dir); err != nil {
			slog.Warn("library watcher: could not watch directory", "dir", dir, "error", err)
		} else {
			slog.Info("library watcher: watching for changes", "dir", dir)
			watching++
		}
	}
	if watching == 0 {
		w.Close()
		return
	}

	go func() {
		defer w.Close()

		var (
			debounceTimer *time.Timer
			scanRunning   atomic.Bool
		)

		// resetDebounce arms or re-arms the debounce timer. Must only be called
		// from the select loop (single goroutine).
		resetDebounce := func() {
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(watchDebounce)
				return
			}
			if !debounceTimer.Stop() {
				select {
				case <-debounceTimer.C:
				default:
				}
			}
			debounceTimer.Reset(watchDebounce)
		}

		triggerScan := func() {
			cfg := getCfg()
			if cfg.DisableScanAuto || cfg.LibrarySyncMode == "manual" {
				slog.Debug("library watcher: scan skipped (auto disabled or manual mode)")
				return
			}
			if scanRunning.Swap(true) {
				slog.Debug("library watcher: scan already in progress, skipping trigger")
				return
			}
			go func() {
				defer scanRunning.Store(false)
				slog.Info("library watcher: filesystem change detected, triggering scan")
				bus.Publish("automation.scan.started", map[string]any{"trigger": "filesystem_watch"})
				changed, err := runScanJob(ctx, scanService, bus)
				if err != nil {
					return
				}
				if !getCfg().DisableProbeAuto && changed > 0 {
					select {
					case probeSignal <- changed:
					default:
					}
				}
			}()
		}

		for {
			// Re-evaluate the debounce channel each iteration. A nil channel
			// in a select case is never selected — this is idiomatic Go for
			// "optional" cases.
			var debounceCh <-chan time.Time
			if debounceTimer != nil {
				debounceCh = debounceTimer.C
			}

			select {
			case <-ctx.Done():
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				return

			case <-debounceCh:
				debounceTimer = nil
				triggerScan()

			case event, ok := <-w.Events:
				if !ok {
					return
				}
				// Ignore pure permission changes — they don't affect library contents.
				if event.Has(fsnotify.Chmod) && !event.Has(fsnotify.Create) && !event.Has(fsnotify.Rename) && !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Write) {
					continue
				}
				// Throttle Write events to media files only — avoids thrashing on
				// metadata DB writes or subtitle downloads happening in the same dir.
				if event.Has(fsnotify.Write) &&
					!event.Has(fsnotify.Create) &&
					!event.Has(fsnotify.Rename) &&
					!event.Has(fsnotify.Remove) &&
					!isMediaFileExt(event.Name) {
					continue
				}
				slog.Debug("library watcher: filesystem event", "op", event.Op, "name", event.Name)
				resetDebounce()

			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				slog.Warn("library watcher: watcher error", "error", err)
			}
		}
	}()
}

func isMediaFileExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mkv", ".mp4", ".avi", ".m4v", ".mov", ".wmv", ".ts", ".m2ts", ".iso", ".m4p", ".mpg", ".mpeg":
		return true
	}
	return false
}
