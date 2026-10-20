package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/events"
)

type contextKey string

const correlationIDKey contextKey = "correlation_id"

type RequestMetric struct {
	Method          string  `json:"method"`
	Path            string  `json:"path"`
	Count           int64   `json:"count"`
	Errors          int64   `json:"errors"`
	AverageLatency  float64 `json:"averageLatencyMs"`
	MaxLatency      float64 `json:"maxLatencyMs"`
	LastStatus      int     `json:"lastStatus"`
	LastCorrelation string  `json:"lastCorrelationId"`
	LastSeenAt      string  `json:"lastSeenAt"`
}

type EventMetric struct {
	Type       string `json:"type"`
	Count      int64  `json:"count"`
	LastSeenAt string `json:"lastSeenAt"`
}

type Alert struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Action   string `json:"action"`
}

type TimelineEntry struct {
	Kind          string `json:"kind"`
	Type          string `json:"type"`
	Severity      string `json:"severity"`
	Message       string `json:"message"`
	Detail        string `json:"detail,omitempty"`
	CorrelationID string `json:"correlationId,omitempty"`
	CreatedAt     string `json:"createdAt"`
}

type Service struct {
	mu       sync.RWMutex
	requests map[string]requestCounter
	events   map[string]eventCounter
	timeline []TimelineEntry
	playback playbackSLOState
}

type requestCounter struct {
	method          string
	path            string
	count           int64
	errors          int64
	totalLatencyMS  float64
	maxLatencyMS    float64
	lastStatus      int
	lastCorrelation string
	lastSeenAt      time.Time
}

type eventCounter struct {
	eventType  string
	count      int64
	lastSeenAt time.Time
}

func NewService() *Service {
	return &Service{
		requests: map[string]requestCounter{},
		events:   map[string]eventCounter{},
		timeline: []TimelineEntry{},
		playback: newPlaybackSLOState(),
	}
}

func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey, id)
}

func CorrelationID(ctx context.Context) string {
	value, _ := ctx.Value(correlationIDKey).(string)
	return value
}

func NewCorrelationID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "req_" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return "req_" + hex.EncodeToString(buf[:])
}

func NormalizeCorrelationID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return NewCorrelationID()
	}
	for _, ch := range value {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' {
			continue
		}
		return NewCorrelationID()
	}
	return value
}

func (s *Service) ObserveRequest(method string, path string, status int, duration time.Duration, correlationID string) {
	if s == nil {
		return
	}
	if status == 0 {
		status = 200
	}
	key := method + " " + path
	latencyMS := float64(duration.Microseconds()) / 1000
	s.mu.Lock()
	defer s.mu.Unlock()
	counter := s.requests[key]
	counter.method = method
	counter.path = path
	counter.count++
	if status >= 500 {
		counter.errors++
	}
	counter.totalLatencyMS += latencyMS
	if latencyMS > counter.maxLatencyMS {
		counter.maxLatencyMS = latencyMS
	}
	counter.lastStatus = status
	counter.lastCorrelation = correlationID
	counter.lastSeenAt = time.Now().UTC()
	s.requests[key] = counter
	s.appendTimeline(TimelineEntry{
		Kind:          "request",
		Type:          key,
		Severity:      requestSeverity(status),
		Message:       method + " " + path,
		Detail:        strings.TrimSpace(formatStatus(status) + " - " + formatLatency(duration)),
		CorrelationID: correlationID,
		CreatedAt:     formatTime(counter.lastSeenAt),
	})
}

func (s *Service) ObserveEvent(eventType string) {
	s.observeEventData(eventType, nil, time.Now().UTC())
}

func (s *Service) observeEventData(eventType string, data any, createdAt time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	counter := s.events[eventType]
	counter.eventType = eventType
	counter.count++
	counter.lastSeenAt = createdAt
	s.events[eventType] = counter
	s.observePlaybackEventLocked(eventType, data, createdAt)
	s.appendTimeline(TimelineEntry{
		Kind:      "event",
		Type:      eventType,
		Severity:  eventSeverity(eventType),
		Message:   eventMessage(eventType),
		Detail:    eventDetail(data),
		CreatedAt: formatTime(createdAt),
	})
}

func (s *Service) PlaybackSLO() PlaybackSLOMetrics {
	if s == nil {
		return PlaybackSLOMetrics{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.playback.snapshot()
}

func (s *Service) Subscribe(ctx context.Context, bus *events.Bus) {
	if s == nil || bus == nil {
		return
	}
	ch, cancel := bus.Subscribe(ctx)
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-ch:
				if !ok {
					return
				}
				s.observeEventData(event.Type, event.Data, event.CreatedAt)
			}
		}
	}()
}

