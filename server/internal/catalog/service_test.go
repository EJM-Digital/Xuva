package catalog

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/database"
	"github.com/EJM-Digital/Xuva/server/internal/libraries"
	"github.com/EJM-Digital/Xuva/server/internal/movies"
	"github.com/EJM-Digital/Xuva/server/internal/scanner"
	"github.com/EJM-Digital/Xuva/server/internal/tv"
)

func TestSaveMovieScanIsIdempotent(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{ID: "movies", Name: "Movies", Path: `X:\Movies`, Kind: libraries.KindMovies}
	result := scanResult(scanner.KindMovies, library.Path, []scanner.FileCandidate{fileCandidate(`X:\Movies\Heat (1995)\Heat.1995.1080p.mkv`, "Heat (1995)/Heat.1995.1080p.mkv")})
	candidates := []movies.Candidate{{
		Title:        "Heat",
		Year:         1995,
		QualityLabel: "1080p",
		Media:        result.Files[0],
	}}

	if _, err := service.SaveMovieScan(ctx, library, result, candidates); err != nil {
		t.Fatalf("save movie scan: %v", err)
	}
	if _, err := service.SaveMovieScan(ctx, library, result, candidates); err != nil {
		t.Fatalf("save movie scan again: %v", err)
	}
	summary, err := service.Summary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}

	if summary.Libraries != 1 {
		t.Fatalf("expected one library, got %d", summary.Libraries)
	}
	if summary.MediaSources != 1 {
		t.Fatalf("expected one media source, got %d", summary.MediaSources)
	}
	if summary.Movies != 1 {
		t.Fatalf("expected one movie, got %d", summary.Movies)
	}
	if summary.ScanRuns != 2 {
		t.Fatalf("expected two scan runs, got %d", summary.ScanRuns)
	}

	movies, err := service.ListMovies(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list movies: %v", err)
	}
	if len(movies) != 1 || movies[0].Title != "Heat" || movies[0].VersionCount != 1 {
		t.Fatalf("unexpected movies: %#v", movies)
	}
	detail, ok, err := service.GetMovie(ctx, movies[0].ID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}
	if !ok || len(detail.Versions) != 1 {
		t.Fatalf("unexpected movie detail: %#v", detail)
	}
	if detail.Metadata == nil || detail.Metadata.Provider != "filename" || detail.Metadata.Title != "Heat" || detail.Metadata.Year != 1995 {
		t.Fatalf("expected filename metadata on movie detail, got %#v", detail.Metadata)
	}
	records, err := service.ListMetadataRecords(ctx, "movie", movies[0].ID)
	if err != nil {
		t.Fatalf("list metadata records: %v", err)
	}
	if len(records) != 1 || records[0].Provider != "filename" {
		t.Fatalf("expected filename metadata record, got %#v", records)
	}
	if err := service.ApplyMetadata(ctx, MetadataUpdate{
		Kind:     "movie",
		ID:       movies[0].ID,
		Title:    "Heat",
		Year:     1995,
		Provider: "manual",
		Overview: "A professional thief weighs one last score.",
	}); err != nil {
		t.Fatalf("apply metadata: %v", err)
	}
	best, ok, err := service.GetBestMetadata(ctx, "movie", movies[0].ID)
	if err != nil {
		t.Fatalf("best metadata: %v", err)
	}
	if !ok || best.Provider != "manual" || best.Overview == "" {
		t.Fatalf("expected manual metadata to win, got %#v", best)
	}
	sources, err := service.ListMediaSources(ctx, 10, true)
	if err != nil {
		t.Fatalf("list media sources: %v", err)
	}
	if len(sources) != 1 || sources[0].Probed {
		t.Fatalf("expected one unprobed source, got %#v", sources)
	}
	if err := service.SaveProbe(ctx, sources[0].ID, ProbeResult{
		Container:       "matroska,webm",
		DurationSeconds: 120,
		Bitrate:         1000,
		VideoCodec:      "h264",
		Width:           1920,
		Height:          1080,
		AudioStreams:    1,
		SubtitleStreams: 1,
		RawJSON:         "{}",
	}); err != nil {
		t.Fatalf("save probe: %v", err)
	}
	source, ok, err := service.GetMediaSource(ctx, sources[0].ID)
	if err != nil {
		t.Fatalf("get media source: %v", err)
	}
	if !ok || !source.Probed || source.VideoCodec != "h264" {
		t.Fatalf("unexpected probed source: %#v", source)
	}
}

func TestSaveTVScanStoresSeriesEpisodeAndSource(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{ID: "tv", Name: "TV", Path: `X:\TV`, Kind: libraries.KindTV}
	result := scanResult(scanner.KindTV, library.Path, []scanner.FileCandidate{fileCandidate(`X:\TV\The Bear\Season 02\The.Bear.S02E03.Sundae.mkv`, "The Bear/Season 02/The.Bear.S02E03.Sundae.mkv")})
	candidates := []tv.EpisodeCandidate{{
		SeriesTitle:   "The Bear",
		SeasonNumber:  2,
		EpisodeNumber: 3,
		EpisodeTitle:  "Sundae",
		QualityLabel:  "1080p",
		Media:         result.Files[0],
	}}

	if _, err := service.SaveTVScan(ctx, library, result, candidates); err != nil {
		t.Fatalf("save tv scan: %v", err)
	}
	summary, err := service.Summary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}

	if summary.Series != 1 {
		t.Fatalf("expected one series, got %d", summary.Series)
	}
	if summary.Episodes != 1 {
		t.Fatalf("expected one episode, got %d", summary.Episodes)
	}
	if summary.MediaSources != 1 {
		t.Fatalf("expected one media source, got %d", summary.MediaSources)
	}

	series, err := service.ListSeries(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list series: %v", err)
	}
	if len(series) != 1 || series[0].Title != "The Bear" || series[0].EpisodeCount != 1 {
		t.Fatalf("unexpected series: %#v", series)
	}
	detail, ok, err := service.GetSeries(ctx, series[0].ID)
	if err != nil {
		t.Fatalf("get series: %v", err)
	}
	if !ok || len(detail.Seasons) != 1 || len(detail.Seasons[0].Episodes) != 1 {
		t.Fatalf("unexpected series detail: %#v", detail)
	}
	if detail.Metadata == nil || detail.Metadata.Provider != "filename" || detail.Metadata.Title != "The Bear" {
		t.Fatalf("expected filename metadata on series detail, got %#v", detail.Metadata)
	}
}

