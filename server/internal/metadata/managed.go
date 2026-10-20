package metadata

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/config"
)

type ProviderHealth struct {
	ID                  string `json:"id"`
	Managed             bool   `json:"managed"`
	Configured          bool   `json:"configured"`
	Healthy             bool   `json:"healthy"`
	Status              string `json:"status"`
	LastError           string `json:"lastError,omitempty"`
	LastSuccessAt       string `json:"lastSuccessAt,omitempty"`
	LastFailureAt       string `json:"lastFailureAt,omitempty"`
	BackoffUntil        string `json:"backoffUntil,omitempty"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	QuotaLimited        bool   `json:"quotaLimited"`
}

type providerFailureKind string

const (
	providerFailureNone        providerFailureKind = "none"
	providerFailureRateLimited providerFailureKind = "rate_limited"
	providerFailureAuth        providerFailureKind = "auth"
	providerFailureUnavailable providerFailureKind = "unavailable"
	providerFailureClient      providerFailureKind = "client"
	providerFailureTransport   providerFailureKind = "transport"
)

type providerRuntimeState struct {
	lastSuccessAt       time.Time
	lastFailureAt       time.Time
	backoffUntil        time.Time
	lastError           string
	consecutiveFailures int
	quotaLimited        bool
}

func (s *Service) ProviderHealth(ctx context.Context) map[string]ProviderHealth {
	cfg := s.activeConfig()
	output := map[string]ProviderHealth{}
	for _, provider := range []string{"tvmaze", "wikipedia", "wikidata", "tmdb", "tvdb", "fanart", "omdb"} {
		output[provider] = s.providerHealth(provider, cfg)
	}
	return output
}

func (s *Service) providerHealth(provider string, cfg config.Config) ProviderHealth {
	provider = normalizeProviderID(provider)
	managed := isManagedProvider(provider)
	configured := true
	if managed {
		configured = managedProviderConfigured(provider, cfg)
	}
	state := s.readProviderState(provider)
	now := time.Now().UTC()
	healthy := true
	status := "ready"
	if managed && !configured {
		healthy = false
		status = "not_provisioned"
	} else if !state.backoffUntil.IsZero() && now.Before(state.backoffUntil) {
		healthy = false
		if state.quotaLimited {
			status = "rate_limited"
		} else {
			status = "degraded"
		}
	}
	if state.consecutiveFailures >= 3 && status == "ready" {
		healthy = false
		status = "degraded"
	}
	return ProviderHealth{
		ID:                  provider,
		Managed:             managed,
		Configured:          configured,
		Healthy:             healthy,
		Status:              status,
		LastError:           state.lastError,
		LastSuccessAt:       formatMaybeTime(state.lastSuccessAt),
		LastFailureAt:       formatMaybeTime(state.lastFailureAt),
		BackoffUntil:        formatMaybeTime(state.backoffUntil),
		ConsecutiveFailures: state.consecutiveFailures,
		QuotaLimited:        state.quotaLimited,
	}
}

func (s *Service) runProvider(provider string, managed bool, cfg config.Config, fn func() error) error {
	provider = normalizeProviderID(provider)
	if managed {
		if !managedProviderConfigured(provider, cfg) {
			s.noteProviderSkip(provider, "server-managed credentials are not provisioned")
			return nil
		}
	}
	if wait := s.providerBackoffRemaining(provider); wait > 0 {
		health := s.providerHealth(provider, cfg)
		if health.QuotaLimited {
			return fmt.Errorf("provider temporarily rate-limited; retry in %s", wait.Round(time.Second))
		}
		return fmt.Errorf("provider temporarily unavailable; retry in %s", wait.Round(time.Second))
	}
	if err := fn(); err != nil {
		class := classifyProviderFailure(err)
		s.recordProviderFailure(provider, class, err)
		return err
	}
	s.recordProviderSuccess(provider)
	return nil
}

func (s *Service) providerBackoffRemaining(provider string) time.Duration {
	state := s.readProviderState(provider)
	if state.backoffUntil.IsZero() {
		return 0
	}
	now := time.Now().UTC()
	if !now.Before(state.backoffUntil) {
		return 0
	}
	return state.backoffUntil.Sub(now)
}

func (s *Service) readProviderState(provider string) providerRuntimeState {
	provider = normalizeProviderID(provider)
	s.providerStateMu.RLock()
	defer s.providerStateMu.RUnlock()
	return s.providerState[provider]
}

func (s *Service) noteProviderSkip(provider string, reason string) {
	provider = normalizeProviderID(provider)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	s.providerStateMu.Lock()
	defer s.providerStateMu.Unlock()
	current := s.providerState[provider]
	current.lastError = reason
	s.providerState[provider] = current
}

func (s *Service) recordProviderSuccess(provider string) {
	provider = normalizeProviderID(provider)
	now := time.Now().UTC()
	s.providerStateMu.Lock()
	defer s.providerStateMu.Unlock()
	current := s.providerState[provider]
	current.lastSuccessAt = now
	current.backoffUntil = time.Time{}
	current.lastError = ""
	current.consecutiveFailures = 0
	current.quotaLimited = false
	s.providerState[provider] = current
}

func (s *Service) recordProviderFailure(provider string, class providerFailureKind, err error) {
	provider = normalizeProviderID(provider)
	now := time.Now().UTC()
	s.providerStateMu.Lock()
	defer s.providerStateMu.Unlock()
	current := s.providerState[provider]
	current.lastFailureAt = now
	current.lastError = providerErrorSummary(err)
	current.consecutiveFailures++
	current.quotaLimited = class == providerFailureRateLimited
	cooldown := providerFailureCooldown(class, current.consecutiveFailures)
	if cooldown > 0 {
		current.backoffUntil = now.Add(cooldown)
	}
	s.providerState[provider] = current
}

func providerFailureCooldown(class providerFailureKind, failures int) time.Duration {
	switch class {
	case providerFailureRateLimited:
		return 15 * time.Minute
	case providerFailureAuth:
		return 30 * time.Minute
	case providerFailureUnavailable:
		return 8 * time.Minute
	case providerFailureTransport:
		if failures >= 3 {
			return 3 * time.Minute
		}
		return 0
	case providerFailureClient:
		if failures >= 4 {
			return 5 * time.Minute
		}
		return 0
	default:
		return 0
	}
}

func classifyProviderFailure(err error) providerFailureKind {
	if err == nil {
		return providerFailureNone
	}
	var httpErr providerHTTPError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.StatusCode == 429:
			return providerFailureRateLimited
		case httpErr.StatusCode == 401 || httpErr.StatusCode == 402 || httpErr.StatusCode == 403:
			return providerFailureAuth
		case httpErr.StatusCode >= 500:
			return providerFailureUnavailable
		default:
			return providerFailureClient
		}
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(text, "too many requests"),
		strings.Contains(text, "rate limit"),
		strings.Contains(text, "request limit"),
		strings.Contains(text, "quota"):
		return providerFailureRateLimited
	case strings.Contains(text, "unauthorized"),
		strings.Contains(text, "forbidden"),
		strings.Contains(text, "invalid api key"),
		strings.Contains(text, "authentication"):
		return providerFailureAuth
	case strings.Contains(text, "timeout"),
		strings.Contains(text, "connection reset"),
		strings.Contains(text, "dial tcp"),
		strings.Contains(text, "temporary"):
		return providerFailureTransport
	case strings.Contains(text, "service unavailable"),
		strings.Contains(text, "provider returned 5"):
		return providerFailureUnavailable
	default:
		return providerFailureClient
	}
}

func providerErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	var httpErr providerHTTPError
	if errors.As(err, &httpErr) {
		if httpErr.Detail != "" {
			return httpErr.Detail
		}
		return httpErr.Error()
	}
	return strings.TrimSpace(err.Error())
}

func managedProviderConfigured(provider string, cfg config.Config) bool {
	return strings.TrimSpace(managedProviderCredential(provider, cfg)) != ""
}

func managedProviderCredential(provider string, cfg config.Config) string {
	switch normalizeProviderID(provider) {
	case "tmdb":
		return firstNonEmptyTrimmed(
			os.Getenv("XUVA_MANAGED_TMDB_API_KEY"),
			os.Getenv("XUVA_TMDB_API_KEY"),
			cfg.TMDBAPIKey,
			config.DefaultTMDBAPIKey,
		)
	case "tvdb":
		// Permanently empty — TVDB provider is disabled in this build.
		return ""
	case "fanart":
		return firstNonEmptyTrimmed(
			os.Getenv("XUVA_MANAGED_FANARTTV_API_KEY"),
			os.Getenv("XUVA_FANARTTV_API_KEY"),
			cfg.FanartTVAPIKey,
			config.DefaultFanartTVAPIKey,
		)
	case "omdb":
		return firstNonEmptyTrimmed(
			os.Getenv("XUVA_MANAGED_OMDB_API_KEY"),
			os.Getenv("XUVA_OMDB_API_KEY"),
			cfg.OMDbAPIKey,
			config.DefaultOMDbAPIKey,
		)
	default:
		return ""
	}
}

func ManagedProviderConfigured(provider string, cfg config.Config) bool {
	return managedProviderConfigured(provider, cfg)
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func isManagedProvider(provider string) bool {
	switch normalizeProviderID(provider) {
	case "tmdb", "tvdb", "fanart", "omdb":
		return true
	default:
		return false
	}
}

func normalizeProviderID(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

func formatMaybeTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
