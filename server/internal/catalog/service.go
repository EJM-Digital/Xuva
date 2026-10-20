package catalog

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/database"
	"github.com/EJM-Digital/Xuva/server/internal/libraries"
	"github.com/EJM-Digital/Xuva/server/internal/media"
	"github.com/EJM-Digital/Xuva/server/internal/metasources"
	"github.com/EJM-Digital/Xuva/server/internal/movies"
	"github.com/EJM-Digital/Xuva/server/internal/probe"
	"github.com/EJM-Digital/Xuva/server/internal/scanner"
	"github.com/EJM-Digital/Xuva/server/internal/tv"
)

type Service struct {
	db *sql.DB
}

type PersistSummary struct {
	LibraryID        string `json:"libraryId"`
	ScanRunID        int64  `json:"scanRunId"`
	MediaSources     int    `json:"mediaSources"`
	ChangedSources   int    `json:"changedSources"`
	UnchangedSources int    `json:"unchangedSources"`
	Movies           int    `json:"movies,omitempty"`
	Episodes         int    `json:"episodes,omitempty"`
}

type RuntimeSettings struct {
	HTTPAddr          string `json:"httpAddr"`
	DataDir           string `json:"dataDir"`
	TranscodeDir      string `json:"transcodeDir"`
	DownloadsDir      string `json:"downloadsDir"`
	MetadataDir       string `json:"metadataDir"`
	CacheDir          string `json:"cacheDir"`
	TempDir           string `json:"tempDir"`
	FFmpegPath        string `json:"ffmpegPath"`
	FFprobePath       string `json:"ffprobePath"`
	EventBuffer       int    `json:"eventBuffer"`
	ScanWorkers       int    `json:"scanWorkers"`
	ProbeWorkers      int    `json:"probeWorkers"`
	TranscodeWorkers  int    `json:"transcodeWorkers"`
	GPUWorkers        int    `json:"gpuWorkers"`
	LibrarySyncMode   string `json:"librarySyncMode"`
	SyncIntervalMins  int    `json:"syncIntervalMins"`
	WatchDebounceSecs int    `json:"watchDebounceSecs"`
	ProbeBatchLimit   int    `json:"probeBatchLimit"`
	Country           string `json:"country,omitempty"`
	Timezone          string `json:"timezone,omitempty"`
	SetupComplete     bool   `json:"setupComplete,omitempty"`
}

type Summary struct {
	Libraries    int `json:"libraries"`
	MediaSources int `json:"mediaSources"`
	Movies       int `json:"movies"`
	Series       int `json:"series"`
	Episodes     int `json:"episodes"`
	ScanRuns     int `json:"scanRuns"`
	Unprobed     int `json:"unprobed"`
}

type Health struct {
	Summary       Summary `json:"summary"`
	NeedsReview   int     `json:"needsReview"`
	Unprobed      int     `json:"unprobed"`
	Unsupported   int     `json:"unsupported"`
	HighBitrate   int     `json:"highBitrate"`
	WithSubtitles int     `json:"withSubtitles"`
	// TotalSizeBytes is the sum of size_bytes across all media_sources rows.
	// Surfaces the library footprint on disk on the dashboard so users can
	// see "4.2 TB across 4,418 files" alongside the file count.
	TotalSizeBytes int64 `json:"totalSizeBytes"`
}

// CodecCount is one entry in the codec histogram — codec name + how many
// probed files use it. Sorted descending by count when returned.
type CodecCount struct {
	Codec string `json:"codec"`
	Count int    `json:"count"`
}

// CodecBreakdown is the response to GET /api/catalog/codecs. Gives the
// dashboard everything it needs to render an honest "what's in your library"
// view that maps onto real playback behaviour.
type CodecBreakdown struct {
	// VideoCodecs sorted by count desc. Empty codecs are excluded.
	VideoCodecs []CodecCount `json:"videoCodecs"`
	// Total of all VideoCodecs counts — the population the rest are slicing.
	Total int `json:"total"`
}

type MovieListItem struct {
	ID           string          `json:"id"`
	Title        string          `json:"title"`
	Year         int             `json:"year"`
	SortTitle    string          `json:"sortTitle"`
	NeedsReview  bool            `json:"needsReview"`
	Probed       bool            `json:"probed"`
	VersionCount int             `json:"versionCount"`
	AddedAt      string          `json:"addedAt,omitempty"`
	Watched      bool            `json:"watched,omitempty"`
	Metadata     *MetadataRecord `json:"metadata,omitempty"`
}

type MovieDetail struct {
	MovieListItem
	Versions []MovieVersion `json:"versions"`
}

type MovieVersion struct {
	MediaSourceID string `json:"mediaSourceId"`
	Path          string `json:"path"`
	RelPath       string `json:"relPath"`
	Edition       string `json:"edition,omitempty"`
	QualityLabel  string `json:"qualityLabel,omitempty"`
	SizeBytes     int64  `json:"sizeBytes"`
	ModifiedAt    string `json:"modifiedAt"`
}

type SeriesListItem struct {
	ID           string          `json:"id"`
	Title        string          `json:"title"`
	SortTitle    string          `json:"sortTitle"`
	SeasonCount  int             `json:"seasonCount"`
	EpisodeCount int             `json:"episodeCount"`
	AddedAt      string          `json:"addedAt,omitempty"`
	Watched      bool            `json:"watched,omitempty"`
	NeedsReview  bool            `json:"needsReview,omitempty"`
	Metadata     *MetadataRecord `json:"metadata,omitempty"`
}

type SeriesDetail struct {
	SeriesListItem
	Seasons []SeasonDetail `json:"seasons"`
}

type SeasonDetail struct {
	ID           string         `json:"id"`
	SeasonNumber int            `json:"seasonNumber"`
	Episodes     []EpisodeBrief `json:"episodes"`
	Metadata     *MetadataRecord `json:"metadata,omitempty"`
}

type EpisodeBrief struct {
	ID            string         `json:"id"`
	SeasonNumber  int            `json:"seasonNumber"`
	EpisodeNumber int            `json:"episodeNumber"`
	EpisodeEnd    int            `json:"episodeEnd,omitempty"`
	Title         string         `json:"title,omitempty"`
	NeedsReview   bool           `json:"needsReview"`
	VersionCount  int            `json:"versionCount"`
	Versions      []MovieVersion `json:"versions,omitempty"`
	Metadata      *MetadataRecord `json:"metadata,omitempty"`
}

type ReviewItem struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	Title        string `json:"title"`
	ReviewReason string `json:"reviewReason"`
}

type MetadataCredit struct {
	Name        string `json:"name,omitempty"`
	Role        string `json:"role,omitempty"`
	Character   string `json:"character,omitempty"`
	Department  string `json:"department,omitempty"`
	ProfileURL  string `json:"profileUrl,omitempty"`
	SortOrder   int    `json:"sortOrder,omitempty"`
}

type MetadataCollection struct {
	ID              string `json:"id,omitempty"`
	Name            string `json:"name,omitempty"`
	PosterURL       string `json:"posterUrl,omitempty"`
	BackdropURL     string `json:"backdropUrl,omitempty"`
	LogoURL         string `json:"logoUrl,omitempty"`
	BannerURL       string `json:"bannerUrl,omitempty"`
	LandscapeURL    string `json:"landscapeUrl,omitempty"`
}

type MetadataStatus struct {
	Primary             string   `json:"primary,omitempty"`
	States              []string `json:"states,omitempty"`
	MissingFields       []string `json:"missingFields,omitempty"`
	MissingArtwork      []string `json:"missingArtwork,omitempty"`
	Matched             bool     `json:"matched,omitempty"`
	UserOverrideApplied bool     `json:"userOverrideApplied,omitempty"`
}

type MetadataRecord struct {
	Kind        string             `json:"kind"`
	ItemID      string             `json:"itemId"`
	Provider    string             `json:"provider"`
	ExternalID  string             `json:"externalId,omitempty"`
	Title       string             `json:"title"`
	Year        int                `json:"year,omitempty"`
	Overview    string             `json:"overview,omitempty"`
	PosterURL   string             `json:"posterUrl,omitempty"`
	BackdropURL string             `json:"backdropUrl,omitempty"`
	ThumbnailURL string            `json:"thumbnailUrl,omitempty"`
	LogoURL     string             `json:"logoUrl,omitempty"`
	BannerURL   string             `json:"bannerUrl,omitempty"`
	VideoKey    string             `json:"videoKey,omitempty"`    // YouTube key for the official trailer
	TrailerPath string             `json:"trailerPath,omitempty"` // Local MP4 path once downloaded
	OriginalTitle string           `json:"originalTitle,omitempty"`
	ReleaseDate string             `json:"releaseDate,omitempty"`
	FirstAirDate string            `json:"firstAirDate,omitempty"`
	AirDate     string             `json:"airDate,omitempty"`
	RuntimeMinutes int             `json:"runtimeMinutes,omitempty"`
	Genres      []string           `json:"genres,omitempty"`
	ContentRating string           `json:"contentRating,omitempty"`
	Cast        []MetadataCredit   `json:"cast,omitempty"`
	Crew        []MetadataCredit   `json:"crew,omitempty"`
	GuestCast   []MetadataCredit   `json:"guestCast,omitempty"`
	Directors   []string           `json:"directors,omitempty"`
	Writers     []string           `json:"writers,omitempty"`
	Studios     []string           `json:"studios,omitempty"`
	ProductionCompanies []string   `json:"productionCompanies,omitempty"`
	Networks    []string           `json:"networks,omitempty"`
	Country     []string           `json:"country,omitempty"`
	Language    []string           `json:"language,omitempty"`
	StatusText  string             `json:"statusText,omitempty"`
	Collection  *MetadataCollection `json:"collection,omitempty"`
	SeasonNumber int               `json:"seasonNumber,omitempty"`
	EpisodeNumber int              `json:"episodeNumber,omitempty"`
	EpisodeCount int               `json:"episodeCount,omitempty"`
	Confidence  float64            `json:"confidence"`
	RawJSON     string             `json:"rawJson,omitempty"`
	FetchedAt   string             `json:"fetchedAt"`
	UpdatedAt   string             `json:"updatedAt"`
	Ratings     Ratings            `json:"ratings,omitempty"`
	ExternalIDs map[string]string  `json:"externalIds,omitempty"`
	Provenance  MetadataProvenance `json:"provenance,omitempty"`
	MetadataStatus MetadataStatus  `json:"metadataStatus,omitempty"`

	ArtworkJSON string `json:"-"`
	DetailsJSON string `json:"-"`
}

type MetadataProvenance struct {
	Fields      map[string]string `json:"fields,omitempty"`
	Ratings     map[string]string `json:"ratings,omitempty"`
	ExternalIDs map[string]string `json:"externalIds,omitempty"`
}

type Rating struct {
	Kind         string  `json:"kind"`
	ItemID       string  `json:"itemId"`
	Provider     string  `json:"provider"`
	RatingType   string  `json:"ratingType"`
	Value        float64 `json:"value"`
	DisplayValue string  `json:"displayValue"`
	Scale        float64 `json:"scale,omitempty"`
	Votes        int     `json:"votes,omitempty"`
	SourceURL    string  `json:"sourceUrl,omitempty"`
	FetchedAt    string  `json:"fetchedAt"`
	UpdatedAt    string  `json:"updatedAt"`
}

type Ratings map[string]any

type ExternalID struct {
	Kind       string `json:"kind"`
	ItemID     string `json:"itemId"`
	Provider   string `json:"provider"`
	ExternalID string `json:"externalId"`
	UpdatedAt  string `json:"updatedAt"`
}

type VersionGroup struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	Title        string `json:"title"`
	VersionCount int    `json:"versionCount"`
}

type MetadataUpdate struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Title       string `json:"title"`
	Year        int    `json:"year,omitempty"`
	Overview    string `json:"overview,omitempty"`
	Provider    string `json:"provider,omitempty"`
	ExternalID  string `json:"externalId,omitempty"`
	PosterURL   string `json:"posterUrl,omitempty"`
	BackdropURL string `json:"backdropUrl,omitempty"`
	ThumbnailURL string `json:"thumbnailUrl,omitempty"`
	LogoURL     string `json:"logoUrl,omitempty"`
	BannerURL   string `json:"bannerUrl,omitempty"`
	Review      bool   `json:"review"`
}

type MediaSourceItem struct {
	ID              string  `json:"id"`
	LibraryID       string  `json:"libraryId"`
	Kind            string  `json:"kind"`
	Path            string  `json:"path"`
	RelPath         string  `json:"relPath"`
	Name            string  `json:"name"`
	Extension       string  `json:"extension"`
	SizeBytes       int64   `json:"sizeBytes"`
	ModifiedAt      string  `json:"modifiedAt"`
	Probed          bool    `json:"probed"`
	Container       string  `json:"container,omitempty"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	Bitrate         int64   `json:"bitrate,omitempty"`
	VideoCodec      string  `json:"videoCodec,omitempty"`
	VideoProfile    string  `json:"videoProfile,omitempty"`
	VideoLevel      string  `json:"videoLevel,omitempty"`
	VideoBitDepth   int     `json:"videoBitDepth,omitempty"`
	VideoFrameRate  float64 `json:"videoFrameRate,omitempty"`
	PixelFormat     string  `json:"pixelFormat,omitempty"`
	ColorPrimaries  string  `json:"colorPrimaries,omitempty"`
	ColorTransfer   string  `json:"colorTransfer,omitempty"`
	ColorSpace      string  `json:"colorSpace,omitempty"`
	HDRFormat       string  `json:"hdrFormat,omitempty"`
	DoviProfile     int     `json:"doviProfile,omitempty"`
	MaxCLL          int     `json:"maxCll,omitempty"`
	MaxFALL         int     `json:"maxFall,omitempty"`
	Width           int     `json:"width,omitempty"`
	Height          int     `json:"height,omitempty"`
	AudioStreams    int     `json:"audioStreams,omitempty"`
	SubtitleStreams int     `json:"subtitleStreams,omitempty"`
}

type MediaSourceDisplay struct {
	Kind          string `json:"kind"`
	ItemID        string `json:"itemId"`
	Title         string `json:"title"`
	ArtworkKind   string `json:"artworkKind"`
	ArtworkID     string `json:"artworkId"`
	QualityLabel  string `json:"qualityLabel,omitempty"`
	// Episode-specific fields (populated when Kind == "episode")
	SeasonNumber  int    `json:"seasonNumber,omitempty"`
	EpisodeNumber int    `json:"episodeNumber,omitempty"`
	EpisodeTitle  string `json:"episodeTitle,omitempty"`
	SeriesID      string `json:"seriesId,omitempty"`
}

type MediaSourceTracks struct {
	AudioTracks    []probe.Track `json:"audioTracks"`
	SubtitleTracks []probe.Track `json:"subtitleTracks"`
}

type ProbeResult struct {
	Container       string  `json:"container"`
	DurationSeconds float64 `json:"durationSeconds"`
	Bitrate         int64   `json:"bitrate"`
	VideoCodec      string  `json:"videoCodec"`
	VideoProfile    string  `json:"videoProfile"`
	VideoLevel      string  `json:"videoLevel"`
	VideoBitDepth   int     `json:"videoBitDepth"`
	VideoFrameRate  float64 `json:"videoFrameRate"`
	PixelFormat     string  `json:"pixelFormat"`
	ColorPrimaries  string  `json:"colorPrimaries"`
	ColorTransfer   string  `json:"colorTransfer"`
	ColorSpace      string  `json:"colorSpace"`
	HDRFormat       string  `json:"hdrFormat"`
	DoviProfile     int     `json:"doviProfile"`
	MaxCLL          int     `json:"maxCll"`
	MaxFALL         int     `json:"maxFall"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	AudioStreams    int     `json:"audioStreams"`
	SubtitleStreams int     `json:"subtitleStreams"`
	RawJSON         string  `json:"rawJson"`
}

func NewService(database *database.Service) *Service {
	return &Service{db: database.DB()}
}

func (s *Service) ScanState(ctx context.Context, libraryID string) (map[string]scanner.FileSignature, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rel_path, size_bytes, modified_at
		FROM scan_file_state
		WHERE library_id = ?
	`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	output := map[string]scanner.FileSignature{}
	for rows.Next() {
		var relPath string
		var size int64
		var modifiedAt string
		if err := rows.Scan(&relPath, &size, &modifiedAt); err != nil {
			return nil, err
		}
		output[relPath] = scanner.FileSignature{Size: size, ModifiedAt: parseTimestamp(modifiedAt)}
	}
	return output, rows.Err()
}

func (s *Service) ListLibraries(ctx context.Context) ([]libraries.Library, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, name, path, storage_type, metadata_sources_json, artwork_sources_json
		FROM libraries
		ORDER BY kind, name, path
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	output := []libraries.Library{}
	for rows.Next() {
		var item libraries.Library
		var rawSources string
		var rawArtworkSources string
		if err := rows.Scan(&item.ID, &item.Kind, &item.Name, &item.Path, &item.StorageType, &rawSources, &rawArtworkSources); err != nil {
			return nil, err
		}
		item.MetadataSources = decodeLibraryMetadataSources(item.Kind, rawSources)
		item.ArtworkSources = decodeLibraryArtworkSources(item.Kind, rawArtworkSources)
		output = append(output, item)
	}
	return output, rows.Err()
}

func (s *Service) SaveLibrary(ctx context.Context, library libraries.Library) (libraries.Library, error) {
	if strings.TrimSpace(library.Path) == "" {
		return libraries.Library{}, errors.New("library path is required")
	}
	if library.Kind != libraries.KindMovies && library.Kind != libraries.KindTV {
		return libraries.Library{}, errors.New("library kind must be movies or tv")
	}
	if strings.TrimSpace(library.Name) == "" {
		if library.Kind == libraries.KindMovies {
			library.Name = "Movies"
		} else {
			library.Name = "TV"
		}
	}
	if library.ID == "" {
		library.ID = libraries.IDFor(library.Kind, library.Path)
	}
	if library.StorageType == "" {
		library.StorageType = libraries.DetectStorageType(library.Path)
	}
	library.MetadataSources = metasources.NormalizeRequestedSourceOrder(string(library.Kind), library.MetadataSources)
	library.ArtworkSources = metasources.NormalizeRequestedArtworkOrder(string(library.Kind), library.ArtworkSources)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return libraries.Library{}, err
	}
	defer tx.Rollback()
	if err := upsertLibrary(ctx, tx, library); err != nil {
		return libraries.Library{}, err
	}
	return library, tx.Commit()
}

func (s *Service) GetLibraryForItem(ctx context.Context, kind string, itemID string) (libraries.Library, bool, error) {
	query := ""
	switch kind {
	case "movie":
		query = `
			SELECT l.id, l.kind, l.name, l.path, l.storage_type, l.metadata_sources_json, l.artwork_sources_json
			FROM libraries l
			JOIN media_sources ms ON ms.library_id = l.id
			JOIN movie_versions mv ON mv.media_source_id = ms.id
			WHERE mv.movie_id = ?
			LIMIT 1
		`
	case "series":
		query = `
			SELECT l.id, l.kind, l.name, l.path, l.storage_type, l.metadata_sources_json, l.artwork_sources_json
			FROM libraries l
			JOIN media_sources ms ON ms.library_id = l.id
			JOIN episode_versions ev ON ev.media_source_id = ms.id
			JOIN tv_episodes e ON e.id = ev.episode_id
			WHERE e.series_id = ?
			LIMIT 1
		`
	case "season":
		query = `
			SELECT l.id, l.kind, l.name, l.path, l.storage_type, l.metadata_sources_json, l.artwork_sources_json
			FROM libraries l
			JOIN media_sources ms ON ms.library_id = l.id
			JOIN episode_versions ev ON ev.media_source_id = ms.id
			JOIN tv_episodes e ON e.id = ev.episode_id
			WHERE e.season_id = ?
			LIMIT 1
		`
	case "episode":
		query = `
			SELECT l.id, l.kind, l.name, l.path, l.storage_type, l.metadata_sources_json, l.artwork_sources_json
			FROM libraries l
			JOIN media_sources ms ON ms.library_id = l.id
			JOIN episode_versions ev ON ev.media_source_id = ms.id
			WHERE ev.episode_id = ?
			LIMIT 1
		`
	default:
		return libraries.Library{}, false, nil
	}
	var item libraries.Library
	var rawSources string
	var rawArtworkSources string
	err := s.db.QueryRowContext(ctx, query, itemID).Scan(&item.ID, &item.Kind, &item.Name, &item.Path, &item.StorageType, &rawSources, &rawArtworkSources)
	if errors.Is(err, sql.ErrNoRows) {
		return libraries.Library{}, false, nil
	}
	if err != nil {
		return libraries.Library{}, false, err
	}
	item.MetadataSources = decodeLibraryMetadataSources(item.Kind, rawSources)
	item.ArtworkSources = decodeLibraryArtworkSources(item.Kind, rawArtworkSources)
	return item, true, nil
}

func (s *Service) DeleteLibrary(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM libraries WHERE id = ?", id)
	return err
}

func (s *Service) SaveSettings(ctx context.Context, settings RuntimeSettings) error {
	now := timestamp(time.Now())
	values := map[string]string{
		"httpAddr":          settings.HTTPAddr,
		"dataDir":           settings.DataDir,
		"transcodeDir":      settings.TranscodeDir,
		"downloadsDir":      settings.DownloadsDir,
		"metadataDir":       settings.MetadataDir,
		"cacheDir":          settings.CacheDir,
		"tempDir":           settings.TempDir,
		"ffmpegPath":        settings.FFmpegPath,
		"ffprobePath":       settings.FFprobePath,
		"eventBuffer":       intString(settings.EventBuffer),
		"scanWorkers":       intString(settings.ScanWorkers),
		"probeWorkers":      intString(settings.ProbeWorkers),
		"transcodeWorkers":  intString(settings.TranscodeWorkers),
		"gpuWorkers":        intString(settings.GPUWorkers),
		"librarySyncMode":   settings.LibrarySyncMode,
		"syncIntervalMins":  intString(settings.SyncIntervalMins),
		"watchDebounceSecs": intString(settings.WatchDebounceSecs),
		"probeBatchLimit":   intString(settings.ProbeBatchLimit),
		"country":           settings.Country,
		"timezone":          settings.Timezone,
		"setupComplete":     boolString(settings.SetupComplete),
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range values {
		if value == "" || value == "0" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO app_settings(key, value, updated_at)
			VALUES(?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		`, key, value, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) Summary(ctx context.Context) (Summary, error) {
	var summary Summary
	counts := []struct {
		table string
		out   *int
	}{
		{"libraries", &summary.Libraries},
		{"media_sources", &summary.MediaSources},
		{"movies", &summary.Movies},
		{"tv_series", &summary.Series},
		{"tv_episodes", &summary.Episodes},
		{"scan_runs", &summary.ScanRuns},
	}
	for _, count := range counts {
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+count.table).Scan(count.out); err != nil {
			return Summary{}, err
		}
	}
	_ = s.db.QueryRowContext(ctx, "SELECT count(*) FROM media_sources ms LEFT JOIN media_probes mp ON mp.media_source_id = ms.id WHERE mp.media_source_id IS NULL").Scan(&summary.Unprobed)
	return summary, nil
}

