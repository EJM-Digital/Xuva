package metadata

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/catalog"
)

type localMediaContext struct {
	rootPath   string
	mediaPath  string
	versionDir string
	title      string
	year       int
}

type nfoEnvelope struct {
	XMLName       xml.Name `xml:"-"`
	Title         string   `xml:"title"`
	OriginalTitle string   `xml:"originaltitle"`
	Year          string   `xml:"year"`
	Plot          string   `xml:"plot"`
	Thumbs        []string `xml:"thumb"`
	FanartThumbs  []string `xml:"fanart>thumb"`
	ID            string   `xml:"id"`
	IMDbID        string   `xml:"imdbid"`
	TMDBID        string   `xml:"tmdbid"`
	TVDBID        string   `xml:"tvdbid"`
}

func (s *Service) refreshLocal(ctx context.Context, request RefreshRequest, metadataOrder []string, artworkOrder []string, result *RefreshResult) error {
	mediaCtx, err := s.localMediaContext(ctx, request)
	if err != nil || mediaCtx == nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	if sourceEnabled(metadataOrder, "nfo") || sourceEnabled(artworkOrder, "nfo") {
		if nfoPath, nfo, ok := loadNFO(mediaCtx, request.Kind); ok {
			record := catalog.MetadataRecord{
				Kind:        request.Kind,
				ItemID:      request.ID,
				Provider:    "nfo",
				ExternalID:  firstNonEmpty(strings.TrimSpace(nfo.TMDBID), strings.TrimSpace(nfo.IMDbID), strings.TrimSpace(nfo.TVDBID), strings.TrimSpace(nfo.ID)),
				Title:       firstNonEmpty(strings.TrimSpace(nfo.Title), strings.TrimSpace(nfo.OriginalTitle), request.Title),
				Year:        parseLocalYear(nfo.Year, request.Year),
				Overview:    strings.TrimSpace(nfo.Plot),
				PosterURL:   localArtworkURL(firstNonEmpty(resolveRelativeMediaPath(mediaCtx.rootPath, firstNonEmpty(nfo.Thumbs...)), detectArtwork(mediaCtx.rootPath, "poster"))),
				BackdropURL: localArtworkURL(firstNonEmpty(resolveRelativeMediaPath(mediaCtx.rootPath, firstNonEmpty(nfo.FanartThumbs...)), detectArtwork(mediaCtx.rootPath, "backdrop"))),
				OriginalTitle: strings.TrimSpace(nfo.OriginalTitle),
				Confidence:  sourceConfidence(metadataOrder, "nfo", 0.98),
				RawJSON:     mustJSON(map[string]any{"path": nfoPath}),
				FetchedAt:   now,
				UpdatedAt:   now,
			}
			if strings.TrimSpace(record.Title) != "" {
				if err := s.catalog.UpsertMetadataRecord(ctx, record); err != nil {
					return err
				}
				result.Records = append(result.Records, record)
			}
			upsertLocalExternalIDs(ctx, s.catalog, request, nfo, now, result)
			return nil
		}
	}

	if !sourceEnabled(artworkOrder, "artwork") {
		return nil
	}
	poster := detectArtwork(mediaCtx.rootPath, "poster")
	backdrop := detectArtwork(mediaCtx.rootPath, "backdrop")
	if poster == "" && backdrop == "" {
		return nil
	}
	record := catalog.MetadataRecord{
		Kind:        request.Kind,
		ItemID:      request.ID,
		Provider:    "artwork",
		Title:       request.Title,
		Year:        request.Year,
		PosterURL:   localArtworkURL(poster),
		BackdropURL: localArtworkURL(backdrop),
		Confidence:  sourceConfidence(artworkOrder, "artwork", 0.82),
		RawJSON:     mustJSON(map[string]any{"rootPath": mediaCtx.rootPath}),
		FetchedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.catalog.UpsertMetadataRecord(ctx, record); err != nil {
		return err
	}
	result.Records = append(result.Records, record)
	return nil
}

func (s *Service) localMediaContext(ctx context.Context, request RefreshRequest) (*localMediaContext, error) {
	paths, err := s.itemPaths(ctx, request.Kind, request.ID)
	if err != nil || len(paths) == 0 {
		return nil, err
	}
	root := commonAncestor(paths)
	if root == "" {
		return nil, nil
	}
	if request.Kind == "series" {
		root = normalizeSeriesRoot(root)
	}
	return &localMediaContext{
		rootPath:   root,
		mediaPath:  paths[0],
		versionDir: filepath.Dir(paths[0]),
		title:      request.Title,
		year:       request.Year,
	}, nil
}

func (s *Service) itemPaths(ctx context.Context, kind string, id string) ([]string, error) {
	switch kind {
	case "movie":
		detail, ok, err := s.catalog.GetMovie(ctx, id)
		if err != nil || !ok {
			return nil, err
		}
		output := make([]string, 0, len(detail.Versions))
		for _, version := range detail.Versions {
			if strings.TrimSpace(version.Path) != "" {
				output = append(output, filepath.Clean(version.Path))
			}
		}
		return output, nil
	case "series":
		detail, ok, err := s.catalog.GetSeries(ctx, id)
		if err != nil || !ok {
			return nil, err
		}
		output := []string{}
		for _, season := range detail.Seasons {
			for _, episode := range season.Episodes {
				for _, version := range episode.Versions {
					if strings.TrimSpace(version.Path) != "" {
						output = append(output, filepath.Clean(version.Path))
					}
				}
			}
		}
		return output, nil
	case "episode":
		return nil, nil
	default:
		return nil, nil
	}
}

func loadNFO(mediaCtx *localMediaContext, kind string) (string, nfoEnvelope, bool) {
	if mediaCtx == nil {
		return "", nfoEnvelope{}, false
	}
	candidates := []string{}
	if mediaCtx.mediaPath != "" {
		base := strings.TrimSuffix(filepath.Base(mediaCtx.mediaPath), filepath.Ext(mediaCtx.mediaPath))
		candidates = append(candidates,
			filepath.Join(filepath.Dir(mediaCtx.mediaPath), base+".nfo"),
			filepath.Join(mediaCtx.versionDir, "movie.nfo"),
			filepath.Join(mediaCtx.versionDir, "tvshow.nfo"),
			filepath.Join(mediaCtx.versionDir, "series.nfo"),
		)
	}
	if mediaCtx.rootPath != "" {
		candidates = append(candidates,
			filepath.Join(mediaCtx.rootPath, "movie.nfo"),
			filepath.Join(mediaCtx.rootPath, "tvshow.nfo"),
			filepath.Join(mediaCtx.rootPath, "series.nfo"),
			filepath.Join(mediaCtx.rootPath, "index.nfo"),
		)
	}
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		raw, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		var parsed nfoEnvelope
		if err := xml.Unmarshal(raw, &parsed); err != nil {
			continue
		}
		rootKind := strings.ToLower(parsed.XMLName.Local)
		if kind == "movie" && rootKind != "" && rootKind != "movie" {
			continue
		}
		if kind == "series" && rootKind != "" && rootKind != "tvshow" {
			continue
		}
		return candidate, parsed, true
	}
	return "", nfoEnvelope{}, false
}

