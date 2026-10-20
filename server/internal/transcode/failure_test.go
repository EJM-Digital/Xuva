package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/events"
	"github.com/EJM-Digital/Xuva/server/internal/jobs"
	"github.com/EJM-Digital/Xuva/server/internal/resources"
)

func TestClassifyFailureKnownErrors(t *testing.T) {
	cases := []struct {
		name       string
		stderr     string
		contextErr error
		class      string
		code       string
		retryable  bool
	}{
		{"missing", "No such file or directory", nil, "input_missing", "source_file_missing", false},
		{"permission", "Permission denied", nil, "permission_denied", "transcode_permission_denied", false},
		{"disk", "No space left on device", nil, "disk_full", "transcode_disk_full", false},
		{"io", "Input/output error", nil, "retryable_io", "transient_storage_io", true},
		{"codec", "Unknown decoder 'x'", nil, "unsupported_codec", "unsupported_codec", false},
		{"timeout", "", context.DeadlineExceeded, "timeout", "transcode_timeout", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyFailure(tc.stderr, tc.contextErr)
			if got.Class != tc.class || got.ReasonCode != tc.code || got.Retryable != tc.retryable {
				t.Fatalf("unexpected failure classification: %#v", got)
			}
			if got.Remediation == "" || got.ReasonText == "" {
				t.Fatalf("expected actionable text, got %#v", got)
			}
		})
	}
}

func TestRetryableErrorsRetryExactlyPerPolicy(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "counter.txt")
	ffmpeg := fakeFFmpeg(t, "retry", counter)
	service := newTestService(t, ffmpeg)
	source := writeSource(t)

	job, err := service.Start(context.Background(), Request{MediaSourceID: "source-1", SourcePath: source, Mode: ModeRemux, MaxAttempts: 2})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	job = waitForTerminalJob(t, service, job.ID)

	if job.Status != StatusCompleted {
		t.Fatalf("expected completed after retry, got %#v", job)
	}
	if job.Attempts != 2 {
		t.Fatalf("expected exactly two attempts, got %#v", job)
	}
}

func TestTimeoutTransitionsJobAndCleansOutput(t *testing.T) {
	ffmpeg := fakeFFmpeg(t, "sleep", "")
	service := newTestService(t, ffmpeg)
	source := writeSource(t)

	job, err := service.Start(context.Background(), Request{MediaSourceID: "source-1", SourcePath: source, Mode: ModeRemux, TimeoutSeconds: 1, MaxAttempts: 1})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	job = waitForTerminalJob(t, service, job.ID)

	if job.Status != StatusTimeout || job.ReasonCode != "transcode_timeout" {
		t.Fatalf("expected timeout diagnostics, got %#v", job)
	}
	if _, err := os.Stat(job.OutputPath); !os.IsNotExist(err) {
		t.Fatalf("expected timeout cleanup for %s, stat err=%v", job.OutputPath, err)
	}
}

func TestCancelStopsWorkAndCleansOutput(t *testing.T) {
	ffmpeg := fakeFFmpeg(t, "sleep", "")
	service := newTestService(t, ffmpeg)
	source := writeSource(t)

	job, err := service.Start(context.Background(), Request{MediaSourceID: "source-1", SourcePath: source, Mode: ModeRemux, MaxAttempts: 1})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	waitForStatus(t, service, job.ID, StatusRunning)
	cancelled, ok := service.Cancel(job.ID)
	if !ok {
		t.Fatalf("expected cancelable job")
	}
	if cancelled.Status != StatusCanceled || cancelled.ReasonCode != "transcode_cancelled" {
		t.Fatalf("expected cancelled diagnostics, got %#v", cancelled)
	}
	if _, err := os.Stat(cancelled.OutputPath); !os.IsNotExist(err) {
		t.Fatalf("expected cancel cleanup for %s, stat err=%v", cancelled.OutputPath, err)
	}
}

