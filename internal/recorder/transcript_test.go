package recorder

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cwebley/trailmix/internal/meeting"
)

func TestTranscriptCLISelectsAssociatedMeeting(t *testing.T) {
	p := testPaths(t)
	t.Setenv("TRAILMIX_HOME", p.home)
	cli := filepath.Join(p.home, "trailmix")
	build := exec.Command("go", "build", "-o", cli, "./cmd/trailmix")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	id := "0123456789abcdef0123456789abcdef"
	dir := filepath.Join(p.home, "old recording")
	write(filepath.Join(dir, "meeting.json"), `{"id":"`+id+`"}`)
	write(filepath.Join(dir, "session.json"), `{"meeting_id":"`+id+`","state":"stopped","elapsed_seconds":120}`)
	write(filepath.Join(dir, "transcription", "status.json"), `{"meeting_id":"`+id+`","state":"complete","model":"fixture","tracks":{"microphone":{"state":"complete"},"system":{"state":"complete"}}}`)
	// File and provider-arrival order differ from capture order.
	write(filepath.Join(dir, "transcription", "a.passage.json"), `{"id":"system-later","meeting_id":"`+id+`","track":"system","start_seconds":62,"end_seconds":65,"text":"Friday is tentative."}`)
	write(filepath.Join(dir, "transcription", "z.passage.json"), `{"id":"microphone-first","meeting_id":"`+id+`","track":"microphone","start_seconds":58,"end_seconds":63,"text":"When is the review?"}`)
	if err := os.MkdirAll(filepath.Join(p.home, "meetings"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := meeting.WriteJSON(filepath.Join(p.home, "meetings", id+".json"), map[string]string{"id": id, "directory": dir}); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(p.home, "last.json"), `{"state":"stopped","directory":"/missing-newer-meeting"}`)
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(cli, append([]string{"transcript"}, args...)...)
		cmd.Dir = p.home
		return cmd.CombinedOutput()
	}
	out, err := run("--meeting", id, "--json")
	if err != nil {
		t.Fatalf("read associated transcript: %v %s", err, out)
	}
	var got struct {
		MeetingID     string  `json:"meeting_id"`
		Duration      float64 `json:"duration_seconds"`
		Passages      []struct{ ID, Text string }
		Transcription struct{ State string }
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid JSON: %v %s", err, out)
	}
	if got.MeetingID != id || got.Duration != 120 || got.Transcription.State != "complete" || len(got.Passages) != 2 || got.Passages[0].ID != "microphone-first" || got.Passages[1].Text != "Friday is tentative." {
		t.Fatalf("wrong transcript: %s", out)
	}
	if nvim, err := exec.LookPath("nvim"); err == nil {
		t.Setenv("TRAILMIX_TEST_CLI", cli)
		// Two fresh editor processes must resolve the same passage, even though
		// the latest recording belongs to another meeting. No user init is loaded.
		for i := 0; i < 2; i++ {
			cmd := exec.Command(nvim, "--headless", "-u", "NONE", "--noplugin", "-i", "NONE", "-l", "tests/nvim/transcript_cli.lua")
			cmd.Dir = "../.."
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("editor/CLI integration: %v %s", err, out)
			}
		}
	} else {
		t.Log("nvim unavailable; skipping editor/CLI integration")
	}
	// Explicit output and human-readable output remain usable offline.
	if out, err := run("--output", dir); err != nil || !strings.Contains(string(out), "Friday is tentative.") {
		t.Fatalf("text transcript: %v %s", err, out)
	}
	for _, args := range [][]string{
		{"--meeting", "", "--json"},
		{"--meeting", "../last", "--json"},
		{"--meeting", "ffffffffffffffffffffffffffffffff", "--json"},
		{"--meeting", id, "--output", dir},
	} {
		if out, err := run(args...); err == nil || strings.Contains(string(out), "missing-newer-meeting") {
			t.Fatalf("invalid selection fell back to latest: %v %v %s", args, err, out)
		}
	}
	if out, err := run("--meeting", "", "--json"); err == nil || !strings.Contains(string(out), "invalid meeting ID") {
		t.Fatalf("empty explicit meeting was not rejected: %v %s", err, out)
	}
	write(filepath.Join(dir, "transcription", "a.passage.json"), `{"id":"system-later","meeting_id":"ffffffffffffffffffffffffffffffff","track":"system","start_seconds":62,"end_seconds":65,"text":"Wrong meeting."}`)
	if out, err := run("--meeting", id, "--json"); err == nil || !strings.Contains(string(out), "identity mismatch") {
		t.Fatalf("mixed meeting evidence accepted: %v %s", err, out)
	}
	if err := os.Remove(filepath.Join(dir, "transcription", "a.passage.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "transcription", "z.passage.json")); err != nil {
		t.Fatal(err)
	}
	if out, err := run("--meeting", id, "--json"); err != nil || !strings.Contains(string(out), `"passages":[]`) {
		t.Fatalf("empty transcript is not a usable snapshot: %v %s", err, out)
	}
	// A recorder crash must not resurrect a stale streaming status from session.json.
	write(filepath.Join(dir, "transcription", "writer.lock"), "")
	write(filepath.Join(dir, "transcription", "status.json"), `{"meeting_id":"`+id+`","state":"streaming","updated_at":"2026-01-01T00:00:00Z","tracks":{}}`)
	write(filepath.Join(dir, "session.json"), `{"meeting_id":"`+id+`","state":"recording","transcription":{"meeting_id":"`+id+`","state":"streaming","updated_at":"2026-01-01T00:00:01Z","tracks":{}}}`)
	if out, err := run("--meeting", id, "--json"); err != nil || !strings.Contains(string(out), `"state":"interrupted"`) {
		t.Fatalf("stale streaming state resurrected: %v %s", err, out)
	}
}
