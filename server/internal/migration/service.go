package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/database"
	"github.com/EJM-Digital/Xuva/server/internal/events"
)

const (
	SchemaV1      = "xuva.migration.v1"
	ScopePlayback = "playback"
	ScopeMetadata = "metadata"
)

var (
	ErrInvalidPayload = errors.New("migration payload is invalid")
	ErrRunNotFound    = errors.New("migration run not found")
	ErrRunRollback    = errors.New("migration run cannot be rolled back")
)

type FormatSpec struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	Description     string   `json:"description"`
	Sources         []string `json:"sources"`
	Schema          string   `json:"schema"`
	ValidationRules []string `json:"validationRules"`
}

type Bundle struct {
	Schema    string `json:"schema"`
	Source    string `json:"source"`
	CreatedAt string `json:"createdAt,omitempty"`
	Items     []Item `json:"items"`
}

type Item struct {
	ID              string            `json:"id,omitempty"`
	Kind            string            `json:"kind"`
	Title           string            `json:"title,omitempty"`
	Year            int               `json:"year,omitempty"`
	Path            string            `json:"path,omitempty"`
	SeriesTitle     string            `json:"seriesTitle,omitempty"`
	SeasonNumber    int               `json:"seasonNumber,omitempty"`
	EpisodeNumber   int               `json:"episodeNumber,omitempty"`
	ExternalIDs     map[string]string `json:"externalIds,omitempty"`
	Watched         *bool             `json:"watched,omitempty"`
	ProgressSeconds float64           `json:"progressSeconds,omitempty"`
	DurationSeconds float64           `json:"durationSeconds,omitempty"`
	LastPlayedAt    string            `json:"lastPlayedAt,omitempty"`
}

type Request struct {
	Payload            string   `json:"payload"`
	Scopes             []string `json:"scopes,omitempty"`
	UserID             string   `json:"userId,omitempty"`
	SelectedImportKeys []string `json:"selectedImportKeys,omitempty"`
}

type Summary struct {
	Total      int `json:"total"`
	Importable int `json:"importable"`
	Imported   int `json:"imported"`
	Skipped    int `json:"skipped"`
	Conflicted int `json:"conflicted"`
	Verified   int `json:"verified"`
	Failed     int `json:"failed"`
	RolledBack int `json:"rolledBack"`
}

