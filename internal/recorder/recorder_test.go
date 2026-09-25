package recorder

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// The test executable stands in for the native process. These tests cross the
// real process/pipe/socket seam without requesting microphone permissions.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "trailmix-test-assistant-child" {
		for {
			_ = os.WriteFile("heartbeat", []byte(time.Now().String()), 0600)
			time.Sleep(20 * time.Millisecond)
		}
	}
	if len(os.Args) > 2 && os.Args[1] == "trailmix-test-assistant" {
		data, _ := io.ReadAll(os.Stdin)
		_ = os.WriteFile("received.txt", data, 0600)
		switch os.Args[2] {
		case "success":
			fmt.Fprintln(os.Stdout, "The review is tentative. [[passage:system-1]]")
		case "invalid-citation":
			fmt.Fprintln(os.Stdout, "Invented. [[passage:another-meeting]]")
		case "failure":
			fmt.Fprintln(os.Stderr, "fixture runner failed")
			os.Exit(3)
		case "timeout":
			time.Sleep(10 * time.Second)
		case "large":
			fmt.Fprint(os.Stdout, strings.Repeat("x", 5*1024*1024))
		case "orphan":
			executable, _ := os.Executable()
			child := exec.Command(executable, "trailmix-test-assistant-child")
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if child.Start() != nil {
				os.Exit(2)
			}
		case "json":
			fmt.Fprintln(os.Stdout, `{"type":"step_start"}`)
			fmt.Fprintln(os.Stdout, `{"type":"text","part":{"text":"The review is tentative. [[passage:system-1]]"}}`)
			fmt.Fprintln(os.Stdout, `{"type":"step_finish"}`)
		case "json-error":
			fmt.Fprintln(os.Stdout, `{"type":"text","part":{"text":"A partial answer."}}`)
			fmt.Fprintln(os.Stdout, `{"type":"error","error":{"message":"provider error with zero exit"}}`)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "trailmix-test-terminal" {
		for i, arg := range os.Args {
			if arg != "-e" || i+1 >= len(os.Args) {
				continue
			}
			cmd := exec.Command(os.Args[i+1], os.Args[i+2:]...)
			terminal, err := pty.Start(cmd)
			if err != nil {
				os.Exit(1)
			}
			data, _ := json.Marshal(os.Getpid())
			_ = os.WriteFile(os.Getenv("TRAILMIX_TEST_TERMINAL_PID"), data, 0600)
			go func() { _, _ = io.Copy(io.Discard, terminal) }()
			_ = cmd.Wait()
			terminal.Close()
			os.Exit(0)
		}
		os.Exit(1)
	}
	if os.Getenv("TRAILMIX_TEST_HELPER") != "" && len(os.Args) > 1 && os.Args[1] == "devices" {
		_, _ = os.Stdout.WriteString(`{"microphones":[{"id":"test-mic","name":"Test microphone","default":true}]}`)
		os.Exit(0)
	}
	if mode := os.Getenv("TRAILMIX_TEST_HELPER"); mode != "" && len(os.Args) > 1 && os.Args[1] == "record" {
		enc := json.NewEncoder(os.Stdout)
		if _, present := os.LookupEnv("OPENAI_API_KEY"); present {
			_ = enc.Encode(event{Type: "error", Error: "capture helper inherited an API credential"})
			os.Exit(1)
		}
		if mode == "failure" {
			_ = enc.Encode(event{Type: "error", Error: "Microphone access denied"})
			os.Exit(1)
		}
		var output string
		for i, arg := range os.Args {
			if arg == "--output" && i+1 < len(os.Args) {
				output = os.Args[i+1]
			}
		}
		_ = os.WriteFile(filepath.Join(output, "capture.json"), []byte(`{"microphone_id":"test-mic","microphone_name":"Test microphone"}`), 0600)
		if mode == "streaming" {
			for _, name := range []string{"microphone", "system"} {
				pcm := make([]byte, 9600)
				for i := 1; i < len(pcm); i += 2 {
					pcm[i] = 32
				}
				_ = os.WriteFile(filepath.Join(output, name+".stream.pcm"), pcm, 0600)
				_ = os.WriteFile(filepath.Join(output, name+".stream.jsonl"), []byte("{\"offset\":0,\"bytes\":9600,\"start_seconds\":0.1,\"end_seconds\":0.3}\n"), 0600)
			}
		}
		_ = enc.Encode(event{Type: "started"})
		_ = enc.Encode(event{Type: "progress", ElapsedSeconds: 4, Tracks: map[string]track{
			"microphone": {Frames: 48_000, SampleRate: 48_000, EndSeconds: 4, PeakMax: 0.5},
			"system":     {Frames: 48_000, SampleRate: 48_000, EndSeconds: 4, PeakMax: 0.5},
		}})
		_, _ = io.Copy(io.Discard, os.Stdin)
		if mode != "missing-final" {
			_ = enc.Encode(event{Type: "stopped", ElapsedSeconds: 5})
		}
		os.Exit(0)
	}
	// Recorders started by tests must never touch the developer's tmux server.
	// The workspace test substitutes its own private server.
	os.Setenv("TMUX", "/nonexistent/trailmix-test-tmux,0,0")
	os.Exit(m.Run())
}

func testPaths(t *testing.T) paths {
	t.Helper()
	// macOS Unix sockets have a 104-byte limit; t.TempDir can exceed it.
	home, err := os.MkdirTemp("/tmp", "trailmix-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return paths{home: home, socket: filepath.Join(home, "recorder.sock"), helper: helper, maxDuration: defaultMaxDuration}
}

func awaitStatus(t *testing.T, p paths, predicate func(status) bool) status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := request(p.socket, "status"); err == nil && predicate(s) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("recorder did not reach expected status")
	return status{}
}

func TestBackgroundLifecycleAndExclusiveRecording(t *testing.T) {
	t.Setenv("TRAILMIX_TEST_HELPER", "normal")
	p := testPaths(t)
	output := filepath.Join(p.home, "meeting")
	done := make(chan error, 1)
	go func() { done <- serve(p, output, "test-mic") }()
	t.Cleanup(func() { _, _ = request(p.socket, "stop") })
	active := awaitStatus(t, p, func(s status) bool { return s.State == "recording" && len(s.Tracks) == 2 })
	if active.Directory != output || active.Microphone != "test-mic" || len(active.Warnings) != 0 {
		t.Fatalf("unexpected active recording: %+v", active)
	}
	if active.MeetingID == "" || !active.Live {
		t.Fatalf("capture-only recording has no live identity: %+v", active)
	}
	if active.MicrophoneName != "Test microphone" || active.ClockAgeSeconds == nil || *active.ClockAgeSeconds < 0 {
		t.Fatalf("actual microphone/clock freshness missing: %+v", active)
	}
	// A competing recorder must fail before creating a second recording.
	other := filepath.Join(p.home, "other")
	if err := serve(p, other, ""); err == nil {
		t.Fatal("second recorder acquired the lock")
	}
	if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second recorder created output: %v", err)
	}
	stopped, err := request(p.socket, "stop")
	if err != nil || stopped.State != "stopped" || stopped.ElapsedSeconds != 5 {
		t.Fatalf("stop: %+v, %v", stopped, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	last, err := lastStatus(p)
	if err != nil || last.State != "stopped" || last.Directory != output {
		t.Fatalf("persisted status: %+v, %v", last, err)
	}
	if last.MeetingID != active.MeetingID || last.Live {
		t.Fatalf("historical identity or liveness changed: %+v", last)
	}
	if _, err := os.Stat(p.socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket not removed: %v", err)
	}
}

func TestRecordingStopsAtMaxDuration(t *testing.T) {
	t.Setenv("TRAILMIX_TEST_HELPER", "normal")
	p := testPaths(t)
	p.maxDuration = 300 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- serve(p, filepath.Join(p.home, "meeting"), "") }()
	t.Cleanup(func() { _, _ = request(p.socket, "stop") })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recording outlived its limit")
	}
	last, err := lastStatus(p)
	if err != nil || last.State != "stopped" || !strings.Contains(strings.Join(last.Warnings, "\n"), "300ms recording limit") {
		t.Fatalf("limit stop: %+v, %v", last, err)
	}
}

