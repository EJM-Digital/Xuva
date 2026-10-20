package metadata

import (
	"context"
	"strings"

	"github.com/EJM-Digital/Xuva/server/internal/config"
	"github.com/EJM-Digital/Xuva/server/internal/metasources"
)

func (s *Service) sourceOrder(ctx context.Context, request RefreshRequest) []string {
	cfg := s.activeConfig()
	if s.catalog != nil {
		if library, ok, err := s.catalog.GetLibraryForItem(ctx, request.Kind, request.ID); err == nil && ok {
			return metasources.NormalizeSourceOrder(request.Kind, library.MetadataSources, cfg)
		}
	}
	return metasources.NormalizeSourceOrder(request.Kind, metadataSourcePreferenceForKind(cfg, request.Kind), cfg)
}

func (s *Service) artworkOrder(ctx context.Context, request RefreshRequest) []string {
	cfg := s.activeConfig()
	if s.catalog != nil {
		if library, ok, err := s.catalog.GetLibraryForItem(ctx, request.Kind, request.ID); err == nil && ok {
			return metasources.NormalizeRequestedArtworkOrder(request.Kind, library.ArtworkSources)
		}
	}
	return metasources.NormalizeRequestedArtworkOrder(request.Kind, artworkSourcePreferenceForKind(cfg, request.Kind))
}

func metadataSourcePreferenceForKind(cfg config.Config, kind string) []string {
	switch metasources.NormalizeKind(kind) {
	case "series":
		return append([]string(nil), cfg.SeriesMetadataSources...)
	default:
		return append([]string(nil), cfg.MovieMetadataSources...)
	}
}

func artworkSourcePreferenceForKind(cfg config.Config, kind string) []string {
	switch metasources.NormalizeKind(kind) {
	case "series":
		return append([]string(nil), cfg.SeriesArtworkSources...)
	default:
		return append([]string(nil), cfg.MovieArtworkSources...)
	}
}

func sourceEnabled(order []string, provider string) bool {
	for _, item := range order {
		if strings.EqualFold(item, provider) {
			return true
		}
	}
	return false
}

func sourceConfidence(order []string, provider string, fallback float64) float64 {
	index := -1
	for i, item := range order {
		if strings.EqualFold(item, provider) {
			index = i
			break
		}
	}
	if index < 0 || len(order) == 0 {
		return fallback
	}
	boost := float64(len(order)-index) / float64(len(order)) * 0.06
	value := fallback + boost
	if value > 0.99 {
		return 0.99
	}
	if value < 0.1 {
		return 0.1
	}
	return value
}
