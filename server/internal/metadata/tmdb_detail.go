package metadata

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/catalog"
	"github.com/EJM-Digital/Xuva/server/internal/config"
	"github.com/EJM-Digital/Xuva/server/internal/trailers"
)

// metadataLangCode extracts the primary language sub-tag from a BCP-47 tag.
// e.g. "en-US" → "en", "fr-FR" → "fr", "de" → "de".
func metadataLangCode(bcp47 string) string {
	if bcp47 == "" {
		return "en"
	}
	if idx := strings.IndexByte(bcp47, '-'); idx > 0 {
		return bcp47[:idx]
	}
	return bcp47
}

// videosToCandidates flattens TMDB's videos.results into the trailer
// picker's input shape. Kept here so refreshTMDBMovie/Series share it.
func videosToCandidates(in []tmdbVideoAsset) []trailers.VideoCandidate {
	out := make([]trailers.VideoCandidate, 0, len(in))
	for _, v := range in {
		out = append(out, trailers.VideoCandidate{
			Key:      v.Key,
			Site:     v.Site,
			Type:     v.Type,
			Official: v.Official,
		})
	}
	return out
}

func (s *Service) refreshTMDB(ctx context.Context, request RefreshRequest, order []string, cfg config.Config, result *RefreshResult) error {
	if request.Kind != "movie" && request.Kind != "series" {
		return nil
	}
	apiKey := managedProviderCredential("tmdb", cfg)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// 1. Direct-ID path: use override ID from request, or extract from filename.
	tmdbID := request.TMDBOverrideID
	if tmdbID == 0 && request.Filename != "" {
		tmdbID = extractTMDBIDFromFilename(request.Filename)
	}
	if tmdbID > 0 {
		if request.Kind == "movie" {
			return s.refreshTMDBMovie(ctx, request, order, apiKey, tmdbID, now, cfg, result)
		}
		return s.refreshTMDBSeries(ctx, request, order, apiKey, tmdbID, now, cfg, result)
	}

	// 2. Search-based path (fallback).
	path := "movie"
	if request.Kind == "series" {
		path = "tv"
	}
	searchURL := fmt.Sprintf("%s/search/%s?", strings.TrimRight(s.tmdbBaseURL, "/"), path) + url.Values{
		"api_key":  {apiKey},
		"query":    {request.Title},
		"language": {cfg.MetadataLanguage},
	}.Encode()
	if request.Year > 0 && request.Kind == "movie" {
		searchURL += "&year=" + strconv.Itoa(request.Year)
	}
	var search tmdbSearch
	if err := s.getJSON(ctx, searchURL, &search); err != nil {
		return err
	}
	if len(search.Results) == 0 {
		return errors.New("no TMDB match")
	}
	match := bestTMDBMatch(request, search.Results)
	if match.ID == 0 {
		return errors.New("no TMDB match")
	}
	if request.Kind == "movie" {
		return s.refreshTMDBMovie(ctx, request, order, apiKey, match.ID, now, cfg, result)
	}
	return s.refreshTMDBSeries(ctx, request, order, apiKey, match.ID, now, cfg, result)
}

