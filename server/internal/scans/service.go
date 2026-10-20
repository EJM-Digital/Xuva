package scans

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/catalog"
	"github.com/EJM-Digital/Xuva/server/internal/config"
	"github.com/EJM-Digital/Xuva/server/internal/events"
	"github.com/EJM-Digital/Xuva/server/internal/jobs"
	"github.com/EJM-Digital/Xuva/server/internal/libraries"
	"github.com/EJM-Digital/Xuva/server/internal/metadata"
	"github.com/EJM-Digital/Xuva/server/internal/movies"
	"github.com/EJM-Digital/Xuva/server/internal/scanner"
	"github.com/EJM-Digital/Xuva/server/internal/tv"
)

type Kind string

const (
	KindMovies Kind = "movies"
	KindTV     Kind = "tv"
	KindAll    Kind = "all"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

type Request struct {
	LibraryID   string `json:"libraryId,omitempty"`
	Kind        Kind   `json:"kind"`
	Path        string `json:"path,omitempty"`
	MoviesPath  string `json:"moviesPath,omitempty"`
	TVPath      string `json:"tvPath,omitempty"`
	SampleLimit int    `json:"sampleLimit,omitempty"`
}

type Job struct {
	ID           string         `json:"id"`
	LibraryID    string         `json:"libraryId,omitempty"`
	Kind         Kind           `json:"kind"`
	Status       Status         `json:"status"`
	Path         string         `json:"path,omitempty"`
	MoviesPath   string         `json:"moviesPath,omitempty"`
	TVPath       string         `json:"tvPath,omitempty"`
	CreatedAt    time.Time      `json:"createdAt"`
	StartedAt    time.Time      `json:"startedAt,omitempty"`
	CompletedAt  time.Time      `json:"completedAt,omitempty"`
	TotalFiles   int            `json:"totalFiles"`
	MediaFiles   int            `json:"mediaFiles"`
	ChangedFiles int            `json:"changedFiles"`
	Unchanged    int            `json:"unchangedFiles"`
	IgnoredFiles int            `json:"ignoredFiles"`
	LastPath     string         `json:"lastPath,omitempty"`
	Error        string         `json:"error,omitempty"`
	Result       map[string]any `json:"result,omitempty"`
}

type Service struct {
	cfg       config.Config
	events    *events.Bus
	queue     *jobs.Queue
	libraries *libraries.Service
	scanner   *scanner.Service
	catalog   *catalog.Service
	metadata  *metadata.Service
	movies    *movies.Service
	tv        *tv.Service

	nextID atomic.Uint64
	mu     sync.RWMutex
	jobs   map[string]Job
}

func NewService(cfg config.Config, eventBus *events.Bus, queue *jobs.Queue, librariesService *libraries.Service, scannerService *scanner.Service, catalogService *catalog.Service, metadataService *metadata.Service, movieService *movies.Service, tvService *tv.Service) *Service {
	return &Service{
		cfg:       cfg,
		events:    eventBus,
		queue:     queue,
		libraries: librariesService,
		scanner:   scannerService,
		catalog:   catalogService,
		metadata:  metadataService,
		movies:    movieService,
		tv:        tvService,
		jobs:      make(map[string]Job),
	}
}

func (s *Service) Start(ctx context.Context, request Request) (Job, error) {
	if request.Kind == "" {
		request.Kind = KindAll
	}
	job := Job{
		ID:         s.nextJobID(),
		LibraryID:  request.LibraryID,
		Kind:       request.Kind,
		Status:     StatusQueued,
		Path:       request.Path,
		MoviesPath: request.MoviesPath,
		TVPath:     request.TVPath,
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.validate(job); err != nil {
		return Job{}, err
	}
	s.store(job)
	s.publish("scan.queued", job)

	if err := s.queue.Submit(ctx, func(workerCtx context.Context) {
		s.run(workerCtx, job.ID, request)
	}); err != nil {
		job.Status = StatusFailed
		job.Error = err.Error()
		job.CompletedAt = time.Now().UTC()
		s.store(job)
		s.publish("scan.failed", job)
		return Job{}, err
	}
	return job, nil
}

func (s *Service) Get(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	return job, ok
}

func (s *Service) List() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	output := make([]Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		output = append(output, job)
	}
	sort.Slice(output, func(i, j int) bool {
		return output[i].CreatedAt.After(output[j].CreatedAt)
	})
	return output
}

func (s *Service) validate(job Job) error {
	switch job.Kind {
	case KindMovies:
		if firstNonEmpty(job.Path, job.MoviesPath, s.cfg.MovieLibraryPath, firstLibraryPathByKind(s.libraries, libraries.KindMovies)) == "" {
			return errors.New("movie library path is required")
		}
	case KindTV:
		if firstNonEmpty(job.Path, job.TVPath, s.cfg.TVLibraryPath, firstLibraryPathByKind(s.libraries, libraries.KindTV)) == "" {
			return errors.New("tv library path is required")
		}
	case KindAll:
		if len(s.libraries.List()) == 0 &&
			firstNonEmpty(job.MoviesPath, s.cfg.MovieLibraryPath, firstLibraryPathByKind(s.libraries, libraries.KindMovies)) == "" &&
			firstNonEmpty(job.TVPath, s.cfg.TVLibraryPath, firstLibraryPathByKind(s.libraries, libraries.KindTV)) == "" {
			return errors.New("at least one movie or tv library path is required")
		}
	default:
		return errors.New("unknown scan kind")
	}
	return nil
}

func (s *Service) run(ctx context.Context, id string, request Request) {
	job, ok := s.Get(id)
	if !ok {
		return
	}
	job.Status = StatusRunning
	job.StartedAt = time.Now().UTC()
	job.Result = map[string]any{}
	s.store(job)
	s.publish("scan.started", job)

	switch request.Kind {
	case KindMovies:
		if request.LibraryID != "" {
			if library, ok := s.libraries.Get(request.LibraryID); ok {
				s.runMoviesLibrary(ctx, id, library)
				break
			}
		}
		path := firstNonEmpty(request.Path, request.MoviesPath, s.cfg.MovieLibraryPath)
		if path == "" {
			if library, ok := firstLibraryByKind(s.libraries, libraries.KindMovies); ok {
				s.runMoviesLibrary(ctx, id, library)
				break
			}
		}
		s.runMovies(ctx, id, path)
	case KindTV:
		if request.LibraryID != "" {
			if library, ok := s.libraries.Get(request.LibraryID); ok {
				s.runTVLibrary(ctx, id, library)
				break
			}
		}
		path := firstNonEmpty(request.Path, request.TVPath, s.cfg.TVLibraryPath)
		if path == "" {
			if library, ok := firstLibraryByKind(s.libraries, libraries.KindTV); ok {
				s.runTVLibrary(ctx, id, library)
				break
			}
		}
		s.runTV(ctx, id, path)
	case KindAll:
		ran := false
		for _, library := range s.libraries.List() {
			switch library.Kind {
			case libraries.KindMovies:
				ran = true
				if !s.runMoviesLibrary(ctx, id, library) {
					return
				}
			case libraries.KindTV:
				ran = true
				if !s.runTVLibrary(ctx, id, library) {
					return
				}
			}
		}
		if !ran {
			if path := firstNonEmpty(request.MoviesPath, s.cfg.MovieLibraryPath); path != "" {
				if !s.runMovies(ctx, id, path) {
					return
				}
			}
			if path := firstNonEmpty(request.TVPath, s.cfg.TVLibraryPath); path != "" {
				if !s.runTV(ctx, id, path) {
					return
				}
			}
		}
	}

	job, _ = s.Get(id)
	job.Status = StatusCompleted
	job.CompletedAt = time.Now().UTC()
	s.store(job)
	s.publish("scan.completed", job)
}

func (s *Service) runMovies(ctx context.Context, id string, path string) bool {
	return s.runMoviesLibrary(ctx, id, libraries.Library{ID: "movies", Name: "Movies", Path: path, Kind: libraries.KindMovies})
}

func (s *Service) runTV(ctx context.Context, id string, path string) bool {
	return s.runTVLibrary(ctx, id, libraries.Library{ID: "tv", Name: "TV", Path: path, Kind: libraries.KindTV})
}

func (s *Service) runMoviesLibrary(ctx context.Context, id string, library libraries.Library) bool {
	if library.ID == "" {
		library.ID = libraries.IDFor(library.Kind, library.Path)
	}
	state := s.scanState(ctx, library.ID)
	result, err := s.scanner.Scan(ctx, scanner.Request{
		Kind:          scanner.KindMovies,
		Root:          library.Path,
		KnownFiles:    state,
		SkipUnchanged: len(state) > 0,
		Progress: func(progress scanner.Progress) {
			s.updateProgress(id, progress)
		},
	})
	if err != nil {
		s.fail(id, err)
		return false
	}
	candidates := s.movies.Classify(result.Files)
	library.Path = result.Root
	if library.Name == "" {
		library.Name = "Movies"
	}
	s.libraries.Set(library)
	persisted, err := s.catalog.SaveMovieScan(ctx, library, result, candidates)
	if err != nil {
		s.fail(id, err)
		return false
	}
	s.mergeResult(id, library.ID, map[string]any{
		"kind":        "movies",
		"summary":     result.Summary,
		"persisted":   persisted,
		"moviesFound": len(candidates),
	})
	s.refreshMetadata(ctx, id, "movie", requestMetadataLimit(result.MediaFiles))
	return true
}

func (s *Service) runTVLibrary(ctx context.Context, id string, library libraries.Library) bool {
	if library.ID == "" {
		library.ID = libraries.IDFor(library.Kind, library.Path)
	}
	state := s.scanState(ctx, library.ID)
	result, err := s.scanner.Scan(ctx, scanner.Request{
		Kind:          scanner.KindTV,
		Root:          library.Path,
		KnownFiles:    state,
		SkipUnchanged: len(state) > 0,
		Progress: func(progress scanner.Progress) {
			s.updateProgress(id, progress)
		},
	})
	if err != nil {
		s.fail(id, err)
		return false
	}
	candidates := s.tv.Classify(result.Files)
	library.Path = result.Root
	if library.Name == "" {
		library.Name = "TV"
	}
	s.libraries.Set(library)
	persisted, err := s.catalog.SaveTVScan(ctx, library, result, candidates)
	if err != nil {
		s.fail(id, err)
		return false
	}
	s.mergeResult(id, library.ID, map[string]any{
		"kind":          "tv",
		"summary":       result.Summary,
		"persisted":     persisted,
		"episodesFound": len(candidates),
	})
	s.refreshMetadata(ctx, id, "series", requestMetadataLimit(result.MediaFiles))
	return true
}

func (s *Service) runMoviesOld(ctx context.Context, id string, path string) bool {
	result, err := s.scanner.Scan(ctx, scanner.Request{
		Kind: scanner.KindMovies,
		Root: path,
		Progress: func(progress scanner.Progress) {
			s.updateProgress(id, progress)
		},
	})
	if err != nil {
		s.fail(id, err)
		return false
	}
	candidates := s.movies.Classify(result.Files)
	library := libraries.Library{ID: "movies", Name: "Movies", Path: result.Root, Kind: libraries.KindMovies}
	s.libraries.Set(library)
	persisted, err := s.catalog.SaveMovieScan(ctx, library, result, candidates)
	if err != nil {
		s.fail(id, err)
		return false
	}
	s.mergeResult(id, "movies", map[string]any{
		"summary":     result.Summary,
		"persisted":   persisted,
		"moviesFound": len(candidates),
	})
	return true
}

func (s *Service) runTVOld(ctx context.Context, id string, path string) bool {
	result, err := s.scanner.Scan(ctx, scanner.Request{
		Kind: scanner.KindTV,
		Root: path,
		Progress: func(progress scanner.Progress) {
			s.updateProgress(id, progress)
		},
	})
	if err != nil {
		s.fail(id, err)
		return false
	}
	candidates := s.tv.Classify(result.Files)
	library := libraries.Library{ID: "tv", Name: "TV", Path: result.Root, Kind: libraries.KindTV}
	s.libraries.Set(library)
	persisted, err := s.catalog.SaveTVScan(ctx, library, result, candidates)
	if err != nil {
		s.fail(id, err)
		return false
	}
	s.mergeResult(id, "tv", map[string]any{
		"summary":       result.Summary,
		"persisted":     persisted,
		"episodesFound": len(candidates),
	})
	return true
}

func (s *Service) updateProgress(id string, progress scanner.Progress) {
	s.mu.Lock()
	job := s.jobs[id]
	job.TotalFiles = progress.TotalFiles
	job.MediaFiles = progress.MediaFiles
	job.ChangedFiles = progress.ChangedFiles
	job.Unchanged = progress.Unchanged
	job.IgnoredFiles = progress.IgnoredFiles
	job.LastPath = progress.LastPath
	s.jobs[id] = job
	s.mu.Unlock()
	s.publish("scan.progress", job)
}

func (s *Service) scanState(ctx context.Context, libraryID string) map[string]scanner.FileSignature {
	if s.catalog == nil || libraryID == "" {
		return nil
	}
	state, err := s.catalog.ScanState(ctx, libraryID)
	if err != nil {
		return nil
	}
	return state
}

func (s *Service) mergeResult(id string, key string, value any) {
	s.mu.Lock()
	job := s.jobs[id]
	if job.Result == nil {
		job.Result = map[string]any{}
	}
	job.Result[key] = value
	s.jobs[id] = job
	s.mu.Unlock()
}

func (s *Service) refreshMetadata(ctx context.Context, id string, kind string, limit int) {
	if s.metadata == nil {
		return
	}
	result, err := s.metadata.RefreshBatch(ctx, kind, limit)
	if err != nil {
		s.events.Publish("metadata.batch.failed", map[string]any{"scanId": id, "kind": kind, "error": err.Error()})
		return
	}
	s.mergeResult(id, "metadata_"+kind, result)
	s.events.Publish("metadata.batch.completed", map[string]any{"scanId": id, "kind": kind, "result": result})
}

func requestMetadataLimit(mediaFiles int) int {
	if mediaFiles <= 0 {
		return 0
	}
	if mediaFiles < 300 {
		return mediaFiles
	}
	return 300
}

func (s *Service) fail(id string, err error) {
	job, ok := s.Get(id)
	if !ok {
		return
	}
	job.Status = StatusFailed
	job.Error = err.Error()
	job.CompletedAt = time.Now().UTC()
	s.store(job)
	s.publish("scan.failed", job)
}

func (s *Service) store(job Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
}

func (s *Service) publish(eventType string, job Job) {
	s.events.Publish(eventType, job)
}

func (s *Service) nextJobID() string {
	return "scan_" + time.Now().UTC().Format("20060102T150405") + "_" + stringID(s.nextID.Add(1))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
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
			return library.Path
		}
	}
	return ""
}

func firstLibraryByKind(service *libraries.Service, kind libraries.Kind) (libraries.Library, bool) {
	if service == nil {
		return libraries.Library{}, false
	}
	for _, library := range service.List() {
		if library.Kind == kind {
			return library, true
		}
	}
	return libraries.Library{}, false
}

func stringID(value uint64) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[i:])
}
