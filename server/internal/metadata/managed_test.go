package metadata

import (
	"testing"

	"github.com/EJM-Digital/Xuva/server/internal/config"
)

func TestManagedProviderCredentialFallsBackToEmbeddedDefaults(t *testing.T) {
	origTMDB := config.DefaultTMDBAPIKey
	origFanart := config.DefaultFanartTVAPIKey
	origOMDb := config.DefaultOMDbAPIKey
	t.Cleanup(func() {
		config.DefaultTMDBAPIKey = origTMDB
		config.DefaultFanartTVAPIKey = origFanart
		config.DefaultOMDbAPIKey = origOMDb
	})

	t.Setenv("XUVA_MANAGED_TMDB_API_KEY", "")
	t.Setenv("XUVA_TMDB_API_KEY", "")
	t.Setenv("XUVA_MANAGED_FANARTTV_API_KEY", "")
	t.Setenv("XUVA_FANARTTV_API_KEY", "")
	t.Setenv("XUVA_MANAGED_OMDB_API_KEY", "")
	t.Setenv("XUVA_OMDB_API_KEY", "")

	config.DefaultTMDBAPIKey = "tmdb-default"
	config.DefaultFanartTVAPIKey = "fanart-default"
	config.DefaultOMDbAPIKey = "omdb-default"

	cfg := config.Config{}
	if got := managedProviderCredential("tmdb", cfg); got != "tmdb-default" {
		t.Fatalf("expected TMDB default key, got %q", got)
	}
	if got := managedProviderCredential("fanart", cfg); got != "fanart-default" {
		t.Fatalf("expected Fanart default key, got %q", got)
	}
	if got := managedProviderCredential("omdb", cfg); got != "omdb-default" {
		t.Fatalf("expected OMDb default key, got %q", got)
	}
}