func TestCommandUsesRequestedAudioTrackWhenProvided(t *testing.T) {
	service := newTestService(t, "ffmpeg")
	job := Job{
		Mode:            ModeTranscode,
		SourcePath:      "input.mkv",
		OutputPath:      "output.mp4",
		AudioTrackIndex: 5,
	}
	args := service.command(job)
	expectedPrefix := []string{"ffmpeg", "-y", "-i", "input.mkv", "-map", "0:v:0", "-map", "0:5?"}
	if len(args) < len(expectedPrefix) {
		t.Fatalf("expected command prefix length %d, got %d (%#v)", len(expectedPrefix), len(args), args)
	}
	if !reflect.DeepEqual(args[:len(expectedPrefix)], expectedPrefix) {
		t.Fatalf("expected command prefix %#v, got %#v", expectedPrefix, args[:len(expectedPrefix)])
	}
}

func TestFindCompletedAndActiveAreScopedByAudioTrack(t *testing.T) {
	service := newTestService(t, "ffmpeg")
	firstOut := filepath.Join(t.TempDir(), "a.mp4")
	secondOut := filepath.Join(t.TempDir(), "b.mp4")
	if err := os.WriteFile(firstOut, []byte("a"), 0o644); err != nil {
		t.Fatalf("write first output: %v", err)
	}
	if err := os.WriteFile(secondOut, []byte("b"), 0o644); err != nil {
		t.Fatalf("write second output: %v", err)
	}
	now := time.Now().UTC()
	service.store(Job{
		ID:              "completed-audio-1",
		MediaSourceID:   "source-1",
		Mode:            ModeTranscode,
		AudioTrackIndex: 1,
		Status:          StatusCompleted,
		OutputPath:      firstOut,
		CreatedAt:       now,
		CompletedAt:     now,
	})
	service.store(Job{
		ID:              "running-audio-2",
		MediaSourceID:   "source-1",
		Mode:            ModeTranscode,
		AudioTrackIndex: 2,
		Status:          StatusRunning,
		OutputPath:      secondOut,
		CreatedAt:       now,
		StartedAt:       now,
	})

	completed, ok := service.FindCompleted("source-1", ModeTranscode, 1)
	if !ok || completed.ID != "completed-audio-1" {
		t.Fatalf("expected completed job for audio track 1, got ok=%v job=%#v", ok, completed)
	}
	if _, ok := service.FindCompleted("source-1", ModeTranscode, 2); ok {
		t.Fatalf("did not expect completed job for audio track 2")
	}

	active, ok := service.FindActive("source-1", ModeTranscode, 2)
	if !ok || active.ID != "running-audio-2" {
		t.Fatalf("expected active job for audio track 2, got ok=%v job=%#v", ok, active)
	}
	if _, ok := service.FindActive("source-1", ModeTranscode, 1); ok {
		t.Fatalf("did not expect active job for audio track 1")
	}
}