type Verification struct {
	Checked int `json:"checked"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
}

type Target struct {
	Kind          string `json:"kind,omitempty"`
	ItemID        string `json:"itemId,omitempty"`
	MediaSourceID string `json:"mediaSourceId,omitempty"`
	Title         string `json:"title,omitempty"`
}

type ItemReport struct {
	ImportKey   string            `json:"importKey"`
	Kind        string            `json:"kind"`
	Title       string            `json:"title"`
	Outcome     string            `json:"outcome"`
	ReasonCode  string            `json:"reasonCode,omitempty"`
	ReasonText  string            `json:"reasonText,omitempty"`
	Changes     []string          `json:"changes,omitempty"`
	Target      Target            `json:"target,omitempty"`
	ExternalIDs map[string]string `json:"externalIds,omitempty"`
}

type Report struct {
	RunID        string       `json:"runId,omitempty"`
	Schema       string       `json:"schema"`
	Source       string       `json:"source"`
	Scopes       []string     `json:"scopes"`
	Status       string       `json:"status"`
	CreatedAt    string       `json:"createdAt,omitempty"`
	CompletedAt  string       `json:"completedAt,omitempty"`
	RolledBackAt string       `json:"rolledBackAt,omitempty"`
	Summary      Summary      `json:"summary"`
	Verification Verification `json:"verification"`
	Items        []ItemReport `json:"items"`
	Warnings     []string     `json:"warnings,omitempty"`
	ErrorText    string       `json:"error,omitempty"`
}

type Service struct {
	db     *sql.DB
	events *events.Bus
	nextID atomic.Uint64
}

type playbackBackup struct {
	Exists          bool    `json:"exists"`
	UserID          string  `json:"userId"`
	MediaSourceID   string  `json:"mediaSourceId"`
	Watched         bool    `json:"watched"`
	ProgressSeconds float64 `json:"progressSeconds"`
	DurationSeconds float64 `json:"durationSeconds"`
	LastPlayedAt    string  `json:"lastPlayedAt"`
	UpdatedAt       string  `json:"updatedAt"`
}

type externalIDBackup struct {
	Exists     bool   `json:"exists"`
	Kind       string `json:"kind"`
	ItemID     string `json:"itemId"`
	Provider   string `json:"provider"`
	ExternalID string `json:"externalId"`
	UpdatedAt  string `json:"updatedAt"`
}

func NewService(database *database.Service, eventBus *events.Bus) *Service {
	return &Service{db: database.DB(), events: eventBus}
}

func Formats() []FormatSpec {
	return []FormatSpec{
		{
			ID:          "watch-history-v1",
			Label:       "Watched and Resume Import v1",
			Description: "Import watched status, resume position, and core metadata identifiers from Plex, Emby, Jellyfin, or a normalized export.",
			Sources:     []string{"plex", "emby", "jellyfin", "generic"},
			Schema:      SchemaV1,
			ValidationRules: []string{
				"Payload must be JSON with schema `xuva.migration.v1`.",
				"Each item must target a movie or episode.",
				"Each item needs a path hint, external ID, or title locator that Xuva can match safely.",
				"Playback imports write watched/resume against exactly one local media source version.",
				"Metadata imports write external identifiers only for matched local items.",
			},
		},
	}
}

func (s *Service) DryRun(ctx context.Context, request Request) (Report, error) {
	bundle, err := parseBundle(request.Payload)
	if err != nil {
		return Report{}, err
	}
	report, err := s.analyze(ctx, bundle, request)
	if err != nil {
		return Report{}, err
	}
	report.Status = "dry_run"
	report.CreatedAt = nowUTC()
	s.publish("migration.previewed", map[string]any{
		"source":     report.Source,
		"schema":     report.Schema,
		"scopes":     report.Scopes,
		"summary":    report.Summary,
		"createdAt":  report.CreatedAt,
		"warnings":   report.Warnings,
		"dryRunOnly": true,
	})
	return report, nil
}

func (s *Service) Import(ctx context.Context, request Request) (Report, error) {
	bundle, err := parseBundle(request.Payload)
	if err != nil {
		return Report{}, err
	}
	report, err := s.analyze(ctx, bundle, request)
	if err != nil {
		return Report{}, err
	}
	selected, err := selectedImportKeys(report.Items, request.SelectedImportKeys)
	if err != nil {
		report.Status = "failed"
		report.ErrorText = err.Error()
		return report, err
	}
	if len(selected) == 0 {
		return Report{}, fmt.Errorf("select at least one importable item before running the import")
	}
	sourceByKey := map[string]Item{}
	for index, item := range bundle.Items {
		normalized := normalizeItem(index, item)
		sourceByKey[normalized.ID] = normalized
	}
	runID := s.nextRunID()
	createdAt := nowUTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback()
	report.RunID = runID
	report.Status = "running"
	report.CreatedAt = createdAt
	if err := s.persistRun(ctx, tx, report); err != nil {
		return Report{}, err
	}

	for _, item := range selected {
		if err := s.backupItem(ctx, tx, runID, request.UserID, item); err != nil {
			return Report{}, err
		}
		source, ok := sourceByKey[item.ImportKey]
		if !ok {
			return Report{}, fmt.Errorf("import source for %s is missing from the payload", item.ImportKey)
		}
		if err := s.applyItem(ctx, tx, request.UserID, item, source, createdAt); err != nil {
			return Report{}, err
		}
	}

	verification, err := s.verifyWithinTx(ctx, tx, request.UserID, selected)
	if err != nil {
		return Report{}, err
	}
	report.RunID = runID
	report.Status = "completed"
	report.CreatedAt = createdAt
	report.CompletedAt = createdAt
	report.Verification = verification
	report.Summary.Imported = len(selected)
	report.Summary.Verified = verification.Passed
	report.Summary.Failed = verification.Failed
	if _, err := tx.ExecContext(ctx, `DELETE FROM migration_run_items WHERE run_id = ?`, report.RunID); err != nil {
		return Report{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE migration_runs
		SET status = ?, scopes_json = ?, summary_json = ?, verification_json = ?, created_at = ?, completed_at = ?, rolled_back_at = ?, error_text = ?
		WHERE id = ?
	`, report.Status, mustJSON(report.Scopes), mustJSON(report.Summary), mustJSON(report.Verification), report.CreatedAt, report.CompletedAt, report.RolledBackAt, report.ErrorText, report.RunID); err != nil {
		return Report{}, err
	}
	for _, item := range report.Items {
		payload, _ := json.Marshal(item)
		if _, err := tx.ExecContext(ctx, `INSERT INTO migration_run_items(run_id, import_key, item_json) VALUES(?, ?, ?)`, report.RunID, item.ImportKey, string(payload)); err != nil {
			return Report{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Report{}, err
	}
	s.publish("migration.completed", map[string]any{
		"runId":        report.RunID,
		"source":       report.Source,
		"schema":       report.Schema,
		"scopes":       report.Scopes,
		"summary":      report.Summary,
		"verification": report.Verification,
		"completedAt":  report.CompletedAt,
	})
	return report, nil
}

func (s *Service) ListRuns(ctx context.Context) ([]Report, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source, schema, status, scopes_json, summary_json, verification_json, created_at, completed_at, rolled_back_at, error_text
		FROM migration_runs
		ORDER BY created_at DESC
		LIMIT 20
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	output := []Report{}
	for rows.Next() {
		var report Report
		var scopesJSON, summaryJSON, verificationJSON string
		if err := rows.Scan(&report.RunID, &report.Source, &report.Schema, &report.Status, &scopesJSON, &summaryJSON, &verificationJSON, &report.CreatedAt, &report.CompletedAt, &report.RolledBackAt, &report.ErrorText); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(scopesJSON), &report.Scopes)
		_ = json.Unmarshal([]byte(summaryJSON), &report.Summary)
		_ = json.Unmarshal([]byte(verificationJSON), &report.Verification)
		output = append(output, report)
	}
	return output, rows.Err()
}

