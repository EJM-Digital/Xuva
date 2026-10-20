package metadata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/catalog"
	"github.com/EJM-Digital/Xuva/server/internal/config"
)

func (s *Service) refreshFanart(ctx context.Context, request RefreshRequest, order []string, cfg config.Config, result *RefreshResult) error {
	apiKey := managedProviderCredential("fanart", cfg)
	if strings.TrimSpace(apiKey) == "" {
		return errors.New("fanart.tv API key is not configured")
	}
	externalIDs, err := s.catalog.ListExternalIDs(ctx, request.Kind, request.ID)
	if err != nil {
		return err
	}
	idMap := map[string]string{}
	for _, item := range externalIDs {
		if trimmed := strings.TrimSpace(item.ExternalID); trimmed != "" {
			idMap[strings.ToLower(strings.TrimSpace(item.Provider))] = trimmed
		}
	}
	var endpoint string
	if request.Kind == "series" {
		tvdbID := firstNonEmpty(idMap["tvdb"])
		if tvdbID == "" {
			return errors.New("fanart.tv requires a TVDB series id")
		}
		endpoint = fmt.Sprintf("https://webservice.fanart.tv/v3/tv/%s?api_key=%s", tvdbID, apiKey)
	} else {
		tmdbID := firstNonEmpty(idMap["tmdb"])
		if tmdbID == "" {
			return errors.New("fanart.tv requires a TMDB movie id")
		}
		endpoint = fmt.Sprintf("https://webservice.fanart.tv/v3/movies/%s?api_key=%s", tmdbID, apiKey)
	}
	var payload fanartPayload
	if err := s.getJSON(ctx, endpoint, &payload); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := catalog.MetadataRecord{
		Kind:         request.Kind,
		ItemID:       request.ID,
		Provider:     "fanart",
		Title:        request.Title,
		PosterURL:    firstNonEmpty(fanartImageURL(payload.MoviePoster), fanartImageURL(payload.TVPoster)),
		BackdropURL:  firstNonEmpty(fanartImageURL(payload.MovieBackground), fanartImageURL(payload.ShowBackground), fanartImageURL(payload.TVThumb)),
		ThumbnailURL: firstNonEmpty(fanartImageURL(payload.MovieThumb), fanartImageURL(payload.TVThumb)),
		LogoURL:      firstNonEmpty(fanartImageURL(payload.HDMovieLogo), fanartImageURL(payload.ClearLogo), fanartImageURL(payload.HDTVLogo), fanartImageURL(payload.ClearArt)),
		BannerURL:    firstNonEmpty(fanartImageURL(payload.MovieBanner), fanartImageURL(payload.TVBanner)),
		Confidence:   sourceConfidence(order, "fanart", 0.92),
		RawJSON:      mustJSON(payload),
		FetchedAt:    now,
		UpdatedAt:    now,
	}
	if record.PosterURL == "" && record.BackdropURL == "" && record.LogoURL == "" && record.BannerURL == "" && record.ThumbnailURL == "" {
		return errors.New("no fanart.tv artwork found")
	}
	if err := s.catalog.UpsertMetadataRecord(ctx, record); err != nil {
		return err
	}
	result.Records = append(result.Records, record)
	return nil
}

func fanartImageURL(items []fanartImage) string {
	for _, item := range items {
		if strings.EqualFold(item.Lang, "en") && strings.TrimSpace(item.URL) != "" {
			return strings.TrimSpace(item.URL)
		}
	}
	for _, item := range items {
		if trimmed := strings.TrimSpace(item.URL); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

type fanartPayload struct {
	MoviePoster     []fanartImage `json:"movieposter"`
	MovieBackground []fanartImage `json:"moviebackground"`
	MovieThumb      []fanartImage `json:"moviethumb"`
	MovieBanner     []fanartImage `json:"moviebanner"`
	HDMovieLogo     []fanartImage `json:"hdmovielogo"`
	ClearLogo       []fanartImage `json:"clearlogo"`
	ClearArt        []fanartImage `json:"clearart"`
	TVPoster        []fanartImage `json:"tvposter"`
	ShowBackground  []fanartImage `json:"showbackground"`
	TVThumb         []fanartImage `json:"tvthumb"`
	TVBanner        []fanartImage `json:"tvbanner"`
	HDTVLogo        []fanartImage `json:"hdtvlogo"`
}

type fanartImage struct {
	URL  string `json:"url"`
	Lang string `json:"lang"`
}