func TestExistingOutputIsNeverOverwritten(t *testing.T) {
	p := testPaths(t)
	output := filepath.Join(p.home, "meeting")
	if err := os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(output, "notes.md")
	if err := os.WriteFile(note, []byte("my notes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := serve(p, output, ""); err == nil {
		t.Fatal("accepted an existing directory")
	}
	data, err := os.ReadFile(note)
	if err != nil || string(data) != "my notes" {
		t.Fatalf("existing note changed: %q, %v", data, err)
	}
}

func TestNativeFailureIsPersisted(t *testing.T) {
	t.Setenv("TRAILMIX_TEST_HELPER", "failure")
	p := testPaths(t)
	if err := serve(p, "", ""); err == nil || !strings.Contains(err.Error(), "Microphone access denied") {
		t.Fatalf("wrong capture failure: %v", err)
	}
	s, err := lastStatus(p)
	if err != nil || s.State != "failed" || s.Error != "Microphone access denied" {
		t.Fatalf("failure was not retained: %+v, %v", s, err)
	}
}

func TestMissingFinalizationIsFailure(t *testing.T) {
	t.Setenv("TRAILMIX_TEST_HELPER", "missing-final")
	p := testPaths(t)
	done := make(chan error, 1)
	go func() { done <- serve(p, "", "") }()
	t.Cleanup(func() { _, _ = request(p.socket, "stop") })
	awaitStatus(t, p, func(s status) bool { return s.State == "recording" })
	s, err := request(p.socket, "stop")
	if err != nil || s.State != "failed" || !strings.Contains(s.Error, "without a finalization event") {
		t.Fatalf("unfinalized capture presented as successful: %+v, %v", s, err)
	}
	if err := <-done; err == nil {
		t.Fatal("expected failure")
	}
}

func TestCrashStatusBecomesInterrupted(t *testing.T) {
	p := testPaths(t)
	s := status{State: "recording", Directory: p.home}
	if err := saveStatus(p, s); err != nil {
		t.Fatal(err)
	}
	got, err := lastStatus(p)
	if err != nil || got.State != "interrupted" || got.Error == "" {
		t.Fatalf("interrupted capture presented as active: %+v, %v", got, err)
	}
}

func TestMissingTrackIsVisible(t *testing.T) {
	for _, state := range []string{"recording", "stopped"} {
		s := &session{status: status{State: state, ElapsedSeconds: 5, Tracks: map[string]track{
			"system": {Frames: 480, EndSeconds: 1},
		}}}
		got := s.snapshot()
		if len(got.Warnings) != 2 || !strings.Contains(got.Warnings[0], "microphone") || !strings.Contains(got.Warnings[1], "system") {
			t.Fatalf("missing/stalled tracks not reported: %+v", got)
		}
	}
}

func TestSilentBuffersAreNotPresentedAsHealthyAudio(t *testing.T) {
	s := &session{status: status{State: "recording", ElapsedSeconds: 5, Tracks: map[string]track{
		"microphone": {Frames: 80_000, EndSeconds: 5, PeakMax: 0},
		"system":     {Frames: 240_000, EndSeconds: 5, PeakMax: 0.25, Peak: 0},
	}}}
	got := s.snapshot()
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "microphone: only silent samples") {
		t.Fatalf("silent input was not distinguished from a momentarily quiet track: %+v", got)
	}
}

func TestTranscriptionFailureDoesNotStopNativeRecording(t *testing.T) {
	t.Setenv("TRAILMIX_TEST_HELPER", "streaming")
	t.Setenv("OPENAI_API_KEY", "")
	p := testPaths(t)
	p.transcribe = true
	done := make(chan error, 1)
	go func() { done <- serve(p, "", "") }()
	t.Cleanup(func() { _, _ = request(p.socket, "stop") })
	active := awaitStatus(t, p, func(s status) bool {
		return s.State == "recording" && s.Transcription != nil && s.Transcription.State == "degraded"
	})
	if active.Error != "" || len(active.Tracks) != 2 {
		t.Fatalf("provider failure stopped capture: %+v", active)
	}
	stopped, err := request(p.socket, "stop")
	if err != nil || stopped.State != "stopped" || stopped.Transcription.State != "degraded" {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	if len(stopped.Transcription.Tracks["microphone"].Pending) != 1 || len(stopped.Transcription.Tracks["system"].Pending) != 1 {
		t.Fatalf("incomplete audio ranges lost: %+v", stopped.Transcription)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stopped.Directory, "transcription", "status.json")); err != nil {
		t.Fatal(err)
	}
}
