package catalog

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/database"
	"github.com/EJM-Digital/Xuva/server/internal/libraries"
	"github.com/EJM-Digital/Xuva/server/internal/movies"
	"github.com/EJM-Digital/Xuva/server/internal/scanner"
)

func BenchmarkCatalogMovieBrowse(b *testing.B) {
	ctx := context.Background()
	service := newBenchmarkService(b)
	library := libraries.Library{ID: "movies", Name: "Movies", Path: `X:\Movies`, Kind: libraries.KindMovies}
	files := make([]scanner.FileCandidate, 0, 1000)
	candidates := make([]movies.Candidate, 0, 1000)
	for i := 0; i < 1000; i++ {
		title := fmt.Sprintf("Movie %04d", i)
		file := scanner.FileCandidate{
			Path:       filepath.Clean(fmt.Sprintf(`X:\Movies\%s (2024)\%s.2024.mkv`, title, title)),
			RelPath:    filepath.Clean(fmt.Sprintf(`%s (2024)\%s.2024.mkv`, title, title)),
			Name:       title + ".2024.mkv",
			Extension:  ".mkv",
			Size:       1024,
			ModifiedAt: time.Now().UTC(),
			Changed:    true,
		}
		files = append(files, file)
		candidates = append(candidates, movies.Candidate{Title: title, Year: 2024, QualityLabel: "1080p", Media: file})
	}
	if _, err := service.SaveMovieScan(ctx, library, scanResult(scanner.KindMovies, library.Path, files), candidates); err != nil {
		b.Fatalf("save scan: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := service.ListMovies(ctx, 100, "", ""); err != nil {
			b.Fatalf("list movies: %v", err)
		}
	}
}

func BenchmarkCatalogMovieScanPersistFull(b *testing.B) {
	ctx := context.Background()
	library, files, candidates := benchmarkMovieCandidates(1000)
	service := newBenchmarkService(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := service.SaveMovieScan(ctx, library, scanResult(scanner.KindMovies, library.Path, files), candidates); err != nil {
			b.Fatalf("save full scan: %v", err)
		}
	}
}

func BenchmarkCatalogMovieScanPersistIncrementalLowChange(b *testing.B) {
	ctx := context.Background()
	library, files, candidates := benchmarkMovieCandidates(1000)
	service := newBenchmarkService(b)
	if _, err := service.SaveMovieScan(ctx, library, scanResult(scanner.KindMovies, library.Path, files), candidates); err != nil {
		b.Fatalf("seed full scan: %v", err)
	}
	for index := range files {
		files[index].Changed = false
	}
	files[0].Changed = true
	candidates = []movies.Candidate{{Title: "Movie 0000", Year: 2024, QualityLabel: "1080p", Media: files[0]}}
	result := scanResult(scanner.KindMovies, library.Path, files)
	result.Files = []scanner.FileCandidate{files[0]}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := service.SaveMovieScan(ctx, library, result, candidates); err != nil {
			b.Fatalf("save incremental scan: %v", err)
		}
	}
}

func benchmarkMovieCandidates(count int) (libraries.Library, []scanner.FileCandidate, []movies.Candidate) {
	library := libraries.Library{ID: "movies", Name: "Movies", Path: `X:\Movies`, Kind: libraries.KindMovies}
	files := make([]scanner.FileCandidate, 0, count)
	candidates := make([]movies.Candidate, 0, count)
	for i := 0; i < count; i++ {
		title := fmt.Sprintf("Movie %04d", i)
		file := scanner.FileCandidate{
			Path:       filepath.Clean(fmt.Sprintf(`X:\Movies\%s (2024)\%s.2024.mkv`, title, title)),
			RelPath:    filepath.Clean(fmt.Sprintf(`%s (2024)\%s.2024.mkv`, title, title)),
			Name:       title + ".2024.mkv",
			Extension:  ".mkv",
			Size:       1024,
			ModifiedAt: time.Now().UTC(),
			Changed:    true,
		}
		files = append(files, file)
		candidates = append(candidates, movies.Candidate{Title: title, Year: 2024, QualityLabel: "1080p", Media: file})
	}
	return library, files, candidates
}

// BenchmarkSearchLibraryPeople measures the people search path against the
// materialized people/people_credits tables introduced in issue #85.
func BenchmarkSearchLibraryPeople(b *testing.B) {
	ctx := context.Background()
	service := newBenchmarkService(b)
	library := libraries.Library{ID: "movies", Name: "Movies", Path: `X:\Movies`, Kind: libraries.KindMovies}
	// Seed 500 movies each with a 10-person cast so the people table is
	// representative of a mid-size library.
	firstNames := []string{"Alice", "Bob", "Carol", "David", "Eva", "Frank", "Grace", "Henry", "Iris", "Jack"}
	for i := 0; i < 500; i++ {
		title := fmt.Sprintf("Bench Movie %04d", i)
		file := scanner.FileCandidate{
			Path:       filepath.Clean(fmt.Sprintf(`X:\Movies\%s (2024)\%s.mkv`, title, title)),
			RelPath:    filepath.Clean(fmt.Sprintf(`%s (2024)\%s.mkv`, title, title)),
			Name:       title + ".mkv",
			Extension:  ".mkv",
			Size:       1024,
			ModifiedAt: time.Now().UTC(),
			Changed:    true,
		}
		if _, err := service.SaveMovieScan(ctx, library, scanResult(scanner.KindMovies, library.Path, []scanner.FileCandidate{file}),
			[]movies.Candidate{{Title: title, Year: 2024, QualityLabel: "1080p", Media: file}}); err != nil {
			b.Fatalf("seed movie: %v", err)
		}
		cast := make([]MetadataCredit, len(firstNames))
		for j, n := range firstNames {
			cast[j] = MetadataCredit{Name: fmt.Sprintf("%s Smith-%04d", n, i), Character: "Role"}
		}
		movieList, err := service.ListMovies(ctx, 10000, "", "")
		if err != nil {
			b.Fatalf("list movies: %v", err)
		}
		for _, m := range movieList {
			if m.Title != title {
				continue
			}
			if err := service.UpsertMetadataRecord(ctx, MetadataRecord{
				Kind: "movie", ItemID: m.ID, Provider: "tmdb",
				Title: title, Confidence: 0.9, Cast: cast,
			}); err != nil {
				b.Fatalf("upsert metadata: %v", err)
			}
			break
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := service.SearchLibrary(ctx, "Alice", 8, ""); err != nil {
			b.Fatalf("search: %v", err)
		}
	}
}

func newBenchmarkService(b *testing.B) *Service {
	b.Helper()
	db, err := database.Open(context.Background(), b.TempDir())
	if err != nil {
		b.Fatalf("open database: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	return NewService(db)
}