func (s *Service) GetRun(ctx context.Context, runID string) (Report, bool, error) {
	var report Report
	var scopesJSON, summaryJSON, verificationJSON string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, source, schema, status, scopes_json, summary_json, verification_json, created_at, completed_at, rolled_back_at, error_text
		FROM migration_runs
		WHERE id = ?
	`, runID).Scan(&report.RunID, &report.Source, &report.Schema, &report.Status, &scopesJSON, &summaryJSON, &verificationJSON, &report.CreatedAt, &report.CompletedAt, &report.RolledBackAt, &report.ErrorText)
	if errors.Is(err, sql.ErrNoRows) {
		return Report{}, false, nil
	}
	if err != nil {
		return Report{}, false, err
	}
	_ = json.Unmarshal([]byte(scopesJSON), &report.Scopes)
	_ = json.Unmarshal([]byte(summaryJSON), &report.Summary)
	_ = json.Unmarshal([]byte(verificationJSON), &report.Verification)
	itemRows, err := s.db.QueryContext(ctx, `
		SELECT item_json
		FROM migration_run_items
		WHERE run_id = ?
		ORDER BY import_key
	`, runID)
	if err != nil {
		return Report{}, false, err
	}
	defer itemRows.Close()
	for itemRows.Next() {
		var raw string
		if err := itemRows.Scan(&raw); err != nil {
			return Report{}, false, err
		}
		var item ItemReport
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			return Report{}, false, err
		}
		report.Items = append(report.Items, item)
	}
	return report, true, itemRows.Err()
}

func (s *Service) Rollback(ctx context.Context, runID string) (Report, error) {
	report, ok, err := s.GetRun(ctx, runID)
	if err != nil {
		return Report{}, err
	}
	if !ok {
		return Report{}, ErrRunNotFound
	}
	if report.Status != "completed" {
		return Report{}, ErrRunRollback
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT scope, backup_json
		FROM migration_backups
		WHERE run_id = ?
		ORDER BY scope, target_kind, target_id, provider, media_source_id
	`, runID)
	if err != nil {
		return Report{}, err
	}
	defer rows.Close()
	var restored int
	for rows.Next() {
		var scope string
		var backupJSON string
		if err := rows.Scan(&scope, &backupJSON); err != nil {
			return Report{}, err
		}
		switch scope {
		case ScopePlayback:
			var backup playbackBackup
			if err := json.Unmarshal([]byte(backupJSON), &backup); err != nil {
				return Report{}, err
			}
			if err := restorePlaybackState(ctx, tx, backup); err != nil {
				return Report{}, err
			}
		case ScopeMetadata:
			var backup externalIDBackup
			if err := json.Unmarshal([]byte(backupJSON), &backup); err != nil {
				return Report{}, err
			}
			if err := restoreExternalID(ctx, tx, backup); err != nil {
				return Report{}, err
			}
		}
		restored++
	}
	rolledBackAt := nowUTC()
	summary := report.Summary
	summary.RolledBack = restored
	summary.Imported = 0
	summary.Verified = 0
	verification := Verification{}
	summaryJSON, _ := json.Marshal(summary)
	verificationJSON, _ := json.Marshal(verification)
	if _, err := tx.ExecContext(ctx, `
		UPDATE migration_runs
		SET status = ?, rolled_back_at = ?, summary_json = ?, verification_json = ?
		WHERE id = ?
	`, "rolled_back", rolledBackAt, string(summaryJSON), string(verificationJSON), runID); err != nil {
		return Report{}, err
	}
	if err := tx.Commit(); err != nil {
		return Report{}, err
	}
	report.Status = "rolled_back"
	report.RolledBackAt = rolledBackAt
	report.Summary = summary
	report.Verification = verification
	s.publish("migration.rolled_back", map[string]any{
		"runId":        report.RunID,
		"rolledBackAt": report.RolledBackAt,
		"summary":      report.Summary,
	})
	return report, nil
}

func parseBundle(payload string) (Bundle, error) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return Bundle{}, fmt.Errorf("%w: payload is required", ErrInvalidPayload)
	}
	var bundle Bundle
	if err := json.Unmarshal([]byte(payload), &bundle); err != nil {
		return Bundle{}, fmt.Errorf("%w: %s", ErrInvalidPayload, err.Error())
	}
	bundle.Schema = strings.TrimSpace(bundle.Schema)
	bundle.Source = strings.ToLower(strings.TrimSpace(bundle.Source))
	if bundle.Schema != SchemaV1 {
		return Bundle{}, fmt.Errorf("%w: supported schema is %s", ErrInvalidPayload, SchemaV1)
	}
	switch bundle.Source {
	case "plex", "emby", "jellyfin", "generic":
	default:
		return Bundle{}, fmt.Errorf("%w: source must be plex, emby, jellyfin, or generic", ErrInvalidPayload)
	}
	if len(bundle.Items) == 0 {
		return Bundle{}, fmt.Errorf("%w: at least one item is required", ErrInvalidPayload)
	}
	return bundle, nil
}

func normalizeScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return []string{ScopePlayback, ScopeMetadata}
	}
	set := map[string]struct{}{}
	for _, scope := range scopes {
		switch strings.ToLower(strings.TrimSpace(scope)) {
		case ScopePlayback, ScopeMetadata:
			set[strings.ToLower(strings.TrimSpace(scope))] = struct{}{}
		}
	}
	output := make([]string, 0, len(set))
	for _, scope := range []string{ScopePlayback, ScopeMetadata} {
		if _, ok := set[scope]; ok {
			output = append(output, scope)
		}
	}
	if len(output) == 0 {
		return []string{ScopePlayback, ScopeMetadata}
	}
	return output
}

func (s *Service) analyze(ctx context.Context, bundle Bundle, request Request) (Report, error) {
	report := Report{
		Schema: bundle.Schema,
		Source: bundle.Source,
		Scopes: normalizeScopes(request.Scopes),
		Status: "preview_ready",
		Items:  make([]ItemReport, 0, len(bundle.Items)),
	}
	seenTargets := map[string]string{}
	for index, item := range bundle.Items {
		entry := normalizeItem(index, item)
		analysis, err := s.analyzeItem(ctx, entry, report.Scopes)
		if err != nil {
			return Report{}, err
		}
		if analysis.Outcome == "importable" {
			targetKey := strings.Join([]string{analysis.Target.Kind, analysis.Target.ItemID, analysis.Target.MediaSourceID, strings.Join(analysis.Changes, ",")}, "|")
			if existing, exists := seenTargets[targetKey]; exists {
				analysis.Outcome = "conflict"
				analysis.ReasonCode = "duplicate_target"
				analysis.ReasonText = "Multiple import rows point at the same local target. Remove the duplicate before importing."
				analysis.Changes = nil
				report.Warnings = append(report.Warnings, "Duplicate import target detected for "+analysis.Title+" and "+existing+".")
			} else {
				seenTargets[targetKey] = analysis.Title
			}
		}
		report.Items = append(report.Items, analysis)
	}
	report.Summary = summarizeItems(report.Items)
	return report, nil
}

func normalizeItem(index int, item Item) Item {
	item.Kind = strings.ToLower(strings.TrimSpace(item.Kind))
	item.Title = strings.TrimSpace(item.Title)
	item.Path = strings.TrimSpace(item.Path)
	item.SeriesTitle = strings.TrimSpace(item.SeriesTitle)
	item.ID = strings.TrimSpace(item.ID)
	if item.ID == "" {
		item.ID = fmt.Sprintf("item-%03d", index+1)
	}
	if item.ExternalIDs == nil {
		item.ExternalIDs = map[string]string{}
	}
	normalizedIDs := map[string]string{}
	for key, value := range item.ExternalIDs {
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			normalizedIDs[key] = value
		}
	}
	item.ExternalIDs = normalizedIDs
	return item
}

func (s *Service) analyzeItem(ctx context.Context, item Item, scopes []string) (ItemReport, error) {
	report := ItemReport{
		ImportKey:   item.ID,
		Kind:        item.Kind,
		Title:       entryTitle(item),
		ExternalIDs: item.ExternalIDs,
	}
	if item.Kind != "movie" && item.Kind != "episode" {
		report.Outcome = "conflict"
		report.ReasonCode = "unsupported_kind"
		report.ReasonText = "Xuva can import only movie and episode history in this migration format."
		return report, nil
	}
	changes := plannedChanges(item, scopes)
	if len(changes) == 0 {
		report.Outcome = "skipped"
		report.ReasonCode = "no_selected_data"
		report.ReasonText = "This row does not include any data for the selected import scopes."
		return report, nil
	}
	target, outcome, reasonCode, reasonText, err := s.matchTarget(ctx, item, changes)
	if err != nil {
		return ItemReport{}, err
	}
	report.Target = target
	report.Changes = changes
	report.Outcome = outcome
	report.ReasonCode = reasonCode
	report.ReasonText = reasonText
	return report, nil
}

func plannedChanges(item Item, scopes []string) []string {
	output := []string{}
	hasPlayback := item.Watched != nil || item.ProgressSeconds > 0 || item.DurationSeconds > 0 || strings.TrimSpace(item.LastPlayedAt) != ""
	hasMetadata := len(item.ExternalIDs) > 0
	for _, scope := range scopes {
		if scope == ScopePlayback && hasPlayback {
			output = append(output, ScopePlayback)
		}
		if scope == ScopeMetadata && hasMetadata {
			output = append(output, ScopeMetadata)
		}
	}
	return output
}

func (s *Service) matchTarget(ctx context.Context, item Item, changes []string) (Target, string, string, string, error) {
	switch item.Kind {
	case "movie":
		return s.matchMovie(ctx, item, changes)
	case "episode":
		return s.matchEpisode(ctx, item, changes)
	default:
		return Target{}, "conflict", "unsupported_kind", "Unsupported import item kind.", nil
	}
}

