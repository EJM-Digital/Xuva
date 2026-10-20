package movies

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/media"
	"github.com/EJM-Digital/Xuva/server/internal/scanner"
)

type Movie struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	SortTitle string `json:"sortTitle"`
}

type Version struct {
	MovieID       string         `json:"movieId"`
	MediaSourceID media.SourceID `json:"mediaSourceId"`
	Edition       string         `json:"edition,omitempty"`
	QualityLabel  string         `json:"qualityLabel,omitempty"`
}

type Candidate struct {
	Title        string                `json:"title"`
	Year         int                   `json:"year,omitempty"`
	Edition      string                `json:"edition,omitempty"`
	QualityLabel string                `json:"qualityLabel,omitempty"`
	NeedsReview  bool                  `json:"needsReview"`
	ReviewReason string                `json:"reviewReason,omitempty"`
	Media        scanner.FileCandidate `json:"media"`
}

type Service struct{}

func NewService() *Service {
	return &Service{}
}

func (s *Service) Classify(files []scanner.FileCandidate) []Candidate {
	candidates := make([]Candidate, 0, len(files))
	for _, file := range files {
		candidates = append(candidates, classifyMovie(file))
	}
	return candidates
}

func classifyMovie(file scanner.FileCandidate) Candidate {
	source := movieSourceName(file)
	fullSource := source + " " + file.Name
	year, titleSource := extractMovieYear(source)
	title := cleanMovieTitle(titleSource)
	candidate := Candidate{
		Title:        title,
		Year:         year,
		Edition:      detectMovieEdition(fullSource),
		QualityLabel: detectMovieQuality(fullSource),
		Media:        file,
	}
	if candidate.Title == "" {
		candidate.Title = cleanMovieTitle(strings.TrimSuffix(file.Name, filepath.Ext(file.Name)))
	}
	if candidate.Title == "" {
		candidate.NeedsReview = true
		candidate.ReviewReason = "unable to infer movie title"
	}
	if candidate.Year == 0 {
		candidate.NeedsReview = true
		if candidate.ReviewReason == "" {
			candidate.ReviewReason = "unable to infer movie year"
		}
	}
	return candidate
}

func movieSourceName(file scanner.FileCandidate) string {
	parent := filepath.Base(filepath.Dir(file.RelPath))
	fileName := strings.TrimSuffix(file.Name, filepath.Ext(file.Name))
	if parent != "." && parent != string(filepath.Separator) && parent != "" {
		if year, _ := extractMovieYear(parent); year != 0 {
			return parent
		}
	}
	return fileName
}

func extractMovieYear(source string) (int, string) {
	matches := yearRE.FindAllStringIndex(source, -1)
	maxYear := time.Now().Year() + 2
	for i := len(matches) - 1; i >= 0; i-- {
		raw := source[matches[i][0]:matches[i][1]]
		year, err := strconv.Atoi(raw)
		if err != nil {
			continue
		}
		if year >= 1888 && year <= maxYear {
			return year, source[:matches[i][0]]
		}
	}
	return 0, source
}

func cleanMovieTitle(source string) string {
	cleaned := cleanupTitle(source)
	cleaned = movieNoiseRE.ReplaceAllString(cleaned, " ")
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	return cleaned
}

func detectMovieEdition(source string) string {
	lower := strings.ToLower(source)
	switch {
	case strings.Contains(lower, "director") && strings.Contains(lower, "cut"):
		return "Director's Cut"
	case strings.Contains(lower, "extended"):
		return "Extended"
	case strings.Contains(lower, "theatrical"):
		return "Theatrical"
	case strings.Contains(lower, "unrated"):
		return "Unrated"
	case strings.Contains(lower, "imax"):
		return "IMAX"
	case strings.Contains(lower, "remastered"):
		return "Remastered"
	default:
		return ""
	}
}

func detectMovieQuality(source string) string {
	lower := strings.ToLower(source)
	parts := make([]string, 0, 2)
	switch {
	case strings.Contains(lower, "4320p") || strings.Contains(lower, "8k"):
		parts = append(parts, "8K")
	case strings.Contains(lower, "2160p") || strings.Contains(lower, "uhd") || strings.Contains(lower, "4k"):
		parts = append(parts, "4K")
	case strings.Contains(lower, "1080p"):
		parts = append(parts, "1080p")
	case strings.Contains(lower, "720p"):
		parts = append(parts, "720p")
	case strings.Contains(lower, "480p"):
		parts = append(parts, "480p")
	}
	switch {
	case strings.Contains(lower, "remux"):
		parts = append(parts, "Remux")
	case strings.Contains(lower, "bluray") || strings.Contains(lower, "blu-ray"):
		parts = append(parts, "Blu-ray")
	case strings.Contains(lower, "web-dl") || strings.Contains(lower, "webdl"):
		parts = append(parts, "WEB-DL")
	case strings.Contains(lower, "webrip"):
		parts = append(parts, "WEBRip")
	}
	return strings.Join(parts, " ")
}

func cleanupTitle(source string) string {
	replacer := strings.NewReplacer(".", " ", "_", " ", "-", " ", "(", " ", ")", " ", "[", " ", "]", " ")
	return replacer.Replace(source)
}

var (
	yearRE       = regexp.MustCompile(`(?:19|20)\d{2}`)
	movieNoiseRE = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|4320p|8k|4k|uhd|hdr|hdr10|dv|dolby\s+vision|remux|bluray|blu\s*ray|web[-\s]?dl|web[-\s]?rip|hdtv|x264|x265|h264|h265|hevc|aac|dts|truehd|atmos|proper|repack)\b`)
)