func (s *Service) refreshTMDBMovie(ctx context.Context, request RefreshRequest, order []string, apiKey string, tmdbID int, now string, cfg config.Config, result *RefreshResult) error {
	imgLang := metadataLangCode(cfg.MetadataLanguage) + ",en,null"
	detailURL := fmt.Sprintf("%s/movie/%d?", strings.TrimRight(s.tmdbBaseURL, "/"), tmdbID) + url.Values{
		"api_key":                 {apiKey},
		"language":                {cfg.MetadataLanguage},
		"append_to_response":      {"credits,release_dates,images,external_ids,videos"},
		"include_image_language":  {imgLang},
		"include_video_language":  {metadataLangCode(cfg.MetadataLanguage) + ",en,null"},
	}.Encode()
	var detail tmdbMovieDetail
	if err := s.getJSON(ctx, detailURL, &detail); err != nil {
		return err
	}
	backdropPath := tmdbBestBackdropPath(detail.Images.Backdrops, detail.BackdropPath)
	record := catalog.MetadataRecord{
		Kind:                request.Kind,
		ItemID:              request.ID,
		Provider:            "tmdb",
		ExternalID:          strconv.Itoa(detail.ID),
		Title:               firstNonEmpty(detail.Title, request.Title),
		OriginalTitle:       detail.OriginalTitle,
		Year:                parseYear(detail.ReleaseDate, request.Year),
		ReleaseDate:         detail.ReleaseDate,
		Overview:            detail.Overview,
		RuntimeMinutes:      detail.Runtime,
		Genres:              tmdbGenreNames(detail.Genres),
		ContentRating:       tmdbMovieCertification(detail.ReleaseDates.Results),
		Cast:                tmdbCredits(detail.Credits.Cast, "cast"),
		Crew:                tmdbCredits(detail.Credits.Crew, "crew"),
		Directors:           tmdbCrewNames(detail.Credits.Crew, "Director"),
		Writers:             tmdbCrewNames(detail.Credits.Crew, "Writer", "Screenplay", "Story"),
		Studios:             tmdbCompanyNames(detail.ProductionCompanies),
		ProductionCompanies: tmdbCompanyNames(detail.ProductionCompanies),
		PosterURL:           tmdbBestPoster(detail.Images.Posters, detail.PosterPath),
		BackdropURL:         tmdbImageURL(backdropPath, "original"),
		LogoURL:             tmdbLogoURL(detail.Images.Logos),
		BannerURL:           "",
		ThumbnailURL:        tmdbImageURL(backdropPath, "w780"),
		VideoKey:            trailers.PickBestTrailer(videosToCandidates(detail.Videos.Results)),
		Collection:          tmdbCollectionRecord(detail.BelongsToCollection),
		Confidence:          sourceConfidence(order, "tmdb", 0.9),
		RawJSON:             mustJSON(detail),
		FetchedAt:           now,
		UpdatedAt:           now,
	}
	if err := s.catalog.UpsertMetadataRecord(ctx, record); err != nil {
		return err
	}
	result.Records = append(result.Records, record)
	// Kick off a background trailer download for this title. Fire-and-forget;
	// the downloader handles dedupe, backoff, and disk caching internally.
	if s.trailers != nil && record.VideoKey != "" {
		s.trailers.Queue(trailers.Job{
			Kind:     request.Kind,
			ItemID:   request.ID,
			TMDBID:   strconv.Itoa(detail.ID),
			VideoKey: record.VideoKey,
		})
	}
	upsertTMDBExternalIDs(ctx, s.catalog, request.Kind, request.ID, detail.ExternalIDs, strconv.Itoa(detail.ID), now, result)
	return s.upsertTMDBRating(ctx, request.Kind, request.ID, "movie", detail.ID, detail.VoteAverage, detail.VoteCount, now, result)
}

