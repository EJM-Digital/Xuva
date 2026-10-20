package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/catalog"
	"github.com/EJM-Digital/Xuva/server/internal/config"
	"github.com/EJM-Digital/Xuva/server/internal/events"
	"github.com/EJM-Digital/Xuva/server/internal/trailers"
)

type Service struct {
	cfg                 config.Config
	catalog             *catalog.Service
	events              *events.Bus
	trailers            *trailers.Service // optional: nil disables trailer fetch
	client              *http.Client
	tmdbBaseURL         string
	omdbBaseURL         string
	tvMazeBaseURL       string
	tvdbBaseURL         string
	wikidataSearchURL   string
	wikidataEntityURL   string
	wikipediaSearchURL  string
	wikipediaSummaryURL string
	providerStateMu     sync.RWMutex
	providerState       map[string]providerRuntimeState
	backfillMu          sync.Mutex
	backfill            BackfillStatus
	backfillCancel      context.CancelFunc
}

var (
	// ErrBackfillProviderNotConfigured indicates the requested managed provider
	// has no effective credential in the current runtime.
	ErrBackfillProviderNotConfigured = errors.New("backfill provider not configured")
	// ErrBackfillAlreadyRunning indicates a one-shot backfill is already active.
	ErrBackfillAlreadyRunning = errors.New("backfill already running")
)

// BackfillStatus is a snapshot of the missing-provider backfill job.
// Exposed via the API for the Settings UI's "library health" panel.
type BackfillStatus struct {
	Running    bool      `json:"running"`
	Provider   string    `json:"provider,omitempty"` // which provider we're filling in (e.g. "tmdb")
	Kind       string    `json:"kind,omitempty"`     // current sweep: "movie" | "series"
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	Total      int       `json:"total"`     // total items needing this provider (computed once at start)
	Refreshed  int       `json:"refreshed"` // successful refreshes so far
	Failed     int       `json:"failed"`    // attempted but errored
	Remaining  int       `json:"remaining"` // live count from catalog (decreases as we go)
	LastTitle  string    `json:"lastTitle,omitempty"`
	LastError  string    `json:"lastError,omitempty"`
}

// SetTrailers wires the trailer downloader so post-refresh hooks can queue
// downloads. Called from app.go after both services are constructed —
// kept as a setter to avoid a circular construction order.
func (s *Service) SetTrailers(t *trailers.Service) {
	s.trailers = t
}

type RefreshRequest struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	Year  int    `json:"year,omitempty"`
	// TMDBOverrideID, when set, skips the search and fetches this TMDB ID directly.
	// It can be extracted from the filename ({tmdb-12345} notation) or supplied
	// by the user via the manual "Match by TMDB ID" UI.
	TMDBOverrideID int `json:"tmdbOverrideId,omitempty"`
	// Filename is the original media source filename. Used to extract embedded
	// TMDB IDs ({tmdb-12345}) before falling back to the search-based path.
	Filename string `json:"filename,omitempty"`
}

type RefreshResult struct {
	Kind        string                   `json:"kind"`
	ID          string                   `json:"id"`
	Configured  map[string]bool          `json:"configured"`
	Records     []catalog.MetadataRecord `json:"records"`
	Ratings     []catalog.Rating         `json:"ratings"`
	ExternalIDs []catalog.ExternalID     `json:"externalIds"`
	Warnings    []string                 `json:"warnings,omitempty"`
}

type BatchResult struct {
	Kind      string          `json:"kind"`
	Limit     int             `json:"limit"`
	Attempted int             `json:"attempted"`
	Refreshed int             `json:"refreshed"`
	Skipped   int             `json:"skipped"`
	Warnings  []string        `json:"warnings,omitempty"`
	Items     []RefreshResult `json:"items,omitempty"`
}

func NewService(cfg config.Config, catalogService *catalog.Service, eventBus *events.Bus) *Service {
	return newServiceWithClient(cfg, catalogService, eventBus, &http.Client{Timeout: 4 * time.Second})
}

