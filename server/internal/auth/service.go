package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/EJM-Digital/Xuva/server/internal/database"
)

const (
	SessionCookieName = "xuva_session"
	CSRFCookieName    = "xuva_csrf"
)

var (
	ErrUnauthorized       = errors.New("unauthorized")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrCSRF               = errors.New("invalid csrf token")
	ErrLocked             = errors.New("login temporarily locked")
	ErrBootstrapComplete  = errors.New("bootstrap already complete")
	ErrUserNotFound       = errors.New("user not found")
	ErrLastAdmin          = errors.New("cannot remove the last admin account")
	ErrInvalidPreferences = errors.New("invalid user preferences")
)

type Principal struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	DisplayName  string `json:"displayName"`
	AvatarURL    string `json:"avatarUrl,omitempty"`
	AvatarPreset string `json:"avatarPreset,omitempty"`
	AvatarColor  string `json:"avatarColor,omitempty"`
	Role         string `json:"role"`
	IsRestricted bool   `json:"isRestricted,omitempty"`
	MaxRating    string `json:"maxRating,omitempty"`
}

// UserAccount is the full admin-visible user record (no password fields).
type UserAccount struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	DisplayName  string `json:"displayName"`
	AvatarURL    string `json:"avatarUrl,omitempty"`
	AvatarPreset string `json:"avatarPreset,omitempty"`
	AvatarColor  string `json:"avatarColor,omitempty"`
	Role         string `json:"role"`
	IsRestricted bool   `json:"isRestricted"`
	MaxRating    string `json:"maxRating,omitempty"`
	HasPin       bool   `json:"hasPin"`
	CreatedAt    string `json:"createdAt,omitempty"`
}

// ProfileCard is the public-facing profile info shown on the "Who's Watching?" screen.
// It deliberately omits username, role, and all credential/PIN data.
type ProfileCard struct {
	ID           string `json:"id"`
	DisplayName  string `json:"displayName"`
	AvatarURL    string `json:"avatarUrl,omitempty"`
	AvatarPreset string `json:"avatarPreset,omitempty"`
	AvatarColor  string `json:"avatarColor,omitempty"`
	// IsRestricted = true means this is a kids/restricted profile.
	// PIN (if any) guards the EXIT — no PIN needed to enter.
	IsRestricted bool `json:"isRestricted"`
	// HasEntryPin = true on a non-restricted profile that requires a PIN to enter.
	HasEntryPin bool `json:"hasEntryPin"`
	// HasExitPin = true on a restricted profile that requires a PIN to switch away from.
	HasExitPin bool `json:"hasExitPin"`
}

type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	DeviceID  string    `json:"deviceId,omitempty"`
	CSRFToken string    `json:"csrfToken,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}

type ResolvedSession struct {
	Principal Principal
	Session   Session
	Token     string
	Rotated   bool
	DevBypass bool
}

type contextKey string

const (
	resolvedSessionKey contextKey = "auth.resolvedSession"
	activeProfileKey   contextKey = "auth.activeProfileUserID"
)

type Service struct {
	db               *sql.DB
	disabled         bool
	sessionTTL       time.Duration
	sessionTouchTTL  time.Duration
	rotationInterval time.Duration
	lockoutThreshold int
	lockoutWindow    time.Duration

	mu             sync.Mutex
	unknownAttempt map[string]attempt
}

type attempt struct {
	count       int
	lastFailed  time.Time
	lockedUntil time.Time
}

type userRecord struct {
	ID               string
	Username         string
	DisplayName      string
	AvatarURL        string
	AvatarPreset     string
	AvatarColor      string
	Role             string
	PinHash          string
	IsRestricted     int
	MaxRating        string
	PasswordHash     string
	LockedUntil      string
	FailedLoginCount int
	LastFailedAt     string
}

type BootstrapOptions struct {
	Username    string
	Password    string
	DisplayName string
}

func NewService(databaseService *database.Service, disabled bool) *Service {
	return &Service{
		db:         databaseService.DB(),
		disabled:   disabled,
		sessionTTL: 24 * time.Hour,
		// Refresh persisted session timestamps at a low cadence to avoid high-write
		// contention when many authenticated requests land in parallel.
		sessionTouchTTL: 45 * time.Second,
		// Disabled by default. Rotating session secrets during request resolution can
		// invalidate concurrent in-flight requests and force unnecessary re-auth flows.
		rotationInterval: 0,
		lockoutThreshold: 5,
		lockoutWindow:    15 * time.Minute,
		unknownAttempt:   map[string]attempt{},
	}
}

func (s *Service) Disabled() bool {
	return s == nil || s.disabled
}

func (s *Service) Bootstrap(ctx context.Context, options BootstrapOptions) error {
	if s.Disabled() {
		return nil
	}
	required, err := s.RequiresBootstrap(ctx)
	if err != nil {
		return err
	}
	if !required {
		return nil
	}
	username := normalizeUsername(options.Username)
	if username == "" {
		username = "admin"
	}
	// Fail closed rather than generate a random password and log it in plaintext
	// (the old behaviour leaked an admin credential into persisted/rotated logs).
	// The only live caller supplies a password from XUVA_ADMIN_PASSWORD; zero-config
	// first-run instead defers to the interactive sign-in bootstrap (see app.New).
	password := strings.TrimSpace(options.Password)
	if password == "" {
		return errors.New("bootstrap password is required")
	}
	displayName := strings.TrimSpace(options.DisplayName)
	if displayName == "" {
		displayName = "Administrator"
	}
	principal, err := s.bootstrapAdminUser(ctx, username, password, displayName)
	if err != nil {
		return err
	}
	slog.Info("auth bootstrap user created", "username", principal.Username)
	return nil
}

func (s *Service) RequiresBootstrap(ctx context.Context) (bool, error) {
	if s.Disabled() {
		return false, nil
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE username <> '' AND password_hash <> ''`).Scan(&count); err != nil {
		return false, err
	}
	return count == 0, nil
}

