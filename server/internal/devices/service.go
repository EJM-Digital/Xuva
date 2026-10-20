package devices

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EJM-Digital/Xuva/server/internal/database"
)

var (
	ErrNotFound            = errors.New("approved device not found")
	ErrRegistryUnavailable = errors.New("approved device registry is not available")
)

const (
	StatusApproved = "approved"
	StatusRevoked  = "revoked"
)

type Profile struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Containers     []string `json:"containers"`
	VideoCodecs    []string `json:"videoCodecs"`
	AudioCodecs    []string `json:"audioCodecs"`
	SubtitleCodecs []string `json:"subtitleCodecs"`
	// MaxVideoBitDepth is the highest luma bit depth this profile can decode
	// in hardware. 0 means "unspecified" (treated as 8 in the decision tree).
	// Values: 8 (SDR-only / older clients), 10 (Main10 / HDR-capable), 12 (rare).
	MaxVideoBitDepth int `json:"maxVideoBitDepth,omitempty"`
	// MaxVideoFrameRate caps direct-play frame rate. 0 means "unspecified"
	// (no cap applied). Sources above this are routed to transcode.
	MaxVideoFrameRate   float64 `json:"maxVideoFrameRate,omitempty"`
	SupportsHDR         bool    `json:"supportsHdr"`
	SupportsToneMapping bool    `json:"supportsToneMapping"`
	SupportsHLS         bool    `json:"supportsHls"`
}

