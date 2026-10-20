package movies

import (
	"testing"

	"github.com/EJM-Digital/Xuva/server/internal/scanner"
)

func TestClassifyMovieUsesFolderYearAndQuality(t *testing.T) {
	files := []scanner.FileCandidate{{
		Name:      "Blade.Runner.2049.2017.2160p.REMUX.mkv",
		RelPath:   "Blade Runner 2049 (2017)/Blade.Runner.2049.2017.2160p.REMUX.mkv",
		Extension: ".mkv",
	}}

	candidates := NewService().Classify(files)
	if len(candidates) != 1 {
		t.Fatalf("expected one candidate, got %d", len(candidates))
	}
	candidate := candidates[0]
	if candidate.Title != "Blade Runner 2049" {
		t.Fatalf("expected title Blade Runner 2049, got %q", candidate.Title)
	}
	if candidate.Year != 2017 {
		t.Fatalf("expected year 2017, got %d", candidate.Year)
	}
	if candidate.QualityLabel != "4K Remux" {
		t.Fatalf("expected quality 4K Remux, got %q", candidate.QualityLabel)
	}
	if candidate.NeedsReview {
		t.Fatalf("expected confident classification: %#v", candidate)
	}
}

func TestClassifyMovieMarksMissingYearForReview(t *testing.T) {
	files := []scanner.FileCandidate{{
		Name:      "Unknown.Movie.mkv",
		RelPath:   "Unknown.Movie.mkv",
		Extension: ".mkv",
	}}

	candidate := NewService().Classify(files)[0]
	if !candidate.NeedsReview {
		t.Fatal("expected missing year to need review")
	}
}