func (s *Service) BootstrapUser(ctx context.Context, options BootstrapOptions) (Principal, error) {
	if s.Disabled() {
		return Principal{}, ErrUnauthorized
	}
	required, err := s.RequiresBootstrap(ctx)
	if err != nil {
		return Principal{}, err
	}
	if !required {
		return Principal{}, ErrBootstrapComplete
	}
	username := normalizeUsername(options.Username)
	if username == "" {
		username = "admin"
	}
	if strings.TrimSpace(options.Password) == "" {
		return Principal{}, errors.New("password is required")
	}
	displayName := strings.TrimSpace(options.DisplayName)
	if displayName == "" {
		displayName = username
	}
	return s.bootstrapAdminUser(ctx, username, options.Password, displayName)
}

func (s *Service) bootstrapAdminUser(ctx context.Context, username string, password string, displayName string) (Principal, error) {
	hash, err := hashPassword(password)
	if err != nil {
		return Principal{}, err
	}
	now := timestamp(time.Now())

	// If a placeholder admin row already exists, claim it instead of inserting a duplicate id.
	updateResult, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET username = ?, display_name = ?, role = 'admin', password_hash = ?, password_updated_at = ?, updated_at = ?
		WHERE id = 'admin' AND username = '' AND password_hash = ''
	`, username, displayName, hash, now, now)
	if err != nil {
		return Principal{}, err
	}
	if rows, _ := updateResult.RowsAffected(); rows > 0 {
		return Principal{ID: "admin", Username: username, DisplayName: displayName, Role: "admin"}, nil
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO users(id, username, display_name, role, password_hash, password_updated_at, updated_at)
		VALUES(?, ?, ?, 'admin', ?, ?, ?)
	`, "admin", username, displayName, hash, now, now)
	if err != nil {
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "users.id") || strings.Contains(lower, "users.username") {
			return Principal{}, ErrBootstrapComplete
		}
		return Principal{}, err
	}
	return Principal{ID: "admin", Username: username, DisplayName: displayName, Role: "admin"}, nil
}

func (s *Service) CreateUser(ctx context.Context, username string, password string, displayName string, role string) (Principal, error) {
	if s.Disabled() {
		return Principal{}, ErrUnauthorized
	}
	username = normalizeUsername(username)
	if username == "" {
		return Principal{}, errors.New("username is required")
	}
	role = normalizeRole(role)
	if role == "" {
		return Principal{}, errors.New("role is required")
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = username
	}
	hash, err := hashPassword(password)
	if err != nil {
		return Principal{}, err
	}
	now := timestamp(time.Now())
	id := "user_" + uuid.NewString()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO users(id, username, display_name, role, password_hash, password_updated_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
	`, id, username, displayName, role, hash, now, now)
	if err != nil {
		return Principal{}, err
	}
	return Principal{ID: id, Username: username, DisplayName: displayName, Role: role}, nil
}

func (s *Service) ListUsers(ctx context.Context) ([]UserAccount, error) {
	if s.Disabled() {
		return nil, ErrUnauthorized
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, display_name, avatar_url, avatar_preset, avatar_color,
		       role, is_restricted, max_rating, pin_hash, created_at
		FROM users
		WHERE username <> ''
		ORDER BY LOWER(username) ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UserAccount{}
	for rows.Next() {
		var a UserAccount
		var pinHash string
		var isRestricted int
		if err := rows.Scan(
			&a.ID, &a.Username, &a.DisplayName, &a.AvatarURL, &a.AvatarPreset, &a.AvatarColor,
			&a.Role, &isRestricted, &a.MaxRating, &pinHash, &a.CreatedAt,
		); err != nil {
			return nil, err
		}
		a.IsRestricted = isRestricted != 0
		a.HasPin = pinHash != ""
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListProfiles returns public-facing profile cards for all real users.
// It omits credentials, role, and PIN values — only metadata needed for
// the "Who's Watching?" picker screen is included.
func (s *Service) ListProfiles(ctx context.Context) ([]ProfileCard, error) {
	if s.Disabled() {
		return nil, ErrUnauthorized
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, display_name, avatar_url, avatar_preset, avatar_color,
		       is_restricted, pin_hash
		FROM users
		WHERE username <> ''
		ORDER BY LOWER(display_name) ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProfileCard{}
	for rows.Next() {
		var c ProfileCard
		var isRestricted int
		var pinHash string
		if err := rows.Scan(
			&c.ID, &c.DisplayName, &c.AvatarURL, &c.AvatarPreset, &c.AvatarColor,
			&isRestricted, &pinHash,
		); err != nil {
			return nil, err
		}
		c.IsRestricted = isRestricted != 0
		if c.IsRestricted {
			c.HasExitPin = pinHash != ""
		} else {
			c.HasEntryPin = pinHash != ""
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) DeleteUser(ctx context.Context, userID string) error {
	if s.Disabled() {
		return ErrUnauthorized
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ErrUserNotFound
	}
	var role string
	var username string
	err := s.db.QueryRowContext(ctx, `
		SELECT role, username
		FROM users
		WHERE id = ?
	`, userID).Scan(&role, &username)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(username) == "" {
		return ErrUserNotFound
	}
	if strings.EqualFold(strings.TrimSpace(role), "admin") {
		var admins int
		if err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM users
			WHERE username <> '' AND role = 'admin'
		`).Scan(&admins); err != nil {
			return err
		}
		if admins <= 1 {
			return ErrLastAdmin
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (s *Service) UpdateUserProfile(ctx context.Context, userID string, displayName string, avatarURL string) (Principal, error) {
	if s.Disabled() {
		return Principal{}, ErrUnauthorized
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return Principal{}, ErrUserNotFound
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return Principal{}, errors.New("display name is required")
	}
	now := timestamp(time.Now())
	result, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET display_name = ?, avatar_url = ?, updated_at = ?
		WHERE id = ? AND username <> ''
	`, displayName, strings.TrimSpace(avatarURL), now, userID)
	if err != nil {
		return Principal{}, err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return Principal{}, ErrUserNotFound
	}
	var principal Principal
	err = s.db.QueryRowContext(ctx, `
		SELECT id, username, display_name, avatar_url, role
		FROM users
		WHERE id = ?
	`, userID).Scan(&principal.ID, &principal.Username, &principal.DisplayName, &principal.AvatarURL, &principal.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUserNotFound
	}
	if err != nil {
		return Principal{}, err
	}
	return principal, nil
}

func (s *Service) SetUserPassword(ctx context.Context, userID string, password string) error {
	if s.Disabled() {
		return ErrUnauthorized
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ErrUserNotFound
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	now := timestamp(time.Now())
	result, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET password_hash = ?, password_updated_at = ?, failed_login_count = 0, last_failed_at = '', locked_until = '', updated_at = ?
		WHERE id = ? AND username <> ''
	`, hash, now, now, userID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrUserNotFound
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return nil
}

func normalizeRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "admin":
		return "admin"
	case "standard":
		return "standard"
	default:
		return ""
	}
}

