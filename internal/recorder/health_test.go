package recorder

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cwebley/trailmix/internal/meeting"
)

func TestCaptureHealthUsesObservedTracksNotElapsedClock(t *testing.T) {
	started := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	origin := started.Add(5 * time.Minute)
	s := status{State: "failed", Error: "display lost", StartedAt: started, CaptureOriginAt: captureOrigin(origin.Format(time.RFC3339Nano)), ElapsedSeconds: 120, Tracks: map[string]track{
		"microphone": {Frames: 48000, EndSeconds: 64},
		"system":     {Frames: 48000, EndSeconds: 61},
	}}
	h := captureHealthFor(s)
	if h.State != "failed" || h.Error != "display lost" || h.LastCapturedSeconds == nil || *h.LastCapturedSeconds != 64 || h.LastCapturedAt == nil || !h.LastCapturedAt.Equal(origin.Add(64*time.Second)) {
		t.Fatalf("wrong capture boundary: %+v", h)
	}
	if len(h.Tracks) != 2 || h.Tracks["system"].LastCapturedSeconds != 61 || h.Tracks["system"].LastCapturedAt == nil || !h.Tracks["system"].LastCapturedAt.Equal(origin.Add(61*time.Second)) {
		t.Fatalf("track boundaries were conflated: %+v", h.Tracks)
	}
	unknown := captureHealthFor(status{State: "stopped", ElapsedSeconds: 120})
	if unknown.LastCapturedSeconds != nil || unknown.LastCapturedAt != nil || len(unknown.Tracks) != 0 {
		t.Fatalf("elapsed clock presented as audio evidence: %+v", unknown)
	}
	// A failure to persist final status does not retroactively fail capture.
	s.Capture, s.State, s.Error = unknown, "failed", "persist final status"
	if captureFailed(s) {
		t.Fatal("persistence failure presented as capture failure")
	}
}

func TestCaptureErrorIsVisibleDuringFinalizationAndAfterCrash(t *testing.T) {
	s := &session{status: status{State: "recording", Error: "No display available", Tracks: map[string]track{"system": {Frames: 48000, EndSeconds: 4}}}}
	got := s.snapshot()
	if got.Capture.State != "failed" || !captureFailed(got) || got.Capture.Error != "No display available" {
		t.Fatalf("native error hidden during finalization: %+v", got)
	}
	interruptStatus(&got)
	if got.Capture.State != "failed" || got.Capture.Error != "No display available" || *got.Capture.LastCapturedSeconds != 4 {
		t.Fatalf("crash replaced original capture error: %+v", got)
	}
}

func healthTranscriptFixture(t *testing.T, p paths, s status) string {
	t.Helper()
	dir := filepath.Join(p.home, "recording")
	if err := os.MkdirAll(filepath.Join(dir, "transcription"), 0700); err != nil {
		t.Fatal(err)
	}
	id, err := meeting.Ensure(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	s.MeetingID, s.Directory = id, dir
	if err := saveStatus(p, s); err != nil {
		t.Fatal(err)
	}
	state := `{"meeting_id":"` + id + `","state":"complete","model":"fixture","tracks":{"microphone":{"state":"complete"},"system":{"state":"complete"}}}`
	if err := os.WriteFile(filepath.Join(dir, "transcription", "status.json"), []byte(state), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCompleteTranscriptRetainsCaptureFailure(t *testing.T) {
	p := testPaths(t)
	dir := healthTranscriptFixture(t, p, status{State: "failed", Error: "No display available", ElapsedSeconds: 120, StartedAt: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC), CaptureOriginAt: "2026-11-18T12:05:00Z", Tracks: map[string]track{
		"microphone": {Frames: 48000, EndSeconds: 64}, "system": {Frames: 48000, EndSeconds: 61},
	}})
	snapshot, err := readTranscript(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Transcription.State != "complete" || snapshot.Capture == nil || snapshot.Capture.State != "failed" || snapshot.Capture.Error != "No display available" {
		t.Fatalf("complete transcription hid capture failure: %+v", snapshot)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"capture":{"state":"failed"`, `"last_captured_seconds":64`, `"last_captured_at":"2026-11-18T12:06:04Z"`, `"error":"No display available"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("JSON missing %s: %s", want, data)
		}
	}
	markdown := transcriptMarkdown(snapshot)
	for _, want := range []string{"Transcription: complete", "Capture: failed", "No display available", "Last observed audio: 01:04", "microphone last observed audio: 01:04", "system last observed audio: 01:01"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("export missing %q: %s", want, markdown)
		}
	}
	// No recorder socket is available. The tmux warning must survive process exit.
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	defer func() { os.Stdout = old }()
	err = statusCommand(p, []string{"status", "--tmux"})
	writer.Close()
	os.Stdout = old
	out, readErr := io.ReadAll(reader)
	reader.Close()
	if err != nil || readErr != nil || !strings.Contains(string(out), "!rec failed") {
		t.Fatalf("offline tmux warning: %s, %v, %v", out, err, readErr)
	}
}

func TestStoppedTranscriptDoesNotClaimUncapturedConversation(t *testing.T) {
	p := testPaths(t)
	dir := healthTranscriptFixture(t, p, status{State: "stopped", ElapsedSeconds: 120, Tracks: map[string]track{"microphone": {Frames: 48000, EndSeconds: 64}}})
	snapshot, err := readTranscript(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	text := transcriptMarkdown(snapshot)
	if snapshot.Capture.State != "stopped" || strings.Contains(text, "Capture error") || strings.Contains(text, "uncaptured") || strings.Contains(text, "Incomplete") {
		t.Fatalf("intentional stop became missing conversation: %s", text)
	}
}

func TestTranscriptDetectsAbandonedCaptureWithCompleteTranscription(t *testing.T) {
	p := testPaths(t)
	dir := healthTranscriptFixture(t, p, status{State: "recording", ElapsedSeconds: 120, Tracks: map[string]track{"system": {Frames: 48000, EndSeconds: 64}}})
	f, err := os.OpenFile(filepath.Join(dir, "capture.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readTranscript(dir, "")
	if err != nil || snapshot.Capture.State != "recording" {
		t.Fatalf("live capture was interrupted: %+v %v", snapshot, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	snapshot, err = readTranscript(dir, "")
	if err != nil || snapshot.Capture.State != "interrupted" || snapshot.Transcription.State != "complete" {
		t.Fatalf("abandoned capture hidden by complete transcript: %+v %v", snapshot, err)
	}
}