type ApprovedDevice struct {
	ID            string    `json:"id"`
	DeviceID      string    `json:"-"`
	DeviceName    string    `json:"deviceName"`
	ClientProfile string    `json:"clientProfile"`
	DisplayName   string    `json:"displayName"`
	Status        string    `json:"status"`
	ApprovedAt    time.Time `json:"approvedAt"`
	ApprovedBy    string    `json:"approvedBy"`
	AuthSessionID string    `json:"-"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type ApproveInput struct {
	DeviceID      string
	DeviceName    string
	ClientProfile string
	ApprovedBy    string
}

type Service struct {
	database *database.Service
}

func NewService() *Service {
	return &Service{}
}

func NewPersistentService(databaseService *database.Service) *Service {
	return &Service{database: databaseService}
}

func (s *Service) Profiles() []Profile {
	return []Profile{
		{
			ID:   "web",
			Name: "Web Player",
			// Browsers play these natively or via MSE. We list them generously so
			// the playback decision can pick "remux + audio-transcode" rather than
			// a full video re-encode — same approach Plex/Jellyfin use for web.
			// Container list is conservative (MP4 family) because remux always
			// repackages to MP4 anyway; the bigger win is having the video codec
			// accepted so canRemux() passes for HEVC sources.
			Containers:        []string{"mp4", "mov", "webm"},
			VideoCodecs:       []string{"h264", "hevc", "av1", "vp9"},
			AudioCodecs:       []string{"aac", "ac3", "eac3", "opus", "mp3", "flac"},
			SubtitleCodecs:    []string{"webvtt", "srt"},
			MaxVideoBitDepth:  10,
			MaxVideoFrameRate: 60,
			SupportsHLS:       true,
		},
		{
			ID:                  "android-tv",
			Name:                "Android TV",
			Containers:          []string{"mp4", "mkv", "webm", "mpegts"},
			VideoCodecs:         []string{"h264", "hevc", "av1", "vp9"},
			AudioCodecs:         []string{"aac", "ac3", "eac3", "opus", "mp3"},
			SubtitleCodecs:      []string{"srt", "ass", "webvtt", "pgs"},
			MaxVideoBitDepth:    10,
			MaxVideoFrameRate:   60,
			SupportsHDR:         true,
			SupportsToneMapping: true,
			SupportsHLS:         true,
		},
		{
			ID:                  "apple-tv",
			Name:                "Apple TV",
			Containers:          []string{"mp4", "mov", "m4v"},
			VideoCodecs:         []string{"h264", "hevc"},
			AudioCodecs:         []string{"aac", "ac3", "eac3", "alac"},
			SubtitleCodecs:      []string{"webvtt", "srt"},
			MaxVideoBitDepth:    10,
			MaxVideoFrameRate:   60,
			SupportsHDR:         true,
			SupportsToneMapping: true,
			SupportsHLS:         true,
		},
		{
			ID:                  "ios",
			Name:                "iPhone / iPad",
			Containers:          []string{"mp4", "mov", "m4v"},
			VideoCodecs:         []string{"h264", "hevc"},
			AudioCodecs:         []string{"aac", "ac3", "eac3", "alac"},
			SubtitleCodecs:      []string{"webvtt", "srt"},
			MaxVideoBitDepth:    10,
			MaxVideoFrameRate:   60,
			SupportsHDR:         true,
			SupportsToneMapping: true,
			SupportsHLS:         true,
		},
		{
			ID:                "chromecast",
			Name:              "Chromecast",
			Containers:        []string{"mp4", "webm"},
			VideoCodecs:       []string{"h264", "vp9", "av1"},
			AudioCodecs:       []string{"aac", "ac3", "eac3", "opus"},
			SubtitleCodecs:    []string{"webvtt", "srt"},
			MaxVideoBitDepth:  10,
			MaxVideoFrameRate: 60,
			SupportsHDR:       true,
			SupportsHLS:       true,
		},
	}
}

func (s *Service) GetProfile(id string) (Profile, bool) {
	if id == "" {
		id = "web"
	}
	for _, profile := range s.Profiles() {
		if profile.ID == id {
			return profile, true
		}
	}
	return Profile{}, false
}

func (s *Service) ListApproved(ctx context.Context) ([]ApprovedDevice, error) {
	if s == nil || s.database == nil {
		return []ApprovedDevice{}, nil
	}
	rows, err := s.database.DB().QueryContext(ctx, `
		SELECT id, device_id, device_name, client_profile, display_name, status, approved_at, approved_by, auth_session_id, created_at, updated_at
		FROM approved_devices
		WHERE status = ?
		ORDER BY updated_at DESC, approved_at DESC
	`, StatusApproved)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ApprovedDevice, 0)
	for rows.Next() {
		item, err := scanApprovedDevice(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Service) IsApproved(ctx context.Context, deviceID string) (bool, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false, nil
	}
	item, found, err := s.findByDeviceID(ctx, deviceID)
	if err != nil {
		return false, err
	}
	return found && item.Status == StatusApproved, nil
}

func (s *Service) Approve(ctx context.Context, input ApproveInput) (ApprovedDevice, error) {
	if s == nil || s.database == nil {
		return ApprovedDevice{}, ErrRegistryUnavailable
	}
	deviceID := strings.TrimSpace(input.DeviceID)
	if deviceID == "" {
		return ApprovedDevice{}, errors.New("device id is required")
	}
	profileID := strings.TrimSpace(input.ClientProfile)
	if profileID == "" {
		profileID = "apple-tv"
	}
	deviceName := strings.TrimSpace(input.DeviceName)
	if deviceName == "" {
		deviceName = s.profileName(profileID)
	}
	approvedBy := strings.TrimSpace(input.ApprovedBy)
	if approvedBy == "" {
		approvedBy = "local-admin"
	}
	displayName := deviceName
	now := time.Now().UTC()
	existing, found, err := s.findByDeviceID(ctx, deviceID)
	if err != nil {
		return ApprovedDevice{}, err
	}
	if !found {
		item := ApprovedDevice{
			ID:            "approved_device_" + uuid.NewString(),
			DeviceID:      deviceID,
			DeviceName:    deviceName,
			ClientProfile: profileID,
			DisplayName:   displayName,
			Status:        StatusApproved,
			ApprovedAt:    now,
			ApprovedBy:    approvedBy,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if _, err := s.database.DB().ExecContext(ctx, `
			INSERT INTO approved_devices(id, device_id, device_name, client_profile, display_name, status, approved_at, approved_by, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, item.ID, item.DeviceID, item.DeviceName, item.ClientProfile, item.DisplayName, item.Status, item.ApprovedAt.Format(time.RFC3339Nano), item.ApprovedBy, item.CreatedAt.Format(time.RFC3339Nano), item.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
			return ApprovedDevice{}, err
		}
		return item, nil
	}
	approvedAt := existing.ApprovedAt
	if approvedAt.IsZero() || existing.Status != StatusApproved {
		approvedAt = now
	}
	approvedByValue := existing.ApprovedBy
	if approvedByValue == "" || existing.Status != StatusApproved {
		approvedByValue = approvedBy
	}
	updated := ApprovedDevice{
		ID:            existing.ID,
		DeviceID:      existing.DeviceID,
		DeviceName:    deviceName,
		ClientProfile: profileID,
		DisplayName:   displayName,
		Status:        StatusApproved,
		ApprovedAt:    approvedAt,
		ApprovedBy:    approvedByValue,
		CreatedAt:     existing.CreatedAt,
		UpdatedAt:     now,
	}
	if updated.CreatedAt.IsZero() {
		updated.CreatedAt = now
	}
	if _, err := s.database.DB().ExecContext(ctx, `
		UPDATE approved_devices
		SET device_name = ?, client_profile = ?, display_name = ?, status = ?, approved_at = ?, approved_by = ?, updated_at = ?
		WHERE id = ?
	`, updated.DeviceName, updated.ClientProfile, updated.DisplayName, updated.Status, updated.ApprovedAt.Format(time.RFC3339Nano), updated.ApprovedBy, updated.UpdatedAt.Format(time.RFC3339Nano), updated.ID); err != nil {
		return ApprovedDevice{}, err
	}
	return updated, nil
}