// extendedSessionTTL is the session lifetime used when the user ticks
// "Trust this device" at sign-in. 90 days matches the user-confirmed default
// for a self-hosted media-server context — long enough to feel like the
// device stays signed in, short enough that an abandoned shared device
// eventually drops out of trust on its own. See AuthenticateWithTrust.
const extendedSessionTTL = 90 * 24 * time.Hour

// AuthenticateWithTrust is the trust-aware variant of Authenticate. When
// trustDevice is true the issued session uses extendedSessionTTL (90 days)
// instead of the default 24-hour s.sessionTTL. Callers from /api/auth/login
// pass the user's checkbox state through to this method.
//
// The default Authenticate path is preserved unchanged for any consumer
// that doesn't surface a trust toggle (the desktop pairing flow, the
// device pairing API, internal callers, etc.) so the security posture for
// those paths is unaffected.
//
// Implemented via authenticateWithTTL so the TTL flows down as an explicit
// parameter rather than a mutated receiver field — concurrent logins are
// safe regardless of trust-flag mix.
func (s *Service) AuthenticateWithTrust(ctx context.Context, username, password, remoteAddr, userAgent string, trustDevice bool) (Principal, Session, string, error) {
	ttl := s.sessionTTL
	if trustDevice {
		ttl = extendedSessionTTL
	}
	return s.authenticateWithTTL(ctx, username, password, remoteAddr, userAgent, ttl)
}

func (s *Service) Authenticate(ctx context.Context, username string, password string, remoteAddr string, userAgent string) (Principal, Session, string, error) {
	return s.authenticateWithTTL(ctx, username, password, remoteAddr, userAgent, s.sessionTTL)
}

