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
)

func (s *Service) refreshAutomaticOnline(ctx context.Context, request RefreshRequest, metadataOrder []string, artworkOrder []string, cfg config.Config, result *RefreshResult) error {
	warnings := []string{}
	for _, source := range combinedSourceOrder(metadataOrder, artworkOrder) {
		source = normalizeProviderID(source)
		switch source {
		case "filename", "manual", "nfo", "artwork":
			continue
		case "tvmaze":
			if request.Kind != "series" {
				continue
			}
			if err := s.runProvider(source, false, cfg, func() error {
				return s.refreshTVMaze(ctx, request, metadataOrder, result)
			}); err != nil {
				warnings = appendProviderWarning(warnings, "TVMaze refresh failed: ", err)
			}
		case "wikipedia":
			if request.Kind != "movie" && request.Kind != "series" {
				continue
			}
			if err := s.runProvider(source, false, cfg, func() error {
				return s.refreshWikipedia(ctx, request, metadataOrder, result)
			}); err != nil {
				warnings = appendProviderWarning(warnings, "Wikipedia refresh failed: ", err)
			}
		case "wikidata":
			if request.Kind != "movie" && request.Kind != "series" {
				continue
			}
			if err := s.runProvider(source, false, cfg, func() error {
				return s.refreshWikidata(ctx, request, metadataOrder, result)
			}); err != nil {
				warnings = appendProviderWarning(warnings, "Wikidata refresh failed: ", err)
			}
		case "tvdb":
			if request.Kind != "movie" && request.Kind != "series" {
				continue
			}
			if err := s.runProvider(source, true, cfg, func() error {
				return s.refreshTVDB(ctx, request, metadataOrder, cfg, result)
			}); err != nil {
				warnings = appendProviderWarning(warnings, "TheTVDB refresh failed: ", err)
			}
		case "tmdb":
			if request.Kind != "movie" && request.Kind != "series" {
				continue
			}
			if err := s.runProvider(source, true, cfg, func() error {
				return s.refreshTMDB(ctx, request, metadataOrder, cfg, result)
			}); err != nil {
				warnings = appendProviderWarning(warnings, "TMDB refresh failed: ", err)
			}
		case "fanart":
			if request.Kind != "movie" && request.Kind != "series" {
				continue
			}
			if err := s.runProvider(source, true, cfg, func() error {
				return s.refreshFanart(ctx, request, artworkOrder, cfg, result)
			}); err != nil {
				warnings = appendProviderWarning(warnings, "Fanart.tv refresh failed: ", err)
			}
		case "omdb":
			if request.Kind != "movie" && request.Kind != "series" {
				continue
			}
			if err := s.runProvider(source, true, cfg, func() error {
				return s.refreshOMDb(ctx, request, metadataOrder, cfg, result)
			}); err != nil {
				warnings = appendProviderWarning(warnings, "OMDb refresh failed: ", err)
			}
		}
	}
	if len(warnings) == 0 {
		return nil
	}
	return errors.New(strings.Join(warnings, "; "))
}

func combinedSourceOrder(metadataOrder []string, artworkOrder []string) []string {
	output := []string{}
	seen := map[string]struct{}{}
	for _, group := range [][]string{metadataOrder, artworkOrder} {
		for _, source := range group {
			normalized := normalizeProviderID(source)
			if normalized == "" {
				continue
			}
			if _, exists := seen[normalized]; exists {
				continue
			}
			seen[normalized] = struct{}{}
			output = append(output, normalized)
		}
	}
	return output
}

func appendProviderWarning(warnings []string, prefix string, err error) []string {
	if err == nil || isProviderNoMatch(err) {
		return warnings
	}
	return append(warnings, prefix+err.Error())
}

func isProviderNoMatch(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(text, "no show match"),
		strings.Contains(text, "no article match"),
		strings.Contains(text, "no wikidata item match"),
		strings.Contains(text, "no tvdb match"),
		strings.Contains(text, "no tmdb match"),
		strings.Contains(text, "no omdb match"):
		return true
	default:
		return false
	}
}

