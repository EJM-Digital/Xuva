package metadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/catalog"
	"github.com/EJM-Digital/Xuva/server/internal/config"
	"github.com/EJM-Digital/Xuva/server/internal/database"
	"github.com/EJM-Digital/Xuva/server/internal/libraries"
	"github.com/EJM-Digital/Xuva/server/internal/movies"
	"github.com/EJM-Digital/Xuva/server/internal/scanner"
	"github.com/EJM-Digital/Xuva/server/internal/tv"
)

func TestRefreshUsesLocalNFOAndArtworkWithoutUserKeys(t *testing.T) {
	ctx := context.Background()
	service, catalogService, root := newMetadataTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/w/api.php") {
			_ = json.NewEncoder(w).Encode([]any{"Arrival", []any{}, []any{}, []any{}})
			return
		}
		http.NotFound(w, r)
	}))

	movieDir := filepath.Join(root, "Movies", "Arrival (2016)")
	writeFile(t, filepath.Join(movieDir, "Arrival.2016.1080p.mkv"), "video")
	writeFile(t, filepath.Join(movieDir, "movie.nfo"), `<movie><title>Arrival</title><year>2016</year><plot>A linguist works to communicate with visitors.</plot><imdbid>tt2543164</imdbid></movie>`)
	writeFile(t, filepath.Join(movieDir, "poster.jpg"), "poster")
	writeFile(t, filepath.Join(movieDir, "fanart.jpg"), "fanart")

	movieID := seedMovie(t, ctx, catalogService, filepath.Join(root, "Movies"), filepath.Join(movieDir, "Arrival.2016.1080p.mkv"))
	result, err := service.Refresh(ctx, RefreshRequest{Kind: "movie", ID: movieID, Title: "Arrival", Year: 2016})
	if err != nil {
		t.Fatalf("refresh local metadata: %v", err)
	}
	best, ok, err := catalogService.GetBestMetadata(ctx, "movie", movieID)
	if err != nil {
		t.Fatalf("best metadata: %v", err)
	}
	if !ok || best.Provider != "nfo" || best.Overview == "" {
		t.Fatalf("expected nfo metadata to win, got %#v", best)
	}
	if !strings.EqualFold(best.PosterURL, filepath.Join(movieDir, "poster.jpg")) || !strings.EqualFold(best.BackdropURL, filepath.Join(movieDir, "fanart.jpg")) {
		t.Fatalf("expected local artwork paths, got %#v", best)
	}
	if result.Configured["tvmaze"] != true || result.Configured["wikipedia"] != true || result.Configured["wikidata"] != true || result.Configured["tmdb"] != false || result.Configured["omdb"] != false {
		t.Fatalf("unexpected provider map: %#v", result.Configured)
	}
	if len(result.ExternalIDs) == 0 || result.ExternalIDs[0].Provider != "imdb" {
		t.Fatalf("expected imdb id from nfo, got %#v", result.ExternalIDs)
	}
}