func TestSaveTVScanKeepsUnmatchedEpisodesSeparate(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{ID: "tv", Name: "TV", Path: `X:\TV`, Kind: libraries.KindTV}
	files := []scanner.FileCandidate{
		fileCandidate(`X:\TV\Series\Special.Video.A.mkv`, "Series/Special.Video.A.mkv"),
		fileCandidate(`X:\TV\Series\Special.Video.B.mkv`, "Series/Special.Video.B.mkv"),
	}
	result := scanResult(scanner.KindTV, library.Path, files)
	candidates := []tv.EpisodeCandidate{
		{SeriesTitle: "Series", NeedsReview: true, ReviewReason: "unable to infer episode number", Media: files[0]},
		{SeriesTitle: "Series", NeedsReview: true, ReviewReason: "unable to infer episode number", Media: files[1]},
	}

	persisted, err := service.SaveTVScan(ctx, library, result, candidates)
	if err != nil {
		t.Fatalf("save tv scan: %v", err)
	}
	if persisted.Episodes != 2 {
		t.Fatalf("expected two separate review episodes, got %d", persisted.Episodes)
	}
	summary, err := service.Summary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Episodes != 2 {
		t.Fatalf("expected two stored review episodes, got %d", summary.Episodes)
	}
	reviewItems, err := service.ReviewItems(ctx, 10)
	if err != nil {
		t.Fatalf("review items: %v", err)
	}
	if len(reviewItems) != 2 {
		t.Fatalf("expected two review items, got %#v", reviewItems)
	}
}

func TestSaveMovieScanPersistsIncrementalState(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{ID: "movies", Name: "Movies", Path: `X:\Movies`, Kind: libraries.KindMovies}
	file := fileCandidate(`X:\Movies\Heat (1995)\Heat.1995.1080p.mkv`, "Heat (1995)/Heat.1995.1080p.mkv")
	result := scanResult(scanner.KindMovies, library.Path, []scanner.FileCandidate{file})
	candidates := []movies.Candidate{{Title: "Heat", Year: 1995, QualityLabel: "1080p", Media: file}}
	persisted, err := service.SaveMovieScan(ctx, library, result, candidates)
	if err != nil {
		t.Fatalf("save movie scan: %v", err)
	}
	if persisted.ChangedSources != 1 || persisted.UnchangedSources != 0 {
		t.Fatalf("expected changed source count, got %#v", persisted)
	}
	state, err := service.ScanState(ctx, library.ID)
	if err != nil {
		t.Fatalf("scan state: %v", err)
	}
	signature, ok := state[file.RelPath]
	if !ok {
		t.Fatalf("expected scan state for %q, got %#v", file.RelPath, state)
	}
	if signature.Size != file.Size || !signature.ModifiedAt.Equal(file.ModifiedAt) {
		t.Fatalf("unexpected signature: %#v", signature)
	}
	file.Changed = false
	result = scanResult(scanner.KindMovies, library.Path, []scanner.FileCandidate{file})
	candidates[0].Media = file
	persisted, err = service.SaveMovieScan(ctx, library, result, candidates)
	if err != nil {
		t.Fatalf("save incremental movie scan: %v", err)
	}
	if persisted.ChangedSources != 0 || persisted.UnchangedSources != 1 {
		t.Fatalf("expected unchanged source count, got %#v", persisted)
	}
}

func TestBestMetadataHonorsLibrarySourceOrder(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{
		ID:              "movies",
		Name:            "Movies",
		Path:            `X:\Movies`,
		Kind:            libraries.KindMovies,
		MetadataSources: []string{"wikipedia", "filename", "nfo"},
	}
	file := fileCandidate(`X:\Movies\Arrival (2016)\Arrival.2016.1080p.mkv`, "Arrival (2016)/Arrival.2016.1080p.mkv")
	result := scanResult(scanner.KindMovies, library.Path, []scanner.FileCandidate{file})
	candidates := []movies.Candidate{{
		Title:        "Arrival",
		Year:         2016,
		QualityLabel: "1080p",
		Media:        file,
	}}
	if _, err := service.SaveMovieScan(ctx, library, result, candidates); err != nil {
		t.Fatalf("save movie scan: %v", err)
	}
	movieList, err := service.ListMovies(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list movies: %v", err)
	}
	if len(movieList) != 1 {
		t.Fatalf("expected one movie, got %#v", movieList)
	}
	movieID := movieList[0].ID
	if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
		Kind:       "movie",
		ItemID:     movieID,
		Provider:   "filename",
		Title:      "Arrival",
		Year:       2016,
		Confidence: 0.98,
	}); err != nil {
		t.Fatalf("upsert filename metadata: %v", err)
	}
	if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
		Kind:       "movie",
		ItemID:     movieID,
		Provider:   "wikipedia",
		Title:      "Arrival (Film)",
		Year:       2016,
		Confidence: 0.45,
	}); err != nil {
		t.Fatalf("upsert wikipedia metadata: %v", err)
	}
	best, ok, err := service.GetBestMetadata(ctx, "movie", movieID)
	if err != nil {
		t.Fatalf("get best metadata: %v", err)
	}
	if !ok {
		t.Fatalf("expected metadata record")
	}
	if best.Provider != "wikipedia" {
		t.Fatalf("expected library source order to prioritize wikipedia, got %#v", best)
	}
	records, err := service.ListMetadataRecords(ctx, "movie", movieID)
	if err != nil {
		t.Fatalf("list metadata records: %v", err)
	}
	if len(records) < 2 {
		t.Fatalf("expected at least two metadata records, got %#v", records)
	}
	if records[0].Provider != "wikipedia" {
		t.Fatalf("expected ordered metadata records to start with wikipedia, got %#v", records[0])
	}
}

