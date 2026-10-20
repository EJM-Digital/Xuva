package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/backup"
)

// backupExportHandler streams a .tar.gz archive of xuva.db + settings.json + manifest.json.
func backupExportHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc := backup.New(deps.Config.DataDir, deps.Database.DB())
		filename := fmt.Sprintf("xuva-backup-%s.tar.gz", time.Now().UTC().Format("2006-01-02"))
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		if err := svc.Export(w, deps.Config.MovieLibraryPath, deps.Config.TVLibraryPath); err != nil {
			slog.Error("backup export failed", "err", err)
		}
	}
}

// backupImportHandler accepts a multipart archive upload and stages a restore.
// The restore is applied on next server startup via backup.ApplyIfPending.
func backupImportHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const maxUpload = 2 << 30 // 2 GiB
		r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, "could not parse upload: "+err.Error(), http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("archive")
		if err != nil {
			http.Error(w, "field 'archive' is required", http.StatusBadRequest)
			return
		}
		defer file.Close()

		svc := backup.New(deps.Config.DataDir, deps.Database.DB())
		m, err := svc.Stage(file)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":          "staged",
			"requiresRestart": true,
			"manifest":        m,
		})
	}
}