func (s *Service) matchMovie(ctx context.Context, item Item, changes []string) (Target, string, string, string, error) {
	if item.Path != "" {
		targets, err := s.findMovieByPath(ctx, item.Path)
		if err != nil {
			return Target{}, "", "", "", err
		}
		return pickSingleTarget(targets, "conflict", "movie_path_not_found", "No local movie version matched the imported path.", changes, item.Kind)
	}
	if len(item.ExternalIDs) > 0 {
		targets, err := s.findMovieByExternalIDs(ctx, item.ExternalIDs)
		if err != nil {
			return Target{}, "", "", "", err
		}
		targets, err = s.hydrateMovieTargets(ctx, targets, changes)
		if err != nil {
			return Target{}, "", "", "", err
		}
		if len(targets) > 0 {
			return pickSingleTarget(targets, "conflict", "movie_external_id_not_found", "No local movie matched the imported external identifiers.", changes, item.Kind)
		}
	}
	if item.Title != "" && item.Year > 0 {
		targets, err := s.findMovieByTitleYear(ctx, item.Title, item.Year)
		if err != nil {
			return Target{}, "", "", "", err
		}
		targets, err = s.hydrateMovieTargets(ctx, targets, changes)
		if err != nil {
			return Target{}, "", "", "", err
		}
		return pickSingleTarget(targets, "conflict", "movie_title_year_not_found", "No local movie matched the imported title and year.", changes, item.Kind)
	}
	return Target{}, "conflict", "movie_locator_missing", "Add a path, external ID, or title and year so Xuva can find the local movie safely.", nil
}

func (s *Service) matchEpisode(ctx context.Context, item Item, changes []string) (Target, string, string, string, error) {
	if item.Path != "" {
		targets, err := s.findEpisodeByPath(ctx, item.Path)
		if err != nil {
			return Target{}, "", "", "", err
		}
		return pickSingleTarget(targets, "conflict", "episode_path_not_found", "No local episode version matched the imported path.", changes, item.Kind)
	}
	if len(item.ExternalIDs) > 0 && item.SeasonNumber > 0 && item.EpisodeNumber > 0 {
		targets, err := s.findEpisodeBySeriesExternalIDs(ctx, item.ExternalIDs, item.SeasonNumber, item.EpisodeNumber)
		if err != nil {
			return Target{}, "", "", "", err
		}
		targets, err = s.hydrateEpisodeTargets(ctx, targets, changes)
		if err != nil {
			return Target{}, "", "", "", err
		}
		if len(targets) > 0 {
			return pickSingleTarget(targets, "conflict", "episode_external_id_not_found", "No local episode matched the imported series identifiers.", changes, item.Kind)
		}
	}
	if item.SeriesTitle != "" && item.SeasonNumber > 0 && item.EpisodeNumber > 0 {
		targets, err := s.findEpisodeBySeriesTitle(ctx, item.SeriesTitle, item.SeasonNumber, item.EpisodeNumber)
		if err != nil {
			return Target{}, "", "", "", err
		}
		targets, err = s.hydrateEpisodeTargets(ctx, targets, changes)
		if err != nil {
			return Target{}, "", "", "", err
		}
		return pickSingleTarget(targets, "conflict", "episode_locator_not_found", "No local episode matched the imported series title, season, and episode numbers.", changes, item.Kind)
	}
	return Target{}, "conflict", "episode_locator_missing", "Add a path or series locator so Xuva can find the local episode safely.", nil
}

func pickSingleTarget(targets []Target, notFoundOutcome string, notFoundCode string, notFoundText string, changes []string, kind string) (Target, string, string, string, error) {
	if len(targets) == 0 {
		return Target{}, "conflict", notFoundCode, notFoundText, nil
	}
	if len(targets) > 1 {
		return Target{}, "conflict", "ambiguous_match", "More than one local item matched this import row. Narrow it with a version path or stronger identifiers.", nil
	}
	target := targets[0]
	if requiresMediaSource(changes) && target.MediaSourceID == "" {
		return Target{}, "conflict", "version_required", "This import row needs one exact local file version before watched or resume state can be written.", nil
	}
	if !requiresMediaSource(changes) {
		target.MediaSourceID = ""
	}
	return target, "importable", "ready", "Ready to import.", nil
}

func requiresMediaSource(changes []string) bool {
	for _, change := range changes {
		if change == ScopePlayback {
			return true
		}
	}
	return false
}

func selectedImportKeys(items []ItemReport, requested []string) ([]ItemReport, error) {
	if len(requested) == 0 {
		requested = []string{}
		for _, item := range items {
			if item.Outcome == "importable" {
				requested = append(requested, item.ImportKey)
			}
		}
	}
	allowed := map[string]ItemReport{}
	for _, item := range items {
		allowed[item.ImportKey] = item
	}
	selected := []ItemReport{}
	for _, key := range requested {
		item, ok := allowed[key]
		if !ok {
			return nil, fmt.Errorf("selected import key %q is not present in the dry-run report", key)
		}
		if item.Outcome != "importable" {
			return nil, fmt.Errorf("selected import key %q is not importable: %s", key, firstNonEmpty(item.ReasonText, item.ReasonCode))
		}
		selected = append(selected, item)
	}
	return selected, nil
}

func summarizeItems(items []ItemReport) Summary {
	summary := Summary{Total: len(items)}
	for _, item := range items {
		switch item.Outcome {
		case "importable":
			summary.Importable++
		case "skipped":
			summary.Skipped++
		case "conflict":
			summary.Conflicted++
		}
	}
	return summary
}