func (s *Service) refreshTVMaze(ctx context.Context, request RefreshRequest, order []string, result *RefreshResult) error {
	endpoint := s.tvMazeBaseURL + "/search/shows?" + url.Values{"q": {request.Title}}.Encode()
	var payload []tvMazeSearchResult
	if err := s.getJSON(ctx, endpoint, &payload); err != nil {
		return err
	}
	if len(payload) == 0 {
		return errors.New("no show match")
	}
	match := bestTVMazeMatch(request, payload)
	if match.Show.ID == 0 {
		return errors.New("no show match")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := catalog.MetadataRecord{
		Kind:        request.Kind,
		ItemID:      request.ID,
		Provider:    "tvmaze",
		ExternalID:  strconv.Itoa(match.Show.ID),
		Title:       firstNonEmpty(match.Show.Name, request.Title),
		Year:        parseYear(match.Show.Premiered, request.Year),
		Overview:    stripHTML(match.Show.Summary),
		PosterURL:   firstNonEmpty(match.Show.Image.Original, match.Show.Image.Medium),
		BackdropURL: "",
		Confidence:  sourceConfidence(order, "tvmaze", matchScore(request.Title, match.Show.Name, request.Year, parseYear(match.Show.Premiered, 0))),
		RawJSON:     mustJSON(match.Show),
		FetchedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.catalog.UpsertMetadataRecord(ctx, record); err != nil {
		return err
	}
	result.Records = append(result.Records, record)

	ratings := []catalog.Rating{}
	if match.Show.Rating.Average > 0 {
		ratings = append(ratings, catalog.Rating{
			Kind:         request.Kind,
			ItemID:       request.ID,
			Provider:     "tvmaze",
			RatingType:   "tvmaze",
			Value:        match.Show.Rating.Average,
			DisplayValue: fmt.Sprintf("%.1f/10", match.Show.Rating.Average),
			Scale:        10,
			SourceURL:    match.Show.URL,
			FetchedAt:    now,
			UpdatedAt:    now,
		})
	}
	if len(ratings) > 0 {
		if err := s.catalog.UpsertRatings(ctx, ratings); err != nil {
			return err
		}
		result.Ratings = append(result.Ratings, ratings...)
	}
	externalIDs := []catalog.ExternalID{
		{Kind: request.Kind, ItemID: request.ID, Provider: "tvmaze", ExternalID: strconv.Itoa(match.Show.ID), UpdatedAt: now},
		{Kind: request.Kind, ItemID: request.ID, Provider: "imdb", ExternalID: strings.TrimSpace(match.Show.Externals.IMDb), UpdatedAt: now},
		{Kind: request.Kind, ItemID: request.ID, Provider: "tvdb", ExternalID: intString(match.Show.Externals.TVDB), UpdatedAt: now},
	}
	for _, item := range externalIDs {
		if strings.TrimSpace(item.ExternalID) == "" {
			continue
		}
		_ = s.catalog.UpsertExternalID(ctx, item)
		result.ExternalIDs = append(result.ExternalIDs, item)
	}
	return nil
}

func (s *Service) refreshWikipedia(ctx context.Context, request RefreshRequest, order []string, result *RefreshResult) error {
	title := ""
	for _, term := range wikipediaSearchTerms(request) {
		searchValues := url.Values{
			"action":    {"opensearch"},
			"search":    {term},
			"limit":     {"5"},
			"namespace": {"0"},
			"format":    {"json"},
		}
		var search []any
		if err := s.getJSON(ctx, s.wikipediaSearchURL+"?"+searchValues.Encode(), &search); err != nil {
			return err
		}
		if len(search) < 2 {
			continue
		}
		titlesRaw, ok := search[1].([]any)
		if !ok || len(titlesRaw) == 0 {
			continue
		}
		if candidate := bestWikipediaTitle(request, titlesRaw); candidate != "" {
			title = candidate
			break
		}
	}
	if title == "" {
		return errors.New("no article match")
	}
	var summary wikipediaSummary
	if err := s.getJSON(ctx, s.wikipediaSummaryURL+"/"+url.PathEscape(title), &summary); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := catalog.MetadataRecord{
		Kind:        request.Kind,
		ItemID:      request.ID,
		Provider:    "wikipedia",
		ExternalID:  firstNonEmpty(summary.Titles.Canonical, title),
		Title:       wikipediaDisplayTitle(request, firstNonEmpty(summary.Titles.Normalized, title, request.Title)),
		Year:        request.Year,
		Overview:    strings.TrimSpace(summary.Extract),
		PosterURL:   firstNonEmpty(summary.OriginalImage.Source, summary.Thumbnail.Source),
		BackdropURL: "",
		Confidence:  sourceConfidence(order, "wikipedia", matchScore(request.Title, firstNonEmpty(summary.Titles.Normalized, title), request.Year, request.Year)),
		RawJSON:     mustJSON(summary),
		FetchedAt:   now,
		UpdatedAt:   now,
	}
	if record.Overview == "" && record.PosterURL == "" {
		return errors.New("article summary missing metadata")
	}
	if err := s.catalog.UpsertMetadataRecord(ctx, record); err != nil {
		return err
	}
	result.Records = append(result.Records, record)
	return nil
}

func (s *Service) refreshWikidata(ctx context.Context, request RefreshRequest, order []string, result *RefreshResult) error {
	match := wikidataSearchItem{}
	for _, term := range wikidataSearchTerms(request) {
		searchValues := url.Values{
			"action":   {"wbsearchentities"},
			"search":   {term},
			"language": {"en"},
			"format":   {"json"},
			"limit":    {"5"},
			"type":     {"item"},
		}
		var search wikidataSearchResponse
		if err := s.getJSON(ctx, s.wikidataSearchURL+"?"+searchValues.Encode(), &search); err != nil {
			return err
		}
		candidate := bestWikidataMatch(request, search.Search)
		if strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		match = candidate
		break
	}
	if strings.TrimSpace(match.ID) == "" {
		return errors.New("no wikidata item match")
	}
	var entityData wikidataEntityData
	if err := s.getJSON(ctx, s.wikidataEntityURL+"/"+url.PathEscape(match.ID)+".json", &entityData); err != nil {
		return err
	}
	entity, ok := entityData.Entities[match.ID]
	if !ok {
		return errors.New("wikidata entity payload missing item")
	}
	title := firstNonEmpty(entity.Labels["en"].Value, match.Label, request.Title)
	overview := firstNonEmpty(entity.Descriptions["en"].Value, match.Description)
	poster := wikimediaCommonsFileURL(firstClaimString(entity.Claims["P18"]))
	imdbID := firstClaimString(entity.Claims["P345"])
	if strings.TrimSpace(title) == "" || (strings.TrimSpace(overview) == "" && strings.TrimSpace(poster) == "" && strings.TrimSpace(imdbID) == "") {
		return errors.New("wikidata item did not contain useful metadata")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := catalog.MetadataRecord{
		Kind:       request.Kind,
		ItemID:     request.ID,
		Provider:   "wikidata",
		ExternalID: match.ID,
		Title:      title,
		Year:       request.Year,
		Overview:   overview,
		PosterURL:  poster,
		Confidence: sourceConfidence(order, "wikidata", wikidataConfidence(request, title)),
		RawJSON:    mustJSON(entity),
		FetchedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.catalog.UpsertMetadataRecord(ctx, record); err != nil {
		return err
	}
	result.Records = append(result.Records, record)
	if imdbID != "" {
		item := catalog.ExternalID{Kind: request.Kind, ItemID: request.ID, Provider: "imdb", ExternalID: imdbID, UpdatedAt: now}
		_ = s.catalog.UpsertExternalID(ctx, item)
		result.ExternalIDs = append(result.ExternalIDs, item)
	}
	item := catalog.ExternalID{Kind: request.Kind, ItemID: request.ID, Provider: "wikidata", ExternalID: match.ID, UpdatedAt: now}
	_ = s.catalog.UpsertExternalID(ctx, item)
	result.ExternalIDs = append(result.ExternalIDs, item)
	return nil
}

func (s *Service) refreshTVDB(ctx context.Context, request RefreshRequest, order []string, cfg config.Config, result *RefreshResult) error {
	// TVDB support is disabled: subscription licence is incompatible with
	// embedded-key UX. Function preserved (call sites still compile) but
	// short-circuits to a no-op. Re-enabling would require restoring the
	// TVDBAPIKey field to config and supplying a corporate licence key.
	return errors.New("tvdb provider disabled")
	// unreachable: code kept for reference if/when we re-enable.
	var login tvdbLoginResponse
	if err := s.postJSON(ctx, s.tvdbBaseURL+"/login", map[string]string{"apikey": ""}, nil, &login); err != nil {
		return err
	}
	if strings.TrimSpace(login.Data.Token) == "" {
		return errors.New("tvdb login did not return a token")
	}
	kind := "movie"
	if request.Kind == "series" {
		kind = "series"
	}
	headers := map[string]string{"Authorization": "Bearer " + login.Data.Token}
	var search tvdbSearchResponse
	if err := s.getJSONHeaders(ctx, s.tvdbBaseURL+"/search?"+url.Values{"query": {request.Title}, "type": {kind}}.Encode(), headers, &search); err != nil {
		return err
	}
	match := bestTVDBMatch(request, search.Data)
	if match.TVDBID == "" {
		return errors.New("no tvdb match")
	}
	detailPath := "/movies/" + url.PathEscape(match.TVDBID) + "/extended"
	if request.Kind == "series" {
		detailPath = "/series/" + url.PathEscape(match.TVDBID) + "/extended"
	}
	var detail tvdbEntityResponse
	if err := s.getJSONHeaders(ctx, s.tvdbBaseURL+detailPath, headers, &detail); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := catalog.MetadataRecord{
		Kind:        request.Kind,
		ItemID:      request.ID,
		Provider:    "tvdb",
		ExternalID:  match.TVDBID,
		Title:       firstNonEmpty(detail.Data.Name, detail.Data.Translations.Name, match.Name, request.Title),
		Year:        parseYear(firstNonEmpty(detail.Data.FirstRelease.Date, detail.Data.FirstAired), request.Year),
		Overview:    firstNonEmpty(detail.Data.Overview, detail.Data.Translations.Overview),
		PosterURL:   detail.Data.Image,
		BackdropURL: firstNonEmpty(detail.Data.Artworks.Background, detail.Data.Artworks.Banner),
		Confidence:  sourceConfidence(order, "tvdb", matchScore(request.Title, firstNonEmpty(detail.Data.Name, match.Name), request.Year, parseYear(firstNonEmpty(detail.Data.FirstRelease.Date, detail.Data.FirstAired), request.Year))),
		RawJSON:     mustJSON(detail.Data),
		FetchedAt:   now,
		UpdatedAt:   now,
	}
	if strings.TrimSpace(record.Title) == "" {
		return errors.New("tvdb match missing title")
	}
	if err := s.catalog.UpsertMetadataRecord(ctx, record); err != nil {
		return err
	}
	result.Records = append(result.Records, record)
	externalIDs := []catalog.ExternalID{
		{Kind: request.Kind, ItemID: request.ID, Provider: "tvdb", ExternalID: match.TVDBID, UpdatedAt: now},
		{Kind: request.Kind, ItemID: request.ID, Provider: "imdb", ExternalID: detail.Data.RemoteIDs.IMDb, UpdatedAt: now},
		{Kind: request.Kind, ItemID: request.ID, Provider: "tmdb", ExternalID: detail.Data.RemoteIDs.TMDB, UpdatedAt: now},
	}
	for _, item := range externalIDs {
		if strings.TrimSpace(item.ExternalID) == "" {
			continue
		}
		_ = s.catalog.UpsertExternalID(ctx, item)
		result.ExternalIDs = append(result.ExternalIDs, item)
	}
	if detail.Data.Score > 0 {
		rating := catalog.Rating{
			Kind:         request.Kind,
			ItemID:       request.ID,
			Provider:     "tvdb",
			RatingType:   "tvdb",
			Value:        detail.Data.Score,
			DisplayValue: fmt.Sprintf("%.1f/10", detail.Data.Score),
			Scale:        10,
			SourceURL:    detail.Data.URL,
			FetchedAt:    now,
			UpdatedAt:    now,
		}
		if err := s.catalog.UpsertRatings(ctx, []catalog.Rating{rating}); err != nil {
			return err
		}
		result.Ratings = append(result.Ratings, rating)
	}
	return nil
}

type tvMazeSearchResult struct {
	Show tvMazeShow `json:"show"`
}

type tvMazeShow struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Premiered string `json:"premiered"`
	Summary   string `json:"summary"`
	URL       string `json:"url"`
	Rating    struct {
		Average float64 `json:"average"`
	} `json:"rating"`
	Image struct {
		Medium   string `json:"medium"`
		Original string `json:"original"`
	} `json:"image"`
	Externals struct {
		IMDb string `json:"imdb"`
		TVDB int    `json:"thetvdb"`
	} `json:"externals"`
}

type wikipediaSummary struct {
	Titles struct {
		Canonical  string `json:"canonical"`
		Normalized string `json:"normalized"`
	} `json:"titles"`
	Extract   string `json:"extract"`
	Thumbnail struct {
		Source string `json:"source"`
	} `json:"thumbnail"`
	OriginalImage struct {
		Source string `json:"source"`
	} `json:"originalimage"`
}

type wikidataSearchResponse struct {
	Search []wikidataSearchItem `json:"search"`
}

type wikidataSearchItem struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type wikidataEntityData struct {
	Entities map[string]wikidataEntity `json:"entities"`
}

type wikidataEntity struct {
	Labels       map[string]wikidataText          `json:"labels"`
	Descriptions map[string]wikidataText          `json:"descriptions"`
	Claims       map[string][]wikidataEntityClaim `json:"claims"`
}

type wikidataText struct {
	Value string `json:"value"`
}

type wikidataEntityClaim struct {
	MainSnak struct {
		DataValue struct {
			Value any `json:"value"`
		} `json:"datavalue"`
	} `json:"mainsnak"`
}

type tvdbLoginResponse struct {
	Data struct {
		Token string `json:"token"`
	} `json:"data"`
}

type tvdbSearchResponse struct {
	Data []tvdbSearchItem `json:"data"`
}

type tvdbSearchItem struct {
	TVDBID string `json:"tvdb_id"`
	Name   string `json:"name"`
	Year   string `json:"year"`
}

type tvdbEntityResponse struct {
	Data tvdbEntity `json:"data"`
}

type tvdbEntity struct {
	Name         string  `json:"name"`
	Overview     string  `json:"overview"`
	Image        string  `json:"image"`
	FirstAired   string  `json:"firstAired"`
	URL          string  `json:"url"`
	Score        float64 `json:"score"`
	Translations struct {
		Name     string `json:"name"`
		Overview string `json:"overview"`
	} `json:"translations"`
	FirstRelease struct {
		Date string `json:"date"`
	} `json:"first_release"`
	Artworks struct {
		Background string `json:"background"`
		Banner     string `json:"banner"`
	} `json:"artworks"`
	RemoteIDs struct {
		IMDb string `json:"imdb"`
		TMDB string `json:"tmdb"`
	} `json:"remote_ids"`
}

func bestTVMazeMatch(request RefreshRequest, results []tvMazeSearchResult) tvMazeSearchResult {
	best := tvMazeSearchResult{}
	bestValue := -1.0
	for _, item := range results {
		score := matchScore(request.Title, item.Show.Name, request.Year, parseYear(item.Show.Premiered, 0))
		if score > bestValue {
			bestValue = score
			best = item
		}
	}
	return best
}

const minimumMetadataMatchScore = 0.74

func wikipediaSearchTerms(request RefreshRequest) []string {
	title := strings.TrimSpace(request.Title)
	if title == "" {
		return nil
	}
	terms := []string{}
	if request.Kind == "series" {
		terms = append(terms, title+" TV series", title+" television series", title)
	} else if request.Year > 0 {
		terms = append(terms, fmt.Sprintf("%s %d film", title, request.Year), title+" film", title)
	} else {
		terms = append(terms, title+" film", title)
	}
	return uniqueSearchTerms(terms)
}

func bestWikipediaTitle(request RefreshRequest, titles []any) string {
	best := ""
	bestValue := -1.0
	for _, raw := range titles {
		title, _ := raw.(string)
		if strings.TrimSpace(title) == "" {
			continue
		}
		score := matchScore(request.Title, title, request.Year, request.Year)
		cleaned := stripWikipediaQualifier(title)
		if cleaned != title {
			if cleanedScore := matchScore(request.Title, cleaned, request.Year, request.Year); cleanedScore > score {
				score = cleanedScore
			}
		}
		if score > bestValue {
			bestValue = score
			best = title
		}
	}
	if bestValue < minimumMetadataMatchScore {
		return ""
	}
	return best
}

func bestWikidataMatch(request RefreshRequest, items []wikidataSearchItem) wikidataSearchItem {
	best := wikidataSearchItem{}
	bestValue := -1.0
	for _, item := range items {
		score := matchScore(request.Title, item.Label, request.Year, request.Year)
		description := strings.ToLower(strings.TrimSpace(item.Description))
		if request.Kind == "movie" && strings.Contains(description, "film") {
			score += 0.04
		}
		if request.Kind == "series" && (strings.Contains(description, "television") || strings.Contains(description, "tv series")) {
			score += 0.04
		}
		if score > 0.99 {
			score = 0.99
		}
		if score > bestValue {
			bestValue = score
			best = item
		}
	}
	if bestValue < minimumMetadataMatchScore {
		return wikidataSearchItem{}
	}
	return best
}

func bestTVDBMatch(request RefreshRequest, items []tvdbSearchItem) tvdbSearchItem {
	best := tvdbSearchItem{}
	bestValue := -1.0
	for _, item := range items {
		score := matchScore(request.Title, item.Name, request.Year, parseYear(item.Year, request.Year))
		if score > bestValue {
			bestValue = score
			best = item
		}
	}
	return best
}

func wikidataSearchTerms(request RefreshRequest) []string {
	title := strings.TrimSpace(request.Title)
	if title == "" {
		return nil
	}
	terms := []string{}
	if request.Kind == "series" {
		terms = append(terms, title+" television series", title+" TV series", title)
	} else if request.Year > 0 {
		terms = append(terms, fmt.Sprintf("%s %d film", title, request.Year), title+" film", title)
	} else {
		terms = append(terms, title+" film", title)
	}
	return uniqueSearchTerms(terms)
}

func uniqueSearchTerms(values []string) []string {
	output := []string{}
	seen := map[string]struct{}{}
	for _, value := range values {
		candidate := strings.TrimSpace(value)
		if candidate == "" {
			continue
		}
		key := strings.ToLower(candidate)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		output = append(output, candidate)
	}
	return output
}

func firstClaimString(claims []wikidataEntityClaim) string {
	for _, claim := range claims {
		switch value := claim.MainSnak.DataValue.Value.(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func wikimediaCommonsFileURL(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return "https://commons.wikimedia.org/wiki/Special:FilePath/" + url.PathEscape(name)
}

func wikidataConfidence(request RefreshRequest, title string) float64 {
	score := matchScore(request.Title, title, request.Year, request.Year) - 0.18
	if score < 0.55 {
		return 0.55
	}
	return score
}

func wikipediaDisplayTitle(request RefreshRequest, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return strings.TrimSpace(request.Title)
	}
	cleaned := stripWikipediaQualifier(value)
	if cleaned != "" && matchScore(request.Title, cleaned, request.Year, request.Year) >= 0.82 {
		return cleaned
	}
	return value
}

func stripWikipediaQualifier(value string) string {
	value = strings.TrimSpace(value)
	if !strings.HasSuffix(value, ")") {
		return value
	}
	index := strings.LastIndex(value, " (")
	if index <= 0 {
		return value
	}
	return strings.TrimSpace(value[:index])
}

// extractTMDBIDFromFilename parses the Plex-style {tmdb-12345} annotation
// from a filename or path. Returns 0 when no annotation is found.
func extractTMDBIDFromFilename(filename string) int {
	base := strings.ToLower(filename)
	// Normalise path separators
	base = strings.ReplaceAll(base, "\\", "/")
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
	// Look for {tmdb-NNN} or {tmdb=NNN}
	const prefix1 = "{tmdb-"
	const prefix2 = "{tmdb="
	for _, prefix := range []string{prefix1, prefix2} {
		start := strings.Index(base, prefix)
		if start < 0 {
			continue
		}
		rest := base[start+len(prefix):]
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			continue
		}
		raw := strings.TrimSpace(rest[:end])
		if id, err := strconv.Atoi(raw); err == nil && id > 0 {
			return id
		}
	}
	return 0
}

func matchScore(left string, right string, leftYear int, rightYear int) float64 {
	leftNorm := normalizeTitle(left)
	rightNorm := normalizeTitle(right)
	if leftNorm == "" || rightNorm == "" {
		return 0.1
	}
	score := 0.2
	if leftNorm == rightNorm {
		score = 0.96
	} else if strings.Contains(rightNorm, leftNorm) || strings.Contains(leftNorm, rightNorm) {
		score = 0.82
	} else {
		score = 0.55
	}
	if leftYear > 0 && rightYear > 0 {
		diff := leftYear - rightYear
		if diff < 0 {
			diff = -diff
		}
		switch diff {
		case 0:
			score += 0.04
		case 1:
			score -= 0.03
		default:
			score -= 0.12
		}
	}
	if score < 0.1 {
		return 0.1
	}
	if score > 0.99 {
		return 0.99
	}
	return score
}

func normalizeTitle(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("(", " ", ")", " ", ":", " ", "-", " ", "'", "", "\"", "", ",", " ", ".", " ")
	value = replacer.Replace(value)
	return strings.Join(strings.Fields(value), " ")
}

func stripHTML(value string) string {
	value = strings.NewReplacer("<p>", "", "</p>", "", "<b>", "", "</b>", "", "<i>", "", "</i>", "", "<br>", " ", "<br/>", " ", "<br />", " ").Replace(value)
	value = strings.ReplaceAll(value, "&amp;", "&")
	value = strings.ReplaceAll(value, "&quot;", "\"")
	return strings.TrimSpace(value)
}

func intString(value int) string {
	if value <= 0 {
		return ""
	}
	return strconv.Itoa(value)
}