// authenticateWithTTL is the shared implementation of Authenticate +
// AuthenticateWithTrust. The TTL is threaded through to issueSessionWithTTL
// so different callers can request different lifetimes without mutating
// receiver state.
func (s *Service) authenticateWithTTL(ctx context.Context, username, password, remoteAddr, userAgent string, ttl time.Duration) (Principal, Session, string, error) {
	if s.Disabled() {
		return Principal{}, Session{}, "", ErrUnauthorized
	}
	key := s.unknownKey(username, remoteAddr)
	if until, locked := s.checkUnknownLockout(key); locked {
		slog.Warn("auth login blocked", "username", normalizeUsername(username), "remote_addr", remoteAddr, "locked_until", until.Format(time.RFC3339))
		return Principal{}, Session{}, "", lockoutError(until)
	}
	user, ok, err := s.lookupUser(ctx, username)
	if err != nil {
		return Principal{}, Session{}, "", err
	}
	if !ok || user.PasswordHash == "" {
		if until, locked := s.recordUnknownFailure(key); locked {
			slog.Warn("auth login locked", "username", normalizeUsername(username), "remote_addr", remoteAddr, "locked_until", until.Format(time.RFC3339))
			return Principal{}, Session{}, "", lockoutError(until)
		}
		slog.Warn("auth login failed", "username", normalizeUsername(username), "remote_addr", remoteAddr, "reason", "unknown user")
		return Principal{}, Session{}, "", ErrInvalidCredentials
	}
	if until, locked := parseTimestamp(user.LockedUntil); locked && until.After(time.Now().UTC()) {
		slog.Warn("auth login blocked", "username", user.Username, "remote_addr", remoteAddr, "locked_until", until.Format(time.RFC3339))
		return Principal{}, Session{}, "", lockoutError(until)
	}
	valid, err := verifyPassword(user.PasswordHash, password)
	if err != nil {
		return Principal{}, Session{}, "", err
	}
	if !valid {
		until, err := s.recordFailedLogin(ctx, user)
		if err != nil {
			return Principal{}, Session{}, "", err
		}
		if !until.IsZero() {
			slog.Warn("auth login locked", "username", user.Username, "remote_addr", remoteAddr, "locked_until", until.Format(time.RFC3339))
			return Principal{}, Session{}, "", lockoutError(until)
		}
		slog.Warn("auth login failed", "username", user.Username, "remote_addr", remoteAddr, "reason", "password mismatch")
		return Principal{}, Session{}, "", ErrInvalidCredentials
	}
	if err := s.clearFailedLogin(ctx, user.ID); err != nil {
		return Principal{}, Session{}, "", err
	}
	s.clearUnknownFailure(key)
	principal := Principal{ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, AvatarURL: user.AvatarURL, Role: user.Role}
	session, token, err := s.issueSessionWithTTL(ctx, principal, "", remoteAddr, userAgent, ttl)
	if err != nil {
		return Principal{}, Session{}, "", err
	}
	slog.Info("auth login success", "username", principal.Username, "remote_addr", remoteAddr, "session_id", session.ID, "ttl", ttl.String())
	return principal, session, token, nil
}

func (s *Service) IssueSessionForUser(ctx context.Context, userID string, remoteAddr string, userAgent string) (Principal, Session, string, error) {
	return s.issueSessionForUser(ctx, userID, "", remoteAddr, userAgent)
}

func (s *Service) IssueDeviceSessionForUser(ctx context.Context, userID string, deviceID string, remoteAddr string, userAgent string) (Principal, Session, string, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return Principal{}, Session{}, "", ErrUnauthorized
	}
	return s.issueSessionForUser(ctx, userID, deviceID, remoteAddr, userAgent)
}