func (s *Service) Health(ctx context.Context) (Health, error) {
	summary, err := s.Summary(ctx)
	if err != nil {
		return Health{}, err
	}
	health := Health{Summary: summary, Unprobed: summary.Unprobed}
	queries := []struct {
		query string
		out   *int
	}{
		{"SELECT (SELECT count(*) FROM movies WHERE needs_review = 1) + (SELECT count(*) FROM tv_episodes WHERE needs_review = 1)", &health.NeedsReview},
		// "Unsupported" = codecs that cannot direct-play OR remux on any of our
		// supported clients (web + apple-tv + android-tv + chromecast). The
		// playable set includes hevc (handled via web MSE since PR #278) and
		// the rest of the modern codecs. Anything else (MPEG-2, VC-1, WMV,
		// RealVideo, etc.) needs a full video re-encode, which is genuinely
		// "unsupported" from a no-friction-playback perspective.
		{"SELECT count(*) FROM media_probes WHERE video_codec NOT IN ('h264', 'hevc', 'av1', 'vp9')", &health.Unsupported},
		{"SELECT count(*) FROM media_probes WHERE bitrate > 40000000", &health.HighBitrate},
		{"SELECT count(*) FROM media_probes WHERE subtitle_streams > 0", &health.WithSubtitles},
	}
	for _, item := range queries {
		if err := s.db.QueryRowContext(ctx, item.query).Scan(item.out); err != nil {
			return Health{}, err
		}
	}
	// Total library footprint — COALESCE protects against empty libraries
	// where SUM returns NULL. Indexed by media_source primary key so this
	// scans the full table but it's a tiny per-row read.
	if err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(size_bytes), 0) FROM media_sources",
	).Scan(&health.TotalSizeBytes); err != nil {
		return Health{}, err
	}
	return health, nil
}

// Codecs returns the video-codec histogram for everything ffprobe has
// inspected. Used by the dashboard to break down the bare "unsupported"
// count into "you have 1,200 H.264, 250 HEVC, 5 MPEG-2" — which is the
// information a user actually needs to understand what their library will
// do at playback time.
//
// Single GROUP BY against the indexed video_codec column. Cheap enough to
// hit on every dashboard refresh; we cap at 32 distinct rows so a corrupt
// row never produces a runaway response.
func (s *Service) Codecs(ctx context.Context) (CodecBreakdown, error) {
	const query = `
		SELECT COALESCE(video_codec, '') AS codec, COUNT(*) AS n
		FROM media_probes
		WHERE COALESCE(video_codec, '') <> ''
		GROUP BY video_codec
		ORDER BY n DESC
		LIMIT 32
	`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return CodecBreakdown{}, err
	}
	defer rows.Close()
	out := CodecBreakdown{VideoCodecs: []CodecCount{}}
	for rows.Next() {
		var c CodecCount
		if err := rows.Scan(&c.Codec, &c.Count); err != nil {
			return CodecBreakdown{}, err
		}
		out.VideoCodecs = append(out.VideoCodecs, c)
		out.Total += c.Count
	}
	if err := rows.Err(); err != nil {
		return CodecBreakdown{}, err
	}
	return out, nil
}

// RatingOrder returns a numeric order for a content rating string, allowing
// ceiling comparisons across both theatrical and TV rating scales.
// Returns -1 if the rating is unrecognised (treated as "no rating" — shown
// regardless of ceiling, since hiding all unknown ratings is too aggressive).
func RatingOrder(rating string) int {
	switch strings.ToUpper(strings.TrimSpace(rating)) {
	// Theatrical
	case "G":
		return 10
	case "PG":
		return 20
	case "PG-13":
		return 30
	case "R":
		return 40
	case "NC-17":
		return 50
	// TV
	case "TV-Y":
		return 10
	case "TV-Y7", "TV-Y7-FV":
		return 15
	case "TV-G":
		return 20
	case "TV-PG":
		return 25
	case "TV-14":
		return 35
	case "TV-MA":
		return 45
	}
	return -1
}

// withinCeiling reports whether an item with the given content rating should be
// visible when the profile's max rating is ceiling. Items with unrecognised or
// empty ratings always pass through (shown). Items with a recognised rating are
// shown only when their order ≤ the ceiling order.
func withinCeiling(itemRating, ceiling string) bool {
	if ceiling == "" {
		return true // no restriction
	}
	ceilingOrder := RatingOrder(ceiling)
	if ceilingOrder < 0 {
		return true // unrecognised ceiling — no enforcement
	}
	itemOrder := RatingOrder(itemRating)
	if itemOrder < 0 {
		return true // unrecognised item rating — pass through
	}
	return itemOrder <= ceilingOrder
}

// RatingWithinCeiling is the exported form of withinCeiling so callers outside
// this package (e.g. the API streaming/detail authorization paths) can apply
// the exact same parental-control rule used for list/search filtering.
func RatingWithinCeiling(itemRating, ceiling string) bool {
	return withinCeiling(itemRating, ceiling)
}

// MediaSourceContentRating resolves the content rating of the movie or series
// that owns the given media source. It returns ("", false, nil) when the media
// source isn't found or has no resolvable parent. The rating is read from the
// owning title's best metadata record — the same field used by the list and
// search rating filters — so playback/detail authorization stays consistent
// with what the profile is allowed to see.
func (s *Service) MediaSourceContentRating(ctx context.Context, mediaSourceID string) (string, bool, error) {
	// Movie owner?
	var movieID string
	err := s.db.QueryRowContext(ctx, `
		SELECT movie_id FROM movie_versions WHERE media_source_id = ? LIMIT 1
	`, mediaSourceID).Scan(&movieID)
	if err == nil && movieID != "" {
		if record, ok, mErr := s.GetBestMetadata(ctx, "movie", movieID); mErr != nil {
			return "", false, mErr
		} else if ok {
			return strings.TrimSpace(record.ContentRating), true, nil
		}
		return "", true, nil // owner exists but no rating known — pass-through at the ceiling check
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}

	// Episode owner? Its content rating comes from the parent series.
	var seriesID string
	err = s.db.QueryRowContext(ctx, `
		SELECT e.series_id
		FROM episode_versions ev
		JOIN tv_episodes e ON e.id = ev.episode_id
		WHERE ev.media_source_id = ?
		LIMIT 1
	`, mediaSourceID).Scan(&seriesID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if seriesID == "" {
		return "", false, nil
	}
	if record, ok, mErr := s.GetBestMetadata(ctx, "series", seriesID); mErr != nil {
		return "", false, mErr
	} else if ok {
		return strings.TrimSpace(record.ContentRating), true, nil
	}
	return "", true, nil
}

func (s *Service) ListMovies(ctx context.Context, limit int, maxRating string, userID string) ([]MovieListItem, error) {
	// limit <= 0 means "return everything". A positive limit is honoured as-is.
	// When a rating ceiling is active we over-fetch so the Go-level filter
	// still has enough candidates to fill the requested limit.
	sqlLimit := limit
	if sqlLimit <= 0 {
		sqlLimit = -1 // SQLite: LIMIT -1 = no limit
	} else if maxRating != "" {
		sqlLimit = limit * 10
	}
	// Read aggregates from movies_list_view (denormalized snapshot maintained
	// by triggers in migration 0002). The old query joined movies × movie_versions
	// × media_probes × playback_states with a GROUP BY m.id over the entire table
	// before LIMIT could clip it — ~2.4s on a 4000-item library. The new shape
	// is an index seek + LIMIT.
	//
	// The watched flag is per-user so it can't live in the snapshot — we pull
	// it via a correlated subquery so SQLite only computes it for the LIMIT-
	// clipped rows, not the whole table.
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.movie_id, v.title, v.year, v.sort_title, v.needs_review,
		       v.version_count, v.created_at,
		       COALESCE((SELECT MAX(CASE WHEN ps.watched != 0 THEN 1 ELSE 0 END)
		                 FROM movie_versions mv
		                 LEFT JOIN playback_states ps
		                     ON ps.media_source_id = mv.media_source_id AND ps.user_id = ?
		                 WHERE mv.movie_id = v.movie_id), 0) AS is_watched,
		       v.is_probed
		FROM movies_list_view v
		ORDER BY v.sort_title, v.year
		LIMIT ?
	`, userID, sqlLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type movieStub struct {
		id, title, sortTitle, addedAt string
		year, versionCount            int
		needsReview, isWatched        int
		isProbed                      int
	}
	var stubs []movieStub
	for rows.Next() {
		var st movieStub
		if err := rows.Scan(&st.id, &st.title, &st.year, &st.sortTitle, &st.needsReview, &st.versionCount, &st.addedAt, &st.isWatched, &st.isProbed); err != nil {
			return nil, err
		}
		stubs = append(stubs, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Batch-fetch all metadata in one round-trip.
	ids := make([]string, len(stubs))
	for i, st := range stubs {
		ids[i] = st.id
	}
	metaMap, err := s.GetBestMetadataBatch(ctx, "movie", ids)
	if err != nil {
		return nil, err
	}

	output := make([]MovieListItem, 0, len(stubs))
	for _, st := range stubs {
		item := MovieListItem{
			ID:           st.id,
			Title:        st.title,
			Year:         st.year,
			SortTitle:    st.sortTitle,
			NeedsReview:  st.needsReview != 0,
			Probed:       st.isProbed != 0,
			VersionCount: st.versionCount,
			AddedAt:      st.addedAt,
			Watched:      st.isWatched != 0,
		}
		if record, ok := metaMap[st.id]; ok {
			item.Metadata = &record
			applyMovieMetadata(&item.Title, &item.Year, &item.SortTitle, record)
		}
		if maxRating != "" {
			cr := ""
			if item.Metadata != nil {
				cr = item.Metadata.ContentRating
			}
			if !withinCeiling(cr, maxRating) {
				continue
			}
		}
		output = append(output, item)
		if limit > 0 && len(output) >= limit {
			break
		}
	}
	return output, nil
}

// CollectionResult is the header record returned by ListMoviesByCollection.
type CollectionResult struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	PosterURL   string `json:"posterUrl,omitempty"`
	BackdropURL string `json:"backdropUrl,omitempty"`
	LogoURL     string `json:"logoUrl,omitempty"`
}

// PersonProfile is the person header for the people detail endpoint.
type PersonProfile struct {
	Name       string `json:"name"`
	ProfileURL string `json:"profileUrl,omitempty"`
	Department string `json:"department,omitempty"`
}

// PersonCreditItem is one entry in a person's filmography.
type PersonCreditItem struct {
	Kind      string          `json:"kind"`
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Year      int             `json:"year,omitempty"`
	Character string          `json:"character,omitempty"`
	Role      string          `json:"role,omitempty"`
	Metadata  *MetadataRecord `json:"metadata,omitempty"`
}

// MissingProviderItem is a minimal stub for backfill callers: just enough to
// dispatch a metadata refresh request without paying for full metadata loads
// that the caller is about to overwrite anyway.
type MissingProviderItem struct {
	Kind   string // "movie" | "series"
	ID     string
	Title  string
	Year   int
}

// ListItemsMissingProvider returns library items (movies or series) that do
// NOT yet have a metadata_records row for the given provider. This is the
// inverse of "shouldSkipMetadata" — we want items lacking THIS specific
// provider, even if they're "enriched" by another (Wikipedia etc).
//
// Used by the metadata backfill to populate TMDB rows for items that
// currently only have wiki/filename data.
func (s *Service) ListItemsMissingProvider(ctx context.Context, kind string, provider string, limit int) ([]MissingProviderItem, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		return nil, errors.New("provider required")
	}
	var query string
	switch kind {
	case "movie", "movies":
		query = `
			SELECT m.id, m.title, m.year
			FROM movies m
			WHERE NOT EXISTS (
				SELECT 1 FROM metadata_records mr
				WHERE mr.kind = 'movie' AND mr.item_id = m.id AND mr.provider = ?
			)
			ORDER BY m.sort_title, m.year
			LIMIT ?
		`
	case "series", "tv":
		query = `
			SELECT s.id, s.title, 0
			FROM tv_series s
			WHERE NOT EXISTS (
				SELECT 1 FROM metadata_records mr
				WHERE mr.kind = 'series' AND mr.item_id = s.id AND mr.provider = ?
			)
			ORDER BY s.sort_title
			LIMIT ?
		`
	default:
		return nil, errors.New("kind must be movie or series")
	}
	rows, err := s.db.QueryContext(ctx, query, provider, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	normalizedKind := "movie"
	if kind == "series" || kind == "tv" {
		normalizedKind = "series"
	}
	output := []MissingProviderItem{}
	for rows.Next() {
		var item MissingProviderItem
		item.Kind = normalizedKind
		if err := rows.Scan(&item.ID, &item.Title, &item.Year); err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	return output, rows.Err()
}

// CountItemsMissingProvider returns the total count of items lacking the
// given provider. Used by the backfill status endpoint to display "x of y
// items remaining" without paginating through the whole list.
func (s *Service) CountItemsMissingProvider(ctx context.Context, kind string, provider string) (int, error) {
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		return 0, nil
	}
	var query string
	switch kind {
	case "movie", "movies":
		query = `
			SELECT COUNT(*) FROM movies m
			WHERE NOT EXISTS (
				SELECT 1 FROM metadata_records mr
				WHERE mr.kind='movie' AND mr.item_id=m.id AND mr.provider=?
			)
		`
	case "series", "tv":
		query = `
			SELECT COUNT(*) FROM tv_series s
			WHERE NOT EXISTS (
				SELECT 1 FROM metadata_records mr
				WHERE mr.kind='series' AND mr.item_id=s.id AND mr.provider=?
			)
		`
	default:
		return 0, errors.New("kind must be movie or series")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, query, provider).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ListMoviesByCollection returns all library movies belonging to a TMDB
// collection identified by collectionID (e.g. "195" for the Bond franchise).
// The collection header is populated from the first movie's metadata.
// Returns (nil, CollectionResult{}, false, nil) when no movies are found.
func (s *Service) ListMoviesByCollection(ctx context.Context, collectionID string) ([]MovieListItem, CollectionResult, bool, error) {
	collectionID = strings.TrimSpace(collectionID)
	if collectionID == "" {
		return nil, CollectionResult{}, false, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT m.id, m.title, m.year, m.sort_title
		FROM movies m
		WHERE EXISTS (
			SELECT 1 FROM metadata_records mr
			WHERE mr.kind = 'movie' AND mr.item_id = m.id
			AND json_extract(mr.details_json, '$.collection.id') = ?
		)
		ORDER BY m.year, m.sort_title
	`, collectionID)
	if err != nil {
		return nil, CollectionResult{}, false, err
	}
	defer rows.Close()
	type stub struct {
		id, title, sortTitle string
		year                 int
	}
	var stubs []stub
	for rows.Next() {
		var st stub
		if err := rows.Scan(&st.id, &st.title, &st.year, &st.sortTitle); err != nil {
			return nil, CollectionResult{}, false, err
		}
		stubs = append(stubs, st)
	}
	if err := rows.Err(); err != nil {
		return nil, CollectionResult{}, false, err
	}
	if len(stubs) == 0 {
		return nil, CollectionResult{}, false, nil
	}
	var header CollectionResult
	var output []MovieListItem
	movieIDs := make([]string, 0, len(stubs))
	for _, st := range stubs {
		movieIDs = append(movieIDs, st.id)
	}
	metaMap, err := s.GetBestMetadataBatch(ctx, "movie", movieIDs)
	if err != nil {
		return nil, CollectionResult{}, false, err
	}
	for _, st := range stubs {
		item := MovieListItem{ID: st.id, Title: st.title, Year: st.year, SortTitle: st.sortTitle}
		if record, ok := metaMap[st.id]; ok {
			item.Metadata = &record
			applyMovieMetadata(&item.Title, &item.Year, &item.SortTitle, record)
			if header.Name == "" && record.Collection != nil {
				header = CollectionResult{
					ID:          collectionID,
					Name:        record.Collection.Name,
					PosterURL:   record.Collection.PosterURL,
					BackdropURL: record.Collection.BackdropURL,
					LogoURL:     record.Collection.LogoURL,
				}
			}
		}
		output = append(output, item)
	}
	if header.ID == "" {
		header.ID = collectionID
	}
	return output, header, true, nil
}

// ListCollections returns all TMDB collections referenced by movies in the
// library, ordered alphabetically. Each entry includes the movie count.
// limit <= 0 means no limit (up to 1000).
func (s *Service) ListCollections(ctx context.Context, limit int) ([]CollectionHit, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			json_extract(mr.details_json, '$.collection.id')          AS coll_id,
			json_extract(mr.details_json, '$.collection.name')        AS coll_name,
			json_extract(mr.details_json, '$.collection.posterUrl')   AS poster,
			json_extract(mr.details_json, '$.collection.backdropUrl') AS backdrop,
			COUNT(*)                                                    AS movie_count
		FROM metadata_records mr
		WHERE mr.kind = 'movie'
		  AND json_extract(mr.details_json, '$.collection.id') IS NOT NULL
		  AND TRIM(json_extract(mr.details_json, '$.collection.id')) != ''
		GROUP BY coll_id
		ORDER BY LOWER(coll_name)
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CollectionHit
	for rows.Next() {
		var id, name, poster, backdrop sql.NullString
		var count int
		if err := rows.Scan(&id, &name, &poster, &backdrop, &count); err != nil {
			return nil, err
		}
		if !id.Valid || strings.TrimSpace(id.String) == "" {
			continue
		}
		out = append(out, CollectionHit{
			ID:          id.String,
			Name:        strings.TrimSpace(name.String),
			PosterURL:   poster.String,
			BackdropURL: backdrop.String,
			MovieCount:  count,
		})
	}
	return out, rows.Err()
}