func (s *Service) Requests() []RequestMetric {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	output := make([]RequestMetric, 0, len(s.requests))
	for _, counter := range s.requests {
		average := 0.0
		if counter.count > 0 {
			average = counter.totalLatencyMS / float64(counter.count)
		}
		output = append(output, RequestMetric{
			Method:          counter.method,
			Path:            counter.path,
			Count:           counter.count,
			Errors:          counter.errors,
			AverageLatency:  average,
			MaxLatency:      counter.maxLatencyMS,
			LastStatus:      counter.lastStatus,
			LastCorrelation: counter.lastCorrelation,
			LastSeenAt:      formatTime(counter.lastSeenAt),
		})
	}
	return output
}

func (s *Service) Events() []EventMetric {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	output := make([]EventMetric, 0, len(s.events))
	for _, counter := range s.events {
		output = append(output, EventMetric{
			Type:       counter.eventType,
			Count:      counter.count,
			LastSeenAt: formatTime(counter.lastSeenAt),
		})
	}
	return output
}

func (s *Service) Recent(limit int) []TimelineEntry {
	if s == nil {
		return nil
	}
	if limit <= 0 {
		limit = 20
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.timeline) == 0 {
		return []TimelineEntry{}
	}
	if limit > len(s.timeline) {
		limit = len(s.timeline)
	}
	output := make([]TimelineEntry, 0, limit)
	for i := len(s.timeline) - 1; i >= 0 && len(output) < limit; i-- {
		output = append(output, s.timeline[i])
	}
	return output
}

func EvaluateAlerts(queueSnapshots []map[string]any, requestMetrics []RequestMetric) []Alert {
	var alerts []Alert
	for _, queue := range queueSnapshots {
		name, _ := queue["name"].(string)
		workers := asInt64(queue["workers"])
		active := asInt64(queue["active"])
		queued := asInt64(queue["queued"])
		if workers > 0 && active >= workers && queued > 0 {
			alerts = append(alerts, Alert{
				Severity: "warning",
				Code:     "queue_saturated",
				Message:  name + " queue is saturated and work is waiting.",
				Action:   "Reduce background work, increase worker limits, or wait for active jobs to finish.",
			})
		}
	}
	for _, metric := range requestMetrics {
		if metric.Count >= 5 && float64(metric.Errors)/float64(metric.Count) >= 0.2 {
			alerts = append(alerts, Alert{
				Severity: "warning",
				Code:     "api_error_rate",
				Message:  metric.Method + " " + metric.Path + " has an elevated server error rate.",
				Action:   "Inspect correlated request logs using " + metric.LastCorrelation + " and check downstream subsystem health.",
			})
		}
	}
	return alerts
}

func asInt64(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	default:
		return 0
	}
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func (s *Service) appendTimeline(entry TimelineEntry) {
	s.timeline = append(s.timeline, entry)
	if len(s.timeline) > 200 {
		s.timeline = s.timeline[len(s.timeline)-200:]
	}
}

func requestSeverity(status int) string {
	switch {
	case status >= 500:
		return "error"
	case status >= 400:
		return "warning"
	default:
		return "info"
	}
}

func formatStatus(status int) string {
	if status == 0 {
		status = 200
	}
	return "HTTP " + strconv.Itoa(status)
}

func formatLatency(duration time.Duration) string {
	return strconv.FormatInt(duration.Milliseconds(), 10) + " ms"
}

func eventSeverity(eventType string) string {
	switch {
	case strings.HasSuffix(eventType, ".failed"), strings.HasPrefix(eventType, "audit.stream.denied"):
		return "error"
	case strings.Contains(eventType, "stale"), strings.Contains(eventType, "route.changed"):
		return "warning"
	default:
		return "info"
	}
}

func eventMessage(eventType string) string {
	replacer := strings.NewReplacer(".", " ", "_", " ")
	title := humanizeWords(replacer.Replace(eventType))
	switch eventType {
	case "session.route.changed":
		return "Playback route changed"
	case "session.started":
		return "Playback session started"
	case "session.stopped":
		return "Playback session stopped"
	case "transcode.failed":
		return "Transcode failed"
	case "probe.failed":
		return "Probe failed"
	case "scan.failed":
		return "Scan failed"
	}
	return title
}

func eventDetail(data any) string {
	if data == nil {
		return ""
	}
	bytes, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	if len(bytes) > 220 {
		bytes = append(bytes[:217], '.', '.', '.')
	}
	return string(bytes)
}

func humanizeWords(value string) string {
	parts := strings.Fields(value)
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}