func (s *Service) issueSessionForUser(ctx context.Context, userID string, deviceID string, remoteAddr string, userAgent string) (Principal, Session, string, error) {
	if s.Disabled() {
		return Principal{}, Session{}, "", ErrUnauthorized
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return Principal{}, Session{}, "", ErrUserNotFound
	}
	var principal Principal
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, display_name, avatar_url, role
		FROM users
		WHERE id = ?
	`, userID).Scan(&principal.ID, &principal.Username, &principal.DisplayName, &principal.AvatarURL, &principal.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, Session{}, "", ErrUserNotFound
	}
	if err != nil {
		return Principal{}, Session{}, "", err
	}
	session, token, err := s.issueSession(ctx, principal, deviceID, remoteAddr, userAgent)
	if err != nil {
		return Principal{}, Session{}, "", err
	}
	return principal, session, token, nil
}

func (s *Service) Resolve(ctx context.Context, token string, remoteAddr string, userAgent string) (ResolvedSession, error) {
	if s.Disabled() {
		return ResolvedSession{}, ErrUnauthorized
	}
	id, secret, err := parseToken(token)
	if err != nil {
		return ResolvedSession{}, ErrUnauthorized
	}
	row := struct {
		SessionID   string
		UserID      string
		Username    string
		DisplayName string
		AvatarURL   string
		Role        string
		SecretHash  string
		CSRFToken   string
		DeviceID    string
		CreatedAt   string
		LastSeenAt  string
		ExpiresAt   string
		RevokedAt   string
	}{}
	err = s.db.QueryRowContext(ctx, `
		SELECT s.id, s.user_id, u.username, u.display_name, u.avatar_url, u.role, s.secret_hash, s.csrf_token, s.device_id, s.created_at, s.last_seen_at, s.expires_at, s.revoked_at
		FROM auth_sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.id = ?
	`, id).Scan(&row.SessionID, &row.UserID, &row.Username, &row.DisplayName, &row.AvatarURL, &row.Role, &row.SecretHash, &row.CSRFToken, &row.DeviceID, &row.CreatedAt, &row.LastSeenAt, &row.ExpiresAt, &row.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ResolvedSession{}, ErrUnauthorized
	}
	if err != nil {
		return ResolvedSession{}, err
	}
	if strings.TrimSpace(row.RevokedAt) != "" {
		return ResolvedSession{}, ErrUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(row.SecretHash), []byte(hashSecret(secret))) != 1 {
		return ResolvedSession{}, ErrUnauthorized
	}
	// Belt-and-braces: reject if the linked device has been revoked even when the
	// session's own revoked_at was not set (e.g. pre-migration devices with empty
	// auth_session_id that RevokeDeviceSessions couldn't reach).
	if deviceID := strings.TrimSpace(row.DeviceID); deviceID != "" {
		var deviceStatus string
		devErr := s.db.QueryRowContext(ctx, `
			SELECT status FROM approved_devices WHERE device_id = ?
		`, deviceID).Scan(&deviceStatus)
		if devErr != nil && !errors.Is(devErr, sql.ErrNoRows) {
			return ResolvedSession{}, devErr
		}
		if deviceStatus == "revoked" {
			return ResolvedSession{}, ErrUnauthorized
		}
	}
	now := time.Now().UTC()
	expiresAt, ok := parseTimestamp(row.ExpiresAt)
	if !ok || now.After(expiresAt) {
		_ = s.revokeSessionID(ctx, row.SessionID)
		return ResolvedSession{}, ErrUnauthorized
	}
	createdAt, _ := parseTimestamp(row.CreatedAt)
	lastSeenAt, _ := parseTimestamp(row.LastSeenAt)
	rotated := false
	nextToken := token
	nextCSRF := row.CSRFToken
	nextExpiresAt := expiresAt
	if createdAt.IsZero() {
		createdAt = now
	}
	if s.rotationInterval > 0 && now.Sub(createdAt) >= s.rotationInterval {
		newSecret := randomToken(32)
		nextToken = row.SessionID + "." + newSecret
		nextCSRF = randomToken(32)
		rotated = true
		if _, err := s.db.ExecContext(ctx, `
			UPDATE auth_sessions
			SET secret_hash = ?, csrf_token = ?, created_at = ?, last_seen_at = ?, expires_at = ?, remote_addr = ?, user_agent = ?
			WHERE id = ?
		`, hashSecret(newSecret), nextCSRF, timestamp(now), timestamp(now), timestamp(now.Add(s.sessionTTL)), remoteAddr, userAgent, row.SessionID); err != nil {
			// Keep the existing token/session valid if rotation persistence fails.
			rotated = false
			nextToken = token
			nextCSRF = row.CSRFToken
			slog.Warn("auth session rotation skipped", "session_id", row.SessionID, "error", err)
		} else {
			nextExpiresAt = now.Add(s.sessionTTL)
		}
	} else {
		shouldTouch := s.sessionTouchTTL <= 0 || lastSeenAt.IsZero() || now.Sub(lastSeenAt) >= s.sessionTouchTTL
		if shouldTouch {
			if _, err := s.db.ExecContext(ctx, `
				UPDATE auth_sessions
				SET last_seen_at = ?, expires_at = ?, remote_addr = ?, user_agent = ?
				WHERE id = ?
			`, timestamp(now), timestamp(now.Add(s.sessionTTL)), remoteAddr, userAgent, row.SessionID); err != nil {
				// A timestamp refresh failure should not invalidate an otherwise valid
				// session during request handling.
				slog.Warn("auth session touch skipped", "session_id", row.SessionID, "error", err)
			} else {
				nextExpiresAt = now.Add(s.sessionTTL)
			}
		}
	}
	return ResolvedSession{
		Principal: Principal{ID: row.UserID, Username: row.Username, DisplayName: row.DisplayName, AvatarURL: row.AvatarURL, Role: row.Role},
		Session: Session{
			ID:        row.SessionID,
			UserID:    row.UserID,
			DeviceID:  row.DeviceID,
			CSRFToken: nextCSRF,
			ExpiresAt: nextExpiresAt,
			CreatedAt: createdAt,
		},
		Token:   nextToken,
		Rotated: rotated,
	}, nil
}

func (s *Service) Revoke(ctx context.Context, token string) error {
	if s.Disabled() {
		return nil
	}
	id, _, err := parseToken(token)
	if err != nil {
		return ErrUnauthorized
	}
	return s.revokeSessionID(ctx, id)
}

func (s *Service) RevokeDeviceSessions(ctx context.Context, deviceID string) error {
	if s.Disabled() {
		return nil
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE auth_sessions
		SET revoked_at = ?, expires_at = ?
		WHERE device_id = ? AND revoked_at = ''
	`, timestamp(time.Now().UTC()), timestamp(time.Now().UTC()), deviceID)
	return err
}

func (s *Service) ValidateCSRF(session ResolvedSession, cookieToken string, headerToken string) error {
	if s.Disabled() {
		return nil
	}
	if cookieToken == "" || headerToken == "" {
		return ErrCSRF
	}
	if subtle.ConstantTimeCompare([]byte(cookieToken), []byte(session.Session.CSRFToken)) != 1 {
		return ErrCSRF
	}
	if subtle.ConstantTimeCompare([]byte(headerToken), []byte(session.Session.CSRFToken)) != 1 {
		return ErrCSRF
	}
	return nil
}

func (s *Service) lookupUser(ctx context.Context, username string) (userRecord, bool, error) {
	name := normalizeUsername(username)
	var user userRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, display_name, avatar_url, role, password_hash, locked_until, failed_login_count, last_failed_at
		FROM users
		WHERE username = ? COLLATE NOCASE
	`, name).Scan(&user.ID, &user.Username, &user.DisplayName, &user.AvatarURL, &user.Role, &user.PasswordHash, &user.LockedUntil, &user.FailedLoginCount, &user.LastFailedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return userRecord{}, false, nil
	}
	if err != nil {
		return userRecord{}, false, err
	}
	return user, true, nil
}

func (s *Service) recordFailedLogin(ctx context.Context, user userRecord) (time.Time, error) {
	now := time.Now().UTC()
	lastFailed, _ := parseTimestamp(user.LastFailedAt)
	count := user.FailedLoginCount
	if lastFailed.IsZero() || now.Sub(lastFailed) > s.lockoutWindow {
		count = 0
	}
	count++
	lockUntil := time.Time{}
	if count >= s.lockoutThreshold {
		lockUntil = now.Add(s.lockoutWindow)
		count = 0
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET failed_login_count = ?, last_failed_at = ?, locked_until = ?, updated_at = ?
		WHERE id = ?
	`, count, timestamp(now), formatMaybeTime(lockUntil), timestamp(now), user.ID)
	return lockUntil, err
}