// ListItemsByPerson returns all library movies and series in which the named
// person appears as cast or crew. Results are ordered by year desc, then title.
// Returns (nil, PersonProfile{}, false, nil) when the person is not found.
func (s *Service) ListItemsByPerson(ctx context.Context, personName string, limit int) ([]PersonCreditItem, PersonProfile, bool, error) {
	personName = strings.TrimSpace(personName)
	if personName == "" {
		return nil, PersonProfile{}, false, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	// Movies featuring this person.
	movieRows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT m.id, m.title, m.year, m.sort_title
		FROM movies m
		WHERE EXISTS (
			SELECT 1 FROM metadata_records mr
			WHERE mr.kind = 'movie' AND mr.item_id = m.id
			AND (
				EXISTS (SELECT 1 FROM json_each(mr.details_json, '$.cast') je WHERE json_extract(je.value, '$.name') = ?)
				OR EXISTS (SELECT 1 FROM json_each(mr.details_json, '$.crew') je WHERE json_extract(je.value, '$.name') = ?)
			)
		)
		ORDER BY m.year DESC, m.sort_title
		LIMIT ?
	`, personName, personName, limit)
	if err != nil {
		return nil, PersonProfile{}, false, err
	}
	defer movieRows.Close()
	type stub struct{ id, title, sortTitle, kind string; year int }
	var stubs []stub
	for movieRows.Next() {
		var st stub
		st.kind = "movie"
		if err := movieRows.Scan(&st.id, &st.title, &st.year, &st.sortTitle); err != nil {
			return nil, PersonProfile{}, false, err
		}
		stubs = append(stubs, st)
	}
	if err := movieRows.Err(); err != nil {
		return nil, PersonProfile{}, false, err
	}
	// Series featuring this person.
	seriesRows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT s.id, s.title, 0, s.sort_title
		FROM tv_series s
		WHERE EXISTS (
			SELECT 1 FROM metadata_records mr
			WHERE mr.kind = 'series' AND mr.item_id = s.id
			AND (
				EXISTS (SELECT 1 FROM json_each(mr.details_json, '$.cast') je WHERE json_extract(je.value, '$.name') = ?)
				OR EXISTS (SELECT 1 FROM json_each(mr.details_json, '$.crew') je WHERE json_extract(je.value, '$.name') = ?)
			)
		)
		ORDER BY s.sort_title
		LIMIT ?
	`, personName, personName, limit)
	if err != nil {
		return nil, PersonProfile{}, false, err
	}
	defer seriesRows.Close()
	for seriesRows.Next() {
		var st stub
		st.kind = "series"
		if err := seriesRows.Scan(&st.id, &st.title, &st.year, &st.sortTitle); err != nil {
			return nil, PersonProfile{}, false, err
		}
		stubs = append(stubs, st)
	}
	if err := seriesRows.Err(); err != nil {
		return nil, PersonProfile{}, false, err
	}
	if len(stubs) == 0 {
		return nil, PersonProfile{}, false, nil
	}
	var profile PersonProfile
	var credits []PersonCreditItem
	movieIDs := make([]string, 0, len(stubs))
	seriesIDs := make([]string, 0, len(stubs))
	for _, st := range stubs {
		if st.kind == "movie" {
			movieIDs = append(movieIDs, st.id)
		} else if st.kind == "series" {
			seriesIDs = append(seriesIDs, st.id)
		}
	}
	movieMeta, err := s.GetBestMetadataBatch(ctx, "movie", movieIDs)
	if err != nil {
		return nil, PersonProfile{}, false, err
	}
	seriesMeta, err := s.GetBestMetadataBatch(ctx, "series", seriesIDs)
	if err != nil {
		return nil, PersonProfile{}, false, err
	}
	for _, st := range stubs {
		item := PersonCreditItem{Kind: st.kind, ID: st.id, Title: st.title, Year: st.year}
		var record MetadataRecord
		var ok bool
		switch st.kind {
		case "movie":
			record, ok = movieMeta[st.id]
		case "series":
			record, ok = seriesMeta[st.id]
		}
		if ok {
			item.Metadata = &record
			if st.kind == "movie" {
				applyMovieMetadata(&item.Title, &item.Year, nil, record)
			}
			// Walk cast first, then crew, to find character / role / profile.
			for _, c := range record.Cast {
				if strings.EqualFold(c.Name, personName) {
					item.Character = c.Character
					item.Role = firstNonEmptyTrimmed(c.Role, "Actor")
					if profile.Name == "" {
						profile = PersonProfile{Name: c.Name, ProfileURL: c.ProfileURL, Department: c.Department}
					}
					break
				}
			}
			if item.Character == "" && item.Role == "" {
				for _, c := range record.Crew {
					if strings.EqualFold(c.Name, personName) {
						item.Role = firstNonEmptyTrimmed(c.Role, c.Department)
						if profile.Name == "" {
							profile = PersonProfile{Name: c.Name, ProfileURL: c.ProfileURL, Department: c.Department}
						}
						break
					}
				}
			}
		}
		if profile.Name == "" {
			profile.Name = personName
		}
		credits = append(credits, item)
	}
	return credits, profile, true, nil
}

// SimilarItem is a lightweight summary of a library title used in the
// "More like this" row on detail screens.
type SimilarItem struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Title     string `json:"title"`
	Year      int    `json:"year,omitempty"`
	PosterURL string `json:"posterUrl,omitempty"`
}

// SimilarSeriesResult carries the items returned by SimilarSeries plus a flag
// indicating whether a genre-based fallback was used (i.e. no direct genre
// overlap was found and results were filled from the primary genre pool).
type SimilarSeriesResult struct {
	Items         []SimilarItem
	Fallback      bool
	FallbackGenre string
}

// SimilarMovies returns up to limit movies from the library that share at
// least one genre with the movie identified by sourceID. Results are ordered
// by genre-overlap count descending so the closest matches appear first.
func (s *Service) SimilarMovies(ctx context.Context, sourceID string, limit int) ([]SimilarItem, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	src, ok, err := s.GetBestMetadata(ctx, "movie", sourceID)
	if err != nil {
		return nil, err
	}
	if !ok || len(src.Genres) == 0 {
		return []SimilarItem{}, nil
	}
	genresJSON, err := json.Marshal(src.Genres)
	if err != nil {
		return nil, err
	}
	// json_valid() guards on every json_each call — see SimilarSeries for the
	// rationale. A single bad row in metadata_records would otherwise abort the
	// whole query with "malformed JSON" and 500 the entire MLT row on a movie
	// detail page. Confirmed broken on TV (#368) until this hardening landed;
	// applied here defensively so movies don't regress the same way.
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.title, m.year,
		       COALESCE((SELECT mr2.poster_url FROM metadata_records mr2
		                 WHERE mr2.kind = 'movie' AND mr2.item_id = m.id
		                   AND mr2.poster_url != ''
		                 ORDER BY mr2.confidence DESC, mr2.updated_at DESC
		                 LIMIT 1), '') AS poster_url,
		       COALESCE((
		         SELECT COUNT(*) FROM json_each(genres_json) g
		         WHERE g.value IN (SELECT value FROM json_each(?))
		       ), 0) AS overlap
		FROM movies m
		LEFT JOIN (
		  SELECT mr3.item_id,
		         json_extract(mr3.details_json, '$.genres') AS genres_json
		  FROM metadata_records mr3
		  WHERE mr3.kind = 'movie'
		    AND json_valid(mr3.details_json)
		    AND json_extract(mr3.details_json, '$.genres') IS NOT NULL
		    AND json_valid(json_extract(mr3.details_json, '$.genres'))
		  GROUP BY mr3.item_id
		) gj ON gj.item_id = m.id
		WHERE m.id != ?
		  AND EXISTS (
		    SELECT 1 FROM metadata_records mr
		    WHERE mr.kind = 'movie' AND mr.item_id = m.id
		      AND json_valid(mr.details_json)
		      AND json_valid(json_extract(mr.details_json, '$.genres'))
		      AND EXISTS (
		        SELECT 1 FROM json_each(json_extract(mr.details_json, '$.genres')) g
		        WHERE g.value IN (SELECT value FROM json_each(?))
		      )
		  )
		ORDER BY overlap DESC
		LIMIT ?
	`, string(genresJSON), sourceID, string(genresJSON), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SimilarItem
	for rows.Next() {
		var it SimilarItem
		var overlap int
		it.Kind = "movie"
		if err := rows.Scan(&it.ID, &it.Title, &it.Year, &it.PosterURL, &overlap); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	if out == nil {
		out = []SimilarItem{}
	}
	return out, rows.Err()
}

// SimilarSeries returns up to limit TV series from the library that share at
// least one genre with the series identified by sourceID. Results are ordered
// by genre-overlap count descending. When fewer than 3 direct genre-overlap
// matches are found, the result is augmented with same-primary-genre titles
// ordered by metadata confidence and SimilarSeriesResult.Fallback is set true.
func (s *Service) SimilarSeries(ctx context.Context, sourceID string, limit int) (SimilarSeriesResult, error) {
	empty := SimilarSeriesResult{Items: []SimilarItem{}}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	src, ok, err := s.GetBestMetadata(ctx, "series", sourceID)
	if err != nil {
		return empty, err
	}
	if !ok || len(src.Genres) == 0 {
		return empty, nil
	}
	genresJSON, err := json.Marshal(src.Genres)
	if err != nil {
		return empty, err
	}
	// IMPORTANT: every json_each() call below is guarded by json_valid().
	// SQLite's json_each() aborts the whole query with a generic
	// "malformed JSON" error when given an invalid input, and a single bad
	// row in metadata_records is enough to 500 the entire similar-series
	// endpoint for every series in the library. The json_valid() guard makes
	// bad rows skip themselves silently instead of taking down the row set.
	//
	// The overlap subquery uses json_extract(...,'$.genres') wrapped in a
	// json_valid() check on the EXTRACTED value too: details_json might be
	// valid JSON overall but have a non-array $.genres (e.g. older records
	// that stored it as a string), which also makes json_each fail.
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.title,
		       COALESCE((SELECT mr4.year FROM metadata_records mr4
		                 WHERE mr4.kind = 'series' AND mr4.item_id = s.id
		                   AND mr4.year > 0
		                 ORDER BY mr4.confidence DESC, mr4.updated_at DESC
		                 LIMIT 1), 0) AS year,
		       COALESCE((SELECT mr2.poster_url FROM metadata_records mr2
		                 WHERE mr2.kind = 'series' AND mr2.item_id = s.id
		                   AND mr2.poster_url != ''
		                 ORDER BY mr2.confidence DESC, mr2.updated_at DESC
		                 LIMIT 1), '') AS poster_url,
		       COALESCE((
		         SELECT COUNT(*) FROM json_each(genres_json) g
		         WHERE g.value IN (SELECT value FROM json_each(?))
		       ), 0) AS overlap
		FROM tv_series s
		LEFT JOIN (
		  SELECT mr3.item_id,
		         json_extract(mr3.details_json, '$.genres') AS genres_json
		  FROM metadata_records mr3
		  WHERE mr3.kind = 'series'
		    AND json_valid(mr3.details_json)
		    AND json_extract(mr3.details_json, '$.genres') IS NOT NULL
		    AND json_valid(json_extract(mr3.details_json, '$.genres'))
		  GROUP BY mr3.item_id
		) gj ON gj.item_id = s.id
		WHERE s.id != ?
		  AND EXISTS (
		    SELECT 1 FROM metadata_records mr
		    WHERE mr.kind = 'series' AND mr.item_id = s.id
		      AND json_valid(mr.details_json)
		      AND json_valid(json_extract(mr.details_json, '$.genres'))
		      AND EXISTS (
		        SELECT 1 FROM json_each(json_extract(mr.details_json, '$.genres')) g
		        WHERE g.value IN (SELECT value FROM json_each(?))
		      )
		  )
		ORDER BY overlap DESC
		LIMIT ?
	`, string(genresJSON), sourceID, string(genresJSON), limit)
	if err != nil {
		return empty, err
	}
	defer rows.Close()
	var out []SimilarItem
	for rows.Next() {
		var it SimilarItem
		var overlap int
		it.Kind = "series"
		if err := rows.Scan(&it.ID, &it.Title, &it.Year, &it.PosterURL, &overlap); err != nil {
			return empty, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return empty, err
	}
	if out == nil {
		out = []SimilarItem{}
	}
	if len(out) >= 3 {
		return SimilarSeriesResult{Items: out}, nil
	}

	// Fallback: fill remaining slots from the primary genre, ordered by
	// metadata confidence. Excludes the source series and any already returned.
	primaryGenre := src.Genres[0]
	excludeIDs := make([]string, 0, len(out)+1)
	excludeIDs = append(excludeIDs, sourceID)
	for _, it := range out {
		excludeIDs = append(excludeIDs, it.ID)
	}
	excludeJSON, err := json.Marshal(excludeIDs)
	if err != nil {
		return SimilarSeriesResult{Items: out}, nil //nolint:nilerr
	}
	needed := limit - len(out)
	// Same json_valid() guards as the primary query above — see comment there.
	// tv_series has no year column either; we pull year from metadata_records
	// the same way the primary query does.
	frows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.title,
		       COALESCE((SELECT mr4.year FROM metadata_records mr4
		                 WHERE mr4.kind = 'series' AND mr4.item_id = s.id
		                   AND mr4.year > 0
		                 ORDER BY mr4.confidence DESC, mr4.updated_at DESC
		                 LIMIT 1), 0) AS year,
		       COALESCE((SELECT mr2.poster_url FROM metadata_records mr2
		                 WHERE mr2.kind = 'series' AND mr2.item_id = s.id
		                   AND mr2.poster_url != ''
		                 ORDER BY mr2.confidence DESC, mr2.updated_at DESC
		                 LIMIT 1), '') AS poster_url
		FROM tv_series s
		WHERE s.id NOT IN (SELECT value FROM json_each(?))
		  AND EXISTS (
		    SELECT 1 FROM metadata_records mr
		    WHERE mr.kind = 'series' AND mr.item_id = s.id
		      AND json_valid(mr.details_json)
		      AND json_valid(json_extract(mr.details_json, '$.genres'))
		      AND EXISTS (
		        SELECT 1 FROM json_each(json_extract(mr.details_json, '$.genres')) g
		        WHERE g.value = ?
		      )
		  )
		ORDER BY (SELECT MAX(mr.confidence) FROM metadata_records mr
		          WHERE mr.kind = 'series' AND mr.item_id = s.id) DESC
		LIMIT ?
	`, string(excludeJSON), primaryGenre, needed)
	if err != nil {
		// Fallback query failed — return what we have without error
		return SimilarSeriesResult{Items: out}, nil //nolint:nilerr
	}
	defer frows.Close()
	for frows.Next() {
		var it SimilarItem
		it.Kind = "series"
		if err := frows.Scan(&it.ID, &it.Title, &it.Year, &it.PosterURL); err != nil {
			break
		}
		out = append(out, it)
	}
	if len(out) == 0 {
		return empty, nil
	}
	return SimilarSeriesResult{Items: out, Fallback: true, FallbackGenre: primaryGenre}, nil
}

// PersonHit is one search hit for a person discovered in metadata cast/crew.
// CreditCount is the number of library items that reference this person.
type PersonHit struct {
	Name        string `json:"name"`
	ProfileURL  string `json:"profileUrl,omitempty"`
	Department  string `json:"department,omitempty"`
	CreditCount int    `json:"creditCount"`
}

// CollectionHit is one search hit for a TMDB collection referenced by movies
// in the library. MovieCount is the number of movies in the collection.
type CollectionHit struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	PosterURL   string `json:"posterUrl,omitempty"`
	BackdropURL string `json:"backdropUrl,omitempty"`
	LogoURL     string `json:"logoUrl,omitempty"`
	MovieCount  int    `json:"movieCount"`
}

// SearchResults is the aggregated payload returned by SearchLibrary.
type SearchResults struct {
	Query       string            `json:"query"`
	Movies      []MovieListItem   `json:"movies"`
	Series      []SeriesListItem  `json:"series"`
	People      []PersonHit       `json:"people"`
	Collections []CollectionHit   `json:"collections"`
}

// scoreSubstring returns 100 for an exact match, 80 for a prefix match, 60 for
// any word-prefix match, 40 for a substring match, and 0 otherwise.
// Both inputs are expected to be lowercased.
func scoreSubstring(haystack, needle string) int {
	if haystack == "" || needle == "" {
		return 0
	}
	if haystack == needle {
		return 100
	}
	if strings.HasPrefix(haystack, needle) {
		return 80
	}
	for _, w := range strings.Fields(haystack) {
		if strings.HasPrefix(w, needle) {
			return 60
		}
	}
	if strings.Contains(haystack, needle) {
		return 40
	}
	return 0
}

// SearchLibrary returns up to `limit` matching movies, series, people, and
// collections for the given query string. It is the single backend behind the
// header search dropdown and the /search page. Limit defaults to 8 per
// category and is clamped to 40.
func (s *Service) SearchLibrary(ctx context.Context, query string, limit int, maxRating string) (SearchResults, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	result := SearchResults{Query: query, Movies: []MovieListItem{}, Series: []SeriesListItem{}, People: []PersonHit{}, Collections: []CollectionHit{}}
	if q == "" {
		return result, nil
	}
	if limit <= 0 {
		limit = 8
	}
	if limit > 40 {
		limit = 40
	}
	like := "%" + q + "%"

	// --- Movies -----------------------------------------------------------
	// Use SQL LIKE on title for an initial set; we then score in Go so the
	// ranking (exact > prefix > word-prefix > contains) is consistent across
	// types. The over-fetch (4×) gives the scorer a useful pool when the
	// catalogue has many partial hits.
	movieFetch := limit * 4
	if movieFetch < 32 {
		movieFetch = 32
	}
	movieRows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.title, m.year, m.sort_title, m.needs_review, count(mv.media_source_id) AS version_count
		FROM movies m
		LEFT JOIN movie_versions mv ON mv.movie_id = m.id
		WHERE LOWER(m.title) LIKE ?
		GROUP BY m.id
		ORDER BY m.sort_title, m.year
		LIMIT ?
	`, like, movieFetch)
	if err != nil {
		return result, err
	}
	type scoredMovie struct {
		item  MovieListItem
		score int
	}
	var movieScored []scoredMovie
	for movieRows.Next() {
		var item MovieListItem
		var needsReview int
		if err := movieRows.Scan(&item.ID, &item.Title, &item.Year, &item.SortTitle, &needsReview, &item.VersionCount); err != nil {
			movieRows.Close()
			return result, err
		}
		item.NeedsReview = needsReview != 0
		movieScored = append(movieScored, scoredMovie{item: item})
	}
	movieRows.Close()
	if err := movieRows.Err(); err != nil {
		return result, err
	}
	movieSearchIDs := make([]string, len(movieScored))
	for i := range movieScored {
		movieSearchIDs[i] = movieScored[i].item.ID
	}
	movieSearchMeta, err := s.GetBestMetadataBatch(ctx, "movie", movieSearchIDs)
	if err != nil {
		return result, err
	}
	for i := range movieScored {
		if record, ok := movieSearchMeta[movieScored[i].item.ID]; ok {
			movieScored[i].item.Metadata = &record
			applyMovieMetadata(&movieScored[i].item.Title, &movieScored[i].item.Year, &movieScored[i].item.SortTitle, record)
		}
		movieScored[i].score = scoreSubstring(strings.ToLower(movieScored[i].item.Title), q)
	}
	sort.SliceStable(movieScored, func(i, j int) bool {
		if movieScored[i].score != movieScored[j].score {
			return movieScored[i].score > movieScored[j].score
		}
		return movieScored[i].item.Title < movieScored[j].item.Title
	})
	for i := 0; i < len(movieScored) && len(result.Movies) < limit; i++ {
		if movieScored[i].score == 0 {
			break
		}
		cr := ""
		if movieScored[i].item.Metadata != nil {
			cr = movieScored[i].item.Metadata.ContentRating
		}
		if !withinCeiling(cr, maxRating) {
			continue
		}
		result.Movies = append(result.Movies, movieScored[i].item)
	}

	// --- Series -----------------------------------------------------------
	seriesFetch := movieFetch
	seriesRows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.title, s.sort_title, count(DISTINCT seasons.id) AS season_count, count(DISTINCT e.id) AS episode_count
		FROM tv_series s
		LEFT JOIN tv_seasons seasons ON seasons.series_id = s.id
		LEFT JOIN tv_episodes e ON e.series_id = s.id
		WHERE LOWER(s.title) LIKE ?
		GROUP BY s.id
		ORDER BY s.sort_title
		LIMIT ?
	`, like, seriesFetch)
	if err != nil {
		return result, err
	}
	type scoredSeries struct {
		item  SeriesListItem
		score int
	}
	var seriesScored []scoredSeries
	for seriesRows.Next() {
		var item SeriesListItem
		if err := seriesRows.Scan(&item.ID, &item.Title, &item.SortTitle, &item.SeasonCount, &item.EpisodeCount); err != nil {
			seriesRows.Close()
			return result, err
		}
		seriesScored = append(seriesScored, scoredSeries{item: item})
	}
	seriesRows.Close()
	if err := seriesRows.Err(); err != nil {
		return result, err
	}
	seriesSearchIDs := make([]string, len(seriesScored))
	for i := range seriesScored {
		seriesSearchIDs[i] = seriesScored[i].item.ID
	}
	seriesSearchMeta, err := s.GetBestMetadataBatch(ctx, "series", seriesSearchIDs)
	if err != nil {
		return result, err
	}
	for i := range seriesScored {
		if record, ok := seriesSearchMeta[seriesScored[i].item.ID]; ok {
			seriesScored[i].item.Metadata = &record
			applyTitleMetadata(&seriesScored[i].item.Title, &seriesScored[i].item.SortTitle, record)
		}
		seriesScored[i].score = scoreSubstring(strings.ToLower(seriesScored[i].item.Title), q)
	}
	sort.SliceStable(seriesScored, func(i, j int) bool {
		if seriesScored[i].score != seriesScored[j].score {
			return seriesScored[i].score > seriesScored[j].score
		}
		return seriesScored[i].item.Title < seriesScored[j].item.Title
	})
	// Build the deduped series list (collapse common title variants),
	// preserving score ordering, applying rating ceiling when set.
	addedSeries := make(map[string]bool)
	for i := 0; i < len(seriesScored); i++ {
		if seriesScored[i].score == 0 {
			break
		}
		cr := ""
		if seriesScored[i].item.Metadata != nil {
			cr = seriesScored[i].item.Metadata.ContentRating
		}
		if !withinCeiling(cr, maxRating) {
			continue
		}
		key := strings.ToLower(seriesScored[i].item.Title)
		if addedSeries[key] {
			continue
		}
		addedSeries[key] = true
		result.Series = append(result.Series, seriesScored[i].item)
		if len(result.Series) >= limit {
			break
		}
	}

	// --- People (from materialized people + people_credits tables) -------
	// Single indexed query replaces the old json_each scan + N count queries.
	type scoredPerson struct {
		hit   PersonHit
		score int
	}
	var personScored []scoredPerson
	{
		pRows, err := s.db.QueryContext(ctx, `
			SELECT p.name, p.profile_url, p.department,
			       COUNT(DISTINCT pc.item_kind || ':' || pc.item_id) AS credit_count
			FROM people p
			JOIN people_credits pc ON pc.person_id = p.id
			WHERE p.name_lower LIKE ?
			GROUP BY p.id
			ORDER BY credit_count DESC, p.name
			LIMIT ?
		`, like, limit*4)
		if err != nil {
			return result, err
		}
		for pRows.Next() {
			var hit PersonHit
			var profileURL, department sql.NullString
			if err := pRows.Scan(&hit.Name, &profileURL, &department, &hit.CreditCount); err != nil {
				pRows.Close()
				return result, err
			}
			hit.ProfileURL = profileURL.String
			hit.Department = department.String
			personScored = append(personScored, scoredPerson{
				hit:   hit,
				score: scoreSubstring(strings.ToLower(hit.Name), q),
			})
		}
		pRows.Close()
		if err := pRows.Err(); err != nil {
			return result, err
		}
	}
	sort.SliceStable(personScored, func(i, j int) bool {
		if personScored[i].score != personScored[j].score {
			return personScored[i].score > personScored[j].score
		}
		return personScored[i].hit.Name < personScored[j].hit.Name
	})
	for _, sp := range personScored {
		if sp.score == 0 {
			break
		}
		result.People = append(result.People, sp.hit)
		if len(result.People) >= limit {
			break
		}
	}

	// --- Collections (from movie metadata $.collection) -------------------
	collectionsAcc := map[string]*CollectionHit{}
	collRows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT
			json_extract(mr.details_json, '$.collection.id') AS coll_id,
			json_extract(mr.details_json, '$.collection.name') AS coll_name,
			json_extract(mr.details_json, '$.collection.posterUrl') AS poster,
			json_extract(mr.details_json, '$.collection.backdropUrl') AS backdrop,
			json_extract(mr.details_json, '$.collection.logoUrl') AS logo
		FROM metadata_records mr
		WHERE mr.kind = 'movie'
		  AND json_extract(mr.details_json, '$.collection.id') IS NOT NULL
		  AND LOWER(json_extract(mr.details_json, '$.collection.name')) LIKE ?
	`, like)
	if err != nil {
		return result, err
	}
	for collRows.Next() {
		var id, name, poster, backdrop, logo sql.NullString
		if err := collRows.Scan(&id, &name, &poster, &backdrop, &logo); err != nil {
			collRows.Close()
			return result, err
		}
		if !id.Valid || strings.TrimSpace(id.String) == "" || strings.TrimSpace(name.String) == "" {
			continue
		}
		key := id.String
		if entry, ok := collectionsAcc[key]; ok {
			if entry.PosterURL == "" {
				entry.PosterURL = poster.String
			}
			if entry.BackdropURL == "" {
				entry.BackdropURL = backdrop.String
			}
			if entry.LogoURL == "" {
				entry.LogoURL = logo.String
			}
			continue
		}
		collectionsAcc[key] = &CollectionHit{
			ID:          id.String,
			Name:        strings.TrimSpace(name.String),
			PosterURL:   poster.String,
			BackdropURL: backdrop.String,
			LogoURL:     logo.String,
		}
	}
	collRows.Close()
	if err := collRows.Err(); err != nil {
		return result, err
	}
	type scoredCollection struct {
		hit   *CollectionHit
		score int
	}
	collScored := make([]scoredCollection, 0, len(collectionsAcc))
	for _, c := range collectionsAcc {
		collScored = append(collScored, scoredCollection{hit: c, score: scoreSubstring(strings.ToLower(c.Name), q)})
	}
	sort.SliceStable(collScored, func(i, j int) bool {
		if collScored[i].score != collScored[j].score {
			return collScored[i].score > collScored[j].score
		}
		return collScored[i].hit.Name < collScored[j].hit.Name
	})
	collCap := limit * 2
	if collCap > len(collScored) {
		collCap = len(collScored)
	}
	for i := 0; i < collCap; i++ {
		if collScored[i].score == 0 {
			break
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT m.id)
			FROM movies m
			JOIN metadata_records mr ON mr.kind = 'movie' AND mr.item_id = m.id
			WHERE json_extract(mr.details_json, '$.collection.id') = ?
		`, collScored[i].hit.ID).Scan(&n); err != nil {
			return result, err
		}
		collScored[i].hit.MovieCount = n
		if n > 0 {
			result.Collections = append(result.Collections, *collScored[i].hit)
			if len(result.Collections) >= limit {
				break
			}
		}
	}

	return result, nil
}

func (s *Service) GetMovie(ctx context.Context, id string) (MovieDetail, bool, error) {
	var detail MovieDetail
	var needsReview int
	err := s.db.QueryRowContext(ctx, `
		SELECT m.id, m.title, m.year, m.sort_title, m.needs_review, count(mv.media_source_id) AS version_count
		FROM movies m
		LEFT JOIN movie_versions mv ON mv.movie_id = m.id
		WHERE m.id = ?
		GROUP BY m.id
	`, id).Scan(&detail.ID, &detail.Title, &detail.Year, &detail.SortTitle, &needsReview, &detail.VersionCount)
	if errors.Is(err, sql.ErrNoRows) {
		return MovieDetail{}, false, nil
	}
	if err != nil {
		return MovieDetail{}, false, err
	}
	detail.NeedsReview = needsReview != 0
	if record, ok, err := s.GetBestMetadata(ctx, "movie", id); err != nil {
		return MovieDetail{}, false, err
	} else if ok {
		detail.Metadata = &record
		applyMovieMetadata(&detail.Title, &detail.Year, &detail.SortTitle, record)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT ms.id, ms.path, ms.rel_path, mv.edition, mv.quality_label, ms.size_bytes, ms.modified_at
		FROM movie_versions mv
		JOIN media_sources ms ON ms.id = mv.media_source_id
		WHERE mv.movie_id = ?
		ORDER BY mv.quality_label DESC, ms.rel_path
	`, id)
	if err != nil {
		return MovieDetail{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var version MovieVersion
		if err := rows.Scan(&version.MediaSourceID, &version.Path, &version.RelPath, &version.Edition, &version.QualityLabel, &version.SizeBytes, &version.ModifiedAt); err != nil {
			return MovieDetail{}, false, err
		}
		detail.Versions = append(detail.Versions, version)
	}
	return detail, true, rows.Err()
}