// AttachSession links an approved device to the auth session that was
// issued when it was approved, so a later Revoke can invalidate the token
// rather than leaving it usable. Best-effort: returns nil if the device
// row no longer exists.
func (s *Service) AttachSession(ctx context.Context, deviceID string, sessionID string) error {
	if s == nil || s.database == nil {
		return ErrRegistryUnavailable
	}
	deviceID = strings.TrimSpace(deviceID)
	sessionID = strings.TrimSpace(sessionID)
	if deviceID == "" || sessionID == "" {
		return nil
	}
	_, err := s.database.DB().ExecContext(ctx, `
		UPDATE approved_devices
		SET auth_session_id = ?, updated_at = ?
		WHERE device_id = ?
	`, sessionID, time.Now().UTC().Format(time.RFC3339Nano), deviceID)
	return err
}

func (s *Service) Revoke(ctx context.Context, id string) (ApprovedDevice, error) {
	if s == nil || s.database == nil {
		return ApprovedDevice{}, ErrRegistryUnavailable
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return ApprovedDevice{}, ErrNotFound
	}
	item, found, err := s.findByID(ctx, id)
	if err != nil {
		return ApprovedDevice{}, err
	}
	if !found {
		return ApprovedDevice{}, ErrNotFound
	}
	item.Status = StatusRevoked
	item.UpdatedAt = time.Now().UTC()
	if _, err := s.database.DB().ExecContext(ctx, `
		UPDATE approved_devices
		SET status = ?, auth_session_id = '', updated_at = ?
		WHERE id = ?
	`, item.Status, item.UpdatedAt.Format(time.RFC3339Nano), item.ID); err != nil {
		return ApprovedDevice{}, err
	}
	// item.AuthSessionID still holds the previous session id so the caller
	// can hand it to auth.RevokeSessionID before discarding.
	return item, nil
}

func (s *Service) profileName(id string) string {
	if profile, ok := s.GetProfile(id); ok && strings.TrimSpace(profile.Name) != "" {
		return profile.Name
	}
	return "Approved device"
}

func (s *Service) findByDeviceID(ctx context.Context, deviceID string) (ApprovedDevice, bool, error) {
	if s == nil || s.database == nil {
		return ApprovedDevice{}, false, ErrRegistryUnavailable
	}
	row := s.database.DB().QueryRowContext(ctx, `
		SELECT id, device_id, device_name, client_profile, display_name, status, approved_at, approved_by, auth_session_id, created_at, updated_at
		FROM approved_devices
		WHERE device_id = ?
		LIMIT 1
	`, deviceID)
	item, err := scanApprovedDevice(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ApprovedDevice{}, false, nil
		}
		return ApprovedDevice{}, false, err
	}
	return item, true, nil
}

func (s *Service) findByID(ctx context.Context, id string) (ApprovedDevice, bool, error) {
	if s == nil || s.database == nil {
		return ApprovedDevice{}, false, ErrRegistryUnavailable
	}
	row := s.database.DB().QueryRowContext(ctx, `
		SELECT id, device_id, device_name, client_profile, display_name, status, approved_at, approved_by, auth_session_id, created_at, updated_at
		FROM approved_devices
		WHERE id = ?
		LIMIT 1
	`, id)
	item, err := scanApprovedDevice(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ApprovedDevice{}, false, nil
		}
		return ApprovedDevice{}, false, err
	}
	return item, true, nil
}

type approvedDeviceScanner interface {
	Scan(dest ...any) error
}

func scanApprovedDevice(scanner approvedDeviceScanner) (ApprovedDevice, error) {
	var (
		item            ApprovedDevice
		approvedAtValue string
		createdAtValue  string
		updatedAtValue  string
	)
	if err := scanner.Scan(&item.ID, &item.DeviceID, &item.DeviceName, &item.ClientProfile, &item.DisplayName, &item.Status, &approvedAtValue, &item.ApprovedBy, &item.AuthSessionID, &createdAtValue, &updatedAtValue); err != nil {
		return ApprovedDevice{}, err
	}
	item.ApprovedAt = parseDeviceTime(approvedAtValue)
	item.CreatedAt = parseDeviceTime(createdAtValue)
	item.UpdatedAt = parseDeviceTime(updatedAtValue)
	return item, nil
}

func parseDeviceTime(value string) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		return parsed
	}
	return time.Time{}
}

func SortApprovedDevices(items []ApprovedDevice) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].ApprovedAt.After(items[j].ApprovedAt)
		}
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
}