func TestBestMetadataUsesSeparateArtworkSourceOrder(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{
		ID:              "movies",
		Name:            "Movies",
		Path:            `X:\Movies`,
		Kind:            libraries.KindMovies,
		MetadataSources: []string{"tmdb", "wikipedia", "filename"},
		ArtworkSources:  []string{"artwork", "fanart", "tmdb"},
	}
	file := fileCandidate(`X:\Movies\Arrival (2016)\Arrival.2016.1080p.mkv`, "Arrival (2016)/Arrival.2016.1080p.mkv")
	result := scanResult(scanner.KindMovies, library.Path, []scanner.FileCandidate{file})
	candidates := []movies.Candidate{{
		Title:        "Arrival",
		Year:         2016,
		QualityLabel: "1080p",
		Media:        file,
	}}
	if _, err := service.SaveMovieScan(ctx, library, result, candidates); err != nil {
		t.Fatalf("save movie scan: %v", err)
	}
	movieList, err := service.ListMovies(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list movies: %v", err)
	}
	if len(movieList) != 1 {
		t.Fatalf("expected one movie, got %#v", movieList)
	}
	movieID := movieList[0].ID
	if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
		Kind:       "movie",
		ItemID:     movieID,
		Provider:   "tmdb",
		Title:      "Arrival",
		Year:       2016,
		Overview:   "TMDB overview",
		Genres:     []string{"Science Fiction", "Drama"},
		PosterURL:  "https://images.example/tmdb-poster.jpg",
		Confidence: 0.95,
	}); err != nil {
		t.Fatalf("upsert tmdb metadata: %v", err)
	}
	if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
		Kind:        "movie",
		ItemID:      movieID,
		Provider:    "artwork",
		Title:       "Arrival",
		PosterURL:   `X:\Movies\Arrival (2016)\poster.jpg`,
		BackdropURL: `X:\Movies\Arrival (2016)\fanart.jpg`,
		Confidence:  1,
	}); err != nil {
		t.Fatalf("upsert local artwork: %v", err)
	}
	if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
		Kind:       "movie",
		ItemID:     movieID,
		Provider:   "fanart",
		Title:      "Arrival",
		LogoURL:    "https://images.example/fanart-logo.png",
		BannerURL:  "https://images.example/fanart-banner.jpg",
		Confidence: 0.7,
	}); err != nil {
		t.Fatalf("upsert fanart artwork: %v", err)
	}

	best, ok, err := service.GetBestMetadata(ctx, "movie", movieID)
	if err != nil {
		t.Fatalf("get best metadata: %v", err)
	}
	if !ok {
		t.Fatalf("expected merged metadata")
	}
	if best.Provider != "tmdb" || best.Overview != "TMDB overview" {
		t.Fatalf("expected tmdb to drive metadata fields, got %#v", best)
	}
	if best.PosterURL != `X:\Movies\Arrival (2016)\poster.jpg` || best.BackdropURL != `X:\Movies\Arrival (2016)\fanart.jpg` {
		t.Fatalf("expected local artwork to win for poster/backdrop, got %#v", best)
	}
	if best.LogoURL != "https://images.example/fanart-logo.png" || best.BannerURL != "https://images.example/fanart-banner.jpg" {
		t.Fatalf("expected fanart artwork enhancement to fill logo/banner, got %#v", best)
	}
}

