package migration

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EJM-Digital/Xuva/server/internal/database"
	"github.com/EJM-Digital/Xuva/server/internal/events"
)

func TestDryRunIdentifiesConflictsBeforeChanges(t *testing.T) {
	service, db := newTestService(t)
	seedMigrationCatalog(t, db)
	payload := readFixture(t, "plex-watch-history.json")

	report, err := service.DryRun(context.Background(), Request{Payload: payload, Scopes: []string{ScopePlayback, ScopeMetadata}, UserID: "local"})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if report.Schema != SchemaV1 || report.Source != "plex" {
		t.Fatalf("expected parsed bundle identity, got %#v", report)
	}
	if report.Summary.Total != 3 || report.Summary.Importable != 2 || report.Summary.Conflicted != 1 {
		t.Fatalf("expected dry-run summary with one conflict, got %#v", report.Summary)
	}
	if countRows(t, db.DB(), `SELECT count(*) FROM playback_states`) != 0 {
		t.Fatalf("expected dry run to leave playback states untouched")
	}
	missing := report.Items[2]
	if missing.Outcome != "conflict" || missing.ReasonCode == "" {
		t.Fatalf("expected missing item conflict classification, got %#v", missing)
	}
}

func TestImportPreservesWatchedResumeAndVerifies(t *testing.T) {
	service, db := newTestService(t)
	seedMigrationCatalog(t, db)
	payload := readFixture(t, "plex-watch-history.json")

	report, err := service.Import(context.Background(), Request{
		Payload:            payload,
		Scopes:             []string{ScopePlayback, ScopeMetadata},
		UserID:             "local",
		SelectedImportKeys: []string{"plex-heat", "plex-bear-s1e1"},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if report.Status != "completed" || report.RunID == "" {
		t.Fatalf("expected completed migration run, got %#v", report)
	}
	if report.Summary.Imported != 2 || report.Verification.Passed != 2 || report.Verification.Failed != 0 {
		t.Fatalf("expected verified import summary, got %#v %#v", report.Summary, report.Verification)
	}
	if countRows(t, db.DB(), `SELECT count(*) FROM playback_states WHERE user_id = 'local'`) != 2 {
		t.Fatalf("expected playback state rows for imported items")
	}
	if countRows(t, db.DB(), `SELECT count(*) FROM metadata_external_ids WHERE kind = 'movie' AND item_id = 'movie_heat' AND provider = 'imdb'`) != 1 {
		t.Fatalf("expected movie imdb id to be imported")
	}
	if countRows(t, db.DB(), `SELECT count(*) FROM metadata_external_ids WHERE kind = 'series' AND item_id = 'series_bear' AND provider = 'tvdb'`) != 1 {
		t.Fatalf("expected series tvdb id to be preserved/imported from episode migration row")
	}
	stored, ok, err := service.GetRun(context.Background(), report.RunID)
	if err != nil || !ok {
		t.Fatalf("expected stored migration run, got ok=%v err=%v", ok, err)
	}
	if len(stored.Items) != 3 {
		t.Fatalf("expected stored run items, got %#v", stored)
	}
}

func TestImportFailureLeavesNoPartialCorruptionAndRollbackRestoresState(t *testing.T) {
	service, db := newTestService(t)
	seedMigrationCatalog(t, db)
	seedPlaybackState(t, db.DB(), "local", "source_heat_1", true, 1200, 10200, "2026-03-01T10:00:00Z")
	payload := readFixture(t, "plex-watch-history.json")

	_, err := service.Import(context.Background(), Request{
		Payload:            payload,
		Scopes:             []string{ScopePlayback, ScopeMetadata},
		UserID:             "local",
		SelectedImportKeys: []string{"plex-heat", "plex-missing"},
	})
	if err == nil || !strings.Contains(err.Error(), "not importable") {
		t.Fatalf("expected conflict-selected import to fail safely, got %v", err)
	}
	progress := queryFloat(t, db.DB(), `SELECT progress_seconds FROM playback_states WHERE user_id = 'local' AND media_source_id = 'source_heat_1'`)
	if progress != 1200 {
		t.Fatalf("expected failed import to leave prior progress intact, got %v", progress)
	}

	successReport, err := service.Import(context.Background(), Request{
		Payload:            readFixture(t, "jellyfin-watch-history.json"),
		Scopes:             []string{ScopePlayback, ScopeMetadata},
		UserID:             "local",
		SelectedImportKeys: []string{"jf-heat"},
	})
	if err != nil {
		t.Fatalf("successful import: %v", err)
	}
	progress = queryFloat(t, db.DB(), `SELECT progress_seconds FROM playback_states WHERE user_id = 'local' AND media_source_id = 'source_heat_1'`)
	if progress != 3600 {
		t.Fatalf("expected imported progress to replace prior value, got %v", progress)
	}
	rolledBack, err := service.Rollback(context.Background(), successReport.RunID)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rolledBack.Status != "rolled_back" || rolledBack.Summary.RolledBack == 0 {
		t.Fatalf("expected rollback report, got %#v", rolledBack)
	}
	progress = queryFloat(t, db.DB(), `SELECT progress_seconds FROM playback_states WHERE user_id = 'local' AND media_source_id = 'source_heat_1'`)
	if progress != 1200 {
		t.Fatalf("expected rollback to restore prior progress, got %v", progress)
	}
}

func newTestService(t *testing.T) (*Service, *database.Service) {
	t.Helper()
	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewService(db, events.NewBus(16)), db
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

func seedMigrationCatalog(t *testing.T, db *database.Service) {
	t.Helper()
	execSQL(t, db.DB(), `INSERT INTO libraries(id, kind, name, path, storage_type, updated_at) VALUES('movies', 'movies', 'Movies', 'C:/Media/Movies', 'local', '2026-04-30T00:00:00Z')`)
	execSQL(t, db.DB(), `INSERT INTO libraries(id, kind, name, path, storage_type, updated_at) VALUES('tv', 'tv', 'TV Shows', 'C:/Media/TV', 'local', '2026-04-30T00:00:00Z')`)

	execSQL(t, db.DB(), `INSERT INTO media_sources(id, library_id, kind, path, rel_path, name, extension, size_bytes, modified_at, discovered_at, updated_at) VALUES('source_heat_1', 'movies', 'movie', 'C:/Media/Movies/Heat (1995)/Heat.1995.1080p.BluRay.mkv', 'Heat (1995)/Heat.1995.1080p.BluRay.mkv', 'Heat.1995.1080p.BluRay.mkv', '.mkv', 100, '2026-04-01T00:00:00Z', '2026-04-01T00:00:00Z', '2026-04-01T00:00:00Z')`)
	execSQL(t, db.DB(), `INSERT INTO media_sources(id, library_id, kind, path, rel_path, name, extension, size_bytes, modified_at, discovered_at, updated_at) VALUES('source_bear_1', 'tv', 'episode', 'C:/Media/TV/The Bear/Season 01/The.Bear.S01E01.1080p.WEB-DL.mkv', 'The Bear/Season 01/The.Bear.S01E01.1080p.WEB-DL.mkv', 'The.Bear.S01E01.1080p.WEB-DL.mkv', '.mkv', 100, '2026-04-01T00:00:00Z', '2026-04-01T00:00:00Z', '2026-04-01T00:00:00Z')`)

	execSQL(t, db.DB(), `INSERT INTO movies(id, title, year, sort_title, needs_review, review_reason, updated_at) VALUES('movie_heat', 'Heat', 1995, 'heat', 0, '', '2026-04-01T00:00:00Z')`)
	execSQL(t, db.DB(), `INSERT INTO movie_versions(movie_id, media_source_id, edition, quality_label, updated_at) VALUES('movie_heat', 'source_heat_1', '', '1080p', '2026-04-01T00:00:00Z')`)

	execSQL(t, db.DB(), `INSERT INTO tv_series(id, title, sort_title, updated_at) VALUES('series_bear', 'The Bear', 'the bear', '2026-04-01T00:00:00Z')`)
	execSQL(t, db.DB(), `INSERT INTO tv_seasons(id, series_id, season_number, updated_at) VALUES('season_bear_1', 'series_bear', 1, '2026-04-01T00:00:00Z')`)
	execSQL(t, db.DB(), `INSERT INTO tv_episodes(id, series_id, season_id, season_number, episode_number, episode_end, title, needs_review, review_reason, updated_at) VALUES('episode_bear_s1e1', 'series_bear', 'season_bear_1', 1, 1, 1, 'System', 0, '', '2026-04-01T00:00:00Z')`)
	execSQL(t, db.DB(), `INSERT INTO episode_versions(episode_id, media_source_id, quality_label, updated_at) VALUES('episode_bear_s1e1', 'source_bear_1', '1080p', '2026-04-01T00:00:00Z')`)

	execSQL(t, db.DB(), `INSERT INTO metadata_external_ids(kind, item_id, provider, external_id, updated_at) VALUES('movie', 'movie_heat', 'tmdb', '949', '2026-04-01T00:00:00Z')`)
	execSQL(t, db.DB(), `INSERT INTO metadata_external_ids(kind, item_id, provider, external_id, updated_at) VALUES('series', 'series_bear', 'tvdb', '411211', '2026-04-01T00:00:00Z')`)
}

func seedPlaybackState(t *testing.T, db *sql.DB, userID string, mediaSourceID string, watched bool, progress float64, duration float64, playedAt string) {
	t.Helper()
	execSQL(t, db, `INSERT INTO playback_states(user_id, media_source_id, watched, progress_seconds, duration_seconds, last_played_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		userID, mediaSourceID, boolInt(watched), progress, duration, playedAt, playedAt)
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

func queryFloat(t *testing.T, db *sql.DB, query string, args ...any) float64 {
	t.Helper()
	var value float64
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&value); err != nil {
		t.Fatalf("query float: %v", err)
	}
	return value
}

func execSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec sql: %v\nquery: %s", err, query)
	}
}