func (s *Service) refreshTMDBSeries(ctx context.Context, request RefreshRequest, order []string, apiKey string, tmdbID int, now string, cfg config.Config, result *RefreshResult) error {
	imgLang := metadataLangCode(cfg.MetadataLanguage) + ",en,null"
	detailURL := fmt.Sprintf("%s/tv/%d?", strings.TrimRight(s.tmdbBaseURL, "/"), tmdbID) + url.Values{
		"api_key":                 {apiKey},
		"language":                {cfg.MetadataLanguage},
		"append_to_response":      {"aggregate_credits,content_ratings,images,external_ids,videos"},
		"include_image_language":  {imgLang},
		"include_video_language":  {metadataLangCode(cfg.MetadataLanguage) + ",en,null"},
	}.Encode()
	var detail tmdbTVDetail
	if err := s.getJSON(ctx, detailURL, &detail); err != nil {
		return err
	}
	backdropPath := tmdbBestBackdropPath(detail.Images.Backdrops, detail.BackdropPath)
	record := catalog.MetadataRecord{
		Kind:                request.Kind,
		ItemID:              request.ID,
		Provider:            "tmdb",
		ExternalID:          strconv.Itoa(detail.ID),
		Title:               firstNonEmpty(detail.Name, request.Title),
		OriginalTitle:       detail.OriginalName,
		Year:                parseYear(detail.FirstAirDate, request.Year),
		FirstAirDate:        detail.FirstAirDate,
		ReleaseDate:         detail.FirstAirDate,
		Overview:            detail.Overview,
		Genres:              tmdbGenreNames(detail.Genres),
		ContentRating:       tmdbTVRatingLabel(detail.ContentRatings.Results),
		Cast:                tmdbAggregateCredits(detail.AggregateCredits.Cast),
		Crew:                tmdbCredits(detail.AggregateCredits.Crew, "crew"),
		Directors:           tmdbCrewNames(detail.AggregateCredits.Crew, "Director"),
		Studios:             tmdbCompanyNames(detail.ProductionCompanies),
		ProductionCompanies: tmdbCompanyNames(detail.ProductionCompanies),
		Networks:            tmdbNetworkNames(detail.Networks),
		StatusText:          detail.Status,
		EpisodeCount:        detail.NumberOfEpisodes,
		PosterURL:           tmdbBestPoster(detail.Images.Posters, detail.PosterPath),
		BackdropURL:         tmdbImageURL(backdropPath, "original"),
		LogoURL:             tmdbLogoURL(detail.Images.Logos),
		ThumbnailURL:        tmdbImageURL(backdropPath, "w780"),
		VideoKey:            trailers.PickBestTrailer(videosToCandidates(detail.Videos.Results)),
		Confidence:          sourceConfidence(order, "tmdb", 0.9),
		RawJSON:             mustJSON(detail),
		FetchedAt:           now,
		UpdatedAt:           now,
	}
	if err := s.catalog.UpsertMetadataRecord(ctx, record); err != nil {
		return err
	}
	result.Records = append(result.Records, record)
	if s.trailers != nil && record.VideoKey != "" {
		s.trailers.Queue(trailers.Job{
			Kind:     request.Kind,
			ItemID:   request.ID,
			TMDBID:   strconv.Itoa(detail.ID),
			VideoKey: record.VideoKey,
		})
	}
	upsertTMDBExternalIDs(ctx, s.catalog, request.Kind, request.ID, detail.ExternalIDs, strconv.Itoa(detail.ID), now, result)
	if err := s.upsertTMDBRating(ctx, request.Kind, request.ID, "tv", detail.ID, detail.VoteAverage, detail.VoteCount, now, result); err != nil {
		return err
	}
	if s.catalog == nil {
		return nil
	}
	series, ok, err := s.catalog.GetSeries(ctx, request.ID)
	if err != nil || !ok {
		return err
	}
	seasonIDs := map[int]string{}
	episodeIDs := map[int]map[int]string{}
	for _, season := range series.Seasons {
		seasonIDs[season.SeasonNumber] = season.ID
		if episodeIDs[season.SeasonNumber] == nil {
			episodeIDs[season.SeasonNumber] = map[int]string{}
		}
		for _, episode := range season.Episodes {
			episodeIDs[season.SeasonNumber][episode.EpisodeNumber] = episode.ID
		}
	}
	for _, season := range detail.Seasons {
		seasonID := seasonIDs[season.SeasonNumber]
		if seasonID == "" {
			continue
		}
		if err := s.refreshTMDBSeason(ctx, request.ID, seasonID, season.SeasonNumber, apiKey, tmdbID, order, now, cfg, result, episodeIDs[season.SeasonNumber]); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("TMDB season %d refresh failed: %v", season.SeasonNumber, err))
		}
	}
	return nil
}