func (s *Service) clearFailedLogin(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET failed_login_count = 0, last_failed_at = '', locked_until = '', updated_at = ?
		WHERE id = ?
	`, timestamp(time.Now().UTC()), userID)
	return err
}

func (s *Service) issueSession(ctx context.Context, principal Principal, deviceID string, remoteAddr string, userAgent string) (Session, string, error) {
	return s.issueSessionWithTTL(ctx, principal, deviceID, remoteAddr, userAgent, s.sessionTTL)
}

// issueSessionWithTTL is the explicit-TTL form of issueSession. The TTL
// parameter lets AuthenticateWithTrust issue a 90-day session without
// touching the receiver's default sessionTTL field (which would race on
// concurrent logins).
func (s *Service) issueSessionWithTTL(ctx context.Context, principal Principal, deviceID string, remoteAddr string, userAgent string, ttl time.Duration) (Session, string, error) {
	if ttl <= 0 {
		ttl = s.sessionTTL
	}
	now := time.Now().UTC()
	sessionID := "auth_" + uuid.NewString()
	secret := randomToken(32)
	csrfToken := randomToken(32)
	token := sessionID + "." + secret
	expiresAt := now.Add(ttl)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_sessions(id, user_id, secret_hash, csrf_token, device_id, remote_addr, user_agent, created_at, last_seen_at, expires_at, revoked_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')
	`, sessionID, principal.ID, hashSecret(secret), csrfToken, strings.TrimSpace(deviceID), remoteAddr, userAgent, timestamp(now), timestamp(now), timestamp(expiresAt))
	if err != nil {
		return Session{}, "", err
	}
	return Session{ID: sessionID, UserID: principal.ID, DeviceID: strings.TrimSpace(deviceID), CSRFToken: csrfToken, ExpiresAt: expiresAt, CreatedAt: now}, token, nil
}

// RevokeSessionID is the public form of revokeSessionID for callers that
// already hold the session id (e.g. device revoke wants to drop the
// token that was issued at pairing time).
func (s *Service) RevokeSessionID(ctx context.Context, sessionID string) error {
	if s.Disabled() {
		return nil
	}
	return s.revokeSessionID(ctx, sessionID)
}

func (s *Service) revokeSessionID(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE auth_sessions
		SET revoked_at = ?, expires_at = ?
		WHERE id = ? AND revoked_at = ''
	`, timestamp(time.Now().UTC()), timestamp(time.Now().UTC()), sessionID)
	return err
}

func (s *Service) checkUnknownLockout(key string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.unknownAttempt[key]
	if !entry.lockedUntil.IsZero() && time.Now().UTC().Before(entry.lockedUntil) {
		return entry.lockedUntil, true
	}
	if !entry.lockedUntil.IsZero() && time.Now().UTC().After(entry.lockedUntil) {
		delete(s.unknownAttempt, key)
	}
	return time.Time{}, false
}

func (s *Service) recordUnknownFailure(key string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	entry := s.unknownAttempt[key]
	if !entry.lastFailed.IsZero() && now.Sub(entry.lastFailed) > s.lockoutWindow {
		entry = attempt{}
	}
	entry.count++
	entry.lastFailed = now
	if entry.count >= s.lockoutThreshold {
		entry.lockedUntil = now.Add(s.lockoutWindow)
		s.unknownAttempt[key] = entry
		return entry.lockedUntil, true
	}
	s.unknownAttempt[key] = entry
	return time.Time{}, false
}

func (s *Service) clearUnknownFailure(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.unknownAttempt, key)
}

func (s *Service) unknownKey(username string, remoteAddr string) string {
	return normalizeUsername(username) + "|" + strings.TrimSpace(remoteAddr)
}

func parseToken(token string) (string, string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", ErrUnauthorized
	}
	return parts[0], parts[1], nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func normalizeUsername(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func randomToken(size int) string {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes)
}

func timestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTimestamp(value string) (time.Time, bool) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func formatMaybeTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return timestamp(value)
}

type lockoutError time.Time

func (e lockoutError) Error() string {
	return fmt.Sprintf("%s until %s", ErrLocked.Error(), time.Time(e).Format(time.RFC3339))
}

func LockoutUntil(err error) (time.Time, bool) {
	var typed lockoutError
	if errors.As(err, &typed) {
		return time.Time(typed), true
	}
	return time.Time{}, false
}

// UserPreferences holds per-user settings stored as JSON in the users table.
type UserPreferences struct {
	AutoSkipIntros bool   `json:"autoSkipIntros,omitempty"`
	PosterSize     string `json:"posterSize,omitempty"`
}

// UserPreferencesPatch is a partial update for per-user settings.
type UserPreferencesPatch struct {
	AutoSkipIntros *bool   `json:"autoSkipIntros,omitempty"`
	PosterSize     *string `json:"posterSize,omitempty"`
}

// GetUserPreferences returns the stored preferences for the given user ID.
func (s *Service) GetUserPreferences(ctx context.Context, userID string) (UserPreferences, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT preferences_json FROM users WHERE id = ?`, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return UserPreferences{}, nil
	}
	if err != nil {
		return UserPreferences{}, err
	}
	var prefs UserPreferences
	if raw != "" && raw != "{}" {
		if err := json.Unmarshal([]byte(raw), &prefs); err != nil {
			return UserPreferences{}, err
		}
	}
	return prefs, nil
}

