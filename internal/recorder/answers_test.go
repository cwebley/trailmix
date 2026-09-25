package recorder

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cwebley/trailmix/internal/meeting"
)

func TestAskCLIUsesSnapshotsAndPreservesNotes(t *testing.T) {
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
	marker := "<!-- trailmix: meeting " + id + " -->\n"
	notePath := filepath.Join(p.home, "notes with spaces", "meeting.md")
	dir := filepath.Join(p.home, "recording")
	write(notePath, marker+"- original on disk\n")
	write(filepath.Join(dir, "meeting.json"), `{"id":"`+id+`"}`)
	write(filepath.Join(dir, "session.json"), `{"meeting_id":"`+id+`","state":"stopped","elapsed_seconds":120}`)
	write(filepath.Join(dir, "transcription", "status.json"), `{"meeting_id":"`+id+`","state":"complete","tracks":{"microphone":{"state":"complete"},"system":{"state":"complete"}}}`)
	write(filepath.Join(dir, "transcription", "system-1.passage.json"), `{"id":"system-1","meeting_id":"`+id+`","track":"system","start_seconds":62,"end_seconds":65,"text":"Friday is tentative."}`)
	write(filepath.Join(p.home, "meetings", id+".json"), "")
	if err := meeting.WriteJSON(filepath.Join(p.home, "meetings", id+".json"), map[string]string{"id": id, "path": notePath, "directory": dir, "date": "2026-09-24", "session": "trailmix-2026-09-24"}); err != nil {
		t.Fatal(err)
	}
	run := func(input string, args ...string) ([]byte, error) {
		cmd := exec.Command(cli, args...)
		cmd.Stdin = strings.NewReader(input)
		return cmd.CombinedOutput()
	}
	configure := func(mode, format string) {
		t.Helper()
		argv, _ := json.Marshal([]string{p.helper, "trailmix-test-assistant", mode, "literal ; $not-a-shell"})
		for _, pair := range [][2]string{{"assistant.command", string(argv)}, {"assistant.format", format}} {
			if out, err := run("", "config", "set", pair[0], pair[1]); err != nil {
				t.Fatalf("configure: %v %s", err, out)
			}
		}
	}
	configure("success", "markdown")
	input, _ := json.Marshal(askInput{Question: "What was decided?", Notes: ptr(marker + "- unsaved question\n"), NotePath: notePath, Selection: "unsaved question"})
	out, err := run(string(input), "ask", "--meeting", id)
	if err != nil {
		t.Fatalf("ask: %v %s", err, out)
	}
	var got answersView
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.MeetingID != id || !strings.Contains(got.Markdown, "[01:02](trailmix://meetings/"+id+"/passages/system-1)") {
		t.Fatalf("uncited answer: %s", out)
	}
	data, _ := os.ReadFile(notePath)
	if string(data) != marker+"- original on disk\n" {
		t.Fatal("source note overwritten")
	}
	directory := filepath.Join(dir, "answers", got.AnswerID)
	prompt, _ := os.ReadFile(filepath.Join(directory, "received.txt"))
	if !strings.Contains(string(prompt), "unsaved question") || !strings.Contains(string(prompt), "Friday is tentative.") || strings.Contains(string(prompt), "original on disk") {
		t.Fatalf("wrong context: %s", prompt)
	}
	if _, err := os.Stat(got.TranscriptPath); err != nil {
		t.Fatal("no transcript sidecar", err)
	}
	info, _ := os.Stat(got.Path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("answers are not owner-only")
	}
	// Preserve existing disk edits and serialize concurrent appends.
	write(got.Path, got.Markdown+"\nManual annotation retained.\n")
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := run("", "ask", "--meeting", id, "--question", "Another question"); err != nil {
				t.Errorf("concurrent ask: %v %s", err, out)
			}
		}()
	}
	wg.Wait()
	before, _ := os.ReadFile(got.Path)
	if strings.Count(string(before), "<!-- trailmix: answer ") != 3 || !strings.Contains(string(before), "Manual annotation retained.") {
		t.Fatalf("lost answer append: %s", before)
	}
	for _, mode := range []string{"invalid-citation", "failure"} {
		configure(mode, "markdown")
		if out, err := run(string(input), "ask", "--meeting", id); err == nil || !strings.Contains(string(out), "request retained") {
			t.Fatalf("bad runner published: %s %v %s", mode, err, out)
		}
		after, _ := os.ReadFile(got.Path)
		if string(after) != string(before) {
			t.Fatal("failed request changed answers")
		}
	}
	configure("json-error", "opencode-json")
	if out, err := run(string(input), "ask", "--meeting", id); err == nil || !strings.Contains(string(out), "OpenCode reported an error") {
		t.Fatalf("zero-exit provider error accepted: %v %s", err, out)
	}
	configure("json", "opencode-json")
	if out, err := run(string(input), "ask", "--meeting", id); err != nil || strings.Contains(string(out), "step_finish") {
		t.Fatalf("event decoding: %v %s", err, out)
	}
	configure("timeout", "markdown")
	if out, err := run("", "config", "set", "assistant.timeout_seconds", "1"); err != nil {
		t.Fatalf("timeout config: %v %s", err, out)
	}
	start := time.Now()
	if out, err := run(string(input), "ask", "--meeting", id); err == nil || !strings.Contains(string(out), "deadline exceeded") || time.Since(start) > 4*time.Second {
		t.Fatalf("timeout: %v %s", err, out)
	}
	if out, err := run("", "config", "set", "assistant.timeout_seconds", "10"); err != nil {
		t.Fatalf("timeout config: %v %s", err, out)
	}
	configure("large", "markdown")
	if out, err := run(string(input), "ask", "--meeting", id); err == nil || !strings.Contains(string(out), "output exceeded") {
		t.Fatalf("oversized output was accepted, err=%v", err)
	}
	configure("orphan", "markdown")
	if _, err := run(string(input), "ask", "--meeting", id); err == nil {
		t.Fatal("orphaned pipe was not reported")
	}
	beats, _ := filepath.Glob(filepath.Join(dir, "answers", "*", "heartbeat"))
	if len(beats) != 1 {
		t.Fatalf("child fixture did not start: %v", beats)
	}
	time.Sleep(50 * time.Millisecond)
	first, _ := os.ReadFile(beats[0])
	time.Sleep(150 * time.Millisecond)
	second, _ := os.ReadFile(beats[0])
	if string(first) != string(second) {
		t.Fatal("assistant child survived wrapper exit")
	}
	// A conflicting user file is rejected before an assistant request.
	configure("success", "markdown")
	if out, err := run("", "config", "set", "assistant.timeout_seconds", "10"); err != nil {
		t.Fatalf("timeout config: %v %s", err, out)
	}
	if nvim, err := exec.LookPath("nvim"); err == nil {
		t.Setenv("TRAILMIX_TEST_CLI", cli)
		t.Setenv("TRAILMIX_TEST_NOTE", notePath)
		cmd := exec.Command(nvim, "--headless", "-u", "NONE", "--noplugin", "-i", "NONE", "-l", "tests/nvim/ask_cli.lua")
		cmd.Dir = "../.."
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ask editor/CLI integration: %v %s", err, out)
		}
	}
	write(got.TranscriptPath, "user-owned transcript file")
	if out, err := run(string(input), "ask", "--meeting", id); err == nil || !strings.Contains(string(out), "unrecognized sidecar") {
		t.Fatalf("sidecar collision: %v %s", err, out)
	}
	data, _ = os.ReadFile(got.TranscriptPath)
	if string(data) != "user-owned transcript file" {
		t.Fatal("sidecar overwritten")
	}
}

func ptr(s string) *string { return &s }
