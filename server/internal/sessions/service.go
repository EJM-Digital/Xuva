package sessions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/events"
	runtimestore "github.com/EJM-Digital/Xuva/server/internal/runtime"
)

type Session struct {
	ID              string            `json:"id"`
	UserID          string            `json:"userId"`
	DeviceID        string            `json:"deviceId"`
	MediaSourceID   string            `json:"mediaSourceId"`
	Title           string            `json:"title,omitempty"`
	ArtworkURL      string            `json:"artworkUrl,omitempty"`
	SourceName      string            `json:"sourceName,omitempty"`
	QualityLabel    string            `json:"qualityLabel,omitempty"`
	Container       string            `json:"container,omitempty"`
	VideoCodec      string            `json:"videoCodec,omitempty"`
	AudioCodec      string            `json:"audioCodec,omitempty"`
	Bitrate         int64             `json:"bitrate,omitempty"`
	ClientProfile   string            `json:"clientProfile,omitempty"`
	Route           string            `json:"route,omitempty"`
	ServerImpact    string            `json:"serverImpact,omitempty"`
	Mode            string            `json:"mode"`
	VideoAction     string            `json:"videoAction,omitempty"`
	AudioAction     string            `json:"audioAction,omitempty"`
	ContainerAction string            `json:"containerAction,omitempty"`
	ReasonCode      string            `json:"reasonCode,omitempty"`
	ReasonText      string            `json:"reasonText,omitempty"`
	EncoderLabel    string            `json:"encoderLabel,omitempty"`
	SelectedTracks  map[string]string `json:"selectedTracks,omitempty"`
	RouteHistory    []RouteChange     `json:"routeHistory,omitempty"`
	Status          string            `json:"status"`
	Progress        float64           `json:"progressSeconds"`
	Duration        float64           `json:"durationSeconds"`
	StartedAt       time.Time         `json:"startedAt"`
	UpdatedAt       time.Time         `json:"updatedAt"`
}

type RouteChange struct {
	FromRoute  string    `json:"fromRoute,omitempty"`
	ToRoute    string    `json:"toRoute"`
	FromReason string    `json:"fromReason,omitempty"`
	ToReason   string    `json:"toReason,omitempty"`
	ChangedAt  time.Time `json:"changedAt"`
}

type Inspector struct {
	SessionID      string            `json:"sessionId"`
	MediaSourceID  string            `json:"mediaSourceId"`
	DeviceID       string            `json:"deviceId"`
	ClientProfile  string            `json:"clientProfile,omitempty"`
	Title          string            `json:"title,omitempty"`
	Route          string            `json:"route,omitempty"`
	Mode           string            `json:"mode,omitempty"`
	ReasonCode     string            `json:"reasonCode,omitempty"`
	ReasonText     string            `json:"reasonText,omitempty"`
	SelectedTracks map[string]string `json:"selectedTracks,omitempty"`
	Bitrate        int64             `json:"bitrate,omitempty"`
	ServerImpact   string            `json:"serverImpact,omitempty"`
	Progress       float64           `json:"progressSeconds"`
	Duration       float64           `json:"durationSeconds"`
	Status         string            `json:"status"`
	RouteHistory   []RouteChange     `json:"routeHistory,omitempty"`
	UpdatedAt      time.Time         `json:"updatedAt"`
}

type StartRequest struct {
	UserID          string            `json:"userId"`
	DeviceID        string            `json:"deviceId"`
	MediaSourceID   string            `json:"mediaSourceId"`
	Title           string            `json:"title"`
	ArtworkURL      string            `json:"artworkUrl"`
	SourceName      string            `json:"sourceName"`
	QualityLabel    string            `json:"qualityLabel"`
	Container       string            `json:"container"`
	VideoCodec      string            `json:"videoCodec"`
	AudioCodec      string            `json:"audioCodec"`
	Bitrate         int64             `json:"bitrate"`
	ClientProfile   string            `json:"clientProfile"`
	Route           string            `json:"route"`
	ServerImpact    string            `json:"serverImpact"`
	Mode            string            `json:"mode"`
	VideoAction     string            `json:"videoAction"`
	AudioAction     string            `json:"audioAction"`
	ContainerAction string            `json:"containerAction"`
	ReasonCode      string            `json:"reasonCode"`
	ReasonText      string            `json:"reasonText"`
	EncoderLabel    string            `json:"encoderLabel,omitempty"`
	SelectedTracks  map[string]string `json:"selectedTracks"`
	ProgressSeconds float64           `json:"progressSeconds"`
	DurationSeconds float64           `json:"durationSeconds"`
}