func TestGetSeriesAttachesSeasonAndEpisodeMetadata(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{ID: "tv", Name: "TV", Path: `X:\TV`, Kind: libraries.KindTV}
	file := fileCandidate(`X:\TV\The Bear\Season 01\The.Bear.S01E01.System.mkv`, "The Bear/Season 01/The.Bear.S01E01.System.mkv")
	result := scanResult(scanner.KindTV, library.Path, []scanner.FileCandidate{file})
	candidates := []tv.EpisodeCandidate{{
		SeriesTitle:   "The Bear",
		SeasonNumber:  1,
		EpisodeNumber: 1,
		EpisodeTitle:  "System",
		QualityLabel:  "1080p",
		Media:         file,
	}}

	if _, err := service.SaveTVScan(ctx, library, result, candidates); err != nil {
		t.Fatalf("save tv scan: %v", err)
	}
	seriesList, err := service.ListSeries(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list series: %v", err)
	}
	if len(seriesList) != 1 {
		t.Fatalf("expected one series, got %#v", seriesList)
	}
	seeded, ok, err := service.GetSeries(ctx, seriesList[0].ID)
	if err != nil {
		t.Fatalf("get seeded series: %v", err)
	}
	if !ok || len(seeded.Seasons) != 1 || len(seeded.Seasons[0].Episodes) != 1 {
		t.Fatalf("expected one season and one episode, got %#v", seeded)
	}

	if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
		Kind:       "series",
		ItemID:     seeded.ID,
		Provider:   "tmdb",
		Title:      "The Bear",
		Overview:   "A chef returns home to run the family restaurant.",
		Genres:     []string{"Drama"},
		PosterURL:  "https://images.example/bear-poster.jpg",
		Confidence: 0.95,
	}); err != nil {
		t.Fatalf("upsert series metadata: %v", err)
	}
	if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
		Kind:         "season",
		ItemID:       seeded.Seasons[0].ID,
		Provider:     "tmdb",
		Title:        "Season 1",
		Overview:     "Season overview",
		PosterURL:    "https://images.example/bear-season-1.jpg",
		SeasonNumber: 1,
		EpisodeCount: 1,
		Confidence:   0.9,
	}); err != nil {
		t.Fatalf("upsert season metadata: %v", err)
	}
	if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
		Kind:          "episode",
		ItemID:        seeded.Seasons[0].Episodes[0].ID,
		Provider:      "tmdb",
		Title:         "System",
		Overview:      "Episode overview",
		ThumbnailURL:  "https://images.example/bear-s01e01.jpg",
		SeasonNumber:  1,
		EpisodeNumber: 1,
		RuntimeMinutes: 42,
		Confidence:    0.9,
	}); err != nil {
		t.Fatalf("upsert episode metadata: %v", err)
	}

	detail, ok, err := service.GetSeries(ctx, seeded.ID)
	if err != nil {
		t.Fatalf("get enriched series: %v", err)
	}
	if !ok {
		t.Fatalf("expected enriched series detail")
	}
	if detail.Metadata == nil || detail.Metadata.Provider != "tmdb" || detail.Metadata.Overview == "" {
		t.Fatalf("expected show metadata on detail, got %#v", detail.Metadata)
	}
	season := detail.Seasons[0]
	if season.Metadata == nil || season.Metadata.PosterURL != "https://images.example/bear-season-1.jpg" || season.Metadata.EpisodeCount != 1 {
		t.Fatalf("expected season metadata on detail, got %#v", season.Metadata)
	}
	episode := season.Episodes[0]
	if episode.Metadata == nil || episode.Metadata.ThumbnailURL != "https://images.example/bear-s01e01.jpg" || episode.Metadata.RuntimeMinutes != 42 {
		t.Fatalf("expected episode metadata on detail, got %#v", episode.Metadata)
	}
}

func TestListSeriesCollapsesDuplicatesBySharedExternalIdentity(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{ID: "tv", Name: "TV", Path: `X:\TV`, Kind: libraries.KindTV}
	files := []scanner.FileCandidate{
		fileCandidate(`X:\TV\Dangerously Obese\Season 01\Dangerously.Obese.S01E01.mkv`, "Dangerously Obese/Season 01/Dangerously.Obese.S01E01.mkv"),
		fileCandidate(`X:\TV\Dangerously Obese UK\Season 02\Dangerously.Obese.UK.S02E01.mkv`, "Dangerously Obese UK/Season 02/Dangerously.Obese.UK.S02E01.mkv"),
	}
	result := scanResult(scanner.KindTV, library.Path, files)
	candidates := []tv.EpisodeCandidate{
		{SeriesTitle: "Dangerously Obese", SeasonNumber: 1, EpisodeNumber: 1, EpisodeTitle: "Episode 1", Media: files[0]},
		{SeriesTitle: "Dangerously Obese UK", SeasonNumber: 2, EpisodeNumber: 1, EpisodeTitle: "Episode 1", Media: files[1]},
	}
	if _, err := service.SaveTVScan(ctx, library, result, candidates); err != nil {
		t.Fatalf("save tv scan: %v", err)
	}

	series, err := service.ListSeries(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list raw series: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected two raw series before metadata grouping assumptions, got %#v", series)
	}
	for _, item := range series {
		if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
			Kind:       "series",
			ItemID:     item.ID,
			Provider:   "tmdb",
			Title:      "Dangerously Obese",
			Overview:   "Merged by shared metadata identity.",
			PosterURL:  "https://images.example/dangerously-obese.jpg",
			Confidence: 0.9,
		}); err != nil {
			t.Fatalf("upsert series metadata: %v", err)
		}
		if err := service.UpsertExternalID(ctx, ExternalID{
			Kind:       "series",
			ItemID:     item.ID,
			Provider:   "tmdb",
			ExternalID: "9991",
		}); err != nil {
			t.Fatalf("upsert shared tmdb id: %v", err)
		}
	}

	grouped, err := service.ListSeries(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list grouped series: %v", err)
	}
	if len(grouped) != 1 {
		t.Fatalf("expected duplicate series to collapse into one list item, got %#v", grouped)
	}
	if grouped[0].SeasonCount != 2 || grouped[0].EpisodeCount != 2 {
		t.Fatalf("expected grouped counts across both records, got %#v", grouped[0])
	}
	if grouped[0].Metadata == nil || grouped[0].Metadata.Title != "Dangerously Obese" {
		t.Fatalf("expected grouped metadata title, got %#v", grouped[0].Metadata)
	}
}

