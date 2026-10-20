package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/config"
	"github.com/EJM-Digital/Xuva/server/internal/secret"
	"github.com/EJM-Digital/Xuva/server/internal/smb"
)

// smbCredential is the plaintext shape sealed into Config.NetworkCredentials.
// Field names are terse because the marshaled form is encrypted, not read by
// humans.
type smbCredential struct {
	Username string `json:"u"`
	Password string `json:"p"`
}

// sealSMBCredential seals username/password and returns a base64 string safe to
// persist in settings.json. The plaintext never touches disk.
func sealSMBCredential(store secret.Store, username, password string) (string, error) {
	if store == nil {
		return "", errors.New("secret store unavailable")
	}
	blob, err := json.Marshal(smbCredential{Username: username, Password: password})
	if err != nil {
		return "", err
	}
	sealed, err := store.Seal(blob)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// openSMBCredential reverses sealSMBCredential.
func openSMBCredential(store secret.Store, encoded string) (username, password string, err error) {
	if store == nil {
		return "", "", errors.New("secret store unavailable")
	}
	sealed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", "", err
	}
	blob, err := store.Open(sealed)
	if err != nil {
		return "", "", err
	}
	var c smbCredential
	if err := json.Unmarshal(blob, &c); err != nil {
		return "", "", err
	}
	return c.Username, c.Password, nil
}

// smbBrowseTimeout bounds a single credentialed browse. WNetAddConnection2 is
// not cancelable, so on timeout we stop waiting and return 504; the underlying
// syscall unwinds on its own when the OS network timeout elapses.
const smbBrowseTimeout = 25 * time.Second

// settingsSMBBrowseHandler browses a credentialed UNC share and optionally
// persists the (validated) credentials, sealed, for that share. Windows-only at
// runtime; other platforms report 501 via smb.ErrUnsupported.
//
// Request body: {"path","username","password","save"}. When username/password
// are omitted, previously stored credentials for the path are used (re-browse).
func settingsSMBBrowseHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path     string `json:"path"`
			Username string `json:"username"`
			Password string `json:"password"`
			Save     bool   `json:"save"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		path := strings.TrimSpace(req.Path)
		if !smb.IsUNC(path) {
			writeError(w, http.StatusBadRequest, "path must be a UNC share (\\\\host\\share)")
			return
		}
		key := smb.NormalizeUNC(path)

		username, password := req.Username, req.Password
		// Fall back to stored credentials when none are supplied (re-browse).
		if username == "" && password == "" {
			if enc, ok := currentConfig(deps).NetworkCredentials[key]; ok {
				if u, p, err := openSMBCredential(deps.Secret, enc); err == nil {
					username, password = u, p
				}
			}
		}

		entries, err := browseSMBWithTimeout(r.Context(), path, username, password)
		if err != nil {
			status, msg := smbBrowseErrorStatus(err)
			writeJSON(w, status, map[string]any{"path": key, "currentPath": key, "error": msg})
			return
		}

		// Persist the now-validated credentials only on explicit request.
		saved := false
		if req.Save {
			if err := saveSMBCredential(deps, key, username, password); err != nil {
				writeError(w, http.StatusInternalServerError, "share reachable but saving credentials failed")
				return
			}
			saved = true
		}

		folders := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			folders = append(folders, map[string]any{"name": e.Name, "path": e.Path, "isDir": true})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"path":            key,
			"currentPath":     key,
			"entries":         folders,
			"credentialSaved": saved,
		})
	}
}

// browseSMBWithTimeout runs the (uncancelable) credentialed browse on a
// goroutine and bounds the wait, so a dead host can't pin a request goroutine
// for the full OS network timeout.
func browseSMBWithTimeout(ctx context.Context, path, username, password string) ([]smb.Entry, error) {
	type result struct {
		entries []smb.Entry
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		e, err := smb.Browse(path, username, password)
		ch <- result{entries: e, err: err}
	}()
	tctx, cancel := context.WithTimeout(ctx, smbBrowseTimeout)
	defer cancel()
	select {
	case <-tctx.Done():
		return nil, tctx.Err()
	case res := <-ch:
		return res.entries, res.err
	}
}

func smbBrowseErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, smb.ErrUnsupported):
		return http.StatusNotImplemented, "credentialed SMB browsing is only available on the Windows server"
	case errors.Is(err, smb.ErrNotUNC):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timed out connecting to the share"
	default:
		return http.StatusBadGateway, err.Error()
	}
}

// saveSMBCredential seals the credentials and persists them in settings.json
// under Config.NetworkCredentials, keyed by the normalized UNC path.
func saveSMBCredential(deps Deps, key, username, password string) error {
	encoded, err := sealSMBCredential(deps.Secret, username, password)
	if err != nil {
		return err
	}
	updated := currentConfig(deps)
	if updated.NetworkCredentials == nil {
		updated.NetworkCredentials = map[string]string{}
	}
	updated.NetworkCredentials[key] = encoded
	if err := config.SaveFile(deps.Config.DataDir, updated); err != nil {
		return err
	}
	if deps.Events != nil {
		deps.Events.Publish("settings.updated", settingsPayload(updated))
	}
	return nil
}