type UpdateRequest struct {
	ProgressSeconds float64           `json:"progressSeconds"`
	DurationSeconds float64           `json:"durationSeconds"`
	Status          string            `json:"status"`
	Route           string            `json:"route"`
	Mode            string            `json:"mode"`
	ReasonCode      string            `json:"reasonCode"`
	ReasonText      string            `json:"reasonText"`
	ServerImpact    string            `json:"serverImpact"`
	SelectedTracks  map[string]string `json:"selectedTracks"`
}

type Service struct {
	events *events.Bus
	store  *runtimestore.Store
	nextID atomic.Uint64
	mu     sync.RWMutex
	items  map[string]Session
}

func NewService(eventBus *events.Bus) *Service {
	return &Service{events: eventBus, items: map[string]Session{}}
}

func NewPersistentService(ctx context.Context, eventBus *events.Bus, store *runtimestore.Store) (*Service, error) {
	service := &Service{events: eventBus, store: store, items: map[string]Session{}}
	if err := service.recover(ctx, 15*time.Minute); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *Service) Start(request StartRequest) (Session, error) {
	if request.MediaSourceID == "" {
		return Session{}, errors.New("media source id is required")
	}
	if request.UserID == "" {
		request.UserID = "local"
	}
	if request.DeviceID == "" {
		request.DeviceID = "web"
	}
	if request.Mode == "" {
		request.Mode = "direct"
	}
	now := time.Now().UTC()
	session := Session{
		ID:              s.nextSessionID(),
		UserID:          request.UserID,
		DeviceID:        request.DeviceID,
		MediaSourceID:   request.MediaSourceID,
		Title:           request.Title,
		ArtworkURL:      request.ArtworkURL,
		SourceName:      request.SourceName,
		QualityLabel:    request.QualityLabel,
		Container:       request.Container,
		VideoCodec:      request.VideoCodec,
		AudioCodec:      request.AudioCodec,
		Bitrate:         request.Bitrate,
		ClientProfile:   request.ClientProfile,
		Route:           request.Route,
		ServerImpact:    request.ServerImpact,
		Mode:            request.Mode,
		VideoAction:     request.VideoAction,
		AudioAction:     request.AudioAction,
		ContainerAction: request.ContainerAction,
		ReasonCode:      request.ReasonCode,
		ReasonText:      request.ReasonText,
		EncoderLabel:    request.EncoderLabel,
		SelectedTracks:  cloneMap(request.SelectedTracks),
		Status:          "playing",
		Progress:        nonNegative(request.ProgressSeconds),
		Duration:        nonNegative(request.DurationSeconds),
		StartedAt:       now,
		UpdatedAt:       now,
	}
	s.remember(session)
	_ = s.persist(context.Background(), session)
	s.publish("session.started", session)
	return session, nil
}

func (s *Service) Update(id string, request UpdateRequest) (Session, bool) {
	s.mu.Lock()
	session, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return Session{}, false
	}
	session.Progress = nonNegative(request.ProgressSeconds)
	session.Duration = nonNegative(request.DurationSeconds)
	previousRoute := session.Route
	previousReason := session.ReasonCode
	if request.Status != "" {
		session.Status = request.Status
	}
	if request.Route != "" {
		session.Route = request.Route
	}
	if request.Mode != "" {
		session.Mode = request.Mode
	}
	if request.ReasonCode != "" {
		session.ReasonCode = request.ReasonCode
	}
	if request.ReasonText != "" {
		session.ReasonText = request.ReasonText
	}
	if request.ServerImpact != "" {
		session.ServerImpact = request.ServerImpact
	}
	if request.SelectedTracks != nil {
		session.SelectedTracks = cloneMap(request.SelectedTracks)
	}
	routeChanged := request.Route != "" && request.Route != previousRoute
	if routeChanged {
		session.RouteHistory = append(session.RouteHistory, RouteChange{
			FromRoute:  previousRoute,
			ToRoute:    session.Route,
			FromReason: previousReason,
			ToReason:   session.ReasonCode,
			ChangedAt:  time.Now().UTC(),
		})
	}
	session.UpdatedAt = time.Now().UTC()
	s.items[id] = session
	s.mu.Unlock()
	_ = s.persist(context.Background(), session)
	s.publish("session.updated", session)
	s.publish("session.inspector.updated", s.inspectorFor(session))
	if routeChanged {
		s.publish("session.route.changed", session.RouteHistory[len(session.RouteHistory)-1])
	}
	return session, true
}

func (s *Service) Stop(id string) (Session, bool) {
	s.mu.Lock()
	session, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return Session{}, false
	}
	session.Status = "stopped"
	session.UpdatedAt = time.Now().UTC()
	delete(s.items, id)
	s.mu.Unlock()
	_ = s.persist(context.Background(), session)
	s.publish("session.stopped", session)
	return session, true
}