func (s *Service) refreshTMDBSeason(ctx context.Context, seriesID string, seasonID string, seasonNumber int, apiKey string, tmdbSeriesID int, order []string, now string, cfg config.Config, result *RefreshResult, episodeIDs map[int]string) error {
	seasonURL := fmt.Sprintf("%s/tv/%d/season/%d?", strings.TrimRight(s.tmdbBaseURL, "/"), tmdbSeriesID, seasonNumber) + url.Values{
		"api_key":                {apiKey},
		"language":               {cfg.MetadataLanguage},
		"append_to_response":     {"credits"},
		"include_image_language": {metadataLangCode(cfg.MetadataLanguage) + ",en,null"},
	}.Encode()
	var detail tmdbSeasonDetail
	if err := s.getJSON(ctx, seasonURL, &detail); err != nil {
		return err
	}
	seasonRecord := catalog.MetadataRecord{
		Kind:          "season",
		ItemID:        seasonID,
		Provider:      "tmdb",
		ExternalID:    strconv.Itoa(detail.ID),
		Title:         firstNonEmpty(detail.Name, fmt.Sprintf("Season %d", seasonNumber)),
		Overview:      detail.Overview,
		AirDate:       detail.AirDate,
		PosterURL:     tmdbImageURL(detail.PosterPath, "original"),
		SeasonNumber:  seasonNumber,
		EpisodeCount:  len(detail.Episodes),
		Cast:          tmdbCredits(detail.Credits.Cast, "cast"),
		Crew:          tmdbCredits(detail.Credits.Crew, "crew"),
		Confidence:    sourceConfidence(order, "tmdb", 0.88),
		RawJSON:       mustJSON(detail),
		FetchedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.catalog.UpsertMetadataRecord(ctx, seasonRecord); err != nil {
		return err
	}
	result.Records = append(result.Records, seasonRecord)
	for _, episode := range detail.Episodes {
		if episodeIDs == nil {
			continue
		}
		episodeID := episodeIDs[episode.EpisodeNumber]
		if episodeID == "" {
			continue
		}
		episodeRecord := catalog.MetadataRecord{
			Kind:           "episode",
			ItemID:         episodeID,
			Provider:       "tmdb",
			ExternalID:     strconv.Itoa(episode.ID),
			Title:          firstNonEmpty(episode.Name, fmt.Sprintf("Episode %d", episode.EpisodeNumber)),
			Overview:       episode.Overview,
			AirDate:        episode.AirDate,
			RuntimeMinutes: episode.Runtime,
			ContentRating:  "",
			GuestCast:      tmdbCredits(episode.GuestStars, "guest"),
			Crew:           tmdbCredits(episode.Crew, "crew"),
			Directors:      tmdbCrewNames(episode.Crew, "Director"),
			ThumbnailURL:   tmdbImageURL(episode.StillPath, "original"),
			SeasonNumber:   episode.SeasonNumber,
			EpisodeNumber:  episode.EpisodeNumber,
			Confidence:     sourceConfidence(order, "tmdb", 0.86),
			RawJSON:        mustJSON(episode),
			FetchedAt:      now,
			UpdatedAt:      now,
		}
		if err := s.catalog.UpsertMetadataRecord(ctx, episodeRecord); err != nil {
			return err
		}
		result.Records = append(result.Records, episodeRecord)
		if episode.VoteAverage > 0 {
			rating := catalog.Rating{
				Kind:         "episode",
				ItemID:       episodeID,
				Provider:     "tmdb",
				RatingType:   "tmdb",
				Value:        episode.VoteAverage,
				DisplayValue: fmt.Sprintf("%.1f/10", episode.VoteAverage),
				Scale:        10,
				SourceURL:    fmt.Sprintf("https://www.themoviedb.org/tv/%d/season/%d/episode/%d", tmdbSeriesID, episode.SeasonNumber, episode.EpisodeNumber),
				FetchedAt:    now,
				UpdatedAt:    now,
			}
			if err := s.catalog.UpsertRatings(ctx, []catalog.Rating{rating}); err != nil {
				return err
			}
			result.Ratings = append(result.Ratings, rating)
		}
	}
	return nil
}

func (s *Service) upsertTMDBRating(ctx context.Context, kind string, itemID string, path string, tmdbID int, voteAverage float64, voteCount int, now string, result *RefreshResult) error {
	rating := catalog.Rating{
		Kind:         kind,
		ItemID:       itemID,
		Provider:     "tmdb",
		RatingType:   "tmdb",
		Value:        voteAverage,
		DisplayValue: fmt.Sprintf("%.1f/10", voteAverage),
		Scale:        10,
		Votes:        voteCount,
		SourceURL:    fmt.Sprintf("https://www.themoviedb.org/%s/%d", path, tmdbID),
		FetchedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.catalog.UpsertRatings(ctx, []catalog.Rating{rating}); err != nil {
		return err
	}
	result.Ratings = append(result.Ratings, rating)
	return nil
}

func upsertTMDBExternalIDs(ctx context.Context, catalogService *catalog.Service, kind string, itemID string, externalIDs tmdbExternalIDs, tmdbID string, now string, result *RefreshResult) {
	items := []catalog.ExternalID{
		{Kind: kind, ItemID: itemID, Provider: "tmdb", ExternalID: tmdbID, UpdatedAt: now},
		{Kind: kind, ItemID: itemID, Provider: "imdb", ExternalID: strings.TrimSpace(externalIDs.IMDbID), UpdatedAt: now},
		{Kind: kind, ItemID: itemID, Provider: "tvdb", ExternalID: intString(externalIDs.TVDBID), UpdatedAt: now},
	}
	for _, item := range items {
		if strings.TrimSpace(item.ExternalID) == "" {
			continue
		}
		_ = catalogService.UpsertExternalID(ctx, item)
		result.ExternalIDs = append(result.ExternalIDs, item)
	}
}

func bestTMDBMatch(request RefreshRequest, items []tmdbResult) tmdbResult {
	best := tmdbResult{}
	bestValue := -1.0
	for _, item := range items {
		score := matchScore(request.Title, firstNonEmpty(item.Title, item.Name), request.Year, parseYear(firstNonEmpty(item.ReleaseDate, item.FirstAirDate), request.Year))
		if score > bestValue {
			bestValue = score
			best = item
		}
	}
	return best
}

func tmdbGenreNames(items []tmdbGenre) []string {
	output := make([]string, 0, len(items))
	for _, item := range items {
		if trimmed := strings.TrimSpace(item.Name); trimmed != "" {
			output = append(output, trimmed)
		}
	}
	return output
}

func tmdbCompanyNames(items []tmdbNamedItem) []string {
	output := make([]string, 0, len(items))
	for _, item := range items {
		if trimmed := strings.TrimSpace(item.Name); trimmed != "" {
			output = append(output, trimmed)
		}
	}
	return output
}

func tmdbNetworkNames(items []tmdbNamedItem) []string {
	return tmdbCompanyNames(items)
}

func tmdbCredits(items []tmdbCredit, fallbackRole string) []catalog.MetadataCredit {
	output := make([]catalog.MetadataCredit, 0, len(items))
	for index, item := range items {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		output = append(output, catalog.MetadataCredit{
			Name:       name,
			Role:       firstNonEmpty(item.Job, item.Character, fallbackRole),
			Character:  strings.TrimSpace(item.Character),
			Department: strings.TrimSpace(item.Department),
			ProfileURL: tmdbImageURL(item.ProfilePath, "w185"),
			SortOrder:  index,
		})
	}
	return output
}

func tmdbAggregateCredits(items []tmdbAggregateCredit) []catalog.MetadataCredit {
	output := make([]catalog.MetadataCredit, 0, len(items))
	for index, item := range items {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		characters := []string{}
		for _, role := range item.Roles {
			if trimmed := strings.TrimSpace(role.Character); trimmed != "" {
				characters = append(characters, trimmed)
			}
		}
		output = append(output, catalog.MetadataCredit{
			Name:       name,
			Role:       firstNonEmpty(strings.Join(characters, ", "), "cast"),
			Character:  strings.Join(characters, ", "),
			ProfileURL: tmdbImageURL(item.ProfilePath, "w185"),
			SortOrder:  index,
		})
	}
	return output
}

func tmdbCrewNames(items []tmdbCredit, jobs ...string) []string {
	allowed := map[string]struct{}{}
	for _, job := range jobs {
		allowed[strings.ToLower(strings.TrimSpace(job))] = struct{}{}
	}
	names := []string{}
	seen := map[string]struct{}{}
	for _, item := range items {
		job := strings.ToLower(strings.TrimSpace(item.Job))
		if _, ok := allowed[job]; !ok {
			continue
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		names = append(names, name)
	}
	return names
}

func tmdbMovieCertification(results []tmdbMovieReleaseCountry) string {
	for _, result := range results {
		if !strings.EqualFold(result.ISO31661, "US") && !strings.EqualFold(result.ISO31661, "AU") {
			continue
		}
		for _, release := range result.ReleaseDates {
			if trimmed := strings.TrimSpace(release.Certification); trimmed != "" {
				return trimmed
			}
		}
	}
	for _, result := range results {
		for _, release := range result.ReleaseDates {
			if trimmed := strings.TrimSpace(release.Certification); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func tmdbTVRatingLabel(results []tmdbTVContentRating) string {
	for _, result := range results {
		if !strings.EqualFold(result.ISO31661, "US") && !strings.EqualFold(result.ISO31661, "AU") {
			continue
		}
		if trimmed := strings.TrimSpace(result.Rating); trimmed != "" {
			return trimmed
		}
	}
	for _, result := range results {
		if trimmed := strings.TrimSpace(result.Rating); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func tmdbLogoURL(items []tmdbImageAsset) string {
	for _, item := range items {
		if strings.EqualFold(item.ISO6391, "en") && strings.TrimSpace(item.FilePath) != "" {
			return tmdbImageURL(item.FilePath, "original")
		}
	}
	for _, item := range items {
		if strings.TrimSpace(item.FilePath) != "" {
			return tmdbImageURL(item.FilePath, "original")
		}
	}
	return ""
}

// tmdbBestPoster picks the best English-language poster from the filtered
// images list (already limited to en+null by include_image_language=en,null).
// Falls back through: English-tagged → language-neutral → raw poster_path.
func tmdbBestPoster(items []tmdbImageAsset, fallbackPath string) string {
	for _, item := range items {
		if strings.EqualFold(item.ISO6391, "en") && strings.TrimSpace(item.FilePath) != "" {
			return tmdbImageURL(item.FilePath, "original")
		}
	}
	for _, item := range items {
		if item.ISO6391 == "" && strings.TrimSpace(item.FilePath) != "" {
			return tmdbImageURL(item.FilePath, "original")
		}
	}
	if strings.TrimSpace(fallbackPath) != "" {
		return tmdbImageURL(fallbackPath, "original")
	}
	return ""
}

// tmdbBestBackdropPath returns the file_path of the best backdrop image,
// using the same English → language-neutral → raw fallback priority as
// tmdbBestPoster.  Returns the path (not a full URL) so callers can
// generate different size variants (original, w780, etc.).
func tmdbBestBackdropPath(items []tmdbImageAsset, fallbackPath string) string {
	for _, item := range items {
		if strings.EqualFold(item.ISO6391, "en") && strings.TrimSpace(item.FilePath) != "" {
			return item.FilePath
		}
	}
	for _, item := range items {
		if item.ISO6391 == "" && strings.TrimSpace(item.FilePath) != "" {
			return item.FilePath
		}
	}
	return fallbackPath
}

func tmdbCollectionRecord(item *tmdbCollection) *catalog.MetadataCollection {
	if item == nil || item.ID == 0 {
		return nil
	}
	return &catalog.MetadataCollection{
		ID:          strconv.Itoa(item.ID),
		Name:        strings.TrimSpace(item.Name),
		PosterURL:   tmdbImageURL(item.PosterPath, "original"),
		BackdropURL: tmdbImageURL(item.BackdropPath, "original"),
	}
}

type tmdbMovieDetail struct {
	ID                  int                `json:"id"`
	Title               string             `json:"title"`
	OriginalTitle       string             `json:"original_title"`
	Overview            string             `json:"overview"`
	ReleaseDate         string             `json:"release_date"`
	Runtime             int                `json:"runtime"`
	PosterPath          string             `json:"poster_path"`
	BackdropPath        string             `json:"backdrop_path"`
	VoteAverage         float64            `json:"vote_average"`
	VoteCount           int                `json:"vote_count"`
	Genres              []tmdbGenre        `json:"genres"`
	ProductionCompanies []tmdbNamedItem    `json:"production_companies"`
	BelongsToCollection *tmdbCollection    `json:"belongs_to_collection"`
	ExternalIDs         tmdbExternalIDs    `json:"external_ids"`
	Credits             struct {
		Cast []tmdbCredit `json:"cast"`
		Crew []tmdbCredit `json:"crew"`
	} `json:"credits"`
	ReleaseDates struct {
		Results []tmdbMovieReleaseCountry `json:"results"`
	} `json:"release_dates"`
	Images struct {
		Posters   []tmdbImageAsset `json:"posters"`
		Logos     []tmdbImageAsset `json:"logos"`
		Backdrops []tmdbImageAsset `json:"backdrops"`
	} `json:"images"`
	Videos struct {
		Results []tmdbVideoAsset `json:"results"`
	} `json:"videos"`
}

type tmdbTVDetail struct {
	ID                  int                 `json:"id"`
	Name                string              `json:"name"`
	OriginalName        string              `json:"original_name"`
	Overview            string              `json:"overview"`
	FirstAirDate        string              `json:"first_air_date"`
	PosterPath          string              `json:"poster_path"`
	BackdropPath        string              `json:"backdrop_path"`
	VoteAverage         float64             `json:"vote_average"`
	VoteCount           int                 `json:"vote_count"`
	Genres              []tmdbGenre         `json:"genres"`
	Networks            []tmdbNamedItem     `json:"networks"`
	ProductionCompanies []tmdbNamedItem     `json:"production_companies"`
	Status              string              `json:"status"`
	NumberOfEpisodes    int                 `json:"number_of_episodes"`
	ExternalIDs         tmdbExternalIDs     `json:"external_ids"`
	Seasons             []tmdbTVSeasonRef   `json:"seasons"`
	ContentRatings struct {
		Results []tmdbTVContentRating `json:"results"`
	} `json:"content_ratings"`
	AggregateCredits struct {
		Cast []tmdbAggregateCredit `json:"cast"`
		Crew []tmdbCredit         `json:"crew"`
	} `json:"aggregate_credits"`
	Images struct {
		Posters   []tmdbImageAsset `json:"posters"`
		Logos     []tmdbImageAsset `json:"logos"`
		Backdrops []tmdbImageAsset `json:"backdrops"`
	} `json:"images"`
	Videos struct {
		Results []tmdbVideoAsset `json:"results"`
	} `json:"videos"`
}

type tmdbVideoAsset struct {
	Key         string `json:"key"`
	Site        string `json:"site"`
	Type        string `json:"type"`
	Official    bool   `json:"official"`
	Size        int    `json:"size"`
	ISO_639_1   string `json:"iso_639_1"`
	PublishedAt string `json:"published_at"`
}

type tmdbSeasonDetail struct {
	ID         int          `json:"id"`
	Name       string       `json:"name"`
	Overview   string       `json:"overview"`
	AirDate    string       `json:"air_date"`
	PosterPath string       `json:"poster_path"`
	Episodes   []tmdbEpisode `json:"episodes"`
	Credits    struct {
		Cast []tmdbCredit `json:"cast"`
		Crew []tmdbCredit `json:"crew"`
	} `json:"credits"`
}

type tmdbEpisode struct {
	ID            int          `json:"id"`
	Name          string       `json:"name"`
	Overview      string       `json:"overview"`
	AirDate       string       `json:"air_date"`
	Runtime       int          `json:"runtime"`
	StillPath     string       `json:"still_path"`
	SeasonNumber  int          `json:"season_number"`
	EpisodeNumber int          `json:"episode_number"`
	VoteAverage   float64      `json:"vote_average"`
	GuestStars    []tmdbCredit `json:"guest_stars"`
	Crew          []tmdbCredit `json:"crew"`
}

type tmdbGenre struct {
	Name string `json:"name"`
}

type tmdbNamedItem struct {
	Name string `json:"name"`
}

type tmdbCollection struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	PosterPath   string `json:"poster_path"`
	BackdropPath string `json:"backdrop_path"`
}

type tmdbExternalIDs struct {
	IMDbID string `json:"imdb_id"`
	TVDBID int    `json:"tvdb_id"`
}

type tmdbCredit struct {
	Name        string `json:"name"`
	Character   string `json:"character"`
	Job         string `json:"job"`
	Department  string `json:"department"`
	ProfilePath string `json:"profile_path"`
}

type tmdbAggregateCredit struct {
	Name        string `json:"name"`
	ProfilePath string `json:"profile_path"`
	Roles       []struct {
		Character string `json:"character"`
	} `json:"roles"`
}

type tmdbMovieReleaseCountry struct {
	ISO31661    string `json:"iso_3166_1"`
	ReleaseDates []struct {
		Certification string `json:"certification"`
	} `json:"release_dates"`
}

type tmdbTVContentRating struct {
	ISO31661 string `json:"iso_3166_1"`
	Rating   string `json:"rating"`
}

type tmdbTVSeasonRef struct {
	SeasonNumber int `json:"season_number"`
}

type tmdbImageAsset struct {
	FilePath string `json:"file_path"`
	ISO6391  string `json:"iso_639_1"`
}