func upsertLocalExternalIDs(ctx context.Context, catalogService *catalog.Service, request RefreshRequest, parsed nfoEnvelope, now string, result *RefreshResult) {
	items := []catalog.ExternalID{
		{Kind: request.Kind, ItemID: request.ID, Provider: "imdb", ExternalID: strings.TrimSpace(parsed.IMDbID), UpdatedAt: now},
		{Kind: request.Kind, ItemID: request.ID, Provider: "tmdb", ExternalID: strings.TrimSpace(parsed.TMDBID), UpdatedAt: now},
		{Kind: request.Kind, ItemID: request.ID, Provider: "tvdb", ExternalID: strings.TrimSpace(parsed.TVDBID), UpdatedAt: now},
	}
	for _, item := range items {
		if strings.TrimSpace(item.ExternalID) == "" {
			continue
		}
		_ = catalogService.UpsertExternalID(ctx, item)
		result.ExternalIDs = append(result.ExternalIDs, item)
	}
}

func detectArtwork(root string, artType string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	names := []string{}
	switch artType {
	case "backdrop":
		names = []string{"fanart.jpg", "fanart.png", "fanart.webp", "backdrop.jpg", "backdrop.png", "backdrop.webp"}
	default:
		names = []string{"poster.jpg", "poster.png", "poster.webp", "folder.jpg", "folder.png", "folder.webp", "cover.jpg", "cover.png", "cover.webp"}
	}
	for _, name := range names {
		path := filepath.Join(root, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return filepath.Clean(path)
		}
	}
	return ""
}

func localArtworkURL(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func resolveRelativeMediaPath(root string, candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}
	if filepath.IsAbs(candidate) {
		return filepath.Clean(candidate)
	}
	if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
		return candidate
	}
	if root == "" {
		return filepath.Clean(candidate)
	}
	return filepath.Join(root, candidate)
}

func parseLocalYear(value string, fallback int) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	if len(value) >= 4 {
		value = value[:4]
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func normalizeSeriesRoot(path string) string {
	path = filepath.Clean(path)
	base := strings.ToLower(filepath.Base(path))
	if strings.HasPrefix(base, "season ") || strings.HasPrefix(base, "series ") || base == "specials" {
		return filepath.Dir(path)
	}
	return path
}

func commonAncestor(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	first := filepath.Dir(paths[0])
	isAbs := filepath.IsAbs(first)
	segments := splitPath(first)
	if len(segments) == 0 {
		return first
	}
	for _, path := range paths[1:] {
		parts := splitPath(filepath.Dir(path))
		limit := len(segments)
		if len(parts) < limit {
			limit = len(parts)
		}
		i := 0
		for i < limit && strings.EqualFold(segments[i], parts[i]) {
			i++
		}
		segments = segments[:i]
		if len(segments) == 0 {
			if vol := filepath.VolumeName(path); vol != "" {
				return vol + string(filepath.Separator)
			}
			if isAbs {
				return string(filepath.Separator)
			}
			return ""
		}
	}
	joined := joinPath(segments)
	if isAbs && !filepath.IsAbs(joined) {
		joined = string(filepath.Separator) + joined
	}
	return joined
}

func splitPath(path string) []string {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	remainder := strings.TrimPrefix(path, volume)
	remainder = strings.TrimPrefix(remainder, string(filepath.Separator))
	parts := []string{}
	if volume != "" {
		parts = append(parts, volume)
	}
	for _, part := range strings.Split(remainder, string(filepath.Separator)) {
		if strings.TrimSpace(part) != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func joinPath(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 && strings.HasSuffix(parts[0], ":") {
		return parts[0] + string(filepath.Separator)
	}
	if strings.HasSuffix(parts[0], ":") {
		return parts[0] + string(filepath.Separator) + filepath.Join(parts[1:]...)
	}
	return filepath.Join(parts...)
}