func newServiceWithClient(cfg config.Config, catalogService *catalog.Service, eventBus *events.Bus, client *http.Client) *Service {
	return &Service{
		cfg:                 cfg,
		catalog:             catalogService,
		events:              eventBus,
		client:              client,
		tmdbBaseURL:         "https://api.themoviedb.org/3",
		omdbBaseURL:         "https://www.omdbapi.com",
		tvMazeBaseURL:       "https://api.tvmaze.com",
		tvdbBaseURL:         "https://api4.thetvdb.com/v4",
		wikidataSearchURL:   "https://www.wikidata.org/w/api.php",
		wikidataEntityURL:   "https://www.wikidata.org/wiki/Special:EntityData",
		wikipediaSearchURL:  "https://en.wikipedia.org/w/api.php",
		wikipediaSummaryURL: "https://en.wikipedia.org/api/rest_v1/page/summary",
		providerState:       map[string]providerRuntimeState{},
	}
}

func (s *Service) activeConfig() config.Config {
	if strings.TrimSpace(s.cfg.DataDir) == "" {
		return s.cfg
	}
	if saved, err := config.LoadFile(s.cfg.DataDir); err == nil {
		return config.Merge(s.cfg, saved)
	}
	return s.cfg
}

func (s *Service) Refresh(ctx context.Context, request RefreshRequest) (RefreshResult, error) {
	if request.Kind == "" || request.ID == "" {
		return RefreshResult{}, errors.New("kind and id are required")
	}
	if request.Title == "" {
		record, ok, err := s.catalog.GetBestMetadata(ctx, request.Kind, request.ID)
		if err != nil {
			return RefreshResult{}, err
		}
		if ok {
			request.Title = record.Title
			request.Year = record.Year
		}
	}
	if strings.TrimSpace(request.Title) == "" {
		return RefreshResult{}, errors.New("title is required")
	}

	cfg := s.activeConfig()
	metadataOrder := s.sourceOrder(ctx, request)
	artworkOrder := s.artworkOrder(ctx, request)
	result := RefreshResult{
		Kind: request.Kind,
		ID:   request.ID,
		Configured: map[string]bool{
			"filename": true,
			"manual":   true,
			"nfo":      true,
			"artwork":  true,
			"tvmaze":   true,
			// TVDB is disabled at the provider level (subscription model
			// incompatible with embedded keys). Code remains dormant.
			"tvdb":      false,
			"wikidata":  true,
			"wikipedia": true,
			"fanart":    managedProviderConfigured("fanart", cfg),
			"omdb":      managedProviderConfigured("omdb", cfg),
			"tmdb":      managedProviderConfigured("tmdb", cfg),
		},
	}

	if err := s.refreshLocal(ctx, request, metadataOrder, artworkOrder, &result); err != nil {
		result.Warnings = append(result.Warnings, "Local metadata refresh failed: "+err.Error())
	}
	if err := s.refreshAutomaticOnline(ctx, request, metadataOrder, artworkOrder, cfg, &result); err != nil {
		result.Warnings = append(result.Warnings, err.Error())
	}

	records, err := s.catalog.ListMetadataRecords(ctx, request.Kind, request.ID)
	if err != nil {
		return RefreshResult{}, err
	}
	ratings, err := s.catalog.ListRatings(ctx, request.Kind, request.ID)
	if err != nil {
		return RefreshResult{}, err
	}
	externalIDs, err := s.catalog.ListExternalIDs(ctx, request.Kind, request.ID)
	if err != nil {
		return RefreshResult{}, err
	}
	result.Records = records
	result.Ratings = ratings
	result.ExternalIDs = externalIDs
	if s.events != nil {
		s.events.Publish("metadata.ratings.updated", result)
	}
	return result, nil
}

