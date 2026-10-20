package config

import (
	"os"
	"strings"
)

// Embedded provider keys.
//
// These variables are populated at link time via `-ldflags "-X ..."` in
// release builds, and fall back to environment variables in local dev. They
// give Xuva the Emby-style UX where a user adds their library and trailers,
// posters, backdrops, logos, ratings just appear — no API-key configuration
// required.
//
// Resolution order (highest precedence first):
//   1. User-saved key in settings.json
//   2. XUVA_<PROVIDER>_API_KEY environment variable
//   3. Build-time embedded default (this file)
//   4. Empty → provider is skipped
//
// PROVIDER LICENSING NOTES (so future maintainers don't have to re-research):
//   • TMDB explicitly allows third-party media-server applications to embed a
//     project-level API key. See https://www.themoviedb.org/talk and TOS §10.
//   • Fanart.TV offers a "Project Key" tier specifically for media-server
//     redistribution. Apply at https://fanart.tv/get-an-api-key/.
//   • TVDB v4 requires per-installation subscription keys after their 2020
//     licence change. Embedding a corporate licence would require a paid
//     Emby-style commercial agreement, so for now we skip TVDB entirely —
//     TMDB has full TV data (episodes, seasons, stills, credits, ratings).
//
// RELEASE BUILD:
//   go build -ldflags "\
//     -X 'github.com/EJM-Digital/Xuva/server/internal/config.DefaultTMDBAPIKey=$TMDB_KEY' \
//     -X 'github.com/EJM-Digital/Xuva/server/internal/config.DefaultFanartTVAPIKey=$FANART_KEY' \
//     -X 'github.com/EJM-Digital/Xuva/server/internal/config.DefaultOMDbAPIKey=$OMDB_KEY' \
//   " ./cmd/xuva
//
// DEV BUILD:
//   Set XUVA_DEFAULT_TMDB_API_KEY (or place keys in .env.local — dev.ps1
//   loads it). The env-var fallback below means `go run` / `air` "just works"
//   without any ldflags ceremony.
//
// SECURITY: these vars hold secrets in release binaries. Treat the compiled
// xuva.exe like any other key-bearing artifact (don't ship debug builds with
// real keys to public CI logs etc.).
var (
	DefaultTMDBAPIKey     string
	DefaultFanartTVAPIKey string
	DefaultOMDbAPIKey     string
)

func init() {
	applyEmbeddedProviderDefaults()
}

// applyEmbeddedProviderDefaults keeps linker-injected defaults intact and only
// falls back to process env vars when no build-time key is embedded.
//
// Note: cmd/link -X only works on uninitialized vars or constant-string
// initializers. We intentionally keep these vars uninitialized so release
// pipelines can inject keys reliably.
func applyEmbeddedProviderDefaults() {
	if strings.TrimSpace(DefaultTMDBAPIKey) == "" {
		DefaultTMDBAPIKey = strings.TrimSpace(os.Getenv("XUVA_DEFAULT_TMDB_API_KEY"))
	}
	if strings.TrimSpace(DefaultFanartTVAPIKey) == "" {
		DefaultFanartTVAPIKey = strings.TrimSpace(os.Getenv("XUVA_DEFAULT_FANARTTV_API_KEY"))
	}
	if strings.TrimSpace(DefaultOMDbAPIKey) == "" {
		DefaultOMDbAPIKey = strings.TrimSpace(os.Getenv("XUVA_DEFAULT_OMDB_API_KEY"))
	}
}

// ProviderKeySource describes where a provider's API key resolved from, so
// the settings UI can show "✓ Built-in" vs "✓ Personal" vs "✗ Not
// configured" without exposing the key itself.
type ProviderKeySource string

const (
	KeySourceUnset    ProviderKeySource = "unset"     // truly nothing — provider is dead
	KeySourceEmbedded ProviderKeySource = "embedded"  // built-in key resolved (build-time or default env)
	KeySourcePersonal ProviderKeySource = "personal"  // user supplied their own key
)

// ResolveProviderKey runs the four-tier resolution and reports both the
// effective key and where it came from. Used by FromEnv at startup and by
// the settings payload to tell the UI which providers are healthy.
func ResolveProviderKey(savedKey string, envVar string, embeddedDefault string) (string, ProviderKeySource) {
	if savedKey != "" {
		return savedKey, KeySourcePersonal
	}
	if envVar != "" {
		// In dev/CI, an env-var-set key still counts as the user opting in
		// explicitly, so we tag it as personal rather than embedded.
		return envVar, KeySourcePersonal
	}
	if embeddedDefault != "" {
		return embeddedDefault, KeySourceEmbedded
	}
	return "", KeySourceUnset
}
