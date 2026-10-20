package tv

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/EJM-Digital/Xuva/server/internal/media"
	"github.com/EJM-Digital/Xuva/server/internal/scanner"
)

type Series struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Year  int    `json:"year"`
}

type Season struct {
	ID           string `json:"id"`
	SeriesID     string `json:"seriesId"`
	SeasonNumber int    `json:"seasonNumber"`
}

type Episode struct {
	ID            string `json:"id"`
	SeriesID      string `json:"seriesId"`
	SeasonID      string `json:"seasonId"`
	EpisodeNumber int    `json:"episodeNumber"`
	Title         string `json:"title"`
}

type EpisodeVersion struct {
	EpisodeID     string         `json:"episodeId"`
	MediaSourceID media.SourceID `json:"mediaSourceId"`
	QualityLabel  string         `json:"qualityLabel,omitempty"`
}

type EpisodeCandidate struct {
	SeriesTitle   string                `json:"seriesTitle"`
	SeasonNumber  int                   `json:"seasonNumber,omitempty"`
	EpisodeNumber int                   `json:"episodeNumber,omitempty"`
	EpisodeEnd    int                   `json:"episodeEnd,omitempty"`
	EpisodeTitle  string                `json:"episodeTitle,omitempty"`
	QualityLabel  string                `json:"qualityLabel,omitempty"`
	NeedsReview   bool                  `json:"needsReview"`
	ReviewReason  string                `json:"reviewReason,omitempty"`
	Media         scanner.FileCandidate `json:"media"`
}

type Service struct{}

func NewService() *Service {
	return &Service{}
}

func (s *Service) Classify(files []scanner.FileCandidate) []EpisodeCandidate {
	candidates := make([]EpisodeCandidate, 0, len(files))
	for _, file := range files {
		candidates = append(candidates, classifyEpisode(file))
	}
	return candidates
}

func classifyEpisode(file scanner.FileCandidate) EpisodeCandidate {
	parts := splitRelativePath(file.RelPath)
	seriesTitle := inferSeriesTitle(parts, file)
	name := strings.TrimSuffix(file.Name, filepath.Ext(file.Name))
	season, episode, episodeEnd, titleSource, ok := parseEpisodeNumbers(name)

	candidate := EpisodeCandidate{
		SeriesTitle:   seriesTitle,
		SeasonNumber:  season,
		EpisodeNumber: episode,
		EpisodeEnd:    episodeEnd,
		EpisodeTitle:  cleanEpisodeTitle(titleSource),
		QualityLabel:  detectEpisodeQuality(name),
		Media:         file,
	}
	if !ok {
		candidate.SeasonNumber = inferSeasonFromPath(parts)
		candidate.NeedsReview = true
		candidate.ReviewReason = "unable to infer episode number"
	}
	if candidate.SeriesTitle == "" {
		candidate.NeedsReview = true
		if candidate.ReviewReason == "" {
			candidate.ReviewReason = "unable to infer series title"
		}
	}
	return candidate
}

func parseEpisodeNumbers(name string) (season int, episode int, episodeEnd int, titleSource string, ok bool) {
	if match := sxeRE.FindStringSubmatchIndex(name); match != nil {
		season = atoiSlice(name, match[2], match[3])
		episode = atoiSlice(name, match[4], match[5])
		if match[6] >= 0 && match[7] >= 0 {
			episodeEnd = atoiSlice(name, match[6], match[7])
		}
		titleSource = name[match[1]:]
		return season, episode, episodeEnd, titleSource, true
	}
	if match := oneXRE.FindStringSubmatchIndex(name); match != nil {
		season = atoiSlice(name, match[2], match[3])
		episode = atoiSlice(name, match[4], match[5])
		titleSource = name[match[1]:]
		return season, episode, 0, titleSource, true
	}
	return 0, 0, 0, "", false
}

func splitRelativePath(relPath string) []string {
	if relPath == "" {
		return nil
	}
	return strings.Split(filepath.Clean(relPath), string(filepath.Separator))
}

func inferSeriesTitle(parts []string, file scanner.FileCandidate) string {
	if len(parts) > 1 {
		return cleanupEpisodeText(parts[0])
	}
	return cleanupEpisodeText(strings.TrimSuffix(file.Name, filepath.Ext(file.Name)))
}

func inferSeasonFromPath(parts []string) int {
	for _, part := range parts {
		if match := seasonDirRE.FindStringSubmatch(part); len(match) == 2 {
			season, _ := strconv.Atoi(match[1])
			return season
		}
	}
	return 0
}

func cleanEpisodeTitle(source string) string {
	cleaned := cleanupEpisodeText(source)
	cleaned = tvNoiseRE.ReplaceAllString(cleaned, " ")
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	return cleaned
}

func cleanupEpisodeText(source string) string {
	replacer := strings.NewReplacer(".", " ", "_", " ", "-", " ", "(", " ", ")", " ", "[", " ", "]", " ")
	return strings.Join(strings.Fields(replacer.Replace(source)), " ")
}

func detectEpisodeQuality(source string) string {
	lower := strings.ToLower(source)
	switch {
	case strings.Contains(lower, "2160p") || strings.Contains(lower, "4k") || strings.Contains(lower, "uhd"):
		return "4K"
	case strings.Contains(lower, "1080p"):
		return "1080p"
	case strings.Contains(lower, "720p"):
		return "720p"
	case strings.Contains(lower, "480p"):
		return "480p"
	default:
		return ""
	}
}

func atoiSlice(source string, start int, end int) int {
	value, _ := strconv.Atoi(source[start:end])
	return value
}

var (
	sxeRE       = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])s(\d{1,2})e(\d{1,3})(?:\s*[-._]?\s*e?(\d{1,3}))?`)
	oneXRE      = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(\d{1,2})x(\d{1,3})`)
	seasonDirRE = regexp.MustCompile(`(?i)season[\s._-]*(\d{1,2})`)
	tvNoiseRE   = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|4320p|8k|4k|uhd|hdr|hdr10|dv|dolby\s+vision|web[-\s]?dl|web[-\s]?rip|hdtv|bluray|blu\s*ray|x264|x265|h264|h265|hevc|aac|dts|truehd|atmos|proper|repack)\b`)
)