// SetUserPreferences persists preferences for the given user ID.
func (s *Service) SetUserPreferences(ctx context.Context, userID string, prefs UserPreferences) error {
	prefs.PosterSize = normalizePosterSize(prefs.PosterSize)
	raw, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE users SET preferences_json = ?, updated_at = ? WHERE id = ?`,
		string(raw), timestamp(time.Now().UTC()), userID)
	return err
}

func (s *Service) UpdateUserPreferences(ctx context.Context, userID string, patch UserPreferencesPatch) (UserPreferences, error) {
	current, err := s.GetUserPreferences(ctx, userID)
	if err != nil {
		return UserPreferences{}, err
	}
	if patch.AutoSkipIntros != nil {
		current.AutoSkipIntros = *patch.AutoSkipIntros
	}
	if patch.PosterSize != nil {
		posterSize := normalizePosterSize(*patch.PosterSize)
		if posterSize == "" && strings.TrimSpace(*patch.PosterSize) != "" {
			return UserPreferences{}, ErrInvalidPreferences
		}
		current.PosterSize = posterSize
	}
	if err := s.SetUserPreferences(ctx, userID, current); err != nil {
		return UserPreferences{}, err
	}
	return current, nil
}

func normalizePosterSize(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "S", "M", "L":
		return strings.ToUpper(strings.TrimSpace(value))
	default:
		return ""
	}
}

func ContextWithResolvedSession(ctx context.Context, resolved ResolvedSession) context.Context {
	return context.WithValue(ctx, resolvedSessionKey, resolved)
}

func ResolvedSessionFromContext(ctx context.Context) (ResolvedSession, bool) {
	value := ctx.Value(resolvedSessionKey)
	resolved, ok := value.(ResolvedSession)
	return resolved, ok
}

// ContextWithActiveProfile injects the active profile user ID into the context.
func ContextWithActiveProfile(ctx context.Context, profileUserID string) context.Context {
	return context.WithValue(ctx, activeProfileKey, profileUserID)
}

// ActiveProfileFromContext retrieves the active profile user ID from the context.
// Returns ("", false) when no profile token has been presented on this request.
func ActiveProfileFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(activeProfileKey).(string)
	return v, ok && v != ""
}

// ─── Profile sessions ─────────────────────────────────────────────────────────

var (
	ErrInvalidPin    = errors.New("incorrect pin")
	ErrProfileLocked = errors.New("current profile requires exit pin")
)

const profileSessionTTL = 24 * time.Hour

// SwitchProfile validates the necessary PINs and issues a new profile session token.
//
// PIN rules:
//   - If the current profile (identified by currentProfileToken) is restricted AND has a
//     pin_hash, the caller must supply the exit PIN in currentProfilePin.
//   - If the target profile is NOT restricted AND has a pin_hash, the caller must supply
//     the entry PIN in targetProfilePin.
//
// Both checks may apply simultaneously (e.g. leaving a kids profile to enter a
// PIN-protected adult profile).  Either field can be empty string when its check
// is not needed.
func (s *Service) SwitchProfile(
	ctx context.Context,
	sessionID string,
	targetProfileUserID string,
	currentProfileToken string,
	currentProfilePin string,
	targetProfilePin string,
) (string, *ProfileCard, error) {
	if s.Disabled() {
		return "", nil, ErrUnauthorized
	}

	// ── 1. Validate exit PIN for current restricted profile (if any) ─────────
	if currentProfileToken != "" {
		var curPinHash string
		var curRestricted int
		err := s.db.QueryRowContext(ctx, `
			SELECT u.pin_hash, u.is_restricted
			FROM profile_sessions ps
			JOIN users u ON u.id = ps.profile_user_id
			WHERE ps.token = ? AND ps.session_id = ? AND ps.expires_at > ?
		`, currentProfileToken, sessionID, timestamp(time.Now().UTC())).Scan(&curPinHash, &curRestricted)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", nil, err
		}
		if err == nil && curRestricted != 0 && curPinHash != "" {
			ok, verr := verifyPassword(curPinHash, currentProfilePin)
			if verr != nil || !ok {
				return "", nil, ErrInvalidPin
			}
		}
	}

	// ── 2. Load target profile ────────────────────────────────────────────────
	var target struct {
		DisplayName  string
		AvatarURL    string
		AvatarPreset string
		AvatarColor  string
		IsRestricted int
		PinHash      string
	}
	err := s.db.QueryRowContext(ctx, `
		SELECT display_name, avatar_url, avatar_preset, avatar_color, is_restricted, pin_hash
		FROM users
		WHERE id = ? AND username <> ''
	`, targetProfileUserID).Scan(
		&target.DisplayName, &target.AvatarURL, &target.AvatarPreset, &target.AvatarColor,
		&target.IsRestricted, &target.PinHash,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrUserNotFound
	}
	if err != nil {
		return "", nil, err
	}

	// ── 3. Validate entry PIN for non-restricted target profile (if any) ──────
	if target.IsRestricted == 0 && target.PinHash != "" {
		ok, verr := verifyPassword(target.PinHash, targetProfilePin)
		if verr != nil || !ok {
			return "", nil, ErrInvalidPin
		}
	}

	// ── 4. Issue profile session token ────────────────────────────────────────
	token := randomToken(32)
	now := time.Now().UTC()
	expiresAt := now.Add(profileSessionTTL)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO profile_sessions(token, session_id, profile_user_id, created_at, expires_at)
		VALUES(?, ?, ?, ?, ?)
	`, token, sessionID, targetProfileUserID, timestamp(now), timestamp(expiresAt)); err != nil {
		return "", nil, err
	}

	card := &ProfileCard{
		ID:           targetProfileUserID,
		DisplayName:  target.DisplayName,
		AvatarURL:    target.AvatarURL,
		AvatarPreset: target.AvatarPreset,
		AvatarColor:  target.AvatarColor,
		IsRestricted: target.IsRestricted != 0,
	}
	if card.IsRestricted {
		card.HasExitPin = target.PinHash != ""
	} else {
		card.HasEntryPin = target.PinHash != ""
	}
	return token, card, nil
}

