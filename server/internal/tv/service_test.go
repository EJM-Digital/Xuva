package tv

import (
	"testing"

	"github.com/EJM-Digital/Xuva/server/internal/scanner"
)

func TestClassifyEpisodeUsesSeriesSeasonEpisode(t *testing.T) {
	files := []scanner.FileCandidate{{
		Name:      "The.Bear.S02E03.Sundae.1080p.WEB-DL.mkv",
		RelPath:   "The Bear/Season 02/The.Bear.S02E03.Sundae.1080p.WEB-DL.mkv",
		Extension: ".mkv",
	}}

	candidates := NewService().Classify(files)
	if len(candidates) != 1 {
		t.Fatalf("expected one candidate, got %d", len(candidates))
	}
	candidate := candidates[0]
	if candidate.SeriesTitle != "The Bear" {
		t.Fatalf("expected The Bear, got %q", candidate.SeriesTitle)
	}
	if candidate.SeasonNumber != 2 {
		t.Fatalf("expected season 2, got %d", candidate.SeasonNumber)
	}
	if candidate.EpisodeNumber != 3 {
		t.Fatalf("expected episode 3, got %d", candidate.EpisodeNumber)
	}
	if candidate.EpisodeTitle != "Sundae" {
		t.Fatalf("expected episode title Sundae, got %q", candidate.EpisodeTitle)
	}
	if candidate.NeedsReview {
		t.Fatalf("expected confident classification: %#v", candidate)
	}
}

func TestClassifyEpisodeMarksUnmatchedFileForReview(t *testing.T) {
	files := []scanner.FileCandidate{{
		Name:      "Special.Video.mkv",
		RelPath:   "Series/Special.Video.mkv",
		Extension: ".mkv",
	}}

	candidate := NewService().Classify(files)[0]
	if !candidate.NeedsReview {
		t.Fatal("expected unmatched episode to need review")
	}
}