func (s *Service) RefreshBatch(ctx context.Context, kind string, limit int) (BatchResult, error) {
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}
	result := BatchResult{Kind: kind, Limit: limit}
	candidateWindow := batchCandidateWindow(limit)
	switch kind {
	case "movie", "movies":
		movies, err := s.catalog.ListMovies(ctx, candidateWindow, "", "")
		if err != nil {
			return BatchResult{}, err
		}
		for _, item := range movies {
			if result.Attempted >= limit {
				break
			}
			if shouldSkipMetadata(item.Metadata) {
				result.Skipped++
				continue
			}
			refresh, err := s.Refresh(ctx, RefreshRequest{Kind: "movie", ID: item.ID, Title: item.Title, Year: item.Year})
			result.Attempted++
			if err != nil {
				result.Warnings = append(result.Warnings, item.Title+": "+err.Error())
				continue
			}
			result.Refreshed++
			result.Items = append(result.Items, refresh)
		}
	case "series", "tv":
		series, err := s.catalog.ListSeries(ctx, candidateWindow, "", "")
		if err != nil {
			return BatchResult{}, err
		}
		for _, item := range series {
			if result.Attempted >= limit {
				break
			}
			if shouldSkipMetadata(item.Metadata) {
				result.Skipped++
				continue
			}
			refresh, err := s.Refresh(ctx, RefreshRequest{Kind: "series", ID: item.ID, Title: item.Title})
			result.Attempted++
			if err != nil {
				result.Warnings = append(result.Warnings, item.Title+": "+err.Error())
				continue
			}
			result.Refreshed++
			result.Items = append(result.Items, refresh)
		}
	default:
		return BatchResult{}, errors.New("metadata batch kind must be movie or series")
	}
	if s.events != nil {
		s.events.Publish("metadata.batch.completed", result)
	}
	return result, nil
}

func shouldSkipMetadata(record *catalog.MetadataRecord) bool {
	if record == nil {
		return false
	}
	provider := normalizeProviderID(record.Provider)
	if provider == "" {
		return false
	}
	if hasEnrichedMetadata(record) {
		return true
	}
	switch provider {
	case "manual", "nfo":
		return true
	case "filename":
		return false
	case "artwork":
		// Keep artwork-only records eligible so online summaries/IDs can still be fetched.
		return false
	case "tmdb", "tvdb", "omdb", "tvmaze", "wikipedia", "wikidata":
		return true
	default:
		return false
	}
}

// hasEnrichedMetadata controls whether shouldSkipMetadata short-circuits a
// batch-refresh for an item. A record is "enriched enough to skip" only when
// BOTH text and visual data are present.
//
// Pre-#401 this returned true the moment overview was set, meaning any item
// where TMDB returned a description but no poster URL (rate-limit response,
// stale CDN entry, region-locked artwork, etc.) was permanently skipped by
// every subsequent RefreshBatch — its library card stayed stuck on the
// gradient placeholder forever. Users with 4 k-movie libraries hit this for
// hundreds of titles. See the Emby comparison audit on #401 for the
// user-visible impact: most missing posters were items with valid overview
// text, not items missing TMDB matches entirely.
//
// New rule: text without artwork (or artwork without text) keeps the record
// eligible for re-fetch. The provider call to re-fill the missing dimension
// is cheap relative to the user-visible quality gap.
func hasEnrichedMetadata(record *catalog.MetadataRecord) bool {
	if record == nil {
		return false
	}
	hasText := strings.TrimSpace(record.Overview) != ""
	hasArt := strings.TrimSpace(record.PosterURL) != "" || strings.TrimSpace(record.BackdropURL) != ""
	// Identifier-only records (just ratings or external IDs) don't satisfy
	// either dimension — keep them eligible so a real metadata refresh can
	// fill both.
	return hasText && hasArt
}

func batchCandidateWindow(limit int) int {
	window := limit * 30
	if window < 400 {
		window = 400
	}
	if window > 5000 {
		window = 5000
	}
	return window
}