func (s *Service) ListSeries(ctx context.Context, limit int, maxRating string, userID string) ([]SeriesListItem, error) {
	// limit <= 0 means "return everything". A positive limit is honoured as-is.
	sqlLimit := limit
	if sqlLimit <= 0 {
		sqlLimit = -1 // SQLite: LIMIT -1 = no limit
	} else if maxRating != "" {
		sqlLimit = limit * 10
	}
	// Snapshot read from tv_series_list_view (migration 0003). Same pattern
	// as ListMovies — the old query joined the entire series tree (seasons +
	// episodes + episode_versions + playback_states) with GROUP BY before
	// LIMIT could clip it. The watched flag stays per-user and is computed via
	// a correlated subquery only for the LIMIT-clipped rows.
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.series_id, v.title, v.sort_title,
		       v.season_count, v.episode_count, v.created_at,
		       COALESCE((SELECT MAX(CASE WHEN ps.watched != 0 THEN 1 ELSE 0 END)
		                 FROM tv_episodes e
		                 JOIN episode_versions ev ON ev.episode_id = e.id
		                 LEFT JOIN playback_states ps
		                     ON ps.media_source_id = ev.media_source_id AND ps.user_id = ?
		                 WHERE e.series_id = v.series_id), 0) AS is_watched
		FROM tv_series_list_view v
		ORDER BY v.sort_title, v.series_id
		LIMIT ?
	`, userID, sqlLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type seriesStub struct {
		id, title, sortTitle, addedAt string
		seasonCount, episodeCount     int
		isWatched                     int
	}
	var stubs []seriesStub
	for rows.Next() {
		var st seriesStub
		if err := rows.Scan(&st.id, &st.title, &st.sortTitle, &st.seasonCount, &st.episodeCount, &st.addedAt, &st.isWatched); err != nil {
			return nil, err
		}
		stubs = append(stubs, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ids := make([]string, len(stubs))
	for i, st := range stubs {
		ids[i] = st.id
	}
	metaMap, err := s.GetBestMetadataBatch(ctx, "series", ids)
	if err != nil {
		return nil, err
	}

	raw := make([]SeriesListItem, 0, len(stubs))
	for _, st := range stubs {
		item := SeriesListItem{
			ID:           st.id,
			Title:        st.title,
			SortTitle:    st.sortTitle,
			SeasonCount:  st.seasonCount,
			EpisodeCount: st.episodeCount,
			AddedAt:      st.addedAt,
			Watched:      st.isWatched != 0,
		}
		if record, ok := metaMap[st.id]; ok {
			item.Metadata = &record
			applyTitleMetadata(&item.Title, &item.SortTitle, record)
		}
		raw = append(raw, item)
	}

	collapsed := collapseSeriesListItems(raw)
	if maxRating == "" {
		if limit > 0 && len(collapsed) > limit {
			return collapsed[:limit], nil
		}
		return collapsed, nil
	}
	output := make([]SeriesListItem, 0, len(collapsed))
	for _, item := range collapsed {
		cr := ""
		if item.Metadata != nil {
			cr = item.Metadata.ContentRating
		}
		if withinCeiling(cr, maxRating) {
			output = append(output, item)
			if limit > 0 && len(output) >= limit {
				break
			}
		}
	}
	return output, nil
}

func (s *Service) GetSeries(ctx context.Context, id string) (SeriesDetail, bool, error) {
	var detail SeriesDetail
	err := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.title, s.sort_title, count(DISTINCT seasons.id) AS season_count, count(DISTINCT e.id) AS episode_count
		FROM tv_series s
		LEFT JOIN tv_seasons seasons ON seasons.series_id = s.id
		LEFT JOIN tv_episodes e ON e.series_id = s.id
		WHERE s.id = ?
		GROUP BY s.id
	`, id).Scan(&detail.ID, &detail.Title, &detail.SortTitle, &detail.SeasonCount, &detail.EpisodeCount)
	if errors.Is(err, sql.ErrNoRows) {
		return SeriesDetail{}, false, nil
	}
	if err != nil {
		return SeriesDetail{}, false, err
	}
	if record, ok, err := s.GetBestMetadata(ctx, "series", id); err != nil {
		return SeriesDetail{}, false, err
	} else if ok {
		detail.Metadata = &record
		applyTitleMetadata(&detail.Title, &detail.SortTitle, record)
	}
	memberIDs, err := s.seriesAggregateMemberIDs(ctx, id, detail.Metadata)
	if err != nil {
		return SeriesDetail{}, false, err
	}
	if len(memberIDs) > 1 {
		if aggregate, err := s.seriesAggregateSummary(ctx, memberIDs); err != nil {
			return SeriesDetail{}, false, err
		} else {
			detail.SeasonCount = aggregate.SeasonCount
			detail.EpisodeCount = aggregate.EpisodeCount
			if aggregate.Metadata != nil {
				detail.Metadata = aggregate.Metadata
				applyTitleMetadata(&detail.Title, &detail.SortTitle, *aggregate.Metadata)
			} else {
				detail.Title = aggregate.Title
				detail.SortTitle = aggregate.SortTitle
			}
			detail.ID = aggregate.ID
		}
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT seasons.id, seasons.season_number, e.id, e.season_number, e.episode_number, e.episode_end, e.title, e.needs_review, count(ev.media_source_id) AS version_count
		FROM tv_seasons seasons
		LEFT JOIN tv_episodes e ON e.season_id = seasons.id
		LEFT JOIN episode_versions ev ON ev.episode_id = e.id
		WHERE seasons.series_id IN (`+sqlInPlaceholders(len(memberIDs))+`)
		GROUP BY seasons.id, e.id
		ORDER BY seasons.season_number, e.episode_number, e.id
	`, stringArgs(memberIDs)...)
	if err != nil {
		return SeriesDetail{}, false, err
	}
	defer rows.Close()

	seasonIndex := map[string]int{}
	episodeLocation := map[string][2]int{}
	for rows.Next() {
		var seasonID string
		var seasonNumber int
		var episode EpisodeBrief
		var needsReview int
		if err := rows.Scan(&seasonID, &seasonNumber, &episode.ID, &episode.SeasonNumber, &episode.EpisodeNumber, &episode.EpisodeEnd, &episode.Title, &needsReview, &episode.VersionCount); err != nil {
			return SeriesDetail{}, false, err
		}
		index, ok := seasonIndex[seasonID]
		if !ok {
			index = len(detail.Seasons)
			seasonIndex[seasonID] = index
			detail.Seasons = append(detail.Seasons, SeasonDetail{ID: seasonID, SeasonNumber: seasonNumber})
		}
		if episode.ID != "" {
			episode.NeedsReview = needsReview != 0
			detail.Seasons[index].Episodes = append(detail.Seasons[index].Episodes, episode)
			episodeLocation[episode.ID] = [2]int{index, len(detail.Seasons[index].Episodes) - 1}
		}
	}
	if err := rows.Err(); err != nil {
		return SeriesDetail{}, false, err
	}

	versionRows, err := s.db.QueryContext(ctx, `
		SELECT ev.episode_id, ms.id, ms.path, ms.rel_path, '' AS edition, ev.quality_label, ms.size_bytes, ms.modified_at
		FROM episode_versions ev
		JOIN media_sources ms ON ms.id = ev.media_source_id
		JOIN tv_episodes e ON e.id = ev.episode_id
		WHERE e.series_id IN (`+sqlInPlaceholders(len(memberIDs))+`)
		ORDER BY e.season_number, e.episode_number, ev.quality_label DESC
	`, stringArgs(memberIDs)...)
	if err != nil {
		return SeriesDetail{}, false, err
	}
	defer versionRows.Close()
	for versionRows.Next() {
		var episodeID string
		var version MovieVersion
		if err := versionRows.Scan(&episodeID, &version.MediaSourceID, &version.Path, &version.RelPath, &version.Edition, &version.QualityLabel, &version.SizeBytes, &version.ModifiedAt); err != nil {
			return SeriesDetail{}, false, err
		}
		location, ok := episodeLocation[episodeID]
		if ok {
			episode := &detail.Seasons[location[0]].Episodes[location[1]]
			episode.Versions = append(episode.Versions, version)
		}
	}
	if err := versionRows.Err(); err != nil {
		return SeriesDetail{}, false, err
	}
	seasonIDs := make([]string, 0, len(detail.Seasons))
	episodeIDs := make([]string, 0, len(episodeLocation))
	for i := range detail.Seasons {
		seasonIDs = append(seasonIDs, detail.Seasons[i].ID)
		for j := range detail.Seasons[i].Episodes {
			episodeIDs = append(episodeIDs, detail.Seasons[i].Episodes[j].ID)
		}
	}
	seasonMeta, err := s.GetBestMetadataBatch(ctx, "season", seasonIDs)
	if err != nil {
		return SeriesDetail{}, false, err
	}
	episodeMeta, err := s.GetBestMetadataBatch(ctx, "episode", episodeIDs)
	if err != nil {
		return SeriesDetail{}, false, err
	}
	for seasonIndex := range detail.Seasons {
		season := &detail.Seasons[seasonIndex]
		if record, ok := seasonMeta[season.ID]; ok {
			season.Metadata = &record
		}
		for episodeIndex := range season.Episodes {
			episode := &season.Episodes[episodeIndex]
			if record, ok := episodeMeta[episode.ID]; ok {
				episode.Metadata = &record
				if strings.TrimSpace(episode.Title) == "" {
					episode.Title = record.Title
				}
			}
		}
	}
	return detail, true, nil
}

func (s *Service) ReviewItems(ctx context.Context, limit int) ([]ReviewItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT 'movie' AS kind, id, title, review_reason
		FROM movies
		WHERE needs_review = 1
		UNION ALL
		SELECT 'episode' AS kind, id, title, review_reason
		FROM tv_episodes
		WHERE needs_review = 1
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	output := []ReviewItem{}
	for rows.Next() {
		var item ReviewItem
		if err := rows.Scan(&item.Kind, &item.ID, &item.Title, &item.ReviewReason); err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	return output, rows.Err()
}

func collapseSeriesListItems(items []SeriesListItem) []SeriesListItem {
	if len(items) <= 1 {
		return items
	}
	output := make([]SeriesListItem, 0, len(items))
	indexByKey := map[string]int{}
	for _, item := range items {
		key := seriesIdentityKey(item.Metadata, item.ID)
		index, ok := indexByKey[key]
		if !ok {
			indexByKey[key] = len(output)
			output = append(output, item)
			continue
		}
		existing := &output[index]
		existing.SeasonCount += item.SeasonCount
		existing.EpisodeCount += item.EpisodeCount
		if shouldPreferSeriesItem(item, *existing) {
			existing.ID = item.ID
			existing.Title = item.Title
			existing.SortTitle = item.SortTitle
			existing.Metadata = item.Metadata
		}
	}
	sort.SliceStable(output, func(i, j int) bool {
		if output[i].SortTitle != output[j].SortTitle {
			return output[i].SortTitle < output[j].SortTitle
		}
		return output[i].ID < output[j].ID
	})
	return output
}

func shouldPreferSeriesItem(candidate SeriesListItem, current SeriesListItem) bool {
	switch {
	case current.Metadata == nil && candidate.Metadata != nil:
		return true
	case current.Metadata != nil && candidate.Metadata == nil:
		return false
	case candidate.EpisodeCount != current.EpisodeCount:
		return candidate.EpisodeCount > current.EpisodeCount
	case candidate.SeasonCount != current.SeasonCount:
		return candidate.SeasonCount > current.SeasonCount
	case strings.TrimSpace(candidate.Title) != strings.TrimSpace(current.Title):
		return strings.Compare(candidate.Title, current.Title) < 0
	default:
		return candidate.ID < current.ID
	}
}

func seriesIdentityKey(record *MetadataRecord, fallbackID string) string {
	if record != nil {
		for _, provider := range []string{"tmdb", "tvdb", "imdb"} {
			if value := strings.TrimSpace(record.ExternalIDs[provider]); value != "" {
				return provider + ":" + value
			}
		}
	}
	return "series:" + strings.TrimSpace(fallbackID)
}

func (s *Service) seriesAggregateMemberIDs(ctx context.Context, id string, record *MetadataRecord) ([]string, error) {
	key := seriesIdentityKey(record, id)
	if !strings.Contains(key, ":") || strings.HasPrefix(key, "series:") {
		return []string{id}, nil
	}
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return []string{id}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT item_id
		FROM metadata_external_ids
		WHERE kind = 'series' AND provider = ? AND external_id = ?
		ORDER BY item_id
	`, parts[0], parts[1])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	memberIDs := []string{}
	seen := map[string]struct{}{}
	for rows.Next() {
		var itemID string
		if err := rows.Scan(&itemID); err != nil {
			return nil, err
		}
		itemID = strings.TrimSpace(itemID)
		if itemID == "" {
			continue
		}
		if _, ok := seen[itemID]; ok {
			continue
		}
		seen[itemID] = struct{}{}
		memberIDs = append(memberIDs, itemID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, ok := seen[id]; !ok {
		memberIDs = append([]string{id}, memberIDs...)
	}
	if len(memberIDs) == 0 {
		return []string{id}, nil
	}
	return memberIDs, nil
}

func (s *Service) seriesAggregateSummary(ctx context.Context, memberIDs []string) (SeriesListItem, error) {
	if len(memberIDs) == 0 {
		return SeriesListItem{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.title, s.sort_title, count(DISTINCT seasons.id) AS season_count, count(DISTINCT e.id) AS episode_count
		FROM tv_series s
		LEFT JOIN tv_seasons seasons ON seasons.series_id = s.id
		LEFT JOIN tv_episodes e ON e.series_id = s.id
		WHERE s.id IN (`+sqlInPlaceholders(len(memberIDs))+`)
		GROUP BY s.id
		ORDER BY s.sort_title, s.id
	`, stringArgs(memberIDs)...)
	if err != nil {
		return SeriesListItem{}, err
	}
	defer rows.Close()
	items := []SeriesListItem{}
	for rows.Next() {
		var item SeriesListItem
		if err := rows.Scan(&item.ID, &item.Title, &item.SortTitle, &item.SeasonCount, &item.EpisodeCount); err != nil {
			return SeriesListItem{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return SeriesListItem{}, err
	}
	aggIDs := make([]string, len(items))
	for i := range items {
		aggIDs[i] = items[i].ID
	}
	aggMeta, err := s.GetBestMetadataBatch(ctx, "series", aggIDs)
	if err != nil {
		return SeriesListItem{}, err
	}
	for i := range items {
		if record, ok := aggMeta[items[i].ID]; ok {
			items[i].Metadata = &record
			applyTitleMetadata(&items[i].Title, &items[i].SortTitle, record)
		}
	}
	grouped := collapseSeriesListItems(items)
	if len(grouped) == 0 {
		return SeriesListItem{}, nil
	}
	return grouped[0], nil
}

func sqlInPlaceholders(count int) string {
	if count <= 0 {
		return "?"
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func stringArgs(values []string) []any {
	args := make([]any, 0, len(values))
	for _, value := range values {
		args = append(args, value)
	}
	return args
}

func (s *Service) VersionGroups(ctx context.Context, limit int) ([]VersionGroup, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT 'movie', m.id, m.title, count(mv.media_source_id) AS version_count
		FROM movies m
		JOIN movie_versions mv ON mv.movie_id = m.id
		GROUP BY m.id
		HAVING version_count > 1
		UNION ALL
		SELECT 'episode', e.id, e.title, count(ev.media_source_id) AS version_count
		FROM tv_episodes e
		JOIN episode_versions ev ON ev.episode_id = e.id
		GROUP BY e.id
		HAVING version_count > 1
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	output := []VersionGroup{}
	for rows.Next() {
		var item VersionGroup
		if err := rows.Scan(&item.Kind, &item.ID, &item.Title, &item.VersionCount); err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	return output, rows.Err()
}

func (s *Service) ApplyMetadata(ctx context.Context, update MetadataUpdate) error {
	update.Title = strings.TrimSpace(update.Title)
	if update.Kind == "" || update.ID == "" || update.Title == "" {
		return errors.New("kind, id, and title are required")
	}
	now := timestamp(time.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	switch update.Kind {
	case "movie":
		_, err = tx.ExecContext(ctx, `
			UPDATE movies
			SET title = ?, year = ?, sort_title = ?, needs_review = ?, review_reason = '', updated_at = ?
			WHERE id = ?
		`, update.Title, update.Year, sortTitle(update.Title), boolInt(update.Review), now, update.ID)
	case "series":
		_, err = tx.ExecContext(ctx, `
			UPDATE tv_series
			SET title = ?, sort_title = ?, updated_at = ?
			WHERE id = ?
		`, update.Title, sortTitle(update.Title), now, update.ID)
	case "season":
		_, err = tx.ExecContext(ctx, `
			UPDATE tv_seasons
			SET updated_at = ?
			WHERE id = ?
		`, now, update.ID)
	case "episode":
		_, err = tx.ExecContext(ctx, `
			UPDATE tv_episodes
			SET title = ?, needs_review = ?, review_reason = '', updated_at = ?
			WHERE id = ?
		`, update.Title, boolInt(update.Review), now, update.ID)
	default:
		return errors.New("metadata kind must be movie, series, season, or episode")
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO metadata_overrides(kind, item_id, title, year, review_resolved, updated_at)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, item_id) DO UPDATE SET
			title = excluded.title,
			year = excluded.year,
			review_resolved = excluded.review_resolved,
			updated_at = excluded.updated_at
	`, update.Kind, update.ID, update.Title, update.Year, boolInt(!update.Review), now); err != nil {
		return err
	}
	provider := update.Provider
	if provider == "" {
		provider = "manual"
	}
	if err := upsertMetadataRecord(ctx, tx, MetadataRecord{
		Kind:        update.Kind,
		ItemID:      update.ID,
		Provider:    provider,
		ExternalID:  update.ExternalID,
		Title:       update.Title,
		Year:        update.Year,
		Overview:    update.Overview,
		PosterURL:   update.PosterURL,
		BackdropURL: update.BackdropURL,
		ThumbnailURL: update.ThumbnailURL,
		LogoURL:     update.LogoURL,
		BannerURL:   update.BannerURL,
		Confidence:  1,
		FetchedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetTrailerPath atomically updates the trailer_path for every provider row
// of a given (kind, item_id). Called by the trailer downloader once the
// local MP4 lands. Empty path clears the value.
func (s *Service) SetTrailerPath(ctx context.Context, kind string, itemID string, trailerPath string) error {
	switch kind {
	case "movie", "series", "season", "episode":
	default:
		return errors.New("trailer kind must be movie, series, season, or episode")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE metadata_records
		SET trailer_path = ?, updated_at = ?
		WHERE kind = ? AND item_id = ?
	`, strings.TrimSpace(trailerPath), time.Now().UTC().Format(time.RFC3339Nano), kind, itemID)
	return err
}

func (s *Service) UpsertMetadataRecord(ctx context.Context, record MetadataRecord) error {
	switch record.Kind {
	case "movie", "series", "season", "episode":
	default:
		return errors.New("metadata kind must be movie, series, season, or episode")
	}
	if strings.TrimSpace(record.ItemID) == "" || strings.TrimSpace(record.Title) == "" {
		return errors.New("metadata item id and title are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertMetadataRecord(ctx, tx, record); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) GetBestMetadata(ctx context.Context, kind string, itemID string) (MetadataRecord, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, item_id, provider, external_id, title, year, overview, poster_url, backdrop_url, thumbnail_url, logo_url, banner_url, video_key, trailer_path, artwork_json, details_json, confidence, raw_json, fetched_at, updated_at
		FROM metadata_records
		WHERE kind = ? AND item_id = ?
		ORDER BY updated_at DESC
	`, kind, itemID)
	if err != nil {
		return MetadataRecord{}, false, err
	}
	defer rows.Close()
	records, err := scanMetadataRecords(rows)
	if err != nil {
		return MetadataRecord{}, false, err
	}
	if len(records) == 0 {
		return MetadataRecord{}, false, nil
	}
	merged := s.mergeMetadataRecords(ctx, kind, itemID, records)
	if err := s.AttachMetadataSignals(ctx, &merged); err != nil {
		return MetadataRecord{}, false, err
	}
	merged.MetadataStatus = s.metadataStatusForRecord(ctx, merged, records)
	return merged, true, nil
}

// GetBestMetadataBatch fetches and merges metadata for multiple items of the same
// kind in two database round-trips instead of 4N. Returns a map from item_id →
// merged MetadataRecord; items with no metadata are absent from the map.
func (s *Service) GetBestMetadataBatch(ctx context.Context, kind string, ids []string) (map[string]MetadataRecord, error) {
	if len(ids) == 0 {
		return map[string]MetadataRecord{}, nil
	}

	// Build "?,?,…" placeholder
	ph := strings.Repeat("?,", len(ids))
	ph = ph[:len(ph)-1]
	args := make([]any, 0, 1+len(ids))
	args = append(args, kind)
	for _, id := range ids {
		args = append(args, id)
	}

	// ── 1 query: all metadata records (without raw_json / artwork_json) ─────
	// raw_json is the full TMDB API response (often 20-50 KB per row) and
	// artwork_json mirrors data we already have in dedicated columns. Neither
	// is exposed on the wire — both fields are `json:"-"` on MetadataRecord —
	// but reading them for every row of a list response added ~2 s to a
	// 4000-item /api/movies call (425 MB DB, ~120 MB of TEXT walked then
	// discarded). The trade-off: trailerVideoKey()'s fallback parse of
	// raw_json no longer fires on list responses; rows missing the
	// video_key column won't surface a trailer in /api/client/home or the
	// search hero band. Detail endpoints still go through the full
	// scanMetadataRecords path, so per-item trailer lookups are unaffected.
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, item_id, provider, external_id, title, year, overview,
		       poster_url, backdrop_url, thumbnail_url, logo_url, banner_url,
		       video_key, trailer_path, details_json, confidence,
		       fetched_at, updated_at
		FROM metadata_records
		WHERE kind = ? AND item_id IN (`+ph+`)
		ORDER BY item_id, updated_at DESC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	allRecords, err := scanMetadataRecordsLite(rows)
	if err != nil {
		return nil, err
	}

	// ── 1 query: all ratings ─────────────────────────────────────────────────
	ratingsByID := make(map[string][]Rating, len(ids))
	ratingRows, err := s.db.QueryContext(ctx, `
		SELECT kind, item_id, provider, rating_type, value, display_value, scale,
		       votes, source_url, fetched_at, updated_at
		FROM metadata_ratings
		WHERE kind = ? AND item_id IN (`+ph+`)
		ORDER BY item_id,
		         CASE rating_type
		           WHEN 'imdb' THEN 0
		           WHEN 'rottenTomatoesCritics' THEN 1
		           WHEN 'rottenTomatoesAudience' THEN 2
		           WHEN 'tmdb' THEN 3
		           WHEN 'metacritic' THEN 4
		           WHEN 'tvdb' THEN 5
		           ELSE 9 END, provider
	`, args...)
	if err == nil {
		defer ratingRows.Close()
		for ratingRows.Next() {
			var r Rating
			if scanErr := ratingRows.Scan(
				&r.Kind, &r.ItemID, &r.Provider, &r.RatingType,
				&r.Value, &r.DisplayValue, &r.Scale, &r.Votes,
				&r.SourceURL, &r.FetchedAt, &r.UpdatedAt,
			); scanErr == nil {
				ratingsByID[r.ItemID] = append(ratingsByID[r.ItemID], r)
			}
		}
	}

	// ── 1 query: all external IDs (needed for series deduplication) ──────────
	externalIDsByItemID := make(map[string]map[string]string, len(ids))
	extRows, extErr := s.db.QueryContext(ctx, `
		SELECT item_id, provider, external_id
		FROM metadata_external_ids
		WHERE kind = ? AND item_id IN (`+ph+`)
		ORDER BY item_id, provider
	`, args...)
	if extErr == nil {
		defer extRows.Close()
		for extRows.Next() {
			var extItemID, extProvider, extID string
			if scanErr := extRows.Scan(&extItemID, &extProvider, &extID); scanErr == nil {
				if externalIDsByItemID[extItemID] == nil {
					externalIDsByItemID[extItemID] = map[string]string{}
				}
				externalIDsByItemID[extItemID][extProvider] = extID
			}
		}
	}

	// ── 1 library lookup: shared by all items of same kind ───────────────────
	metaOrder := metasources.DefaultSourceOrder(string(metadataLibraryKind(kind)))
	artworkOrder := metasources.DefaultArtworkOrder(string(metadataLibraryKind(kind)))
	for _, id := range ids {
		if lib, ok, libErr := s.GetLibraryForItem(ctx, kind, id); libErr == nil && ok {
			if order := metasources.NormalizeRequestedSourceOrder(string(lib.Kind), lib.MetadataSources); len(order) > 0 {
				metaOrder = order
			}
			if order := metasources.NormalizeRequestedArtworkOrder(string(lib.Kind), lib.ArtworkSources); len(order) > 0 {
				artworkOrder = order
			}
			break // all items of the same kind share the same library
		}
	}

	// ── Group records by item_id and merge ───────────────────────────────────
	byID := make(map[string][]MetadataRecord, len(ids))
	for _, r := range allRecords {
		byID[r.ItemID] = append(byID[r.ItemID], r)
	}

	result := make(map[string]MetadataRecord, len(byID))
	for itemID, records := range byID {
		merged := s.mergeMetadataRecordsOrdered(kind, itemID, records, metaOrder, artworkOrder)
		merged.Ratings = RatingMap(ratingsByID[itemID])
		merged.MetadataStatus = s.metadataStatusForRecord(ctx, merged, records)
		// Attach external IDs so callers can group/deduplicate by shared identity.
		if extMap := externalIDsByItemID[itemID]; extMap != nil {
			merged.ExternalIDs = extMap
			if merged.Provenance.ExternalIDs == nil {
				merged.Provenance.ExternalIDs = map[string]string{}
			}
			for provider := range extMap {
				merged.Provenance.ExternalIDs[provider] = provider
			}
		}
		result[itemID] = merged
	}
	return result, nil
}

func (s *Service) mergeMetadataRecords(ctx context.Context, kind string, itemID string, records []MetadataRecord) MetadataRecord {
	return s.mergeMetadataRecordsOrdered(
		kind, itemID, records,
		s.metadataOrderForItem(ctx, kind, itemID),
		s.artworkOrderForItem(ctx, kind, itemID),
	)
}

// mergeMetadataRecordsOrdered is like mergeMetadataRecords but uses
// caller-supplied source orders instead of per-item DB lookups — used by
// GetBestMetadataBatch to avoid N×2 library-lookup queries.
func (s *Service) mergeMetadataRecordsOrdered(kind, itemID string, records []MetadataRecord, metaOrder, artworkOrder []string) MetadataRecord {
	if len(records) == 0 {
		return MetadataRecord{}
	}
	metadataOrdered := append([]MetadataRecord(nil), records...)
	sortMetadataRecords(metadataOrdered, metaOrder)
	artworkOrdered := append([]MetadataRecord(nil), records...)
	sortMetadataRecords(artworkOrdered, artworkOrder)
	merged := MetadataRecord{
		Kind:       kind,
		ItemID:     itemID,
		Provider:   metadataOrdered[0].Provider,
		Confidence: metadataOrdered[0].Confidence,
		FetchedAt:  metadataOrdered[0].FetchedAt,
		UpdatedAt:  metadataOrdered[0].UpdatedAt,
		Provenance: MetadataProvenance{
			Fields:      map[string]string{},
			Ratings:     map[string]string{},
			ExternalIDs: map[string]string{},
		},
	}
	for _, record := range metadataOrdered {
		mergeMetadataFields(&merged, record)
	}
	for _, record := range artworkOrdered {
		mergeArtworkFields(&merged, record)
	}
	if merged.ExternalID == "" {
		for _, record := range metadataOrdered {
			if strings.TrimSpace(record.ExternalID) != "" {
				merged.ExternalID = strings.TrimSpace(record.ExternalID)
				break
			}
		}
	}
	if merged.Provider == "" {
		for _, record := range metadataOrdered {
			if strings.TrimSpace(record.Provider) != "" {
				merged.Provider = strings.TrimSpace(record.Provider)
				break
			}
		}
	}
	return merged
}

func mergeMetadataFields(target *MetadataRecord, source MetadataRecord) {
	if target == nil {
		return
	}
	assignStringField(&target.Title, source.Title, "title", source.Provider, &target.Provenance)
	assignIntField(&target.Year, source.Year, "year", source.Provider, &target.Provenance)
	assignStringField(&target.Overview, source.Overview, "overview", source.Provider, &target.Provenance)
	assignStringField(&target.OriginalTitle, source.OriginalTitle, "originalTitle", source.Provider, &target.Provenance)
	assignStringField(&target.ReleaseDate, source.ReleaseDate, "releaseDate", source.Provider, &target.Provenance)
	assignStringField(&target.FirstAirDate, source.FirstAirDate, "firstAirDate", source.Provider, &target.Provenance)
	assignStringField(&target.AirDate, source.AirDate, "airDate", source.Provider, &target.Provenance)
	assignIntField(&target.RuntimeMinutes, source.RuntimeMinutes, "runtimeMinutes", source.Provider, &target.Provenance)
	assignStringsField(&target.Genres, source.Genres, "genres", source.Provider, &target.Provenance)
	assignStringField(&target.ContentRating, source.ContentRating, "contentRating", source.Provider, &target.Provenance)
	assignCreditsField(&target.Cast, source.Cast, "cast", source.Provider, &target.Provenance)
	assignCreditsField(&target.Crew, source.Crew, "crew", source.Provider, &target.Provenance)
	assignCreditsField(&target.GuestCast, source.GuestCast, "guestCast", source.Provider, &target.Provenance)
	assignStringsField(&target.Directors, source.Directors, "directors", source.Provider, &target.Provenance)
	assignStringsField(&target.Writers, source.Writers, "writers", source.Provider, &target.Provenance)
	assignStringsField(&target.Studios, source.Studios, "studios", source.Provider, &target.Provenance)
	assignStringsField(&target.ProductionCompanies, source.ProductionCompanies, "productionCompanies", source.Provider, &target.Provenance)
	assignStringsField(&target.Networks, source.Networks, "networks", source.Provider, &target.Provenance)
	assignStringsField(&target.Country, source.Country, "country", source.Provider, &target.Provenance)
	assignStringsField(&target.Language, source.Language, "language", source.Provider, &target.Provenance)
	assignStringField(&target.StatusText, source.StatusText, "statusText", source.Provider, &target.Provenance)
	assignCollectionField(&target.Collection, source.Collection, "collection", source.Provider, &target.Provenance)
	assignIntField(&target.SeasonNumber, source.SeasonNumber, "seasonNumber", source.Provider, &target.Provenance)
	assignIntField(&target.EpisodeNumber, source.EpisodeNumber, "episodeNumber", source.Provider, &target.Provenance)
	assignIntField(&target.EpisodeCount, source.EpisodeCount, "episodeCount", source.Provider, &target.Provenance)
}

func mergeArtworkFields(target *MetadataRecord, source MetadataRecord) {
	if target == nil {
		return
	}
	assignStringField(&target.PosterURL, source.PosterURL, "poster", source.Provider, &target.Provenance)
	assignStringField(&target.BackdropURL, source.BackdropURL, "backdrop", source.Provider, &target.Provenance)
	assignStringField(&target.ThumbnailURL, source.ThumbnailURL, "thumbnail", source.Provider, &target.Provenance)
	assignStringField(&target.LogoURL, source.LogoURL, "logo", source.Provider, &target.Provenance)
	assignStringField(&target.BannerURL, source.BannerURL, "banner", source.Provider, &target.Provenance)
	assignStringField(&target.VideoKey, source.VideoKey, "videoKey", source.Provider, &target.Provenance)
	assignStringField(&target.TrailerPath, source.TrailerPath, "trailerPath", source.Provider, &target.Provenance)
}

func assignStringField(target *string, source string, field string, provider string, provenance *MetadataProvenance) {
	if target == nil || strings.TrimSpace(*target) != "" || strings.TrimSpace(source) == "" {
		return
	}
	*target = strings.TrimSpace(source)
	if provenance != nil {
		if provenance.Fields == nil {
			provenance.Fields = map[string]string{}
		}
		provenance.Fields[field] = provider
	}
}

func assignIntField(target *int, source int, field string, provider string, provenance *MetadataProvenance) {
	if target == nil || *target > 0 || source <= 0 {
		return
	}
	*target = source
	if provenance != nil {
		if provenance.Fields == nil {
			provenance.Fields = map[string]string{}
		}
		provenance.Fields[field] = provider
	}
}

func assignStringsField(target *[]string, source []string, field string, provider string, provenance *MetadataProvenance) {
	if target == nil || len(*target) > 0 {
		return
	}
	next := compactStrings(source)
	if len(next) == 0 {
		return
	}
	*target = next
	if provenance != nil {
		if provenance.Fields == nil {
			provenance.Fields = map[string]string{}
		}
		provenance.Fields[field] = provider
	}
}

func assignCreditsField(target *[]MetadataCredit, source []MetadataCredit, field string, provider string, provenance *MetadataProvenance) {
	if target == nil || len(*target) > 0 || len(source) == 0 {
		return
	}
	*target = append([]MetadataCredit(nil), source...)
	if provenance != nil {
		if provenance.Fields == nil {
			provenance.Fields = map[string]string{}
		}
		provenance.Fields[field] = provider
	}
}

func assignCollectionField(target **MetadataCollection, source *MetadataCollection, field string, provider string, provenance *MetadataProvenance) {
	if target == nil || *target != nil || source == nil {
		return
	}
	copy := *source
	*target = &copy
	if provenance != nil {
		if provenance.Fields == nil {
			provenance.Fields = map[string]string{}
		}
		provenance.Fields[field] = provider
	}
}

func (s *Service) metadataStatusForRecord(ctx context.Context, best MetadataRecord, records []MetadataRecord) MetadataStatus {
	states := []string{}
	missingFields := []string{}
	missingArtwork := []string{}
	hasMatch := false
	userOverrideApplied := false
	for _, record := range records {
		provider := strings.ToLower(strings.TrimSpace(record.Provider))
		if provider == "manual" {
			userOverrideApplied = true
		}
		if provider != "" && provider != "filename" && provider != "artwork" {
			hasMatch = true
		}
	}
	if strings.TrimSpace(best.Title) == "" {
		missingFields = append(missingFields, "title")
	}
	if strings.TrimSpace(best.Overview) == "" {
		missingFields = append(missingFields, "overview")
	}
	if len(best.Genres) == 0 {
		missingFields = append(missingFields, "genres")
	}
	if strings.TrimSpace(best.PosterURL) == "" {
		missingArtwork = append(missingArtwork, "poster")
	}
	if strings.TrimSpace(best.BackdropURL) == "" {
		missingArtwork = append(missingArtwork, "backdrop")
	}
	primary := "metadata_ready"
	switch {
	case !hasMatch:
		primary = "match_failed"
	case len(missingFields) > 0 || len(missingArtwork) > 0:
		primary = "partial_metadata"
	}
	states = append(states, primary)
	if len(missingArtwork) > 0 {
		for _, item := range missingArtwork {
			states = append(states, "missing_"+item)
		}
	}
	if userOverrideApplied {
		states = append(states, "user_override_applied")
	}
	return MetadataStatus{
		Primary:             primary,
		States:              compactStrings(states),
		MissingFields:       compactStrings(missingFields),
		MissingArtwork:      compactStrings(missingArtwork),
		Matched:             hasMatch,
		UserOverrideApplied: userOverrideApplied,
	}
}

func (s *Service) artworkOrderForItem(ctx context.Context, kind string, itemID string) []string {
	libraryKind := metadataLibraryKind(kind)
	if library, ok, err := s.GetLibraryForItem(ctx, kind, itemID); err == nil && ok {
		if order := metasources.NormalizeRequestedArtworkOrder(string(libraryKind), library.ArtworkSources); len(order) > 0 {
			return order
		}
	}
	return metasources.DefaultArtworkOrder(string(libraryKind))
}

func metadataLibraryKind(kind string) libraries.Kind {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "series", "season", "episode", "tv":
		return libraries.KindTV
	default:
		return libraries.KindMovies
	}
}

func (s *Service) metadataOrderForItem(ctx context.Context, kind string, itemID string) []string {
	libraryKind := metadataLibraryKind(kind)
	if library, ok, err := s.GetLibraryForItem(ctx, kind, itemID); err == nil && ok {
		if order := metasources.NormalizeRequestedSourceOrder(string(libraryKind), library.MetadataSources); len(order) > 0 {
			return order
		}
	}
	return metasources.DefaultSourceOrder(string(libraryKind))
}

// ListStaleDetailsRecords returns all metadata records for a given provider
// whose details_json is still the migration default ('{}'), but whose raw_json
// is non-empty and can therefore be re-processed without an API call.
func (s *Service) ListStaleDetailsRecords(ctx context.Context, provider string) ([]MetadataRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, item_id, provider, external_id, title, year, overview, poster_url, backdrop_url, thumbnail_url, logo_url, banner_url, video_key, trailer_path, artwork_json, details_json, confidence, raw_json, fetched_at, updated_at
		FROM metadata_records
		WHERE details_json = '{}' AND raw_json != '' AND provider = ?
	`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMetadataRecords(rows)
}

func (s *Service) ListMetadataRecords(ctx context.Context, kind string, itemID string) ([]MetadataRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, item_id, provider, external_id, title, year, overview, poster_url, backdrop_url, thumbnail_url, logo_url, banner_url, video_key, trailer_path, artwork_json, details_json, confidence, raw_json, fetched_at, updated_at
		FROM metadata_records
		WHERE kind = ? AND item_id = ?
		ORDER BY updated_at DESC
	`, kind, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records, err := scanMetadataRecords(rows)
	if err != nil {
		return nil, err
	}
	order := s.metadataOrderForItem(ctx, kind, itemID)
	sortMetadataRecords(records, order)
	for index := range records {
		if err := s.AttachMetadataSignals(ctx, &records[index]); err != nil {
			return nil, err
		}
	}
	return records, nil
}

func sortMetadataRecords(records []MetadataRecord, order []string) {
	rank := map[string]int{}
	for index, provider := range order {
		key := strings.ToLower(strings.TrimSpace(provider))
		if key == "" {
			continue
		}
		if _, exists := rank[key]; !exists {
			rank[key] = index
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		left := records[i]
		right := records[j]
		leftManual := metadataProviderPriority(left.Provider)
		rightManual := metadataProviderPriority(right.Provider)
		if leftManual != rightManual {
			return leftManual < rightManual
		}
		leftRank, leftRankOK := rank[strings.ToLower(strings.TrimSpace(left.Provider))]
		rightRank, rightRankOK := rank[strings.ToLower(strings.TrimSpace(right.Provider))]
		if leftRankOK || rightRankOK {
			if leftRankOK && !rightRankOK {
				return true
			}
			if !leftRankOK && rightRankOK {
				return false
			}
			if leftRank != rightRank {
				return leftRank < rightRank
			}
		}
		if absFloat(left.Confidence-right.Confidence) > 0.01 {
			return left.Confidence > right.Confidence
		}
		leftLocal := metadataLocalPreference(left.Provider)
		rightLocal := metadataLocalPreference(right.Provider)
		if leftLocal != rightLocal {
			return leftLocal < rightLocal
		}
		if left.UpdatedAt != right.UpdatedAt {
			return left.UpdatedAt > right.UpdatedAt
		}
		return strings.Compare(left.Provider, right.Provider) < 0
	})
}

func metadataProviderPriority(provider string) int {
	if strings.EqualFold(provider, "manual") {
		return 0
	}
	return 1
}

func metadataLocalPreference(provider string) int {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "manual":
		return 0
	case "nfo":
		return 1
	case "artwork":
		return 2
	case "filename":
		return 3
	case "fanart":
		return 4
	case "tvmaze":
		return 5
	case "tvdb":
		return 6
	case "tmdb":
		return 7
	case "wikipedia":
		return 8
	case "wikidata":
		return 9
	case "omdb":
		return 10
	default:
		return 11
	}
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func (s *Service) AttachMetadataSignals(ctx context.Context, record *MetadataRecord) error {
	if record == nil || record.Kind == "" || record.ItemID == "" {
		return nil
	}
	ratings, err := s.ListRatings(ctx, record.Kind, record.ItemID)
	if err != nil {
		return err
	}
	record.Ratings = RatingMap(ratings)
	externalIDs, err := s.ListExternalIDs(ctx, record.Kind, record.ItemID)
	if err != nil {
		return err
	}
	record.ExternalIDs = map[string]string{}
	record.Provenance = MetadataProvenance{
		Fields:      map[string]string{},
		Ratings:     map[string]string{},
		ExternalIDs: map[string]string{},
	}
	if record.Provider != "" {
		if strings.TrimSpace(record.Title) != "" {
			record.Provenance.Fields["title"] = record.Provider
		}
		if record.Year > 0 {
			record.Provenance.Fields["year"] = record.Provider
		}
		if strings.TrimSpace(record.Overview) != "" {
			record.Provenance.Fields["overview"] = record.Provider
		}
		if strings.TrimSpace(record.PosterURL) != "" {
			record.Provenance.Fields["poster"] = record.Provider
		}
		if strings.TrimSpace(record.BackdropURL) != "" {
			record.Provenance.Fields["backdrop"] = record.Provider
		}
	}
	for _, item := range externalIDs {
		record.ExternalIDs[item.Provider] = item.ExternalID
		record.Provenance.ExternalIDs[item.Provider] = item.Provider
	}
	for _, rating := range ratings {
		key := rating.Provider
		if rating.RatingType != "" {
			key = rating.RatingType
		}
		record.Provenance.Ratings[key] = rating.Provider
	}
	return nil
}

func RatingMap(ratings []Rating) Ratings {
	output := Ratings{}
	for _, rating := range ratings {
		key := rating.Provider
		if rating.RatingType != "" && rating.RatingType != rating.Provider {
			key = rating.RatingType
		}
		if rating.DisplayValue != "" {
			output[key] = rating.DisplayValue
		} else {
			output[key] = rating.Value
		}
	}
	return output
}

type metadataDetailsPayload struct {
	OriginalTitle       string              `json:"originalTitle,omitempty"`
	ReleaseDate         string              `json:"releaseDate,omitempty"`
	FirstAirDate        string              `json:"firstAirDate,omitempty"`
	AirDate             string              `json:"airDate,omitempty"`
	RuntimeMinutes      int                 `json:"runtimeMinutes,omitempty"`
	Genres              []string            `json:"genres,omitempty"`
	ContentRating       string              `json:"contentRating,omitempty"`
	Cast                []MetadataCredit    `json:"cast,omitempty"`
	Crew                []MetadataCredit    `json:"crew,omitempty"`
	GuestCast           []MetadataCredit    `json:"guestCast,omitempty"`
	Directors           []string            `json:"directors,omitempty"`
	Writers             []string            `json:"writers,omitempty"`
	Studios             []string            `json:"studios,omitempty"`
	ProductionCompanies []string            `json:"productionCompanies,omitempty"`
	Networks            []string            `json:"networks,omitempty"`
	Country             []string            `json:"country,omitempty"`
	Language            []string            `json:"language,omitempty"`
	StatusText          string              `json:"statusText,omitempty"`
	Collection          *MetadataCollection `json:"collection,omitempty"`
	SeasonNumber        int                 `json:"seasonNumber,omitempty"`
	EpisodeNumber       int                 `json:"episodeNumber,omitempty"`
	EpisodeCount        int                 `json:"episodeCount,omitempty"`
}

type metadataArtworkPayload struct {
	ThumbnailURL     string `json:"thumbnailUrl,omitempty"`
	LogoURL          string `json:"logoUrl,omitempty"`
	BannerURL        string `json:"bannerUrl,omitempty"`
	LandscapeURL     string `json:"landscapeUrl,omitempty"`
	ClearLogoURL     string `json:"clearLogoUrl,omitempty"`
	CollectionPosterURL   string `json:"collectionPosterUrl,omitempty"`
	CollectionBackdropURL string `json:"collectionBackdropUrl,omitempty"`
}

func metadataDetailsJSON(record MetadataRecord) string {
	payload := metadataDetailsPayload{
		OriginalTitle:       record.OriginalTitle,
		ReleaseDate:         record.ReleaseDate,
		FirstAirDate:        record.FirstAirDate,
		AirDate:             record.AirDate,
		RuntimeMinutes:      record.RuntimeMinutes,
		Genres:              compactStrings(record.Genres),
		ContentRating:       strings.TrimSpace(record.ContentRating),
		Cast:                record.Cast,
		Crew:                record.Crew,
		GuestCast:           record.GuestCast,
		Directors:           compactStrings(record.Directors),
		Writers:             compactStrings(record.Writers),
		Studios:             compactStrings(record.Studios),
		ProductionCompanies: compactStrings(record.ProductionCompanies),
		Networks:            compactStrings(record.Networks),
		Country:             compactStrings(record.Country),
		Language:            compactStrings(record.Language),
		StatusText:          strings.TrimSpace(record.StatusText),
		Collection:          record.Collection,
		SeasonNumber:        record.SeasonNumber,
		EpisodeNumber:       record.EpisodeNumber,
		EpisodeCount:        record.EpisodeCount,
	}
	raw, _ := json.Marshal(payload)
	return string(raw)
}

func metadataArtworkJSON(record MetadataRecord) string {
	payload := metadataArtworkPayload{
		ThumbnailURL:         strings.TrimSpace(record.ThumbnailURL),
		LogoURL:              strings.TrimSpace(record.LogoURL),
		BannerURL:            strings.TrimSpace(record.BannerURL),
	}
	raw, _ := json.Marshal(payload)
	return string(raw)
}

func applyMetadataDetails(record *MetadataRecord) {
	if record == nil || strings.TrimSpace(record.DetailsJSON) == "" {
		return
	}
	var payload metadataDetailsPayload
	if err := json.Unmarshal([]byte(record.DetailsJSON), &payload); err != nil {
		return
	}
	record.OriginalTitle = strings.TrimSpace(payload.OriginalTitle)
	record.ReleaseDate = strings.TrimSpace(payload.ReleaseDate)
	record.FirstAirDate = strings.TrimSpace(payload.FirstAirDate)
	record.AirDate = strings.TrimSpace(payload.AirDate)
	record.RuntimeMinutes = payload.RuntimeMinutes
	record.Genres = compactStrings(payload.Genres)
	record.ContentRating = strings.TrimSpace(payload.ContentRating)
	record.Cast = payload.Cast
	record.Crew = payload.Crew
	record.GuestCast = payload.GuestCast
	record.Directors = compactStrings(payload.Directors)
	record.Writers = compactStrings(payload.Writers)
	record.Studios = compactStrings(payload.Studios)
	record.ProductionCompanies = compactStrings(payload.ProductionCompanies)
	record.Networks = compactStrings(payload.Networks)
	record.Country = compactStrings(payload.Country)
	record.Language = compactStrings(payload.Language)
	record.StatusText = strings.TrimSpace(payload.StatusText)
	record.Collection = payload.Collection
	record.SeasonNumber = payload.SeasonNumber
	record.EpisodeNumber = payload.EpisodeNumber
	record.EpisodeCount = payload.EpisodeCount
}

func applyMetadataArtwork(record *MetadataRecord) {
	if record == nil || strings.TrimSpace(record.ArtworkJSON) == "" {
		return
	}
	var payload metadataArtworkPayload
	if err := json.Unmarshal([]byte(record.ArtworkJSON), &payload); err != nil {
		return
	}
	record.ThumbnailURL = firstNonEmptyTrimmed(record.ThumbnailURL, payload.ThumbnailURL)
	record.LogoURL = firstNonEmptyTrimmed(record.LogoURL, payload.LogoURL)
	record.BannerURL = firstNonEmptyTrimmed(record.BannerURL, payload.BannerURL)
}

func compactStrings(values []string) []string {
	output := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		output = append(output, trimmed)
	}
	return output
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func scanMetadataRecords(rows *sql.Rows) ([]MetadataRecord, error) {
	output := []MetadataRecord{}
	for rows.Next() {
		var item MetadataRecord
		if err := rows.Scan(
			&item.Kind,
			&item.ItemID,
			&item.Provider,
			&item.ExternalID,
			&item.Title,
			&item.Year,
			&item.Overview,
			&item.PosterURL,
			&item.BackdropURL,
			&item.ThumbnailURL,
			&item.LogoURL,
			&item.BannerURL,
			&item.VideoKey,
			&item.TrailerPath,
			&item.ArtworkJSON,
			&item.DetailsJSON,
			&item.Confidence,
			&item.RawJSON,
			&item.FetchedAt,
			&item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		applyMetadataArtwork(&item)
		applyMetadataDetails(&item)
		output = append(output, item)
	}
	return output, rows.Err()
}

// scanMetadataRecordsLite is the matching scan for GetBestMetadataBatch's
// list-view query — 18 columns instead of 20 (no artwork_json, no raw_json).
// ArtworkJSON / RawJSON fields stay at zero value; applyMetadataArtwork sees
// an empty string and no-ops, which is fine because the dedicated
// thumbnail_url / logo_url / banner_url columns are already populated for
// the rare items that have artwork sidecars.
func scanMetadataRecordsLite(rows *sql.Rows) ([]MetadataRecord, error) {
	output := []MetadataRecord{}
	for rows.Next() {
		var item MetadataRecord
		if err := rows.Scan(
			&item.Kind,
			&item.ItemID,
			&item.Provider,
			&item.ExternalID,
			&item.Title,
			&item.Year,
			&item.Overview,
			&item.PosterURL,
			&item.BackdropURL,
			&item.ThumbnailURL,
			&item.LogoURL,
			&item.BannerURL,
			&item.VideoKey,
			&item.TrailerPath,
			&item.DetailsJSON,
			&item.Confidence,
			&item.FetchedAt,
			&item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		applyMetadataDetails(&item)
		output = append(output, item)
	}
	return output, rows.Err()
}

func (s *Service) UpsertExternalID(ctx context.Context, item ExternalID) error {
	item.Kind = strings.TrimSpace(item.Kind)
	item.ItemID = strings.TrimSpace(item.ItemID)
	item.Provider = strings.TrimSpace(item.Provider)
	item.ExternalID = strings.TrimSpace(item.ExternalID)
	if item.Kind == "" || item.ItemID == "" || item.Provider == "" || item.ExternalID == "" {
		return errors.New("kind, item id, provider, and external id are required")
	}
	now := timestamp(time.Now())
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO metadata_external_ids(kind, item_id, provider, external_id, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(kind, item_id, provider) DO UPDATE SET
			external_id = excluded.external_id,
			updated_at = excluded.updated_at
	`, item.Kind, item.ItemID, item.Provider, item.ExternalID, now)
	return err
}

// FindByExternalID returns the catalog itemID for a given provider + external ID.
// Used to cross-reference TMDB/IMDB trending lists against the local library.
func (s *Service) FindByExternalID(ctx context.Context, kind string, provider string, externalID string) (string, bool, error) {
	var itemID string
	err := s.db.QueryRowContext(ctx, `
		SELECT item_id FROM external_ids
		WHERE kind = ? AND provider = ? AND external_id = ?
		LIMIT 1
	`, kind, provider, externalID).Scan(&itemID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return itemID, true, nil
}

func (s *Service) ListExternalIDs(ctx context.Context, kind string, itemID string) ([]ExternalID, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, item_id, provider, external_id, updated_at
		FROM metadata_external_ids
		WHERE kind = ? AND item_id = ?
		ORDER BY provider
	`, kind, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	output := []ExternalID{}
	for rows.Next() {
		var item ExternalID
		if err := rows.Scan(&item.Kind, &item.ItemID, &item.Provider, &item.ExternalID, &item.UpdatedAt); err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	return output, rows.Err()
}

func (s *Service) UpsertRatings(ctx context.Context, ratings []Rating) error {
	if len(ratings) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := timestamp(time.Now())
	for _, rating := range ratings {
		rating.Kind = strings.TrimSpace(rating.Kind)
		rating.ItemID = strings.TrimSpace(rating.ItemID)
		rating.Provider = strings.TrimSpace(rating.Provider)
		rating.RatingType = strings.TrimSpace(rating.RatingType)
		if rating.Kind == "" || rating.ItemID == "" || rating.Provider == "" || rating.RatingType == "" {
			return errors.New("rating kind, item id, provider, and type are required")
		}
		fetchedAt := rating.FetchedAt
		if fetchedAt == "" {
			fetchedAt = now
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO metadata_ratings(kind, item_id, provider, rating_type, value, display_value, scale, votes, source_url, fetched_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(kind, item_id, provider, rating_type) DO UPDATE SET
				value = excluded.value,
				display_value = excluded.display_value,
				scale = excluded.scale,
				votes = excluded.votes,
				source_url = excluded.source_url,
				fetched_at = excluded.fetched_at,
				updated_at = excluded.updated_at
		`, rating.Kind, rating.ItemID, rating.Provider, rating.RatingType, rating.Value, rating.DisplayValue, rating.Scale, rating.Votes, rating.SourceURL, fetchedAt, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) ListRatings(ctx context.Context, kind string, itemID string) ([]Rating, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, item_id, provider, rating_type, value, display_value, scale, votes, source_url, fetched_at, updated_at
		FROM metadata_ratings
		WHERE kind = ? AND item_id = ?
		ORDER BY CASE rating_type
			WHEN 'imdb' THEN 0
			WHEN 'rottenTomatoesCritics' THEN 1
			WHEN 'rottenTomatoesAudience' THEN 2
			WHEN 'tmdb' THEN 3
			WHEN 'metacritic' THEN 4
			WHEN 'tvdb' THEN 5
			ELSE 9 END, provider
	`, kind, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	output := []Rating{}
	for rows.Next() {
		var item Rating
		if err := rows.Scan(&item.Kind, &item.ItemID, &item.Provider, &item.RatingType, &item.Value, &item.DisplayValue, &item.Scale, &item.Votes, &item.SourceURL, &item.FetchedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	return output, rows.Err()
}

func (s *Service) ListMediaSources(ctx context.Context, limit int, unprobedOnly bool) ([]MediaSourceItem, error) {
	// limit=0 means return all (no LIMIT clause). Negative values fall back to 100.
	if limit < 0 {
		limit = 100
	}
	filter := ""
	if unprobedOnly {
		filter = "WHERE mp.media_source_id IS NULL"
	}
	const baseQuery = `
		SELECT ms.id, ms.library_id, ms.kind, ms.path, ms.rel_path, ms.name, ms.extension, ms.size_bytes, ms.modified_at,
			mp.container, mp.duration_seconds, mp.bitrate, mp.video_codec, mp.video_profile, mp.video_level, mp.video_bit_depth, mp.video_frame_rate, mp.pixel_format, mp.color_primaries, mp.color_transfer, mp.color_space, mp.hdr_format, mp.dovi_profile, mp.max_cll, mp.max_fall, mp.width, mp.height, mp.audio_streams, mp.subtitle_streams
		FROM media_sources ms
		LEFT JOIN media_probes mp ON mp.media_source_id = ms.id
		`
	var rows *sql.Rows
	var err error
	if limit == 0 {
		rows, err = s.db.QueryContext(ctx, baseQuery+filter+` ORDER BY ms.rel_path`)
	} else {
		rows, err = s.db.QueryContext(ctx, baseQuery+filter+` ORDER BY ms.rel_path LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMediaSources(rows)
}

func (s *Service) GetMediaSource(ctx context.Context, id string) (MediaSourceItem, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ms.id, ms.library_id, ms.kind, ms.path, ms.rel_path, ms.name, ms.extension, ms.size_bytes, ms.modified_at,
			mp.container, mp.duration_seconds, mp.bitrate, mp.video_codec, mp.video_profile, mp.video_level, mp.video_bit_depth, mp.video_frame_rate, mp.pixel_format, mp.color_primaries, mp.color_transfer, mp.color_space, mp.hdr_format, mp.dovi_profile, mp.max_cll, mp.max_fall, mp.width, mp.height, mp.audio_streams, mp.subtitle_streams
		FROM media_sources ms
		LEFT JOIN media_probes mp ON mp.media_source_id = ms.id
		WHERE ms.id = ?
	`, id)
	if err != nil {
		return MediaSourceItem{}, false, err
	}
	defer rows.Close()
	items, err := scanMediaSources(rows)
	if err != nil {
		return MediaSourceItem{}, false, err
	}
	if len(items) == 0 {
		return MediaSourceItem{}, false, nil
	}
	return items[0], true, nil
}

// DeleteMediaSource removes a media source from the catalog and deletes the
// underlying file from disk. The DB deletion cascades to movie_versions,
// episode_versions, media_probes, and playback_states. If the DB deletion
// succeeds but the file removal fails (e.g. permissions), the error is
// returned so the caller can surface it, but the catalog entry is already gone.
func (s *Service) DeleteMediaSource(ctx context.Context, id string) error {
	// Fetch the path before we delete so we know what to remove from disk.
	source, ok, err := s.GetMediaSource(ctx, id)
	if err != nil {
		return fmt.Errorf("lookup media source %q: %w", id, err)
	}
	if !ok {
		return fmt.Errorf("media source %q not found", id)
	}
	// Delete from the catalog (foreign-key cascades clean up versions, probes, states).
	if _, err := s.db.ExecContext(ctx, `DELETE FROM media_sources WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete catalog record for %q: %w", id, err)
	}
	// Remove the file from disk. Non-fatal if already gone.
	if source.Path != "" {
		if removeErr := os.Remove(source.Path); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("delete file %q from disk: %w", source.Path, removeErr)
		}
	}
	return nil
}

func (s *Service) GetMediaSourceDisplay(ctx context.Context, id string) (MediaSourceDisplay, bool, error) {
	var movieID string
	var movieTitle string
	var movieYear int
	var movieQuality sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT m.id, m.title, m.year, mv.quality_label
		FROM movie_versions mv
		JOIN movies m ON m.id = mv.movie_id
		WHERE mv.media_source_id = ?
		LIMIT 1
	`, id).Scan(&movieID, &movieTitle, &movieYear, &movieQuality)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return MediaSourceDisplay{}, false, err
	}
	if err == nil {
		sortTitle := sortTitle(movieTitle)
		if record, ok, err := s.GetBestMetadata(ctx, "movie", movieID); err != nil {
			return MediaSourceDisplay{}, false, err
		} else if ok {
			applyMovieMetadata(&movieTitle, &movieYear, &sortTitle, record)
		}
		return MediaSourceDisplay{
			Kind:         "movie",
			ItemID:       movieID,
			Title:        titleWithYear(movieTitle, movieYear),
			ArtworkKind:  "movie",
			ArtworkID:    movieID,
			QualityLabel: movieQuality.String,
		}, true, nil
	}

	var episodeID string
	var episodeTitle string
	var episodeNumber int
	var episodeEnd int
	var seriesID string
	var seriesTitle string
	var seasonNumber int
	var episodeQuality sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT e.id, e.title, e.episode_number, e.episode_end, s.id, s.title, seasons.season_number, ev.quality_label
		FROM episode_versions ev
		JOIN tv_episodes e ON e.id = ev.episode_id
		JOIN tv_series s ON s.id = e.series_id
		JOIN tv_seasons seasons ON seasons.id = e.season_id
		WHERE ev.media_source_id = ?
		LIMIT 1
	`, id).Scan(&episodeID, &episodeTitle, &episodeNumber, &episodeEnd, &seriesID, &seriesTitle, &seasonNumber, &episodeQuality)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaSourceDisplay{}, false, nil
	}
	if err != nil {
		return MediaSourceDisplay{}, false, err
	}
	seriesSortTitle := sortTitle(seriesTitle)
	if record, ok, err := s.GetBestMetadata(ctx, "series", seriesID); err != nil {
		return MediaSourceDisplay{}, false, err
	} else if ok {
		applyTitleMetadata(&seriesTitle, &seriesSortTitle, record)
	}
	return MediaSourceDisplay{
		Kind:          "episode",
		ItemID:        episodeID,
		Title:         episodeDisplayTitle(seriesTitle, seasonNumber, episodeNumber, episodeEnd, episodeTitle),
		ArtworkKind:   "series",
		ArtworkID:     seriesID,
		QualityLabel:  episodeQuality.String,
		SeasonNumber:  seasonNumber,
		EpisodeNumber: episodeNumber,
		EpisodeTitle:  episodeTitle,
		SeriesID:      seriesID,
	}, true, nil
}

func (s *Service) GetMediaSourceTracks(ctx context.Context, id string) (MediaSourceTracks, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT raw_json FROM media_probes WHERE media_source_id = ?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaSourceTracks{}, false, nil
	}
	if err != nil {
		return MediaSourceTracks{}, false, err
	}
	result, err := probe.Parse([]byte(raw))
	if err != nil {
		return MediaSourceTracks{}, false, err
	}
	return MediaSourceTracks{AudioTracks: result.AudioTracks, SubtitleTracks: result.SubtitleTracks}, true, nil
}

func (s *Service) SaveProbe(ctx context.Context, mediaSourceID string, result ProbeResult) error {
	const saveStatement = `
		INSERT INTO media_probes(media_source_id, container, duration_seconds, bitrate, video_codec, video_profile, video_level, video_bit_depth, video_frame_rate, pixel_format, color_primaries, color_transfer, color_space, hdr_format, dovi_profile, max_cll, max_fall, width, height, audio_streams, subtitle_streams, raw_json, probed_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(media_source_id) DO UPDATE SET
			container = excluded.container,
			duration_seconds = excluded.duration_seconds,
			bitrate = excluded.bitrate,
			video_codec = excluded.video_codec,
			video_profile = excluded.video_profile,
			video_level = excluded.video_level,
			video_bit_depth = excluded.video_bit_depth,
			video_frame_rate = excluded.video_frame_rate,
			pixel_format = excluded.pixel_format,
			color_primaries = excluded.color_primaries,
			color_transfer = excluded.color_transfer,
			color_space = excluded.color_space,
			hdr_format = excluded.hdr_format,
			dovi_profile = excluded.dovi_profile,
			max_cll = excluded.max_cll,
			max_fall = excluded.max_fall,
			width = excluded.width,
			height = excluded.height,
			audio_streams = excluded.audio_streams,
			subtitle_streams = excluded.subtitle_streams,
			raw_json = excluded.raw_json,
			probed_at = excluded.probed_at
	`
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		if _, err := s.db.ExecContext(ctx, saveStatement, mediaSourceID, result.Container, result.DurationSeconds, result.Bitrate, result.VideoCodec, result.VideoProfile, result.VideoLevel, result.VideoBitDepth, result.VideoFrameRate, result.PixelFormat, result.ColorPrimaries, result.ColorTransfer, result.ColorSpace, result.HDRFormat, result.DoviProfile, result.MaxCLL, result.MaxFALL, result.Width, result.Height, result.AudioStreams, result.SubtitleStreams, result.RawJSON, timestamp(time.Now())); err != nil {
			lastErr = err
			if !isSQLiteBusyError(err) || attempt == 5 {
				return err
			}
			backoff := time.Duration(attempt*120) * time.Millisecond
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		return nil
	}
	return lastErr
}

func isSQLiteBusyError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "sqlite_busy") || strings.Contains(text, "database is locked")
}

func scanMediaSources(rows *sql.Rows) ([]MediaSourceItem, error) {
	output := []MediaSourceItem{}
	for rows.Next() {
		var item MediaSourceItem
		var container sql.NullString
		var duration sql.NullFloat64
		var bitrate sql.NullInt64
		var videoCodec sql.NullString
		var videoProfile sql.NullString
		var videoLevel sql.NullString
		var videoBitDepth sql.NullInt64
		var videoFrameRate sql.NullFloat64
		var pixelFormat sql.NullString
		var colorPrimaries sql.NullString
		var colorTransfer sql.NullString
		var colorSpace sql.NullString
		var hdrFormat sql.NullString
		var doviProfile sql.NullInt64
		var maxCLL sql.NullInt64
		var maxFALL sql.NullInt64
		var width sql.NullInt64
		var height sql.NullInt64
		var audioStreams sql.NullInt64
		var subtitleStreams sql.NullInt64
		if err := rows.Scan(&item.ID, &item.LibraryID, &item.Kind, &item.Path, &item.RelPath, &item.Name, &item.Extension, &item.SizeBytes, &item.ModifiedAt, &container, &duration, &bitrate, &videoCodec, &videoProfile, &videoLevel, &videoBitDepth, &videoFrameRate, &pixelFormat, &colorPrimaries, &colorTransfer, &colorSpace, &hdrFormat, &doviProfile, &maxCLL, &maxFALL, &width, &height, &audioStreams, &subtitleStreams); err != nil {
			return nil, err
		}
		item.Probed = container.Valid
		if container.Valid {
			item.Container = container.String
			item.DurationSeconds = duration.Float64
			item.Bitrate = bitrate.Int64
			item.VideoCodec = videoCodec.String
			item.VideoProfile = videoProfile.String
			item.VideoLevel = videoLevel.String
			item.VideoBitDepth = int(videoBitDepth.Int64)
			item.VideoFrameRate = videoFrameRate.Float64
			item.PixelFormat = pixelFormat.String
			item.ColorPrimaries = colorPrimaries.String
			item.ColorTransfer = colorTransfer.String
			item.ColorSpace = colorSpace.String
			item.HDRFormat = hdrFormat.String
			item.DoviProfile = int(doviProfile.Int64)
			item.MaxCLL = int(maxCLL.Int64)
			item.MaxFALL = int(maxFALL.Int64)
			item.Width = int(width.Int64)
			item.Height = int(height.Int64)
			item.AudioStreams = int(audioStreams.Int64)
			item.SubtitleStreams = int(subtitleStreams.Int64)
		}
		output = append(output, item)
	}
	return output, rows.Err()
}

func (s *Service) SaveMovieScan(ctx context.Context, library libraries.Library, result scanner.Result, candidates []movies.Candidate) (PersistSummary, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PersistSummary{}, err
	}
	defer tx.Rollback()

	if err := upsertLibrary(ctx, tx, library); err != nil {
		return PersistSummary{}, err
	}
	scanRunID, err := insertScanRun(ctx, tx, result.Summary)
	if err != nil {
		return PersistSummary{}, err
	}

	now := timestamp(time.Now())
	movieIDs := make(map[string]struct{}, len(candidates))
	sourceIDs := observedSourceIDs(result)
	for _, candidate := range candidates {
		sourceID := sourceID(candidate.Media.Path)
		if err := upsertScanFileState(ctx, tx, library.ID, candidate.Media, now); err != nil {
			return PersistSummary{}, err
		}
		movieID := movieRecordID(candidate, sourceID)
		movieIDs[movieID] = struct{}{}
		if !candidate.Media.Changed {
			continue
		}
		if err := upsertMediaSource(ctx, tx, library.ID, media.KindMovie, sourceID, candidate.Media, now); err != nil {
			return PersistSummary{}, err
		}
		if err := upsertMovie(ctx, tx, movieID, candidate, now); err != nil {
			return PersistSummary{}, err
		}
		if err := upsertMetadataRecord(ctx, tx, MetadataRecord{
			Kind:       "movie",
			ItemID:     movieID,
			Provider:   "filename",
			Title:      candidate.Title,
			Year:       candidate.Year,
			Confidence: metadataConfidence(candidate.NeedsReview),
			FetchedAt:  now,
			UpdatedAt:  now,
		}); err != nil {
			return PersistSummary{}, err
		}
		if err := upsertMovieVersion(ctx, tx, movieID, sourceID, candidate, now); err != nil {
			return PersistSummary{}, err
		}
	}
	if err := deleteMissingSources(ctx, tx, library.ID, sourceIDs); err != nil {
		return PersistSummary{}, err
	}
	if err := touchSeenScanState(ctx, tx, library.ID, result.SeenRelPaths, now); err != nil {
		return PersistSummary{}, err
	}
	if err := deleteMissingScanState(ctx, tx, library.ID, result.SeenRelPaths); err != nil {
		return PersistSummary{}, err
	}

	if err := tx.Commit(); err != nil {
		return PersistSummary{}, err
	}
	return PersistSummary{
		LibraryID:        library.ID,
		ScanRunID:        scanRunID,
		MediaSources:     len(result.SeenRelPaths),
		ChangedSources:   result.ChangedFiles,
		UnchangedSources: result.Unchanged,
		Movies:           len(movieIDs),
	}, nil
}

func (s *Service) SaveTVScan(ctx context.Context, library libraries.Library, result scanner.Result, candidates []tv.EpisodeCandidate) (PersistSummary, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PersistSummary{}, err
	}
	defer tx.Rollback()

	if err := upsertLibrary(ctx, tx, library); err != nil {
		return PersistSummary{}, err
	}
	scanRunID, err := insertScanRun(ctx, tx, result.Summary)
	if err != nil {
		return PersistSummary{}, err
	}

	now := timestamp(time.Now())
	episodeIDs := make(map[string]struct{}, len(candidates))
	sourceIDs := observedSourceIDs(result)
	for _, candidate := range candidates {
		sourceID := sourceID(candidate.Media.Path)
		if err := upsertScanFileState(ctx, tx, library.ID, candidate.Media, now); err != nil {
			return PersistSummary{}, err
		}
		seriesID := seriesID(candidate.SeriesTitle)
		seasonID := seasonID(seriesID, candidate.SeasonNumber)
		episodeID := episodeRecordID(seriesID, candidate, sourceID)
		episodeIDs[episodeID] = struct{}{}
		if !candidate.Media.Changed {
			continue
		}
		if err := upsertMediaSource(ctx, tx, library.ID, media.KindEpisode, sourceID, candidate.Media, now); err != nil {
			return PersistSummary{}, err
		}
		if err := upsertSeries(ctx, tx, seriesID, candidate.SeriesTitle, now); err != nil {
			return PersistSummary{}, err
		}
		if err := upsertMetadataRecord(ctx, tx, MetadataRecord{
			Kind:       "series",
			ItemID:     seriesID,
			Provider:   "filename",
			Title:      candidate.SeriesTitle,
			Confidence: metadataConfidence(candidate.SeriesTitle == ""),
			FetchedAt:  now,
			UpdatedAt:  now,
		}); err != nil {
			return PersistSummary{}, err
		}
		if err := upsertSeason(ctx, tx, seasonID, seriesID, candidate.SeasonNumber, now); err != nil {
			return PersistSummary{}, err
		}
		if err := upsertEpisode(ctx, tx, episodeID, seriesID, seasonID, candidate, now); err != nil {
			return PersistSummary{}, err
		}
		if err := upsertMetadataRecord(ctx, tx, MetadataRecord{
			Kind:       "episode",
			ItemID:     episodeID,
			Provider:   "filename",
			Title:      candidate.EpisodeTitle,
			Confidence: metadataConfidence(candidate.NeedsReview || candidate.EpisodeTitle == ""),
			FetchedAt:  now,
			UpdatedAt:  now,
		}); err != nil {
			return PersistSummary{}, err
		}
		if err := upsertEpisodeVersion(ctx, tx, episodeID, sourceID, candidate, now); err != nil {
			return PersistSummary{}, err
		}
	}
	if err := deleteMissingSources(ctx, tx, library.ID, sourceIDs); err != nil {
		return PersistSummary{}, err
	}
	if err := touchSeenScanState(ctx, tx, library.ID, result.SeenRelPaths, now); err != nil {
		return PersistSummary{}, err
	}
	if err := deleteMissingScanState(ctx, tx, library.ID, result.SeenRelPaths); err != nil {
		return PersistSummary{}, err
	}

	if err := tx.Commit(); err != nil {
		return PersistSummary{}, err
	}
	return PersistSummary{
		LibraryID:        library.ID,
		ScanRunID:        scanRunID,
		MediaSources:     len(result.SeenRelPaths),
		ChangedSources:   result.ChangedFiles,
		UnchangedSources: result.Unchanged,
		Episodes:         len(episodeIDs),
	}, nil
}

func upsertLibrary(ctx context.Context, tx *sql.Tx, library libraries.Library) error {
	now := timestamp(time.Now())
	if library.StorageType == "" {
		library.StorageType = libraries.DetectStorageType(library.Path)
	}
	var existingID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM libraries WHERE path = ? LIMIT 1`, library.Path).Scan(&existingID)
	if err == nil && strings.TrimSpace(existingID) != "" {
		library.ID = existingID
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rawSources, err := json.Marshal(library.MetadataSources)
	if err != nil {
		return err
	}
	rawArtworkSources, err := json.Marshal(library.ArtworkSources)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO libraries(id, kind, name, path, storage_type, metadata_sources_json, artwork_sources_json, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			kind = excluded.kind,
			name = excluded.name,
			path = excluded.path,
			storage_type = excluded.storage_type,
			metadata_sources_json = excluded.metadata_sources_json,
			artwork_sources_json = excluded.artwork_sources_json,
			updated_at = excluded.updated_at
	`, library.ID, library.Kind, library.Name, library.Path, library.StorageType, string(rawSources), string(rawArtworkSources), now)
	return err
}

func decodeLibraryMetadataSources(kind libraries.Kind, raw string) []string {
	var values []string
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &values)
	}
	return metasources.NormalizeRequestedSourceOrder(string(kind), values)
}

func decodeLibraryArtworkSources(kind libraries.Kind, raw string) []string {
	var values []string
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &values)
	}
	return metasources.NormalizeRequestedArtworkOrder(string(kind), values)
}

func upsertMetadataRecord(ctx context.Context, tx *sql.Tx, record MetadataRecord) error {
	record.Kind = strings.TrimSpace(record.Kind)
	record.ItemID = strings.TrimSpace(record.ItemID)
	record.Provider = strings.TrimSpace(record.Provider)
	record.Title = strings.TrimSpace(record.Title)
	if record.Provider == "" {
		record.Provider = "filename"
	}
	now := timestamp(time.Now())
	if record.FetchedAt == "" {
		record.FetchedAt = now
	}
	if record.UpdatedAt == "" {
		record.UpdatedAt = now
	}
	record.ThumbnailURL = strings.TrimSpace(record.ThumbnailURL)
	record.LogoURL = strings.TrimSpace(record.LogoURL)
	record.BannerURL = strings.TrimSpace(record.BannerURL)
	record.VideoKey = strings.TrimSpace(record.VideoKey)
	record.TrailerPath = strings.TrimSpace(record.TrailerPath)
	record.DetailsJSON = metadataDetailsJSON(record)
	record.ArtworkJSON = metadataArtworkJSON(record)
	// trailer_path is preserved on upsert: re-fetching metadata should NOT
	// blow away an already-downloaded trailer file. video_key is always
	// overwritten with the freshly-parsed key from the provider response.
	_, err := tx.ExecContext(ctx, `
		INSERT INTO metadata_records(kind, item_id, provider, external_id, title, year, overview, poster_url, backdrop_url, thumbnail_url, logo_url, banner_url, video_key, trailer_path, artwork_json, details_json, confidence, raw_json, fetched_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, item_id, provider) DO UPDATE SET
			external_id = excluded.external_id,
			title = excluded.title,
			year = excluded.year,
			overview = excluded.overview,
			poster_url = excluded.poster_url,
			backdrop_url = excluded.backdrop_url,
			thumbnail_url = excluded.thumbnail_url,
			logo_url = excluded.logo_url,
			banner_url = excluded.banner_url,
			video_key = excluded.video_key,
			trailer_path = CASE WHEN metadata_records.trailer_path != '' THEN metadata_records.trailer_path ELSE excluded.trailer_path END,
			artwork_json = excluded.artwork_json,
			details_json = excluded.details_json,
			confidence = excluded.confidence,
			raw_json = excluded.raw_json,
			fetched_at = excluded.fetched_at,
			updated_at = excluded.updated_at
	`, record.Kind, record.ItemID, record.Provider, record.ExternalID, record.Title, record.Year, record.Overview, record.PosterURL, record.BackdropURL, record.ThumbnailURL, record.LogoURL, record.BannerURL, record.VideoKey, record.TrailerPath, record.ArtworkJSON, record.DetailsJSON, record.Confidence, record.RawJSON, record.FetchedAt, record.UpdatedAt)
	if err != nil {
		return err
	}
	return syncPeopleFromRecord(ctx, tx, record)
}

func personID(name string) string {
	h := sha1.New()
	h.Write([]byte(strings.ToLower(strings.TrimSpace(name))))
	return hex.EncodeToString(h.Sum(nil))
}

func syncPeopleFromRecord(ctx context.Context, tx *sql.Tx, record MetadataRecord) error {
	allCredits := make([]MetadataCredit, 0, len(record.Cast)+len(record.Crew)+len(record.GuestCast))
	allCredits = append(allCredits, record.Cast...)
	allCredits = append(allCredits, record.Crew...)
	allCredits = append(allCredits, record.GuestCast...)
	if len(allCredits) == 0 {
		return nil
	}
	now := timestamp(time.Now())
	seen := map[string]struct{}{}
	for _, credit := range allCredits {
		name := strings.TrimSpace(credit.Name)
		if name == "" {
			continue
		}
		id := personID(name)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO people(id, name, name_lower, profile_url, department, updated_at)
			VALUES(?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				profile_url = CASE WHEN people.profile_url != '' THEN people.profile_url ELSE excluded.profile_url END,
				department  = CASE WHEN people.department  != '' THEN people.department  ELSE excluded.department  END,
				updated_at  = excluded.updated_at
		`, id, name, strings.ToLower(name), credit.ProfileURL, credit.Department, now); err != nil {
			return err
		}
		creditKey := id + "|" + record.Kind + "|" + record.ItemID + "|" + credit.Role
		if _, dup := seen[creditKey]; dup {
			continue
		}
		seen[creditKey] = struct{}{}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO people_credits(person_id, item_kind, item_id, role, character)
			VALUES(?, ?, ?, ?, ?)
		`, id, record.Kind, record.ItemID, credit.Role, credit.Character); err != nil {
			return err
		}
	}
	return nil
}

func metadataConfidence(needsReview bool) float64 {
	if needsReview {
		return 0.35
	}
	return 0.7
}

func applyMovieMetadata(title *string, year *int, sort *string, record MetadataRecord) {
	applyTitleMetadata(title, sort, record)
	if record.Year != 0 {
		*year = record.Year
	}
}

func applyTitleMetadata(title *string, sort *string, record MetadataRecord) {
	if record.Title == "" {
		return
	}
	*title = record.Title
	*sort = sortTitle(record.Title)
}

func titleWithYear(title string, year int) string {
	title = strings.TrimSpace(title)
	if year <= 0 {
		return title
	}
	return title + " (" + intString(year) + ")"
}

// BackfillPeople populates the people and people_credits tables from all
// existing metadata_records rows. Safe to call multiple times; it is
// idempotent thanks to ON CONFLICT/INSERT OR REPLACE semantics.
func (s *Service) BackfillPeople(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, item_id, provider, external_id, title, year, overview,
		       poster_url, backdrop_url, thumbnail_url, logo_url, banner_url,
		       video_key, trailer_path, artwork_json, details_json,
		       confidence, raw_json, fetched_at, updated_at
		FROM metadata_records
		WHERE details_json != '{}' AND details_json != ''
	`)
	if err != nil {
		return err
	}
	defer rows.Close()
	records, err := scanMetadataRecords(rows)
	if err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, record := range records {
		if len(record.Cast)+len(record.Crew)+len(record.GuestCast) == 0 {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := syncPeopleFromRecord(ctx, tx, record); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

func episodeDisplayTitle(seriesTitle string, seasonNumber int, episodeNumber int, episodeEnd int, episodeTitle string) string {
	episodeCode := "S" + twoDigit(seasonNumber) + "E" + twoDigit(episodeNumber)
	if episodeEnd > episodeNumber {
		episodeCode += "-E" + twoDigit(episodeEnd)
	}
	parts := []string{strings.TrimSpace(seriesTitle), episodeCode}
	if strings.TrimSpace(episodeTitle) != "" {
		parts = append(parts, strings.TrimSpace(episodeTitle))
	}
	return strings.Join(parts, " - ")
}

func upsertMediaSource(ctx context.Context, tx *sql.Tx, libraryID string, kind media.Kind, id string, file scanner.FileCandidate, now string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO media_sources(id, library_id, kind, path, rel_path, name, extension, size_bytes, modified_at, discovered_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			library_id = excluded.library_id,
			kind = excluded.kind,
			rel_path = excluded.rel_path,
			name = excluded.name,
			extension = excluded.extension,
			size_bytes = excluded.size_bytes,
			modified_at = excluded.modified_at,
			updated_at = excluded.updated_at
	`, id, libraryID, kind, file.Path, file.RelPath, file.Name, file.Extension, file.Size, timestamp(file.ModifiedAt), now, now)
	return err
}

func upsertScanFileState(ctx context.Context, tx *sql.Tx, libraryID string, file scanner.FileCandidate, now string) error {
	changedAt := now
	if !file.Changed {
		changedAt = timestamp(file.ModifiedAt)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO scan_file_state(library_id, rel_path, size_bytes, modified_at, last_seen_at, changed_at)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(library_id, rel_path) DO UPDATE SET
			size_bytes = excluded.size_bytes,
			modified_at = excluded.modified_at,
			last_seen_at = excluded.last_seen_at,
			changed_at = CASE
				WHEN scan_file_state.size_bytes != excluded.size_bytes OR scan_file_state.modified_at != excluded.modified_at THEN excluded.changed_at
				ELSE scan_file_state.changed_at
			END
	`, libraryID, file.RelPath, file.Size, timestamp(file.ModifiedAt), now, changedAt)
	return err
}

func upsertMovie(ctx context.Context, tx *sql.Tx, id string, candidate movies.Candidate, now string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO movies(id, title, year, sort_title, needs_review, review_reason, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			year = excluded.year,
			sort_title = excluded.sort_title,
			needs_review = excluded.needs_review,
			review_reason = excluded.review_reason,
			updated_at = excluded.updated_at
	`, id, candidate.Title, candidate.Year, sortTitle(candidate.Title), boolInt(candidate.NeedsReview), candidate.ReviewReason, now)
	return err
}

func upsertMovieVersion(ctx context.Context, tx *sql.Tx, movieID string, sourceID string, candidate movies.Candidate, now string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO movie_versions(movie_id, media_source_id, edition, quality_label, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(movie_id, media_source_id) DO UPDATE SET
			edition = excluded.edition,
			quality_label = excluded.quality_label,
			updated_at = excluded.updated_at
	`, movieID, sourceID, candidate.Edition, candidate.QualityLabel, now)
	return err
}

func upsertSeries(ctx context.Context, tx *sql.Tx, id string, title string, now string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO tv_series(id, title, sort_title, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(title) DO UPDATE SET
			sort_title = excluded.sort_title,
			updated_at = excluded.updated_at
	`, id, title, sortTitle(title), now)
	return err
}

func upsertSeason(ctx context.Context, tx *sql.Tx, id string, seriesID string, seasonNumber int, now string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO tv_seasons(id, series_id, season_number, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(series_id, season_number) DO UPDATE SET
			updated_at = excluded.updated_at
	`, id, seriesID, seasonNumber, now)
	return err
}

func upsertEpisode(ctx context.Context, tx *sql.Tx, id string, seriesID string, seasonID string, candidate tv.EpisodeCandidate, now string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO tv_episodes(id, series_id, season_id, season_number, episode_number, episode_end, title, needs_review, review_reason, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			series_id = excluded.series_id,
			season_id = excluded.season_id,
			season_number = excluded.season_number,
			episode_number = excluded.episode_number,
			episode_end = excluded.episode_end,
			title = excluded.title,
			needs_review = excluded.needs_review,
			review_reason = excluded.review_reason,
			updated_at = excluded.updated_at
	`, id, seriesID, seasonID, candidate.SeasonNumber, candidate.EpisodeNumber, candidate.EpisodeEnd, candidate.EpisodeTitle, boolInt(candidate.NeedsReview), candidate.ReviewReason, now)
	return err
}

func upsertEpisodeVersion(ctx context.Context, tx *sql.Tx, episodeID string, sourceID string, candidate tv.EpisodeCandidate, now string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO episode_versions(episode_id, media_source_id, quality_label, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(episode_id, media_source_id) DO UPDATE SET
			quality_label = excluded.quality_label,
			updated_at = excluded.updated_at
	`, episodeID, sourceID, candidate.QualityLabel, now)
	return err
}

func deleteMissingSources(ctx context.Context, tx *sql.Tx, libraryID string, sourceIDs []string) error {
	if len(sourceIDs) == 0 {
		_, err := tx.ExecContext(ctx, "DELETE FROM media_sources WHERE library_id = ?", libraryID)
		return err
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(sourceIDs)), ",")
	args := make([]any, 0, len(sourceIDs)+1)
	args = append(args, libraryID)
	for _, id := range sourceIDs {
		args = append(args, id)
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM media_sources WHERE library_id = ? AND id NOT IN ("+placeholders+")", args...)
	return err
}

func observedSourceIDs(result scanner.Result) []string {
	relPaths := result.SeenRelPaths
	if len(relPaths) == 0 {
		relPaths = make([]string, 0, len(result.Files))
		for _, file := range result.Files {
			relPaths = append(relPaths, file.RelPath)
		}
	}
	fileIDs := make(map[string]string, len(result.Files))
	for _, file := range result.Files {
		fileIDs[file.RelPath] = sourceID(file.Path)
	}
	output := make([]string, 0, len(relPaths))
	for _, relPath := range relPaths {
		if id := fileIDs[relPath]; id != "" {
			output = append(output, id)
		} else {
			output = append(output, sourceID(filepath.Join(result.Root, relPath)))
		}
	}
	return output
}

func deleteMissingScanState(ctx context.Context, tx *sql.Tx, libraryID string, relPaths []string) error {
	if len(relPaths) == 0 {
		_, err := tx.ExecContext(ctx, "DELETE FROM scan_file_state WHERE library_id = ?", libraryID)
		return err
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(relPaths)), ",")
	args := make([]any, 0, len(relPaths)+1)
	args = append(args, libraryID)
	for _, relPath := range relPaths {
		args = append(args, relPath)
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM scan_file_state WHERE library_id = ? AND rel_path NOT IN ("+placeholders+")", args...)
	return err
}

func touchSeenScanState(ctx context.Context, tx *sql.Tx, libraryID string, relPaths []string, now string) error {
	if len(relPaths) == 0 {
		return nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(relPaths)), ",")
	args := make([]any, 0, len(relPaths)+2)
	args = append(args, now, libraryID)
	for _, relPath := range relPaths {
		args = append(args, relPath)
	}
	_, err := tx.ExecContext(ctx, "UPDATE scan_file_state SET last_seen_at = ? WHERE library_id = ? AND rel_path IN ("+placeholders+")", args...)
	return err
}

func insertScanRun(ctx context.Context, tx *sql.Tx, summary scanner.Summary) (int64, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO scan_runs(kind, root, started_at, completed_at, duration_ms, total_files, media_files, ignored_files, error_count)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, summary.Kind, summary.Root, timestamp(summary.StartedAt), timestamp(summary.CompletedAt), summary.DurationMS, summary.TotalFiles, summary.MediaFiles, summary.IgnoredFiles, summary.ErrorCount)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func sourceID(path string) string {
	return idFor("source", filepath.Clean(path))
}

func movieRecordID(candidate movies.Candidate, sourceID string) string {
	if candidate.NeedsReview && candidate.Year == 0 {
		return idFor("movie-review", sourceID)
	}
	return movieID(candidate.Title, candidate.Year)
}

func movieID(title string, year int) string {
	return idFor("movie", normalize(title), intString(year))
}

func seriesID(title string) string {
	return idFor("series", normalize(title))
}

func seasonID(seriesID string, seasonNumber int) string {
	return idFor("season", seriesID, intString(seasonNumber))
}

func episodeRecordID(seriesID string, candidate tv.EpisodeCandidate, sourceID string) string {
	if candidate.NeedsReview && candidate.EpisodeNumber == 0 {
		return idFor("episode-review", sourceID)
	}
	return episodeID(seriesID, candidate.SeasonNumber, candidate.EpisodeNumber, candidate.EpisodeEnd)
}

func episodeID(seriesID string, seasonNumber int, episodeNumber int, episodeEnd int) string {
	return idFor("episode", seriesID, intString(seasonNumber), intString(episodeNumber), intString(episodeEnd))
}

func idFor(parts ...string) string {
	hash := sha1.New()
	for _, part := range parts {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func normalize(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func sortTitle(value string) string {
	return normalize(strings.TrimPrefix(strings.TrimPrefix(value, "The "), "A "))
}

func timestamp(value time.Time) string {
	if value.IsZero() {
		value = time.Now()
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTimestamp(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func intString(value int) string {
	return strconv.Itoa(value)
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return ""
}

func twoDigit(value int) string {
	if value < 10 {
		return "0" + intString(value)
	}
	return intString(value)
}

// SeasonPeer groups a media source ID with its file path and duration for season-level chapter analysis.
type SeasonPeer struct {
	MediaSourceID string
	Path          string
	Duration      float64
}

// GetSeasonPeers returns all media sources in the same TV season as the given media source.
// Used to supply all episode inputs to chapter/intro detection.
func (s *Service) GetSeasonPeers(ctx context.Context, mediaSourceID string) ([]SeasonPeer, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ms.id, ms.path, COALESCE(mp.duration_seconds, 0)
		FROM episode_versions ev2
		JOIN tv_episodes e2 ON e2.id = ev2.episode_id
		JOIN tv_episodes e3 ON e3.season_id = e2.season_id
		JOIN episode_versions ev3 ON ev3.episode_id = e3.id
		JOIN media_sources ms ON ms.id = ev3.media_source_id
		LEFT JOIN media_probes mp ON mp.media_source_id = ms.id
		WHERE ev2.media_source_id = ?
		GROUP BY ms.id
		ORDER BY e3.episode_number ASC
	`, mediaSourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeasonPeer
	for rows.Next() {
		var p SeasonPeer
		if err := rows.Scan(&p.MediaSourceID, &p.Path, &p.Duration); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