// Cleanup expires sessions whose heartbeat has not been updated within ttl,
// marks them "stale", removes them from the in-memory map, and returns the
// full list of sessions that were expired so callers can tear down any
// associated work (e.g. active transcode jobs).
func (s *Service) Cleanup(ctx context.Context, ttl time.Duration, terminalRetention time.Duration) ([]Session, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	now := time.Now().UTC()
	cutoff := now.Add(-ttl)
	var expired []Session
	s.mu.Lock()
	for id, session := range s.items {
		if session.UpdatedAt.Before(cutoff) {
			session.Status = "stale"
			session.ReasonCode = "session_heartbeat_expired"
			session.ReasonText = "Playback heartbeat expired before the server saw a clean stop."
			session.UpdatedAt = now
			delete(s.items, id)
			expired = append(expired, session)
			_ = s.persist(ctx, session)
			s.publish("session.stale", session)
		}
	}
	s.mu.Unlock()
	if terminalRetention > 0 {
		_, err := s.store.CleanupTerminal(ctx, "session", now.Add(-terminalRetention), "stopped", "stale")
		return expired, err
	}
	return expired, nil
}

func (s *Service) List() []Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	output := make([]Session, 0, len(s.items))
	for _, item := range s.items {
		output = append(output, item)
	}
	sort.Slice(output, func(i, j int) bool { return output[i].UpdatedAt.After(output[j].UpdatedAt) })
	return output
}

func (s *Service) Get(id string) (Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.items[id]
	return session, ok
}

func (s *Service) Inspector(id string) (Inspector, bool) {
	session, ok := s.Get(id)
	if !ok {
		return Inspector{}, false
	}
	return s.inspectorFor(session), true
}

func (s *Service) inspectorFor(session Session) Inspector {
	return Inspector{
		SessionID:      session.ID,
		MediaSourceID:  session.MediaSourceID,
		DeviceID:       session.DeviceID,
		ClientProfile:  session.ClientProfile,
		Title:          session.Title,
		Route:          session.Route,
		Mode:           session.Mode,
		ReasonCode:     session.ReasonCode,
		ReasonText:     session.ReasonText,
		SelectedTracks: cloneMap(session.SelectedTracks),
		Bitrate:        session.Bitrate,
		ServerImpact:   session.ServerImpact,
		Progress:       session.Progress,
		Duration:       session.Duration,
		Status:         session.Status,
		RouteHistory:   append([]RouteChange(nil), session.RouteHistory...),
		UpdatedAt:      session.UpdatedAt,
	}
}

func (s *Service) remember(session Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[session.ID] = session
}

func (s *Service) recover(ctx context.Context, ttl time.Duration) error {
	entities, err := s.store.List(ctx, "session")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	cutoff := now.Add(-ttl)
	for _, entity := range entities {
		var session Session
		if err := json.Unmarshal([]byte(entity.PayloadJSON), &session); err != nil {
			continue
		}
		switch session.Status {
		case "stopped", "stale":
			continue
		default:
			if entity.HeartbeatAt.Before(cutoff) || session.UpdatedAt.Before(cutoff) {
				session.Status = "stale"
				session.ReasonCode = "session_recovered_stale"
				session.ReasonText = "The server restarted after this playback session stopped sending heartbeats."
				session.UpdatedAt = now
				_ = s.persist(ctx, session)
				continue
			}
			s.items[session.ID] = session
		}
	}
	return nil
}

func (s *Service) persist(ctx context.Context, session Session) error {
	if s.store == nil {
		return nil
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return s.store.Save(ctx, runtimestore.Entity{
		Type:        "session",
		ID:          session.ID,
		Status:      session.Status,
		PayloadJSON: string(payload),
		CreatedAt:   session.StartedAt,
		UpdatedAt:   session.UpdatedAt,
		HeartbeatAt: session.UpdatedAt,
		CompletedAt: terminalTime(session.Status, session.UpdatedAt),
	})
}

func (s *Service) publish(eventType string, payload any) {
	if s.events != nil {
		s.events.Publish(eventType, payload)
	}
}

func cloneMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func (s *Service) nextSessionID() string {
	// Defense-in-depth alongside the router-level ownership checks: an
	// unpredictable suffix means even if a future API change accidentally
	// re-exposes a session by ID, the ID can't be guessed/enumerated from
	// timing or a previously-known ID. Falls back to the legacy
	// timestamp+counter format on the (theoretically impossible)
	// crypto/rand.Read failure, so this can never break startup.
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return "session_" + time.Now().UTC().Format("20060102T150405") + "_" + hex.EncodeToString(buf[:])
	}
	return "session_" + time.Now().UTC().Format("20060102T150405") + "_" + stringID(s.nextID.Add(1))
}

func nonNegative(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}

func terminalTime(status string, updatedAt time.Time) time.Time {
	switch status {
	case "stopped", "stale":
		return updatedAt
	default:
		return time.Time{}
	}
}

func stringID(value uint64) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[i:])
}
