package metasources

import (
	"strings"

	"github.com/EJM-Digital/Xuva/server/internal/config"
)

type SourceDefinition struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Coverage       string   `json:"coverage,omitempty"`
	Note           string   `json:"note,omitempty"`
	Kinds          []string `json:"kinds"`
	Local          bool     `json:"local"`
	Managed        bool     `json:"managed"`
	RequiresConfig bool     `json:"requiresConfig"`
	Available      bool     `json:"available"`
	SupportsMetadata bool   `json:"supportsMetadata"`
	SupportsArtwork  bool   `json:"supportsArtwork"`
}

func SourceCatalog(cfg config.Config) []SourceDefinition {
	return []SourceDefinition{
		{ID: "filename", Name: "Filename and folders", Description: "Fast local title and year parsing from library paths.", Coverage: "Movies and TV", Note: "Always available", Kinds: []string{"movie", "series"}, Local: true, Available: true, SupportsMetadata: true},
		{ID: "nfo", Name: "Local NFO", Description: "Reads movie.nfo, tvshow.nfo, and sidecar NFO files in the library.", Coverage: "Movies and TV with sidecars", Note: "Always available", Kinds: []string{"movie", "series"}, Local: true, Available: true, SupportsMetadata: true, SupportsArtwork: true},
		{ID: "artwork", Name: "Local artwork", Description: "Uses poster, folder, cover, fanart, backdrop, season, and episode sidecars in the library.", Coverage: "Movies and TV with sidecar images", Note: "Always available", Kinds: []string{"movie", "series"}, Local: true, Available: true, SupportsArtwork: true},
		{ID: "wikipedia", Name: "Wikipedia", Description: "Adds richer summaries and artwork when a matching article is available.", Coverage: "Movies and TV", Note: "No user account required", Kinds: []string{"movie", "series"}, Available: true, SupportsMetadata: true, SupportsArtwork: true},
		{ID: "wikidata", Name: "Wikidata", Description: "Adds structured labels, descriptions, poster files, and external IDs from Wikimedia data.", Coverage: "Movies and TV", Note: "No user account required", Kinds: []string{"movie", "series"}, Available: true, SupportsMetadata: true, SupportsArtwork: true},
		{ID: "tvmaze", Name: "TVMaze", Description: "Adds series metadata, external IDs, and TV ratings without a user account.", Coverage: "TV libraries", Note: "No user account required", Kinds: []string{"series"}, Available: true, SupportsMetadata: true},
		// TMDB and Fanart.tv use embedded project keys by default (config/keys.go),
		// so RequiresConfig=false in the user-facing sense. They're only
		// "unavailable" if the embedded key was stripped AND the user hasn't
		// supplied a personal key — at which point Available reflects that.
		{ID: "tmdb", Name: "TMDB", Description: "Adds movie, show, season, and episode metadata plus artwork through the metadata layer.", Coverage: "Movies and TV", Note: "Built-in", Kinds: []string{"movie", "series"}, Managed: true, RequiresConfig: false, Available: strings.TrimSpace(cfg.TMDBAPIKey) != "", SupportsMetadata: true, SupportsArtwork: true},
		{ID: "fanart", Name: "Fanart.tv", Description: "Adds logos, clearlogo, banners, thumbs, and extra backdrop artwork.", Coverage: "Movies and TV artwork", Note: "Built-in", Kinds: []string{"movie", "series"}, Managed: true, RequiresConfig: false, Available: strings.TrimSpace(cfg.FanartTVAPIKey) != "", SupportsArtwork: true},
		{ID: "omdb", Name: "OMDb", Description: "Adds IMDb, Rotten Tomatoes, and Metacritic ratings through the metadata layer.", Coverage: "Movies and TV", Note: "Optional", Kinds: []string{"movie", "series"}, Managed: true, RequiresConfig: true, Available: strings.TrimSpace(cfg.OMDbAPIKey) != "", SupportsMetadata: true},
		// TheTVDB removed: per-installation subscription model is incompatible
		// with embed-and-ship UX. TMDB covers full TV data including episode
		// stills, season posters, aggregate credits, and content ratings.
	}
}

func DefaultSourceOrder(kind string) []string {
	switch NormalizeKind(kind) {
	case "series":
		return []string{"nfo", "tmdb", "tvmaze", "wikipedia", "wikidata", "omdb", "filename"}
	default:
		return []string{"nfo", "tmdb", "wikipedia", "wikidata", "omdb", "filename"}
	}
}

func DefaultArtworkOrder(kind string) []string {
	switch NormalizeKind(kind) {
	case "series":
		return []string{"artwork", "nfo", "fanart", "tmdb", "wikipedia", "wikidata"}
	default:
		return []string{"artwork", "nfo", "fanart", "tmdb", "wikipedia", "wikidata"}
	}
}

func NormalizeSourceOrder(kind string, requested []string, cfg config.Config) []string {
	return NormalizeRequestedSourceOrder(kind, requested)
}

func NormalizeRequestedSourceOrder(kind string, requested []string) []string {
	return normalizeRequestedOrder(kind, requested, func(source SourceDefinition, normalizedKind string) bool {
		return SupportsKind(source, normalizedKind) && source.SupportsMetadata
	}, DefaultSourceOrder)
}

func NormalizeRequestedArtworkOrder(kind string, requested []string) []string {
	return normalizeRequestedOrder(kind, requested, func(source SourceDefinition, normalizedKind string) bool {
		return SupportsKind(source, normalizedKind) && source.SupportsArtwork
	}, DefaultArtworkOrder)
}

func normalizeRequestedOrder(kind string, requested []string, include func(SourceDefinition, string) bool, defaults func(string) []string) []string {
	normalizedKind := NormalizeKind(kind)
	known := map[string]SourceDefinition{}
	for _, source := range SourceCatalog(config.Config{}) {
		if include(source, normalizedKind) {
			known[source.ID] = source
		}
	}
	output := []string{}
	seen := map[string]struct{}{}
	for _, id := range requested {
		id = strings.ToLower(strings.TrimSpace(id))
		_, ok := known[id]
		if !ok {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		output = append(output, id)
	}
	if len(output) > 0 {
		return output
	}
	for _, id := range defaults(normalizedKind) {
		_, ok := known[id]
		if !ok {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		output = append(output, id)
	}
	return output
}

func SourceCatalogByKind(cfg config.Config) map[string][]SourceDefinition {
	output := map[string][]SourceDefinition{
		"movie":  {},
		"series": {},
	}
	for _, source := range SourceCatalog(cfg) {
		for _, kind := range source.Kinds {
			output[kind] = append(output[kind], source)
		}
	}
	return output
}

func SupportsKind(source SourceDefinition, kind string) bool {
	for _, candidate := range source.Kinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

func NormalizeKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "tv", "series":
		return "series"
	default:
		return "movie"
	}
}