func TestGetSeriesAggregatesDuplicateSeriesMembers(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{ID: "tv", Name: "TV", Path: `X:\TV`, Kind: libraries.KindTV}
	files := []scanner.FileCandidate{
		fileCandidate(`X:\TV\Dangerously Obese\Season 01\Dangerously.Obese.S01E01.mkv`, "Dangerously Obese/Season 01/Dangerously.Obese.S01E01.mkv"),
		fileCandidate(`X:\TV\Dangerously Obese UK\Season 02\Dangerously.Obese.UK.S02E01.mkv`, "Dangerously Obese UK/Season 02/Dangerously.Obese.UK.S02E01.mkv"),
	}
	result := scanResult(scanner.KindTV, library.Path, files)
	candidates := []tv.EpisodeCandidate{
		{SeriesTitle: "Dangerously Obese", SeasonNumber: 1, EpisodeNumber: 1, EpisodeTitle: "Episode 1", Media: files[0]},
		{SeriesTitle: "Dangerously Obese UK", SeasonNumber: 2, EpisodeNumber: 1, EpisodeTitle: "Episode 1", Media: files[1]},
	}
	if _, err := service.SaveTVScan(ctx, library, result, candidates); err != nil {
		t.Fatalf("save tv scan: %v", err)
	}

	rawSeries, err := service.ListSeries(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list series: %v", err)
	}
	if len(rawSeries) != 2 {
		t.Fatalf("expected two seeded series, got %#v", rawSeries)
	}
	for _, item := range rawSeries {
		if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
			Kind:       "series",
			ItemID:     item.ID,
			Provider:   "tmdb",
			Title:      "Dangerously Obese",
			Overview:   "Merged by shared metadata identity.",
			PosterURL:  "https://images.example/dangerously-obese.jpg",
			Confidence: 0.9,
		}); err != nil {
			t.Fatalf("upsert series metadata: %v", err)
		}
		if err := service.UpsertExternalID(ctx, ExternalID{
			Kind:       "series",
			ItemID:     item.ID,
			Provider:   "tmdb",
			ExternalID: "9991",
		}); err != nil {
			t.Fatalf("upsert shared tmdb id: %v", err)
		}
	}

	detail, ok, err := service.GetSeries(ctx, rawSeries[0].ID)
	if err != nil {
		t.Fatalf("get aggregated series: %v", err)
	}
	if !ok {
		t.Fatalf("expected series detail")
	}
	if detail.SeasonCount != 2 || detail.EpisodeCount != 2 {
		t.Fatalf("expected aggregated season and episode counts, got %#v", detail)
	}
	if len(detail.Seasons) != 2 {
		t.Fatalf("expected both seasons to appear in one detail view, got %#v", detail.Seasons)
	}
	if detail.Metadata == nil || detail.Metadata.Title != "Dangerously Obese" {
		t.Fatalf("expected aggregate metadata on detail, got %#v", detail.Metadata)
	}
}

func TestSearchLibrary_EmptyQueryReturnsEmptyBuckets(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	seedSearchableLibrary(t, service)

	results, err := service.SearchLibrary(ctx, "", 8, "")
	if err != nil {
		t.Fatalf("search empty: %v", err)
	}
	if results.Query != "" {
		t.Fatalf("expected echoed empty query, got %q", results.Query)
	}
	if len(results.Movies) != 0 || len(results.Series) != 0 || len(results.People) != 0 || len(results.Collections) != 0 {
		t.Fatalf("expected all buckets empty, got %#v", results)
	}
	// All buckets should be non-nil JSON arrays, not nil.
	if results.Movies == nil || results.Series == nil || results.People == nil || results.Collections == nil {
		t.Fatalf("expected all buckets to be non-nil slices, got %#v", results)
	}
}

func TestSearchLibrary_NoMatchesReturnsEmptyBuckets(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	seedSearchableLibrary(t, service)

	results, err := service.SearchLibrary(ctx, "zzznosuchword", 8, "")
	if err != nil {
		t.Fatalf("search no matches: %v", err)
	}
	if len(results.Movies) != 0 || len(results.Series) != 0 || len(results.People) != 0 || len(results.Collections) != 0 {
		t.Fatalf("expected empty results for no-match query, got %#v", results)
	}
}

func TestSearchLibrary_PopulatesAllFourBuckets(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	seedSearchableLibrary(t, service)

	// "spider" matches a movie ("Spiderhead"), a series ("Spider Forest"), a
	// person ("Spider Lee"), and a collection ("Spider Saga"). See
	// seedSearchableLibrary for the fixture wiring.
	results, err := service.SearchLibrary(ctx, "spider", 8, "")
	if err != nil {
		t.Fatalf("search spider: %v", err)
	}
	if len(results.Movies) < 1 {
		t.Fatalf("expected at least one movie hit, got %#v", results.Movies)
	}
	if len(results.Series) < 1 {
		t.Fatalf("expected at least one series hit, got %#v", results.Series)
	}
	if len(results.People) < 1 {
		t.Fatalf("expected at least one person hit, got %#v", results.People)
	}
	if len(results.Collections) < 1 {
		t.Fatalf("expected at least one collection hit, got %#v", results.Collections)
	}
}

func TestSearchLibrary_LimitEnforcedAndCappedAtForty(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	seedManyMovies(t, service, "limitcheck", 12)

	// Custom limit applied per bucket.
	results, err := service.SearchLibrary(ctx, "limitcheck", 5, "")
	if err != nil {
		t.Fatalf("search with limit 5: %v", err)
	}
	if len(results.Movies) != 5 {
		t.Fatalf("expected exactly 5 movies for limit=5, got %d", len(results.Movies))
	}

	// Default (zero/negative) limit is treated as 8.
	resultsDefault, err := service.SearchLibrary(ctx, "limitcheck", 0, "")
	if err != nil {
		t.Fatalf("search default limit: %v", err)
	}
	if len(resultsDefault.Movies) != 8 {
		t.Fatalf("expected exactly 8 movies for default limit, got %d", len(resultsDefault.Movies))
	}

	// Limit is clamped to 40.
	seedManyMovies(t, service, "bigcap", 60)
	resultsCap, err := service.SearchLibrary(ctx, "bigcap", 100, "")
	if err != nil {
		t.Fatalf("search huge limit: %v", err)
	}
	if len(resultsCap.Movies) > 40 {
		t.Fatalf("expected limit clamped to 40, got %d movies", len(resultsCap.Movies))
	}
}