func (s *Service) backupItem(ctx context.Context, tx *sql.Tx, runID string, userID string, item ItemReport) error {
	if userID == "" {
		userID = "local"
	}
	for _, change := range item.Changes {
		switch change {
		case ScopePlayback:
			backup, err := s.loadPlaybackBackup(ctx, tx, userID, item.Target.MediaSourceID)
			if err != nil {
				return err
			}
			payload, _ := json.Marshal(backup)
			if _, err := tx.ExecContext(ctx, `
				INSERT OR REPLACE INTO migration_backups(run_id, scope, target_kind, target_id, provider, media_source_id, backup_json)
				VALUES(?, ?, ?, ?, '', ?, ?)
			`, runID, ScopePlayback, item.Target.Kind, item.Target.ItemID, item.Target.MediaSourceID, string(payload)); err != nil {
				return err
			}
		case ScopeMetadata:
			targetKind, targetID, err := s.metadataDestination(ctx, tx, item.Target)
			if err != nil {
				return err
			}
			for provider := range item.ExternalIDs {
				backup, err := s.loadExternalIDBackup(ctx, tx, targetKind, targetID, provider)
				if err != nil {
					return err
				}
				payload, _ := json.Marshal(backup)
				if _, err := tx.ExecContext(ctx, `
					INSERT OR REPLACE INTO migration_backups(run_id, scope, target_kind, target_id, provider, media_source_id, backup_json)
					VALUES(?, ?, ?, ?, ?, '', ?)
				`, runID, ScopeMetadata, targetKind, targetID, provider, string(payload)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Service) applyItem(ctx context.Context, tx *sql.Tx, userID string, item ItemReport, source Item, updatedAt string) error {
	if userID == "" {
		userID = "local"
	}
	for _, change := range item.Changes {
		switch change {
		case ScopePlayback:
			if err := applyPlaybackState(ctx, tx, userID, item.Target.MediaSourceID, source, updatedAt); err != nil {
				return err
			}
		case ScopeMetadata:
			targetKind, targetID, err := s.metadataDestination(ctx, tx, item.Target)
			if err != nil {
				return err
			}
			for provider, externalID := range source.ExternalIDs {
				if err := applyExternalID(ctx, tx, targetKind, targetID, provider, externalID, updatedAt); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Service) verifyWithinTx(ctx context.Context, tx *sql.Tx, userID string, items []ItemReport) (Verification, error) {
	if userID == "" {
		userID = "local"
	}
	verification := Verification{Checked: len(items)}
	for _, item := range items {
		ok, err := verifyItem(ctx, tx, userID, item)
		if err != nil {
			return Verification{}, err
		}
		if ok {
			verification.Passed++
		} else {
			verification.Failed++
		}
	}
	return verification, nil
}

func (s *Service) persistRun(ctx context.Context, tx *sql.Tx, report Report) error {
	scopesJSON, _ := json.Marshal(report.Scopes)
	summaryJSON, _ := json.Marshal(report.Summary)
	verificationJSON, _ := json.Marshal(report.Verification)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO migration_runs(id, source, schema, status, scopes_json, summary_json, verification_json, created_at, completed_at, rolled_back_at, error_text)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, report.RunID, report.Source, report.Schema, report.Status, string(scopesJSON), string(summaryJSON), string(verificationJSON), report.CreatedAt, report.CompletedAt, report.RolledBackAt, report.ErrorText); err != nil {
		return err
	}
	for _, item := range report.Items {
		payload, _ := json.Marshal(item)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO migration_run_items(run_id, import_key, item_json)
			VALUES(?, ?, ?)
		`, report.RunID, item.ImportKey, string(payload)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) hydrateMovieTargets(ctx context.Context, targets []Target, changes []string) ([]Target, error) {
	if !requiresMediaSource(changes) {
		return targets, nil
	}
	for index := range targets {
		if targets[index].MediaSourceID != "" {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT media_source_id FROM movie_versions WHERE movie_id = ? ORDER BY media_source_id`, targets[index].ItemID)
		if err != nil {
			return nil, err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) == 1 {
			targets[index].MediaSourceID = ids[0]
		}
	}
	return targets, nil
}

func (s *Service) hydrateEpisodeTargets(ctx context.Context, targets []Target, changes []string) ([]Target, error) {
	if !requiresMediaSource(changes) {
		return targets, nil
	}
	for index := range targets {
		if targets[index].MediaSourceID != "" {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT media_source_id FROM episode_versions WHERE episode_id = ? ORDER BY media_source_id`, targets[index].ItemID)
		if err != nil {
			return nil, err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) == 1 {
			targets[index].MediaSourceID = ids[0]
		}
	}
	return targets, nil
}

func (s *Service) findMovieByPath(ctx context.Context, path string) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT 'movie', m.id, mv.media_source_id, m.title
		FROM media_sources ms
		JOIN movie_versions mv ON mv.media_source_id = ms.id
		JOIN movies m ON m.id = mv.movie_id
		WHERE lower(ms.path) = lower(?) OR lower(ms.rel_path) = lower(?)
	`, path, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTargets(rows)
}

func (s *Service) findMovieByExternalIDs(ctx context.Context, externalIDs map[string]string) ([]Target, error) {
	var matches []Target
	for _, provider := range sortedKeys(externalIDs) {
		rows, err := s.db.QueryContext(ctx, `
			SELECT 'movie', m.id, '', m.title
			FROM metadata_external_ids ids
			JOIN movies m ON m.id = ids.item_id
			WHERE ids.kind = 'movie' AND ids.provider = ? AND ids.external_id = ?
		`, provider, externalIDs[provider])
		if err != nil {
			return nil, err
		}
		current, err := scanTargets(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		matches = appendUniqueTargets(matches, current)
	}
	return matches, nil
}

func (s *Service) findMovieByTitleYear(ctx context.Context, title string, year int) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT 'movie', id, '', title
		FROM movies
		WHERE lower(title) = lower(?) AND year = ?
	`, title, year)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTargets(rows)
}

func (s *Service) findEpisodeByPath(ctx context.Context, path string) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT 'episode', e.id, ev.media_source_id, s.title || ' S' || printf('%02d', e.season_number) || 'E' || printf('%02d', e.episode_number)
		FROM media_sources ms
		JOIN episode_versions ev ON ev.media_source_id = ms.id
		JOIN tv_episodes e ON e.id = ev.episode_id
		JOIN tv_series s ON s.id = e.series_id
		WHERE lower(ms.path) = lower(?) OR lower(ms.rel_path) = lower(?)
	`, path, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTargets(rows)
}

func (s *Service) findEpisodeBySeriesExternalIDs(ctx context.Context, externalIDs map[string]string, seasonNumber int, episodeNumber int) ([]Target, error) {
	var matches []Target
	for _, provider := range sortedKeys(externalIDs) {
		rows, err := s.db.QueryContext(ctx, `
			SELECT 'episode', e.id, '', s.title || ' S' || printf('%02d', e.season_number) || 'E' || printf('%02d', e.episode_number)
			FROM metadata_external_ids ids
			JOIN tv_series s ON s.id = ids.item_id
			JOIN tv_episodes e ON e.series_id = s.id
			WHERE ids.kind = 'series' AND ids.provider = ? AND ids.external_id = ? AND e.season_number = ? AND e.episode_number = ?
		`, provider, externalIDs[provider], seasonNumber, episodeNumber)
		if err != nil {
			return nil, err
		}
		current, err := scanTargets(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		matches = appendUniqueTargets(matches, current)
	}
	return matches, nil
}

func (s *Service) findEpisodeBySeriesTitle(ctx context.Context, seriesTitle string, seasonNumber int, episodeNumber int) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT 'episode', e.id, '', s.title || ' S' || printf('%02d', e.season_number) || 'E' || printf('%02d', e.episode_number)
		FROM tv_series s
		JOIN tv_episodes e ON e.series_id = s.id
		WHERE lower(s.title) = lower(?) AND e.season_number = ? AND e.episode_number = ?
	`, seriesTitle, seasonNumber, episodeNumber)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTargets(rows)
}

func scanTargets(rows *sql.Rows) ([]Target, error) {
	output := []Target{}
	for rows.Next() {
		var item Target
		if err := rows.Scan(&item.Kind, &item.ItemID, &item.MediaSourceID, &item.Title); err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	return output, rows.Err()
}

func appendUniqueTargets(base []Target, extra []Target) []Target {
	seen := map[string]struct{}{}
	for _, item := range base {
		seen[item.Kind+"|"+item.ItemID+"|"+item.MediaSourceID] = struct{}{}
	}
	for _, item := range extra {
		key := item.Kind + "|" + item.ItemID + "|" + item.MediaSourceID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		base = append(base, item)
	}
	return base
}

func (s *Service) loadPlaybackBackup(ctx context.Context, tx *sql.Tx, userID string, mediaSourceID string) (playbackBackup, error) {
	var backup playbackBackup
	backup.UserID = userID
	backup.MediaSourceID = mediaSourceID
	var watched int
	err := tx.QueryRowContext(ctx, `
		SELECT watched, progress_seconds, duration_seconds, last_played_at, updated_at
		FROM playback_states
		WHERE user_id = ? AND media_source_id = ?
	`, userID, mediaSourceID).Scan(&watched, &backup.ProgressSeconds, &backup.DurationSeconds, &backup.LastPlayedAt, &backup.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return backup, nil
	}
	if err != nil {
		return playbackBackup{}, err
	}
	backup.Exists = true
	backup.Watched = watched != 0
	return backup, nil
}

func (s *Service) loadExternalIDBackup(ctx context.Context, tx *sql.Tx, kind string, itemID string, provider string) (externalIDBackup, error) {
	backup := externalIDBackup{Kind: kind, ItemID: itemID, Provider: provider}
	err := tx.QueryRowContext(ctx, `
		SELECT external_id, updated_at
		FROM metadata_external_ids
		WHERE kind = ? AND item_id = ? AND provider = ?
	`, kind, itemID, provider).Scan(&backup.ExternalID, &backup.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return backup, nil
	}
	if err != nil {
		return externalIDBackup{}, err
	}
	backup.Exists = true
	return backup, nil
}

func applyPlaybackState(ctx context.Context, tx *sql.Tx, userID string, mediaSourceID string, source Item, updatedAt string) error {
	watched := false
	if source.Watched != nil {
		watched = *source.Watched
	} else if source.DurationSeconds > 0 && source.ProgressSeconds >= source.DurationSeconds*0.9 {
		watched = true
	}
	lastPlayedAt := strings.TrimSpace(source.LastPlayedAt)
	if lastPlayedAt == "" {
		lastPlayedAt = updatedAt
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO playback_states(user_id, media_source_id, watched, progress_seconds, duration_seconds, last_played_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, media_source_id) DO UPDATE SET
			watched = excluded.watched,
			progress_seconds = excluded.progress_seconds,
			duration_seconds = excluded.duration_seconds,
			last_played_at = excluded.last_played_at,
			updated_at = excluded.updated_at
	`, userID, mediaSourceID, boolInt(watched), nonNegative(source.ProgressSeconds), nonNegative(source.DurationSeconds), lastPlayedAt, updatedAt)
	return err
}

func applyExternalID(ctx context.Context, tx *sql.Tx, kind string, itemID string, provider string, externalID string, updatedAt string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO metadata_external_ids(kind, item_id, provider, external_id, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(kind, item_id, provider) DO UPDATE SET
			external_id = excluded.external_id,
			updated_at = excluded.updated_at
	`, kind, itemID, provider, externalID, updatedAt)
	return err
}

func verifyItem(ctx context.Context, tx *sql.Tx, userID string, item ItemReport) (bool, error) {
	for _, change := range item.Changes {
		switch change {
		case ScopePlayback:
			var count int
			if err := tx.QueryRowContext(ctx, `
				SELECT count(*)
				FROM playback_states
				WHERE user_id = ? AND media_source_id = ?
			`, userID, item.Target.MediaSourceID).Scan(&count); err != nil {
				return false, err
			}
			if count == 0 {
				return false, nil
			}
		case ScopeMetadata:
			targetKind, targetID, err := metadataDestinationStatic(ctx, tx, item.Target)
			if err != nil {
				return false, err
			}
			for provider := range item.ExternalIDs {
				var count int
				if err := tx.QueryRowContext(ctx, `
					SELECT count(*)
					FROM metadata_external_ids
					WHERE kind = ? AND item_id = ? AND provider = ?
				`, targetKind, targetID, provider).Scan(&count); err != nil {
					return false, err
				}
				if count == 0 {
					return false, nil
				}
			}
		}
	}
	return true, nil
}

func (s *Service) metadataDestination(ctx context.Context, tx *sql.Tx, target Target) (string, string, error) {
	return metadataDestinationStatic(ctx, tx, target)
}

func metadataDestinationStatic(ctx context.Context, tx *sql.Tx, target Target) (string, string, error) {
	if target.Kind != "episode" {
		return target.Kind, target.ItemID, nil
	}
	var seriesID string
	if err := tx.QueryRowContext(ctx, `SELECT series_id FROM tv_episodes WHERE id = ?`, target.ItemID).Scan(&seriesID); err != nil {
		return "", "", err
	}
	return "series", seriesID, nil
}

func restorePlaybackState(ctx context.Context, tx *sql.Tx, backup playbackBackup) error {
	if !backup.Exists {
		_, err := tx.ExecContext(ctx, `
			DELETE FROM playback_states
			WHERE user_id = ? AND media_source_id = ?
		`, backup.UserID, backup.MediaSourceID)
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO playback_states(user_id, media_source_id, watched, progress_seconds, duration_seconds, last_played_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, media_source_id) DO UPDATE SET
			watched = excluded.watched,
			progress_seconds = excluded.progress_seconds,
			duration_seconds = excluded.duration_seconds,
			last_played_at = excluded.last_played_at,
			updated_at = excluded.updated_at
	`, backup.UserID, backup.MediaSourceID, boolInt(backup.Watched), backup.ProgressSeconds, backup.DurationSeconds, backup.LastPlayedAt, firstNonEmpty(backup.UpdatedAt, nowUTC()))
	return err
}

func restoreExternalID(ctx context.Context, tx *sql.Tx, backup externalIDBackup) error {
	if !backup.Exists {
		_, err := tx.ExecContext(ctx, `
			DELETE FROM metadata_external_ids
			WHERE kind = ? AND item_id = ? AND provider = ?
		`, backup.Kind, backup.ItemID, backup.Provider)
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO metadata_external_ids(kind, item_id, provider, external_id, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(kind, item_id, provider) DO UPDATE SET
			external_id = excluded.external_id,
			updated_at = excluded.updated_at
	`, backup.Kind, backup.ItemID, backup.Provider, backup.ExternalID, firstNonEmpty(backup.UpdatedAt, nowUTC()))
	return err
}

func (s *Service) publish(event string, payload map[string]any) {
	if s.events == nil {
		return
	}
	s.events.Publish(event, payload)
}

func (s *Service) nextRunID() string {
	id := s.nextID.Add(1)
	return fmt.Sprintf("migration_%s_%d", time.Now().UTC().Format("20060102T150405"), id)
}

func entryTitle(item Item) string {
	switch item.Kind {
	case "episode":
		if item.SeriesTitle != "" && item.SeasonNumber > 0 && item.EpisodeNumber > 0 {
			return fmt.Sprintf("%s S%02dE%02d", item.SeriesTitle, item.SeasonNumber, item.EpisodeNumber)
		}
	}
	if item.Title != "" {
		return item.Title
	}
	if item.Path != "" {
		return item.Path
	}
	return item.ID
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func nonNegative(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
