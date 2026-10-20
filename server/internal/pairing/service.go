package pairing

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/EJM-Digital/Xuva/server/internal/database"
)

const (
	StatusPending  = "pending"
	StatusApproved = "approved"
)

var (
	ErrNotFound = errors.New("pairing request not found")
	ErrClosed   = errors.New("pairing request is no longer pending")
)

type Request struct {
	ID            string     `json:"id"`
	Code          string     `json:"code,omitempty"`
	DeviceName    string     `json:"deviceName"`
	ClientProfile string     `json:"clientProfile"`
	DeviceID      string     `json:"deviceId,omitempty"`
	Auth          *AuthGrant `json:"auth,omitempty"`
	Status        string     `json:"status"`
	ApprovedBy    string     `json:"approvedBy,omitempty"`
	ExpiresAt     time.Time  `json:"expiresAt"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

type AuthGrant struct {
	Method       string    `json:"method"`
	SessionToken string    `json:"sessionToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

type CreateRequest struct {
	DeviceName    string `json:"deviceName"`
	ClientProfile string `json:"clientProfile"`
	DeviceID      string `json:"deviceId"`
}

type Service struct {
	ttl      time.Duration
	database *database.Service
	mu       sync.RWMutex
	byID     map[string]Request
}

func NewService() *Service {
	return &Service{
		ttl:  10 * time.Minute,
		byID: map[string]Request{},
	}
}

func NewPersistentService(databaseService *database.Service) *Service {
	return &Service{
		ttl:      10 * time.Minute,
		database: databaseService,
		byID:     map[string]Request{},
	}
}

func (s *Service) Create(request CreateRequest) (Request, error) {
	now := time.Now().UTC()
	deviceName := strings.TrimSpace(request.DeviceName)
	if deviceName == "" {
		deviceName = "Apple TV"
	}
	clientProfile := strings.TrimSpace(request.ClientProfile)
	if clientProfile == "" {
		clientProfile = "apple-tv"
	}
	code, err := randomCode()
	if err != nil {
		return Request{}, err
	}
	item := Request{
		ID:            "pair_" + uuid.NewString(),
		Code:          code,
		DeviceName:    deviceName,
		ClientProfile: clientProfile,
		DeviceID:      strings.TrimSpace(request.DeviceID),
		Status:        StatusPending,
		ExpiresAt:     now.Add(s.ttl),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if s.database != nil {
		_, err := execPairingWrite(s.database.DB(), `
			INSERT INTO pairing_requests(id, code, device_name, client_profile, device_id, status, approved_by, expires_at, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, item.ID, item.Code, item.DeviceName, item.ClientProfile, item.DeviceID, item.Status, item.ApprovedBy, formatTime(item.ExpiresAt), formatTime(item.CreatedAt), formatTime(item.UpdatedAt))
		if err != nil {
			return Request{}, err
		}
	} else {
		s.mu.Lock()
		s.byID[item.ID] = item
		s.mu.Unlock()
	}
	return item, nil
}

func (s *Service) List() []Request {
	s.expireOld()
	if s.database != nil {
		now := time.Now().UTC()
		rows, err := s.database.DB().Query(`
			SELECT id, code, device_name, client_profile, device_id, auth_method, auth_session_token, auth_expires_at, status, approved_by, expires_at, created_at, updated_at
			FROM pairing_requests
			WHERE status = ? AND expires_at >= ?
			ORDER BY created_at DESC
		`, StatusPending, formatTime(now))
		if err != nil {
			return []Request{}
		}
		defer rows.Close()
		output := make([]Request, 0)
		for rows.Next() {
			item, err := scanRequest(rows)
			if err == nil {
				output = append(output, publicRequest(item, false))
			}
		}
		return output
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	output := make([]Request, 0, len(s.byID))
	for _, item := range s.byID {
		if item.Status != StatusPending {
			continue
		}
		output = append(output, publicRequest(item, false))
	}
	sort.Slice(output, func(i, j int) bool { return output[i].CreatedAt.After(output[j].CreatedAt) })
	return output
}

func (s *Service) Get(id string) (Request, bool) {
	s.expireOld()
	if s.database != nil {
		item, ok := s.getPersistent(id)
		if !ok {
			return Request{}, false
		}
		return publicRequest(item, true), true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.byID[id]
	if !ok {
		return Request{}, false
	}
	return publicRequest(item, true), true
}

func (s *Service) Approve(id string, approvedBy string) (Request, error) {
	return s.close(id, StatusApproved, approvedBy, "")
}

func (s *Service) ApproveWithDeviceID(id string, approvedBy string, deviceID string) (Request, error) {
	return s.close(id, StatusApproved, approvedBy, deviceID)
}

func (s *Service) Deny(id string, _ string) (Request, error) {
	id = strings.TrimSpace(id)
	if s.database != nil {
		item, ok := s.getPersistent(id)
		if !ok {
			return Request{}, ErrNotFound
		}
		if item.Status != StatusPending {
			return Request{}, ErrClosed
		}
		_, err := execPairingWrite(s.database.DB(), `DELETE FROM pairing_requests WHERE id = ?`, id)
		if err != nil {
			return Request{}, err
		}
		return publicRequest(item, false), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.byID[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	if item.Status != StatusPending {
		return Request{}, ErrClosed
	}
	delete(s.byID, id)
	return publicRequest(item, false), nil
}

func (s *Service) AttachAuthGrant(id string, grant AuthGrant) (Request, error) {
	if s.database != nil {
		now := time.Now().UTC()
		result, err := execPairingWrite(s.database.DB(), `
			UPDATE pairing_requests
			SET auth_method = ?, auth_session_token = ?, auth_expires_at = ?, updated_at = ?
			WHERE id = ? AND status = ?
		`, grant.Method, grant.SessionToken, formatTime(grant.ExpiresAt), formatTime(now), strings.TrimSpace(id), StatusApproved)
		if err != nil {
			return Request{}, err
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			item, ok := s.getPersistent(id)
			if !ok {
				return Request{}, ErrNotFound
			}
			if item.Status != StatusApproved {
				return Request{}, ErrClosed
			}
		}
		item, ok := s.getPersistent(id)
		if !ok {
			return Request{}, ErrNotFound
		}
		return publicRequest(item, true), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.byID[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	if item.Status != StatusApproved {
		return Request{}, ErrClosed
	}
	item.Auth = &grant
	item.UpdatedAt = time.Now().UTC()
	s.byID[id] = item
	return publicRequest(item, true), nil
}

func (s *Service) close(id string, status string, approvedBy string, deviceID string) (Request, error) {
	s.expireOld()
	if s.database != nil {
		item, ok := s.getPersistent(id)
		if !ok {
			return Request{}, ErrNotFound
		}
		if item.Status != StatusPending {
			return Request{}, ErrClosed
		}
		item.Status = status
		item.ApprovedBy = strings.TrimSpace(approvedBy)
		item.UpdatedAt = time.Now().UTC()
		if status == StatusApproved {
			item.DeviceID = strings.TrimSpace(firstNonEmpty(item.DeviceID, deviceID))
			if item.DeviceID == "" {
				item.DeviceID = "device_" + uuid.NewString()
			}
		}
		_, err := execPairingWrite(s.database.DB(), `
			UPDATE pairing_requests
			SET status = ?, approved_by = ?, device_id = ?, updated_at = ?
			WHERE id = ?
		`, item.Status, item.ApprovedBy, item.DeviceID, formatTime(item.UpdatedAt), item.ID)
		if err != nil {
			return Request{}, err
		}
		return publicRequest(item, true), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.byID[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	if item.Status != StatusPending {
		return Request{}, ErrClosed
	}
	item.Status = status
	item.ApprovedBy = strings.TrimSpace(approvedBy)
	item.UpdatedAt = time.Now().UTC()
	if status == StatusApproved {
		item.DeviceID = strings.TrimSpace(firstNonEmpty(item.DeviceID, deviceID))
		if item.DeviceID == "" {
			item.DeviceID = "device_" + uuid.NewString()
		}
	}
	s.byID[id] = item
	return publicRequest(item, true), nil
}

func (s *Service) expireOld() {
	now := time.Now().UTC()
	if s.database != nil {
		_, _ = execPairingWrite(s.database.DB(), `
			DELETE FROM pairing_requests WHERE status = ? AND expires_at < ?
		`, StatusPending, formatTime(now))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, item := range s.byID {
		if item.Status == StatusPending && now.After(item.ExpiresAt) {
			delete(s.byID, id)
		}
	}
}

// Purge removes expired pending rows immediately and terminal rows after the
// retention window. Approved rows are kept briefly so native clients can poll
// the approval result and receive their credential after the owner clicks
// approve, but they must not remain visible as pending approval work.
func (s *Service) Purge(retain time.Duration) (int, error) {
	now := time.Now().UTC()
	if retain <= 0 {
		retain = 24 * time.Hour
	}
	cutoff := now.Add(-retain)
	if s.database != nil {
		pending, err := execPairingWrite(s.database.DB(), `
			DELETE FROM pairing_requests WHERE status = ? AND expires_at < ?
		`, StatusPending, formatTime(now))
		if err != nil {
			return 0, err
		}
		terminal, err := execPairingWrite(s.database.DB(), `
			DELETE FROM pairing_requests WHERE status <> ? AND updated_at < ?
		`, StatusPending, formatTime(cutoff))
		if err != nil {
			return 0, err
		}
		pendingCount, _ := pending.RowsAffected()
		terminalCount, _ := terminal.RowsAffected()
		return int(pendingCount + terminalCount), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, item := range s.byID {
		if item.Status == StatusPending && now.After(item.ExpiresAt) {
			delete(s.byID, id)
			removed++
			continue
		}
		if item.Status != StatusPending && item.UpdatedAt.Before(cutoff) {
			delete(s.byID, id)
			removed++
		}
	}
	return removed, nil
}

// Cancel removes a still-pending pairing request that belongs to the given
// deviceId. Used by clients to clean up their own orphan request when the
// user resets the pairing flow without waiting for an admin to approve.
// Returns ErrNotFound if the id doesn't exist or doesn't match the deviceId.
func (s *Service) Cancel(id string, deviceID string) error {
	id = strings.TrimSpace(id)
	deviceID = strings.TrimSpace(deviceID)
	if id == "" || deviceID == "" {
		return ErrNotFound
	}
	if s.database != nil {
		item, ok := s.getPersistent(id)
		if !ok {
			return ErrNotFound
		}
		if item.DeviceID != deviceID {
			return ErrNotFound
		}
		if item.Status == StatusApproved {
			return ErrClosed
		}
		_, err := execPairingWrite(s.database.DB(), `DELETE FROM pairing_requests WHERE id = ?`, id)
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.byID[id]
	if !ok || item.DeviceID != deviceID {
		return ErrNotFound
	}
	if item.Status == StatusApproved {
		return ErrClosed
	}
	delete(s.byID, id)
	return nil
}

func (s *Service) getPersistent(id string) (Request, bool) {
	if s == nil || s.database == nil {
		return Request{}, false
	}
	row := s.database.DB().QueryRow(`
		SELECT id, code, device_name, client_profile, device_id, auth_method, auth_session_token, auth_expires_at, status, approved_by, expires_at, created_at, updated_at
		FROM pairing_requests
		WHERE id = ?
		LIMIT 1
	`, strings.TrimSpace(id))
	item, err := scanRequest(row)
	if err != nil {
		return Request{}, false
	}
	return item, true
}

type requestScanner interface {
	Scan(dest ...any) error
}

func scanRequest(scanner requestScanner) (Request, error) {
	var (
		item             Request
		authMethod       string
		authSessionToken string
		authExpiresAt    string
		expiresAt        string
		createdAt        string
		updatedAt        string
	)
	err := scanner.Scan(
		&item.ID,
		&item.Code,
		&item.DeviceName,
		&item.ClientProfile,
		&item.DeviceID,
		&authMethod,
		&authSessionToken,
		&authExpiresAt,
		&item.Status,
		&item.ApprovedBy,
		&expiresAt,
		&createdAt,
		&updatedAt,
	)
	if err != nil {
		return Request{}, err
	}
	item.ExpiresAt = parseTime(expiresAt)
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	if strings.TrimSpace(authMethod) != "" || strings.TrimSpace(authSessionToken) != "" {
		item.Auth = &AuthGrant{
			Method:       authMethod,
			SessionToken: authSessionToken,
			ExpiresAt:    parseTime(authExpiresAt),
		}
	}
	return item, nil
}

func parseTime(value string) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		return parsed
	}
	return time.Time{}
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func publicRequest(item Request, includeAuth bool) Request {
	if item.Status != StatusPending {
		item.Code = ""
	}
	if !includeAuth {
		item.Auth = nil
	}
	return item
}

type pairingExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func execPairingWrite(db pairingExecer, query string, args ...any) (sql.Result, error) {
	var result sql.Result
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		result, err = db.Exec(query, args...)
		if !isSQLiteBusy(err) {
			return result, err
		}
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	return result, err
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "sqlite_busy") || strings.Contains(text, "database is locked") || strings.Contains(text, "database table is locked")
}

func randomCode() (string, error) {
	var bytes [4]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	value := int(bytes[0])<<24 | int(bytes[1])<<16 | int(bytes[2])<<8 | int(bytes[3])
	if value < 0 {
		value = -value
	}
	return fmt.Sprintf("%06d", value%1000000), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