func TestSearchLibrary_ScoringOrderExactPrefixWordPrefixContains(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{ID: "movies", Name: "Movies", Path: `X:\Movies`, Kind: libraries.KindMovies}

	// All four titles contain "rain" but rank differently:
	//   "Rain"           -> exact match (100)
	//   "Rainmaker"      -> prefix match (80)
	//   "Hard Rain"      -> word-prefix match (60)
	//   "Brain Damage"   -> substring match (40)
	titles := []string{"Rain", "Rainmaker", "Hard Rain", "Brain Damage"}
	for i, title := range titles {
		rel := title + "/" + title + ".1080p.mkv"
		file := fileCandidate(`X:\Movies\`+rel, rel)
		result := scanResult(scanner.KindMovies, library.Path, []scanner.FileCandidate{file})
		candidates := []movies.Candidate{{Title: title, Year: 2000 + i, QualityLabel: "1080p", Media: file}}
		if _, err := service.SaveMovieScan(ctx, library, result, candidates); err != nil {
			t.Fatalf("save movie %q: %v", title, err)
		}
	}

	// Apply tmdb metadata that keeps the title stable so SearchLibrary's
	// scorer (which uses the metadata-applied title) sees the same strings.
	movieList, err := service.ListMovies(ctx, 100, "", "")
	if err != nil {
		t.Fatalf("list movies: %v", err)
	}
	for _, item := range movieList {
		if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
			Kind:       "movie",
			ItemID:     item.ID,
			Provider:   "tmdb",
			Title:      item.Title,
			Year:       item.Year,
			Confidence: 0.9,
		}); err != nil {
			t.Fatalf("upsert movie metadata for %q: %v", item.Title, err)
		}
	}

	results, err := service.SearchLibrary(ctx, "rain", 10, "")
	if err != nil {
		t.Fatalf("search rain: %v", err)
	}
	if len(results.Movies) != 4 {
		t.Fatalf("expected 4 ranked movies, got %#v", results.Movies)
	}
	expectedOrder := []string{"Rain", "Rainmaker", "Hard Rain", "Brain Damage"}
	for i, want := range expectedOrder {
		if results.Movies[i].Title != want {
			t.Fatalf("position %d: expected %q, got %q (full order %#v)", i, want, results.Movies[i].Title, results.Movies)
		}
	}
}

func TestSearchLibrary_PeopleDeduplicatedByName(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{ID: "movies", Name: "Movies", Path: `X:\Movies`, Kind: libraries.KindMovies}

	// Two movies that both credit "Pat Coverstar" â€” the dedupe key is name
	// (lower-cased). After search, we expect a single hit with CreditCount=2.
	for i, title := range []string{"Coverstar Origins", "Coverstar Returns"} {
		rel := title + "/" + title + ".mkv"
		file := fileCandidate(`X:\Movies\`+rel, rel)
		result := scanResult(scanner.KindMovies, library.Path, []scanner.FileCandidate{file})
		candidates := []movies.Candidate{{Title: title, Year: 2010 + i, Media: file}}
		if _, err := service.SaveMovieScan(ctx, library, result, candidates); err != nil {
			t.Fatalf("save %q: %v", title, err)
		}
	}
	movieList, err := service.ListMovies(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list movies: %v", err)
	}
	for _, item := range movieList {
		if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
			Kind:       "movie",
			ItemID:     item.ID,
			Provider:   "tmdb",
			Title:      item.Title,
			Year:       item.Year,
			Confidence: 0.9,
			Cast: []MetadataCredit{
				{Name: "Pat Coverstar", Character: "Lead", ProfileURL: "https://images.example/pat.jpg"},
			},
		}); err != nil {
			t.Fatalf("upsert metadata for %q: %v", item.Title, err)
		}
	}

	results, err := service.SearchLibrary(ctx, "coverstar", 8, "")
	if err != nil {
		t.Fatalf("search coverstar: %v", err)
	}
	if len(results.People) != 1 {
		t.Fatalf("expected one deduped person, got %#v", results.People)
	}
	if results.People[0].Name != "Pat Coverstar" {
		t.Fatalf("expected person name 'Pat Coverstar', got %q", results.People[0].Name)
	}
	if results.People[0].CreditCount < 2 {
		t.Fatalf("expected creditCount >= 2 for person credited in two movies, got %d", results.People[0].CreditCount)
	}
}

func TestSearchLibrary_CollectionsDeduplicatedByID(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{ID: "movies", Name: "Movies", Path: `X:\Movies`, Kind: libraries.KindMovies}

	// Three movies all reference the same collection id "555" but with
	// slightly different cosmetic fields. SearchLibrary should collapse them
	// to one CollectionHit with MovieCount=3.
	titles := []string{"Galaxy Rescue", "Galaxy Heist", "Galaxy Revenge"}
	for i, title := range titles {
		rel := title + "/" + title + ".mkv"
		file := fileCandidate(`X:\Movies\`+rel, rel)
		result := scanResult(scanner.KindMovies, library.Path, []scanner.FileCandidate{file})
		candidates := []movies.Candidate{{Title: title, Year: 2015 + i, Media: file}}
		if _, err := service.SaveMovieScan(ctx, library, result, candidates); err != nil {
			t.Fatalf("save %q: %v", title, err)
		}
	}
	movieList, err := service.ListMovies(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list movies: %v", err)
	}
	for _, item := range movieList {
		if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
			Kind:       "movie",
			ItemID:     item.ID,
			Provider:   "tmdb",
			Title:      item.Title,
			Year:       item.Year,
			Confidence: 0.9,
			Collection: &MetadataCollection{
				ID:        "555",
				Name:      "Galaxy Trilogy",
				PosterURL: "https://images.example/galaxy.jpg",
			},
		}); err != nil {
			t.Fatalf("upsert metadata for %q: %v", item.Title, err)
		}
	}

	results, err := service.SearchLibrary(ctx, "galaxy", 8, "")
	if err != nil {
		t.Fatalf("search galaxy: %v", err)
	}
	if len(results.Collections) != 1 {
		t.Fatalf("expected one deduped collection, got %#v", results.Collections)
	}
	if results.Collections[0].ID != "555" {
		t.Fatalf("expected collection id '555', got %q", results.Collections[0].ID)
	}
	if results.Collections[0].MovieCount != 3 {
		t.Fatalf("expected movieCount=3 for collection across three movies, got %d", results.Collections[0].MovieCount)
	}
}

// seedSearchableLibrary populates the catalog with enough cross-category data
// to exercise all four search buckets simultaneously.
func seedSearchableLibrary(t *testing.T, service *Service) {
	t.Helper()
	ctx := context.Background()

	movieLib := libraries.Library{ID: "movies", Name: "Movies", Path: `X:\Movies`, Kind: libraries.KindMovies}
	tvLib := libraries.Library{ID: "tv", Name: "TV", Path: `X:\TV`, Kind: libraries.KindTV}

	// Movie "Spiderhead" with cast member "Spider Lee" and collection "Spider Saga".
	movieFile := fileCandidate(`X:\Movies\Spiderhead\Spiderhead.1080p.mkv`, "Spiderhead/Spiderhead.1080p.mkv")
	movieResult := scanResult(scanner.KindMovies, movieLib.Path, []scanner.FileCandidate{movieFile})
	movieCandidates := []movies.Candidate{{Title: "Spiderhead", Year: 2022, Media: movieFile}}
	if _, err := service.SaveMovieScan(ctx, movieLib, movieResult, movieCandidates); err != nil {
		t.Fatalf("seed movie: %v", err)
	}

	// A second movie that also references the Spider Saga collection (so
	// MovieCount > 0 and dedupe works deterministically).
	movieFile2 := fileCandidate(`X:\Movies\Spider Sequel\Spider.Sequel.1080p.mkv`, "Spider Sequel/Spider.Sequel.1080p.mkv")
	movieResult2 := scanResult(scanner.KindMovies, movieLib.Path, []scanner.FileCandidate{movieFile2})
	movieCandidates2 := []movies.Candidate{{Title: "Spider Sequel", Year: 2024, Media: movieFile2}}
	if _, err := service.SaveMovieScan(ctx, movieLib, movieResult2, movieCandidates2); err != nil {
		t.Fatalf("seed second movie: %v", err)
	}

	movieList, err := service.ListMovies(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list seed movies: %v", err)
	}
	for _, item := range movieList {
		if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
			Kind:       "movie",
			ItemID:     item.ID,
			Provider:   "tmdb",
			Title:      item.Title,
			Year:       item.Year,
			Confidence: 0.9,
			Cast: []MetadataCredit{
				{Name: "Spider Lee", Character: "Hero", ProfileURL: "https://images.example/spider-lee.jpg"},
			},
			Collection: &MetadataCollection{
				ID:        "42",
				Name:      "Spider Saga",
				PosterURL: "https://images.example/spider-saga.jpg",
			},
		}); err != nil {
			t.Fatalf("upsert movie metadata for %q: %v", item.Title, err)
		}
	}

	// TV series "Spider Forest"
	tvFile := fileCandidate(`X:\TV\Spider Forest\Season 01\Spider.Forest.S01E01.mkv`, "Spider Forest/Season 01/Spider.Forest.S01E01.mkv")
	tvResult := scanResult(scanner.KindTV, tvLib.Path, []scanner.FileCandidate{tvFile})
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
		t.Fatalf("list seed series: %v", err)
	}
	for _, item := range seriesList {
		if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
			Kind:       "series",
			ItemID:     item.ID,
			Provider:   "tmdb",
			Title:      item.Title,
			Confidence: 0.9,
		}); err != nil {
			t.Fatalf("upsert series metadata for %q: %v", item.Title, err)
		}
	}
}

// seedManyMovies inserts n movies whose titles all start with the given prefix
// so SearchLibrary returns a deterministic, scoreable set for limit checks.
func seedManyMovies(t *testing.T, service *Service, prefix string, n int) {
	t.Helper()
	ctx := context.Background()
	library := libraries.Library{ID: "movies-" + prefix, Name: "Movies-" + prefix, Path: `X:\Movies\` + prefix, Kind: libraries.KindMovies}
	for i := 0; i < n; i++ {
		title := prefix + "-" + intToFixedString(i)
		rel := title + "/" + title + ".mkv"
		path := library.Path + `\` + rel
		file := fileCandidate(path, rel)
		result := scanResult(scanner.KindMovies, library.Path, []scanner.FileCandidate{file})
		candidates := []movies.Candidate{{Title: title, Year: 2000 + i, Media: file}}
		if _, err := service.SaveMovieScan(ctx, library, result, candidates); err != nil {
			t.Fatalf("seed many movies (%q): %v", title, err)
		}
	}
}

// intToFixedString formats i with leading zeros so that lexical and numeric
// orders agree across the seeded fixtures (avoids "10" sorting before "2").
func intToFixedString(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return "0" + string(digits[i])
	}
	if i < 100 {
		return string(digits[i/10]) + string(digits[i%10])
	}
	return string(digits[i/100]) + string(digits[(i/10)%10]) + string(digits[i%10])
}

func newTestService(t *testing.T) *Service {
	t.Helper()

	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
	})
	return NewService(db)
}

func scanResult(kind scanner.LibraryKind, root string, files []scanner.FileCandidate) scanner.Result {
	startedAt := time.Now().UTC()
	seen := make([]string, 0, len(files))
	changed := 0
	unchanged := 0
	for _, file := range files {
		seen = append(seen, file.RelPath)
		if file.Changed {
			changed++
		} else {
			unchanged++
		}
	}
	return scanner.Result{
		Summary: scanner.Summary{
			Kind:         kind,
			Root:         root,
			StartedAt:    startedAt,
			CompletedAt:  startedAt.Add(time.Millisecond),
			DurationMS:   1,
			TotalFiles:   len(files),
			MediaFiles:   len(files),
			ChangedFiles: changed,
			Unchanged:    unchanged,
			Extensions:   map[string]int{".mkv": len(files)},
		},
		Files:        files,
		SeenRelPaths: seen,
	}
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

// TestSimilarSeriesHandlesUnmatchedLibraryRows is the regression test for
// #368 / #384. The bug surfaced on the live server as a 500 "similar series
// lookup failed" on every TV detail page; the immediate impact was that the
// "More Like This" rail never rendered on TV detail.
//
// The fix has two layers:
//
//  1. clientSimilarSeriesHandler now returns 200 + empty items on any
//     downstream error, logging the cause via slog.Warn. So even if the
//     underlying query fails for some unexpected reason, the UI no longer
//     blanks — it just hides the rail. That's the production safety net.
//
//  2. The SimilarSeries SQL is hardened against rows where details_json is
//     missing, NULL, or has $.genres as something other than an array, by
//     pre-filtering with json_valid() and json_extract() in a LEFT JOIN.
//     The original query inlined the genre extraction inside json_each(),
//     which made a single problem row stop the whole rowset.
//
// This test exercises the realistic failure case: a freshly-scanned library
// where some candidate series have full metadata (and would normally match
// by genre) while others have none at all (TMDB hasn't matched yet, or the
// match was wiped). Before the LEFT-JOIN refactor the unmatched rows could
// confuse the overlap subquery's COALESCE('{}') path; after the refactor
// they cleanly drop out of consideration without breaking the query.
func TestSimilarSeriesHandlesUnmatchedLibraryRows(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	library := libraries.Library{ID: "tv", Name: "TV", Path: `X:\TV`, Kind: libraries.KindTV}

	// Source series (the one the UI is looking at) — has genres so the query
	// has something to match against.
	srcFile := fileCandidate(`X:\TV\Source Show\Season 01\S01E01.mkv`, "Source Show/Season 01/S01E01.mkv")
	srcCandidate := tv.EpisodeCandidate{SeriesTitle: "Source Show", SeasonNumber: 1, EpisodeNumber: 1, EpisodeTitle: "Pilot", Media: srcFile}
	if _, err := service.SaveTVScan(ctx, library, scanResult(scanner.KindTV, library.Path, []scanner.FileCandidate{srcFile}), []tv.EpisodeCandidate{srcCandidate}); err != nil {
		t.Fatalf("save source series: %v", err)
	}

	// Candidate series — would normally match by genre.
	candFile := fileCandidate(`X:\TV\Candidate Show\Season 01\S01E01.mkv`, "Candidate Show/Season 01/S01E01.mkv")
	candCandidate := tv.EpisodeCandidate{SeriesTitle: "Candidate Show", SeasonNumber: 1, EpisodeNumber: 1, EpisodeTitle: "Pilot", Media: candFile}
	if _, err := service.SaveTVScan(ctx, library, scanResult(scanner.KindTV, library.Path, []scanner.FileCandidate{candFile}), []tv.EpisodeCandidate{candCandidate}); err != nil {
		t.Fatalf("save candidate series: %v", err)
	}

	seriesList, err := service.ListSeries(ctx, 10, "", "")
	if err != nil {
		t.Fatalf("list series: %v", err)
	}
	if len(seriesList) != 2 {
		t.Fatalf("expected 2 series, got %d", len(seriesList))
	}
	var sourceID, candidateID string
	for _, s := range seriesList {
		if s.Title == "Source Show" {
			sourceID = s.ID
		} else {
			candidateID = s.ID
		}
	}

	// Give both series valid metadata so they overlap on a real genre.
	if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
		Kind: "series", ItemID: sourceID, Provider: "tmdb",
		Title: "Source Show", Genres: []string{"Drama"}, Confidence: 0.9,
	}); err != nil {
		t.Fatalf("upsert source metadata: %v", err)
	}
	if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
		Kind: "series", ItemID: candidateID, Provider: "tmdb",
		Title: "Candidate Show", Genres: []string{"Drama"}, Confidence: 0.9,
	}); err != nil {
		t.Fatalf("upsert candidate metadata: %v", err)
	}

	// Seed a third series with NO metadata record at all — a realistic state
	// during a fresh library scan, between SaveTVScan and the first TMDB
	// match. The pre-fix query coalesced this case to '{}' inside json_each
	// which could subtly miscount overlap (counting a phantom row when
	// $.genres was missing); the post-fix LEFT JOIN drops it cleanly so it
	// neither blows up the query nor contaminates the rankings.
	unmatchedFile := fileCandidate(`X:\TV\Unmatched\Season 01\S01E01.mkv`, "Unmatched/Season 01/S01E01.mkv")
	unmatchedCand := tv.EpisodeCandidate{SeriesTitle: "Unmatched Show", SeasonNumber: 1, EpisodeNumber: 1, EpisodeTitle: "Pilot", Media: unmatchedFile}
	if _, err := service.SaveTVScan(ctx, library, scanResult(scanner.KindTV, library.Path, []scanner.FileCandidate{unmatchedFile}), []tv.EpisodeCandidate{unmatchedCand}); err != nil {
		t.Fatalf("save unmatched series: %v", err)
	}

	// SimilarSeries must succeed and return the valid Drama match even with
	// the unmatched row in the table.
	result, err := service.SimilarSeries(ctx, sourceID, 20)
	if err != nil {
		t.Fatalf("SimilarSeries should survive malformed JSON in another row, got: %v", err)
	}
	if len(result.Items) == 0 {
		t.Fatalf("expected the valid Drama candidate to be returned despite a sibling malformed row, got 0 items")
	}
	var foundCandidate bool
	for _, it := range result.Items {
		if it.ID == candidateID {
			foundCandidate = true
			break
		}
	}
	if !foundCandidate {
		t.Fatalf("expected to find candidate series %s in results, got %#v", candidateID, result.Items)
	}
}