// ValidateProfileToken checks a profile session token and returns the profile
// user ID.  Returns ("", ErrUnauthorized) when the token is missing, expired,
// or does not belong to the given session.
func (s *Service) ValidateProfileToken(ctx context.Context, token string, sessionID string) (string, error) {
	if s.Disabled() || strings.TrimSpace(token) == "" {
		return "", ErrUnauthorized
	}
	var profileUserID string
	err := s.db.QueryRowContext(ctx, `
		SELECT profile_user_id
		FROM profile_sessions
		WHERE token = ? AND session_id = ? AND expires_at > ?
	`, token, sessionID, timestamp(time.Now().UTC())).Scan(&profileUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnauthorized
	}
	if err != nil {
		return "", err
	}
	return profileUserID, nil
}

// RevokeProfileSessions deletes all profile session tokens tied to a main session.
// Called on logout so switching to a profile on another device doesn't linger.
func (s *Service) RevokeProfileSessions(ctx context.Context, sessionID string) error {
	if s.Disabled() {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM profile_sessions WHERE session_id = ?`, sessionID)
	return err
}

// SetProfilePin bcrypt-hashes and stores a PIN for the given user.
// Pass an empty string to clear the PIN.
func (s *Service) SetProfilePin(ctx context.Context, userID string, pin string) error {
	if s.Disabled() {
		return ErrUnauthorized
	}
	var hash string
	if strings.TrimSpace(pin) != "" {
		h, err := hashPassword(pin)
		if err != nil {
			return err
		}
		hash = h
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE users SET pin_hash = ?, updated_at = ? WHERE id = ? AND username <> ''
	`, hash, timestamp(time.Now().UTC()), userID)
	return err
}

// UpdateProfileSettings updates avatar, restriction flag, and rating ceiling for a user.
func (s *Service) UpdateProfileSettings(
	ctx context.Context,
	userID string,
	displayName string,
	avatarURL string,
	avatarPreset string,
	avatarColor string,
	isRestricted bool,
	maxRating string,
) (UserAccount, error) {
	if s.Disabled() {
		return UserAccount{}, ErrUnauthorized
	}
	restricted := 0
	if isRestricted {
		restricted = 1
	}
	now := timestamp(time.Now().UTC())
	result, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET display_name = ?, avatar_url = ?, avatar_preset = ?, avatar_color = ?,
		    is_restricted = ?, max_rating = ?, updated_at = ?
		WHERE id = ? AND username <> ''
	`, strings.TrimSpace(displayName), strings.TrimSpace(avatarURL),
		strings.TrimSpace(avatarPreset), strings.TrimSpace(avatarColor),
		restricted, strings.TrimSpace(maxRating), now, userID)
	if err != nil {
		return UserAccount{}, err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return UserAccount{}, ErrUserNotFound
	}
	var a UserAccount
	var pinHash string
	var isRes int
	err = s.db.QueryRowContext(ctx, `
		SELECT id, username, display_name, avatar_url, avatar_preset, avatar_color,
		       role, is_restricted, max_rating, pin_hash, created_at
		FROM users WHERE id = ?
	`, userID).Scan(
		&a.ID, &a.Username, &a.DisplayName, &a.AvatarURL, &a.AvatarPreset, &a.AvatarColor,
		&a.Role, &isRes, &a.MaxRating, &pinHash, &a.CreatedAt,
	)
	if err != nil {
		return UserAccount{}, err
	}
	a.IsRestricted = isRes != 0
	a.HasPin = pinHash != ""
	return a, nil
}

// GetProfileMaxRating returns the max_rating for a profile user ID, or an
// empty string if the user has no ceiling configured or is not found. It is a
// lightweight read used by catalog handlers to enforce content ceilings.
func (s *Service) GetProfileMaxRating(ctx context.Context, profileUserID string) (string, error) {
	if profileUserID == "" {
		return "", nil
	}
	var maxRating string
	err := s.db.QueryRowContext(ctx,
		`SELECT max_rating FROM users WHERE id = ?`, profileUserID,
	).Scan(&maxRating)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return strings.TrimSpace(maxRating), err
}