func (s *Service) refreshOMDb(ctx context.Context, request RefreshRequest, order []string, cfg config.Config, result *RefreshResult) error {
	apiKey := managedProviderCredential("omdb", cfg)
	endpoint := strings.TrimRight(s.omdbBaseURL, "/") + "/?" + url.Values{
		"apikey": {apiKey},
		"t":      {request.Title},
		"type":   {omdbType(request.Kind)},
		"plot":   {"full"},
	}.Encode()
	if request.Year > 0 {
		endpoint += "&y=" + strconv.Itoa(request.Year)
	}
	var payload omdbResponse
	if err := s.getJSON(ctx, endpoint, &payload); err != nil {
		return err
	}
	if strings.EqualFold(payload.Response, "false") {
		return errors.New(firstNonEmpty(payload.Error, "no OMDb match"))
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := catalog.MetadataRecord{
		Kind:       request.Kind,
		ItemID:     request.ID,
		Provider:   "omdb",
		ExternalID: payload.ImdbID,
		Title:      firstNonEmpty(payload.Title, request.Title),
		Year:       parseYear(payload.Year, request.Year),
		Overview:   payload.Plot,
		PosterURL:  emptyNA(payload.Poster),
		Confidence: sourceConfidence(order, "omdb", 0.88),
		RawJSON:    mustJSON(payload),
		FetchedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.catalog.UpsertMetadataRecord(ctx, record); err != nil {
		return err
	}
	result.Records = append(result.Records, record)
	if payload.ImdbID != "" {
		_ = s.catalog.UpsertExternalID(ctx, catalog.ExternalID{
			Kind:       request.Kind,
			ItemID:     request.ID,
			Provider:   "imdb",
			ExternalID: payload.ImdbID,
		})
	}
	if payload.ImdbID != "" {
		result.ExternalIDs = append(result.ExternalIDs, catalog.ExternalID{
			Kind:       request.Kind,
			ItemID:     request.ID,
			Provider:   "imdb",
			ExternalID: payload.ImdbID,
			UpdatedAt:  now,
		})
	}
	ratings := omdbRatings(request, payload, now)
	if err := s.catalog.UpsertRatings(ctx, ratings); err != nil {
		return err
	}
	result.Ratings = append(result.Ratings, ratings...)
	return nil
}

func (s *Service) getJSON(ctx context.Context, endpoint string, target any) error {
	return s.getJSONHeaders(ctx, endpoint, nil, target)
}

func (s *Service) getJSONHeaders(ctx context.Context, endpoint string, headers map[string]string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	for key, value := range headers {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			request.Header.Set(key, value)
		}
	}
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", "Xuva/0.1 (+https://github.com/xuvahq/xuva)")
	}
	if request.Header.Get("Accept") == "" {
		request.Header.Set("Accept", "application/json")
	}
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return newProviderHTTPError(response)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func (s *Service) postJSON(ctx context.Context, endpoint string, body any, headers map[string]string, target any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			request.Header.Set(key, value)
		}
	}
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", "Xuva/0.1 (+https://github.com/xuvahq/xuva)")
	}
	if request.Header.Get("Accept") == "" {
		request.Header.Set("Accept", "application/json")
	}
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return newProviderHTTPError(response)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

type providerHTTPError struct {
	StatusCode int
	Status     string
	Detail     string
}

func (e providerHTTPError) Error() string {
	if strings.TrimSpace(e.Detail) != "" {
		return strings.TrimSpace(e.Detail)
	}
	if strings.TrimSpace(e.Status) != "" {
		return "provider returned " + strings.TrimSpace(e.Status)
	}
	return "provider returned error"
}

func newProviderHTTPError(response *http.Response) error {
	if response == nil {
		return providerHTTPError{Status: "unknown"}
	}
	detail := ""
	if response.Body != nil {
		if payload, err := io.ReadAll(io.LimitReader(response.Body, 2048)); err == nil {
			detail = strings.TrimSpace(string(payload))
		}
	}
	return providerHTTPError{
		StatusCode: response.StatusCode,
		Status:     response.Status,
		Detail:     detail,
	}
}

type omdbResponse struct {
	Title      string       `json:"Title"`
	Year       string       `json:"Year"`
	ImdbID     string       `json:"imdbID"`
	ImdbRating string       `json:"imdbRating"`
	ImdbVotes  string       `json:"imdbVotes"`
	Metascore  string       `json:"Metascore"`
	Plot       string       `json:"Plot"`
	Poster     string       `json:"Poster"`
	Ratings    []omdbRating `json:"Ratings"`
	Response   string       `json:"Response"`
	Error      string       `json:"Error"`
}

type omdbRating struct {
	Source string `json:"Source"`
	Value  string `json:"Value"`
}

type tmdbSearch struct {
	Results []tmdbResult `json:"results"`
}

type tmdbResult struct {
	ID           int     `json:"id"`
	Title        string  `json:"title"`
	Name         string  `json:"name"`
	ReleaseDate  string  `json:"release_date"`
	FirstAirDate string  `json:"first_air_date"`
	Overview     string  `json:"overview"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	VoteAverage  float64 `json:"vote_average"`
	VoteCount    int     `json:"vote_count"`
}

// TMDBCandidate is the shape returned by TMDBCandidates for disambiguation UI.
type TMDBCandidate struct {
	ID          int     `json:"id"`
	Title       string  `json:"title"`
	Year        int     `json:"year"`
	Overview    string  `json:"overview"`
	PosterURL   string  `json:"posterUrl"`
	BackdropURL string  `json:"backdropUrl"`
	VoteAverage float64 `json:"voteAverage"`
	VoteCount   int     `json:"voteCount"`
}

// TMDBCandidates returns the top N TMDB search results for a given title/year
// without persisting anything. Used by the disambiguation UI (#62).
func (s *Service) TMDBCandidates(ctx context.Context, kind string, title string, year int, limit int) ([]TMDBCandidate, error) {
	cfg := s.activeConfig()
	apiKey := managedProviderCredential("tmdb", cfg)
	path := "movie"
	if kind == "series" {
		path = "tv"
	}
	searchURL := strings.TrimRight(s.tmdbBaseURL, "/") + "/search/" + path + "?" + url.Values{
		"api_key":  {apiKey},
		"query":    {title},
		"language": {cfg.MetadataLanguage},
	}.Encode()
	if year > 0 && kind == "movie" {
		searchURL += "&year=" + strconv.Itoa(year)
	}
	var search tmdbSearch
	if err := s.getJSON(ctx, searchURL, &search); err != nil {
		return nil, err
	}
	out := make([]TMDBCandidate, 0, limit)
	for i, r := range search.Results {
		if i >= limit {
			break
		}
		itemTitle := firstNonEmpty(r.Title, r.Name)
		itemYear := parseYear(firstNonEmpty(r.ReleaseDate, r.FirstAirDate), 0)
		out = append(out, TMDBCandidate{
			ID:          r.ID,
			Title:       itemTitle,
			Year:        itemYear,
			Overview:    r.Overview,
			PosterURL:   tmdbImageURL(r.PosterPath, "w342"),
			BackdropURL: tmdbImageURL(r.BackdropPath, "w780"),
			VoteAverage: r.VoteAverage,
			VoteCount:   r.VoteCount,
		})
	}
	return out, nil
}

func omdbRatings(request RefreshRequest, payload omdbResponse, now string) []catalog.Rating {
	output := []catalog.Rating{}
	if value, ok := parseFloat(payload.ImdbRating); ok {
		output = append(output, catalog.Rating{Kind: request.Kind, ItemID: request.ID, Provider: "omdb", RatingType: "imdb", Value: value, DisplayValue: payload.ImdbRating + "/10", Scale: 10, Votes: parseVotes(payload.ImdbVotes), SourceURL: imdbURL(payload.ImdbID), FetchedAt: now, UpdatedAt: now})
	}
	if value, ok := parseFloat(payload.Metascore); ok {
		output = append(output, catalog.Rating{Kind: request.Kind, ItemID: request.ID, Provider: "omdb", RatingType: "metacritic", Value: value, DisplayValue: payload.Metascore, Scale: 100, SourceURL: imdbURL(payload.ImdbID), FetchedAt: now, UpdatedAt: now})
	}
	for _, rating := range payload.Ratings {
		switch strings.ToLower(rating.Source) {
		case "rotten tomatoes":
			if value, ok := parsePercent(rating.Value); ok {
				output = append(output, catalog.Rating{Kind: request.Kind, ItemID: request.ID, Provider: "omdb", RatingType: "rottenTomatoesCritics", Value: value, DisplayValue: rating.Value, Scale: 100, SourceURL: imdbURL(payload.ImdbID), FetchedAt: now, UpdatedAt: now})
			}
		}
	}
	return output
}

func omdbType(kind string) string {
	if kind == "series" || kind == "episode" {
		return "series"
	}
	return "movie"
}

func parseYear(value string, fallback int) int {
	if len(value) >= 4 {
		if parsed, err := strconv.Atoi(value[:4]); err == nil {
			return parsed
		}
	}
	return fallback
}

func parseVotes(value string) int {
	value = strings.ReplaceAll(value, ",", "")
	parsed, _ := strconv.Atoi(value)
	return parsed
}

func parsePercent(value string) (float64, bool) {
	value = strings.TrimSuffix(strings.TrimSpace(value), "%")
	return parseFloat(value)
}

func parseFloat(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "N/A") {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	return parsed, err == nil
}

func imdbURL(id string) string {
	if id == "" {
		return ""
	}
	return "https://www.imdb.com/title/" + id + "/"
}

func tmdbImageURL(path string, size string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	if size == "" {
		size = "original"
	}
	return "https://image.tmdb.org/t/p/" + size + path
}

func emptyNA(value string) string {
	if strings.EqualFold(value, "N/A") {
		return ""
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func mustJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// ── Backfill ────────────────────────────────────────────────────────────────
//
// Backfill walks the catalog and refreshes metadata for every item that
// doesn't yet have a row for a given provider (e.g. TMDB). Unlike
// RefreshBatch, which respects `shouldSkipMetadata` and therefore skips
// items that have any enriched metadata, backfill explicitly targets
// per-provider gaps — exactly what we need when a TMDB key arrives after
// the library was already partially populated by Wikipedia/Wikidata.
//
// One backfill runs at a time, protected by a mutex. Status is published
// to the events bus on every step so the Settings UI can render live
// progress without polling.

// BackfillStatus returns a snapshot of the current backfill state.
func (s *Service) BackfillStatus() BackfillStatus {
	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()
	status := s.backfill
	// If running, refresh `Remaining` lazily so callers see real progress
	// even between batch boundaries.
	if status.Running {
		// fall through — Remaining is updated each batch
	}
	return status
}

// StartBackfill kicks off a one-shot backfill goroutine for the given
// provider. Returns an error if a backfill is already running. Otherwise
// returns immediately and progress can be polled via BackfillStatus or
// observed via the "metadata.backfill.*" event channel.
func (s *Service) StartBackfill(parentCtx context.Context, provider string) error {
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		return errors.New("provider required")
	}
	if s.catalog == nil {
		return errors.New("catalog not available")
	}
	cfg := s.activeConfig()
	if managedProviderCredential(provider, cfg) == "" {
		return fmt.Errorf("%w: provider %s is not configured; set its API key first", ErrBackfillProviderNotConfigured, provider)
	}

	s.backfillMu.Lock()
	if s.backfill.Running {
		s.backfillMu.Unlock()
		return ErrBackfillAlreadyRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.backfill = BackfillStatus{
		Running:   true,
		Provider:  provider,
		StartedAt: time.Now().UTC(),
	}
	s.backfillCancel = cancel
	s.backfillMu.Unlock()

	go s.runBackfill(ctx, parentCtx, provider)
	return nil
}

func IsBackfillProviderNotConfigured(err error) bool {
	return errors.Is(err, ErrBackfillProviderNotConfigured)
}

func IsBackfillAlreadyRunning(err error) bool {
	return errors.Is(err, ErrBackfillAlreadyRunning)
}

// StopBackfill aborts the running backfill, if any. No-op if idle.
func (s *Service) StopBackfill() {
	s.backfillMu.Lock()
	if s.backfillCancel != nil {
		s.backfillCancel()
	}
	s.backfillMu.Unlock()
}

func (s *Service) runBackfill(ctx context.Context, _ context.Context, provider string) {
	defer func() {
		s.backfillMu.Lock()
		s.backfill.Running = false
		s.backfill.FinishedAt = time.Now().UTC()
		s.backfillCancel = nil
		s.backfillMu.Unlock()
		s.publishBackfillEvent("metadata.backfill.finished")
	}()

	// Compute initial totals across both kinds so the UI can show a real
	// progress bar from the first frame.
	totalMovies, _ := s.catalog.CountItemsMissingProvider(ctx, "movie", provider)
	totalSeries, _ := s.catalog.CountItemsMissingProvider(ctx, "series", provider)
	s.backfillMu.Lock()
	s.backfill.Total = totalMovies + totalSeries
	s.backfill.Remaining = s.backfill.Total
	s.backfillMu.Unlock()
	s.publishBackfillEvent("metadata.backfill.started")

	const batchSize = 25
	const pauseBetweenItems = 150 * time.Millisecond // gentle rate-limit

	for _, kind := range []string{"movie", "series"} {
		s.backfillMu.Lock()
		s.backfill.Kind = kind
		s.backfillMu.Unlock()

		for {
			if ctx.Err() != nil {
				return
			}
			items, err := s.catalog.ListItemsMissingProvider(ctx, kind, provider, batchSize)
			if err != nil {
				s.recordBackfillError(err)
				break
			}
			if len(items) == 0 {
				break
			}
			for _, item := range items {
				if ctx.Err() != nil {
					return
				}
				_, err := s.Refresh(ctx, RefreshRequest{
					Kind:  item.Kind,
					ID:    item.ID,
					Title: item.Title,
					Year:  item.Year,
				})
				s.backfillMu.Lock()
				s.backfill.LastTitle = item.Title
				if err != nil {
					s.backfill.Failed++
					s.backfill.LastError = err.Error()
				} else {
					s.backfill.Refreshed++
					s.backfill.LastError = ""
				}
				s.backfillMu.Unlock()
				s.publishBackfillEvent("metadata.backfill.progress")
				select {
				case <-ctx.Done():
					return
				case <-time.After(pauseBetweenItems):
				}
			}
			// Refresh Remaining from the source of truth so a) any new items
			// added by a parallel scan show up, and b) we don't loop forever
			// if Refresh somehow fails to produce a provider row.
			remainingMovies, _ := s.catalog.CountItemsMissingProvider(ctx, "movie", provider)
			remainingSeries, _ := s.catalog.CountItemsMissingProvider(ctx, "series", provider)
			s.backfillMu.Lock()
			s.backfill.Remaining = remainingMovies + remainingSeries
			s.backfillMu.Unlock()
		}
	}
}

func (s *Service) recordBackfillError(err error) {
	s.backfillMu.Lock()
	s.backfill.LastError = err.Error()
	s.backfillMu.Unlock()
}

func (s *Service) publishBackfillEvent(event string) {
	if s.events == nil {
		return
	}
	s.events.Publish(event, s.BackfillStatus())
}