func TestRefreshUsesAutomaticSeriesAndMovieProvidersWithoutUserKeys(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/search/shows"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"show": map[string]any{
					"id":        169,
					"name":      "The Bear",
					"premiered": "2022-06-23",
					"summary":   "<p>A young chef returns to Chicago.</p>",
					"url":       "https://www.tvmaze.com/shows/169/the-bear",
					"rating":    map[string]any{"average": 8.7},
					"image":     map[string]any{"medium": "https://img.example/bear-medium.jpg", "original": "https://img.example/bear-original.jpg"},
					"externals": map[string]any{"imdb": "tt14452776", "thetvdb": 405920},
				},
			}})
		case strings.HasPrefix(r.URL.Path, "/w/api.php"):
			if r.URL.Query().Get("action") == "wbsearchentities" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"search": []map[string]any{{
						"id":          "Q17738",
						"label":       "Arrival",
						"description": "2016 science fiction film",
					}},
				})
				return
			}
			_ = json.NewEncoder(w).Encode([]any{"Arrival", []any{"Arrival (film)"}, []any{""}, []any{""}})
		case strings.HasPrefix(r.URL.Path, "/page/summary/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"titles":        map[string]any{"canonical": "Arrival_(film)", "normalized": "Arrival"},
				"extract":       "A linguist is recruited after alien ships arrive.",
				"thumbnail":     map[string]any{"source": "https://img.example/arrival-thumb.jpg"},
				"originalimage": map[string]any{"source": "https://img.example/arrival-original.jpg"},
			})
		case strings.HasPrefix(r.URL.Path, "/wiki/Special:EntityData/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"entities": map[string]any{
					"Q17738": map[string]any{
						"labels":       map[string]any{"en": map[string]any{"value": "Arrival"}},
						"descriptions": map[string]any{"en": map[string]any{"value": "2016 science fiction film"}},
						"claims": map[string]any{
							"P18":  []map[string]any{{"mainsnak": map[string]any{"datavalue": map[string]any{"value": "Arrival film poster.jpg"}}}},
							"P345": []map[string]any{{"mainsnak": map[string]any{"datavalue": map[string]any{"value": "tt2543164"}}}},
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service, catalogService, root := newMetadataTestService(t, server.Config.Handler)
	service.tvMazeBaseURL = server.URL
	service.wikidataSearchURL = server.URL + "/w/api.php"
	service.wikidataEntityURL = server.URL + "/wiki/Special:EntityData"
	service.wikipediaSearchURL = server.URL + "/w/api.php"
	service.wikipediaSummaryURL = server.URL + "/page/summary"

	moviePath := filepath.Join(root, "Movies", "Arrival (2016)", "Arrival.2016.1080p.mkv")
	tvPath := filepath.Join(root, "TV", "The Bear", "Season 01", "The.Bear.S01E01.1080p.mkv")
	writeFile(t, moviePath, "video")
	writeFile(t, tvPath, "video")

	movieID := seedMovie(t, ctx, catalogService, filepath.Join(root, "Movies"), moviePath)
	seriesID := seedSeries(t, ctx, catalogService, filepath.Join(root, "TV"), tvPath)

	movieResult, err := service.Refresh(ctx, RefreshRequest{Kind: "movie", ID: movieID, Title: "Arrival", Year: 2016})
	if err != nil {
		t.Fatalf("refresh movie: %v", err)
	}
	movieBest, ok, err := catalogService.GetBestMetadata(ctx, "movie", movieID)
	if err != nil {
		t.Fatalf("get movie metadata: %v", err)
	}
	if !ok || movieBest.Provider != "wikipedia" || movieBest.Overview == "" {
		t.Fatalf("expected wikipedia movie metadata, got %#v", movieBest)
	}
	if len(movieResult.Warnings) != 0 {
		t.Fatalf("expected automatic movie metadata without warnings, got %#v", movieResult.Warnings)
	}
	if !hasExternalProvider(movieResult.ExternalIDs, "wikidata") || !hasExternalProvider(movieResult.ExternalIDs, "imdb") {
		t.Fatalf("expected wikidata and imdb ids for movie, got %#v", movieResult.ExternalIDs)
	}

	seriesResult, err := service.Refresh(ctx, RefreshRequest{Kind: "series", ID: seriesID, Title: "The Bear"})
	if err != nil {
		t.Fatalf("refresh series: %v", err)
	}
	seriesBest, ok, err := catalogService.GetBestMetadata(ctx, "series", seriesID)
	if err != nil {
		t.Fatalf("get series metadata: %v", err)
	}
	if !ok || seriesBest.Provider != "tvmaze" || !strings.Contains(seriesBest.Overview, "young chef") {
		t.Fatalf("expected tvmaze series metadata, got %#v", seriesBest)
	}
	if !hasRatingType(seriesResult.Ratings, "tvmaze") {
		t.Fatalf("expected tvmaze rating, got %#v", seriesResult.Ratings)
	}
	if !hasExternalProvider(seriesResult.ExternalIDs, "imdb") || !hasExternalProvider(seriesResult.ExternalIDs, "tvdb") {
		t.Fatalf("expected external ids from tvmaze, got %#v", seriesResult.ExternalIDs)
	}
	if len(seriesResult.Warnings) != 0 {
		t.Fatalf("expected automatic series metadata without warnings, got %#v", seriesResult.Warnings)
	}
}

func TestRefreshUsesTVDBWhenConfigured(t *testing.T) {
	// TVDB provider is permanently disabled in this build (see automatic.go
	// refreshTVDB + config/keys.go notes). Test is preserved but skipped so
	// the historical fixture stays alongside the dormant code path; remove
	// both together if/when TVDB is re-enabled.
	t.Skip("TVDB provider disabled; embedded-keys build does not support per-installation TVDB subscriptions")
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"token": "tvdb-token"}})
		case strings.HasPrefix(r.URL.Path, "/search"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"tvdb_id": "411211",
					"name":    "The Bear",
					"year":    "2022",
				}},
			})
		case strings.HasPrefix(r.URL.Path, "/series/411211/extended"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"name":       "The Bear",
					"overview":   "A chef returns home to run his family sandwich shop.",
					"image":      "https://img.example/tvdb-bear-poster.jpg",
					"firstAired": "2022-06-23",
					"url":        "https://thetvdb.com/series/the-bear",
					"score":      8.9,
					"artworks":   map[string]any{"background": "https://img.example/tvdb-bear-backdrop.jpg"},
					"remote_ids": map[string]any{"imdb": "tt14452776", "tmdb": "136315"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Config{MetadataDir: filepath.Join(root, "metadata")}
	if err := os.MkdirAll(cfg.MetadataDir, 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	catalogService := catalog.NewService(db)
	service := newServiceWithClient(cfg, catalogService, nil, server.Client())
	service.tvdbBaseURL = server.URL

	tvPath := filepath.Join(root, "TV", "The Bear", "Season 01", "The.Bear.S01E01.1080p.mkv")
	writeFile(t, tvPath, "video")
	seriesID := seedSeries(t, ctx, catalogService, filepath.Join(root, "TV"), tvPath)

	result, err := service.Refresh(ctx, RefreshRequest{Kind: "series", ID: seriesID, Title: "The Bear"})
	if err != nil {
		t.Fatalf("refresh series with tvdb: %v", err)
	}
	if !hasExternalProvider(result.ExternalIDs, "tvdb") || !hasRatingType(result.Ratings, "tvdb") {
		t.Fatalf("expected tvdb ids and ratings, got %#v %#v", result.ExternalIDs, result.Ratings)
	}
}

func TestRefreshMovieFallsBackSearchTermsForWikipediaAndWikidata(t *testing.T) {
	ctx := context.Background()
	wikipediaQueries := []string{}
	wikidataQueries := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/w/api.php"):
			action := r.URL.Query().Get("action")
			search := strings.TrimSpace(r.URL.Query().Get("search"))
			switch action {
			case "opensearch":
				wikipediaQueries = append(wikipediaQueries, search)
				if strings.EqualFold(search, "Arrival 2016 film") {
					_ = json.NewEncoder(w).Encode([]any{"Arrival", []any{}, []any{}, []any{}})
					return
				}
				if strings.EqualFold(search, "Arrival film") || strings.EqualFold(search, "Arrival") {
					_ = json.NewEncoder(w).Encode([]any{"Arrival", []any{"Arrival (film)"}, []any{""}, []any{""}})
					return
				}
				_ = json.NewEncoder(w).Encode([]any{"Arrival", []any{}, []any{}, []any{}})
				return
			case "wbsearchentities":
				wikidataQueries = append(wikidataQueries, search)
				if strings.EqualFold(search, "Arrival 2016 film") {
					_ = json.NewEncoder(w).Encode(map[string]any{"search": []map[string]any{}})
					return
				}
				if strings.EqualFold(search, "Arrival film") || strings.EqualFold(search, "Arrival") {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"search": []map[string]any{{
							"id":          "Q17738",
							"label":       "Arrival",
							"description": "2016 science fiction film",
						}},
					})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"search": []map[string]any{}})
				return
			}
		case strings.HasPrefix(r.URL.Path, "/page/summary/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"titles":        map[string]any{"canonical": "Arrival_(film)", "normalized": "Arrival"},
				"extract":       "A linguist is recruited after alien ships arrive.",
				"thumbnail":     map[string]any{"source": "https://img.example/arrival-thumb.jpg"},
				"originalimage": map[string]any{"source": "https://img.example/arrival-original.jpg"},
			})
			return
		case strings.HasPrefix(r.URL.Path, "/wiki/Special:EntityData/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"entities": map[string]any{
					"Q17738": map[string]any{
						"labels":       map[string]any{"en": map[string]any{"value": "Arrival"}},
						"descriptions": map[string]any{"en": map[string]any{"value": "2016 science fiction film"}},
						"claims": map[string]any{
							"P18":  []map[string]any{{"mainsnak": map[string]any{"datavalue": map[string]any{"value": "Arrival film poster.jpg"}}}},
							"P345": []map[string]any{{"mainsnak": map[string]any{"datavalue": map[string]any{"value": "tt2543164"}}}},
						},
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	service, catalogService, root := newMetadataTestService(t, server.Config.Handler)
	service.wikidataSearchURL = server.URL + "/w/api.php"
	service.wikidataEntityURL = server.URL + "/wiki/Special:EntityData"
	service.wikipediaSearchURL = server.URL + "/w/api.php"
	service.wikipediaSummaryURL = server.URL + "/page/summary"

	moviePath := filepath.Join(root, "Movies", "Arrival (2016)", "Arrival.2016.1080p.mkv")
	writeFile(t, moviePath, "video")
	movieID := seedMovie(t, ctx, catalogService, filepath.Join(root, "Movies"), moviePath)

	result, err := service.Refresh(ctx, RefreshRequest{Kind: "movie", ID: movieID, Title: "Arrival", Year: 2016})
	if err != nil {
		t.Fatalf("refresh movie: %v", err)
	}
	best, ok, err := catalogService.GetBestMetadata(ctx, "movie", movieID)
	if err != nil {
		t.Fatalf("get best metadata: %v", err)
	}
	if !ok || best.Provider != "wikipedia" || strings.TrimSpace(best.Overview) == "" {
		t.Fatalf("expected wikipedia metadata after fallback search, got %#v", best)
	}
	if !hasExternalProvider(result.ExternalIDs, "wikidata") {
		t.Fatalf("expected wikidata external id after fallback search, got %#v", result.ExternalIDs)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %#v", result.Warnings)
	}
	if len(wikipediaQueries) < 2 || !strings.EqualFold(wikipediaQueries[0], "Arrival 2016 film") {
		t.Fatalf("expected strict then fallback wikipedia queries, got %#v", wikipediaQueries)
	}
	if len(wikidataQueries) < 2 || !strings.EqualFold(wikidataQueries[0], "Arrival 2016 film") {
		t.Fatalf("expected strict then fallback wikidata queries, got %#v", wikidataQueries)
	}
}

func TestActiveConfigLoadsSavedSettingsFromDataDir(t *testing.T) {
	root := t.TempDir()
	service := &Service{
		cfg: config.Config{
			DataDir:    root,
			TMDBAPIKey: "",
		},
	}
	if err := config.SaveFile(root, config.Config{
		DataDir:    root,
		TMDBAPIKey: "saved-tmdb-key",
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	active := service.activeConfig()
	if active.TMDBAPIKey != "saved-tmdb-key" {
		t.Fatalf("expected active config to load saved key, got %#v", active)
	}
}

func TestManagedProviderRateLimitFallsBackAndSetsHealth(t *testing.T) {
	ctx := context.Background()
	tmdbCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/search/movie"):
			tmdbCalls++
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"status_message":"Rate limit exceeded"}`))
		case strings.HasPrefix(r.URL.Path, "/w/api.php"):
			_ = json.NewEncoder(w).Encode([]any{"Arrival", []any{"Arrival (film)"}, []any{""}, []any{""}})
		case strings.HasPrefix(r.URL.Path, "/page/summary/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"titles":        map[string]any{"canonical": "Arrival_(film)", "normalized": "Arrival"},
				"extract":       "A linguist is recruited after alien ships arrive.",
				"thumbnail":     map[string]any{"source": "https://img.example/arrival-thumb.jpg"},
				"originalimage": map[string]any{"source": "https://img.example/arrival-original.jpg"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service, catalogService, root := newMetadataTestService(t, server.Config.Handler)
	service.cfg.TMDBAPIKey = "server-managed-tmdb-key"
	service.tmdbBaseURL = server.URL
	service.wikipediaSearchURL = server.URL + "/w/api.php"
	service.wikipediaSummaryURL = server.URL + "/page/summary"

	moviePath := filepath.Join(root, "Movies", "Arrival (2016)", "Arrival.2016.1080p.mkv")
	writeFile(t, moviePath, "video")
	movieID := seedMovie(t, ctx, catalogService, filepath.Join(root, "Movies"), moviePath)

	first, err := service.Refresh(ctx, RefreshRequest{Kind: "movie", ID: movieID, Title: "Arrival", Year: 2016})
	if err != nil {
		t.Fatalf("refresh movie first pass: %v", err)
	}
	best, ok, err := catalogService.GetBestMetadata(ctx, "movie", movieID)
	if err != nil {
		t.Fatalf("get best metadata: %v", err)
	}
	if !ok || best.Provider != "wikipedia" {
		t.Fatalf("expected wikipedia fallback metadata, got %#v", best)
	}
	if !containsWarning(first.Warnings, "TMDB refresh failed") {
		t.Fatalf("expected tmdb warning after rate limit, got %#v", first.Warnings)
	}
	if tmdbCalls != 1 {
		t.Fatalf("expected one tmdb call, got %d", tmdbCalls)
	}

	health := service.ProviderHealth(ctx)["tmdb"]
	if health.Status != "rate_limited" || health.QuotaLimited != true || health.Healthy != false {
		t.Fatalf("expected tmdb health to be rate-limited, got %#v", health)
	}
	if health.BackoffUntil == "" {
		t.Fatalf("expected tmdb backoff to be set, got %#v", health)
	}

	second, err := service.Refresh(ctx, RefreshRequest{Kind: "movie", ID: movieID, Title: "Arrival", Year: 2016})
	if err != nil {
		t.Fatalf("refresh movie second pass: %v", err)
	}
	if tmdbCalls != 1 {
		t.Fatalf("expected tmdb call to be skipped during cooldown, got %d calls", tmdbCalls)
	}
	expectedSnippet := "provider temporarily rate-limited"
	if !containsWarning(second.Warnings, expectedSnippet) {
		t.Fatalf("expected cooldown warning containing %q, got %#v", expectedSnippet, second.Warnings)
	}
}

func TestStartBackfillReturnsNotConfiguredWhenTMDBKeyMissing(t *testing.T) {
	origDefault := config.DefaultTMDBAPIKey
	config.DefaultTMDBAPIKey = ""
	t.Cleanup(func() {
		config.DefaultTMDBAPIKey = origDefault
	})
	t.Setenv("XUVA_MANAGED_TMDB_API_KEY", "")
	t.Setenv("XUVA_TMDB_API_KEY", "")

	service, _, _ := newMetadataTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))

	err := service.StartBackfill(context.Background(), "tmdb")
	if err == nil {
		t.Fatalf("expected missing key error")
	}
	if !IsBackfillProviderNotConfigured(err) {
		t.Fatalf("expected ErrBackfillProviderNotConfigured, got %v", err)
	}
}

func newMetadataTestService(t *testing.T, handler http.Handler) (*Service, *catalog.Service, string) {
	t.Helper()

	root := t.TempDir()
	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	cfg := config.Config{MetadataDir: filepath.Join(root, "metadata")}
	if err := os.MkdirAll(cfg.MetadataDir, 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	catalogService := catalog.NewService(db)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	service := newServiceWithClient(cfg, catalogService, nil, client)
	service.tvMazeBaseURL = server.URL
	service.wikidataSearchURL = server.URL + "/w/api.php"
	service.wikidataEntityURL = server.URL + "/wiki/Special:EntityData"
	service.wikipediaSearchURL = server.URL + "/w/api.php"
	service.wikipediaSummaryURL = server.URL + "/page/summary"
	return service, catalogService, root
}

func seedMovie(t *testing.T, ctx context.Context, service *catalog.Service, libraryRoot string, mediaPath string) string {
	t.Helper()
	library := libraries.Library{ID: "movies", Name: "Movies", Path: libraryRoot, Kind: libraries.KindMovies}
	file := fileCandidate(mediaPath, strings.TrimPrefix(strings.ReplaceAll(mediaPath, libraryRoot+string(filepath.Separator), ""), string(filepath.Separator)))
	result := scanResult(scanner.KindMovies, libraryRoot, []scanner.FileCandidate{file})
	candidates := []movies.Candidate{{
		Title:        "Arrival",
		Year:         2016,
		QualityLabel: "1080p",
		Media:        file,
	}}
	if _, err := service.SaveMovieScan(ctx, library, result, candidates); err != nil {
		t.Fatalf("save movie scan: %v", err)
	}
	items, err := service.ListMovies(ctx, 10, "", "")
	if err != nil || len(items) == 0 {
		t.Fatalf("list movies: %v %#v", err, items)
	}
	return items[0].ID
}

func seedSeries(t *testing.T, ctx context.Context, service *catalog.Service, libraryRoot string, mediaPath string) string {
	t.Helper()
	library := libraries.Library{ID: "tv", Name: "TV", Path: libraryRoot, Kind: libraries.KindTV}
	relPath := strings.ReplaceAll(strings.TrimPrefix(mediaPath, libraryRoot+string(filepath.Separator)), string(filepath.Separator), string(filepath.Separator))
	file := fileCandidate(mediaPath, relPath)
	result := scanResult(scanner.KindTV, libraryRoot, []scanner.FileCandidate{file})
	candidates := []tv.EpisodeCandidate{{
		SeriesTitle:   "The Bear",
		SeasonNumber:  1,
		EpisodeNumber: 1,
		EpisodeTitle:  "Pilot",
		QualityLabel:  "1080p",
		Media:         file,
	}}
	if _, err := service.SaveTVScan(ctx, library, result, candidates); err != nil {
		t.Fatalf("save tv scan: %v", err)
	}
	items, err := service.ListSeries(ctx, 10, "", "")
	if err != nil || len(items) == 0 {
		t.Fatalf("list series: %v %#v", err, items)
	}
	return items[0].ID
}

func fileCandidate(path string, relPath string) scanner.FileCandidate {
	return scanner.FileCandidate{
		Path:       filepath.Clean(path),
		RelPath:    filepath.Clean(relPath),
		Name:       filepath.Base(path),
		Extension:  ".mkv",
		Size:       1024,
		ModifiedAt: time.Now().UTC(),
		Changed:    true,
	}
}

func scanResult(kind scanner.LibraryKind, root string, files []scanner.FileCandidate) scanner.Result {
	startedAt := time.Now().UTC()
	return scanner.Result{
		Summary: scanner.Summary{
			Kind:         kind,
			Root:         root,
			StartedAt:    startedAt,
			CompletedAt:  startedAt.Add(time.Millisecond),
			DurationMS:   1,
			TotalFiles:   len(files),
			MediaFiles:   len(files),
			ChangedFiles: len(files),
			Extensions:   map[string]int{".mkv": len(files)},
		},
		Files:        files,
		SeenRelPaths: []string{files[0].RelPath},
	}
}

func hasRatingType(ratings []catalog.Rating, ratingType string) bool {
	for _, rating := range ratings {
		if rating.RatingType == ratingType {
			return true
		}
	}
	return false
}

func hasExternalProvider(items []catalog.ExternalID, provider string) bool {
	for _, item := range items {
		if item.Provider == provider && item.ExternalID != "" {
			return true
		}
	}
	return false
}

func containsWarning(warnings []string, expected string) bool {
	expected = strings.ToLower(strings.TrimSpace(expected))
	for _, warning := range warnings {
		if strings.Contains(strings.ToLower(warning), expected) {
			return true
		}
	}
	return false
}

func writeFile(t *testing.T, path string, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestShouldSkipMetadataKeepsArtlessRecordsEligible is the regression test for
// #401. Before the fix, a TMDB metadata record with overview but no poster
// URL satisfied hasEnrichedMetadata and was permanently skipped by every
// subsequent RefreshBatch — its library card stayed on the gradient
// placeholder forever. After the fix, missing-art records remain eligible
// so the next batch can re-query for the poster.
//
// We exercise hasEnrichedMetadata directly because shouldSkipMetadata's
// outer switch only delegates to hasEnrichedMetadata for the major online
// providers (tmdb / tvdb / omdb / tvmaze / wikipedia / wikidata), and the
// enrichment helper is where the regression lived.
func TestShouldSkipMetadataKeepsArtlessRecordsEligible(t *testing.T) {
	cases := []struct {
		name       string
		record     *catalog.MetadataRecord
		wantSkip   bool
		wantReason string
	}{
		{
			name:       "nil record stays eligible",
			record:     nil,
			wantSkip:   false,
			wantReason: "no record means nothing has tried yet",
		},
		{
			name: "overview without poster stays eligible (#401 regression)",
			record: &catalog.MetadataRecord{
				Kind:     "movie",
				Provider: "tmdb",
				Title:    "Sample",
				Overview: "Some long overview text",
				// PosterURL and BackdropURL intentionally empty
			},
			wantSkip:   false,
			wantReason: "missing poster should not block re-fetch",
		},
		{
			name: "poster without overview stays eligible",
			record: &catalog.MetadataRecord{
				Kind:      "movie",
				Provider:  "tmdb",
				Title:     "Sample",
				PosterURL: "https://image.tmdb.org/t/p/original/abc.jpg",
				// Overview intentionally empty
			},
			wantSkip:   false,
			wantReason: "missing overview should not block re-fetch",
		},
		{
			name: "overview + poster is the only fully-enriched case",
			record: &catalog.MetadataRecord{
				Kind:      "movie",
				Provider:  "tmdb",
				Title:     "Sample",
				Overview:  "Some long overview text",
				PosterURL: "https://image.tmdb.org/t/p/original/abc.jpg",
			},
			wantSkip:   true,
			wantReason: "both dimensions present — no value in another provider call",
		},
		{
			name: "overview + backdrop also counts as enriched (visual covered)",
			record: &catalog.MetadataRecord{
				Kind:        "movie",
				Provider:    "tmdb",
				Title:       "Sample",
				Overview:    "Some long overview text",
				BackdropURL: "https://image.tmdb.org/t/p/original/back.jpg",
			},
			wantSkip:   true,
			wantReason: "backdrop satisfies the visual dimension",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasEnrichedMetadata(tc.record)
			if got != tc.wantSkip {
				t.Fatalf("hasEnrichedMetadata = %v, want %v (%s)", got, tc.wantSkip, tc.wantReason)
			}
		})
	}
}
