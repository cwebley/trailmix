package recorder

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetachedRecordingAndRetryUseXDGCredentials(t *testing.T) {
	p := testPaths(t)
	bin := filepath.Join(p.home, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bin, "trailmix")
	build := exec.Command("go", "build", "-o", executable, "./cmd/trailmix")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test CLI: %v\n%s", err, output)
	}
	credentials := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "trailmix", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(credentials), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, []byte(`{"openai_api_key":"test-app-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	for _, directory := range []string{p.home, elsewhere} {
		if err := os.WriteFile(filepath.Join(directory, ".env"), []byte("OPENAI_API_KEY='malformed\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TRAILMIX_HOME", p.home)
	t.Setenv("TRAILMIX_CAPTURE_HELPER", p.helper)
	t.Setenv("TRAILMIX_TEST_HELPER", "normal")
	t.Setenv("OPENAI_API_KEY", "unrelated-terminal-key")
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(executable, args...)
		cmd.Dir = elsewhere
		return cmd.CombinedOutput()
	}
	if output, err := run("auth", "status"); err != nil || !strings.Contains(string(output), `"configured": true`) || strings.Contains(string(output), "test-app-key") {
		t.Fatalf("auth dispatch: %v %s", err, output)
	}
	// Auth must also work without a usable recorder socket or capture helper.
	t.Setenv("TRAILMIX_HOME", filepath.Join(p.home, strings.Repeat("long", 40)))
	if output, err := run("auth", "status"); err != nil {
		t.Fatalf("auth depends on recorder configuration: %v %s", err, output)
	}
	t.Setenv("TRAILMIX_HOME", p.home)
	t.Cleanup(func() { _, _ = request(p.socket, "stop") })
	if output, err := run("record", "--transcribe"); err != nil {
		t.Fatalf("detached start: %v\n%s", err, output)
	}
	state := awaitStatus(t, p, func(s status) bool { return s.State == "recording" })
	if state.Transcription == nil || state.Transcription.State != "streaming" {
		t.Fatalf("saved credentials unavailable to detached recorder: %+v", state)
	}
	stopRecorder := func() {
		if _, err := request(p.socket, "stop"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			guard, err := lock(p)
			if err == nil {
				guard.Close()
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("detached recorder did not release its lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	stopRecorder()
	// Retry prints exactly one JSON document on stdout. The helper's repair
	// output is progress for stderr. The fake recording has no queues to replay,
	// and the closed proxy keeps any provider connection offline regardless.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	if err := os.WriteFile(filepath.Join(state.Directory, "capture.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	retry := exec.Command(executable, "retry")
	retry.Dir = elsewhere
	var stdout, stderr bytes.Buffer
	retry.Stdout, retry.Stderr = &stdout, &stderr
	_ = retry.Run()
	decoder := json.NewDecoder(&stdout)
	var retried map[string]any
	if err := decoder.Decode(&retried); err != nil || retried["meeting_id"] == nil || retried["schema_version"] != float64(1) || decoder.More() {
		t.Fatalf("retry stdout is not one transcription state: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "prepared_stream") {
		t.Fatalf("helper repair output missing from stderr: %s", stderr.String())
	}
	// The fake helper never publishes PCM queues, so these commands cannot
	// contact OpenAI. Retry must report credential errors before audio recovery.
	if err := os.WriteFile(credentials, []byte(`{"openai_api_key":"secret-value`), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := run("retry"); err == nil || !strings.Contains(string(output), "invalid credentials") || strings.Contains(string(output), "secret-value") {
		t.Fatalf("retry ignored or exposed saved credentials: %v %s", err, output)
	}
	if output, err := run("record", "--transcribe"); err != nil {
		t.Fatalf("second detached start: %v\n%s", err, output)
	}
	state = awaitStatus(t, p, func(s status) bool { return s.State == "recording" })
	if state.Transcription == nil || !strings.Contains(state.Transcription.Tracks["microphone"].Error, "invalid credentials") || strings.Contains(state.Transcription.Tracks["microphone"].Error, "secret-value") {
		t.Fatalf("detached recorder did not use saved credentials safely: %+v", state)
	}
	stopRecorder()
	if output, err := run("auth", "logout"); err != nil {
		t.Fatalf("logout: %v %s", err, output)
	}
	if output, err := run("retry"); err == nil || !strings.Contains(string(output), "run trailmix auth") {
		t.Fatalf("retry fell back to .env or terminal key after logout: %v %s", err, output)
	}
}