func TestCancelActiveForMediaSourceOnlyCancelsMatchingActiveJobs(t *testing.T) {
	service := newTestService(t, "ffmpeg")
	now := time.Now().UTC()
	canceledIDs := []string{}
	service.store(Job{
		ID:            "queued-match",
		MediaSourceID: "source-1",
		Mode:          ModeTranscode,
		Status:        StatusQueued,
		CreatedAt:     now,
	})
	service.storeCancel("queued-match", func() { canceledIDs = append(canceledIDs, "queued-match") })
	service.store(Job{
		ID:            "running-match",
		MediaSourceID: "source-1",
		Mode:          ModeTranscode,
		Status:        StatusRunning,
		CreatedAt:     now,
		StartedAt:     now,
	})
	service.storeCancel("running-match", func() { canceledIDs = append(canceledIDs, "running-match") })
	service.store(Job{
		ID:            "completed-match",
		MediaSourceID: "source-1",
		Mode:          ModeTranscode,
		Status:        StatusCompleted,
		CreatedAt:     now,
		CompletedAt:   now,
	})
	service.store(Job{
		ID:            "running-other",
		MediaSourceID: "source-2",
		Mode:          ModeTranscode,
		Status:        StatusRunning,
		CreatedAt:     now,
		StartedAt:     now,
	})
	service.storeCancel("running-other", func() { canceledIDs = append(canceledIDs, "running-other") })

	cancelled := service.CancelActiveForMediaSource("source-1")
	if cancelled != 2 {
		t.Fatalf("expected 2 cancelled jobs, got %d", cancelled)
	}
	queued, _ := service.Get("queued-match")
	if queued.Status != StatusCanceled {
		t.Fatalf("expected queued match to be cancelled, got %#v", queued)
	}
	running, _ := service.Get("running-match")
	if running.Status != StatusCanceled {
		t.Fatalf("expected running match to be cancelled, got %#v", running)
	}
	completed, _ := service.Get("completed-match")
	if completed.Status != StatusCompleted {
		t.Fatalf("expected completed job to remain completed, got %#v", completed)
	}
	other, _ := service.Get("running-other")
	if other.Status != StatusRunning {
		t.Fatalf("expected other source running job to remain active, got %#v", other)
	}
	sort.Strings(canceledIDs)
	if !reflect.DeepEqual(canceledIDs, []string{"queued-match", "running-match"}) {
		t.Fatalf("unexpected cancel callbacks: %#v", canceledIDs)
	}
}

func newTestService(t *testing.T, ffmpegPath string) *Service {
	t.Helper()
	queue := jobs.NewQueue("transcode-test", resources.PlaybackCritical, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	queue.Start(ctx)
	return NewService(events.NewBus(32), queue, ffmpegPath, t.TempDir())
}

func writeSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.mkv")
	if err := os.WriteFile(path, []byte("source"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	return path
}

func waitForTerminalJob(t *testing.T, service *Service, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := service.Get(id)
		if ok && job.Status != StatusQueued && job.Status != StatusRunning {
			return job
		}
		time.Sleep(25 * time.Millisecond)
	}
	job, _ := service.Get(id)
	t.Fatalf("timed out waiting for terminal job, got %#v", job)
	return Job{}
}

func waitForStatus(t *testing.T, service *Service, id string, status Status) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := service.Get(id)
		if ok && job.Status == status {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	job, _ := service.Get(id)
	t.Fatalf("timed out waiting for status %s, got %#v", status, job)
}

func fakeFFmpeg(t *testing.T, mode string, counter string) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "fake_ffmpeg.go")
	binary := filepath.Join(dir, "fake_ffmpeg.exe")
	program := `package main
import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)
func main() {
	mode := os.Getenv("XUVA_FAKE_FFMPEG_MODE")
	counter := os.Getenv("XUVA_FAKE_FFMPEG_COUNTER")
	if mode == "sleep" {
		time.Sleep(5 * time.Second)
		return
	}
	if mode == "retry" {
		count := 0
		if data, err := os.ReadFile(counter); err == nil {
			count, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		count++
		_ = os.WriteFile(counter, []byte(strconv.Itoa(count)), 0o644)
		if count < 2 {
			fmt.Fprintln(os.Stderr, "Input/output error")
			os.Exit(1)
		}
	}
	if len(os.Args) > 1 {
		out := os.Args[len(os.Args)-1]
		_ = os.WriteFile(out, []byte("ok"), 0o644)
	}
}`
	if err := os.WriteFile(source, []byte(program), 0o644); err != nil {
		t.Fatalf("write fake ffmpeg source: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", binary, source)
	cmd.Env = append(os.Environ(), "XUVA_FAKE_FFMPEG_MODE="+mode, "XUVA_FAKE_FFMPEG_COUNTER="+counter)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake ffmpeg: %v\n%s", err, string(output))
	}
	t.Setenv("XUVA_FAKE_FFMPEG_MODE", mode)
	t.Setenv("XUVA_FAKE_FFMPEG_COUNTER", counter)
	return binary
}
