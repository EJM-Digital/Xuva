package api

import (
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/EJM-Digital/Xuva/server/internal/config"
)

func withSecurity(deps Deps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		if !applyCORS(deps, w, r) {
			writeError(w, http.StatusForbidden, "origin is not allowed")
			return
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob: https:; media-src 'self' blob:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'")
}

func applyCORS(deps Deps, w http.ResponseWriter, r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	if !originAllowed(deps, r, origin) {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-CSRF-Token, Authorization, X-Profile-Token")
	w.Header().Add("Vary", "Origin")
	return true
}

func originAllowed(deps Deps, r *http.Request, origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	if sameOrigin(*parsed, requestOrigin(r)) {
		return true
	}
	if canonical, err := config.NormalizeWebOrigin(deps.Config.CanonicalWebOrigin); err == nil && canonical != "" {
		if strings.EqualFold(canonical, origin) {
			return true
		}
	}
	for _, allowed := range deps.Config.AllowedOrigins {
		if strings.EqualFold(strings.TrimSpace(allowed), origin) {
			return true
		}
	}
	host := parsed.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	httpHost, _, err := net.SplitHostPort(deps.Config.HTTPAddr)
	if err == nil && httpHost != "" && (host == httpHost || (httpHost == "0.0.0.0" && isPrivateLocalHost(host))) {
		return true
	}
	return false
}

func isPrivateLocalHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func safePathSegment(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	if strings.ContainsAny(value, `/\`) {
		return false
	}
	cleaned := filepath.Clean(value)
	return cleaned == value && value != "." && value != ".."
}

func safeChildPath(root string, parts ...string) (string, bool) {
	for _, part := range parts {
		if !safePathSegment(part) {
			return "", false
		}
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	all := append([]string{rootAbs}, parts...)
	target := filepath.Join(all...)
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", false
	}
	if targetAbs != rootAbs && !strings.HasPrefix(targetAbs, rootAbs+string(filepath.Separator)) {
		return "", false
	}
	return targetAbs, true
}
