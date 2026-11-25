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

// Cross the real CLI/editor boundary with a fixture transcript and a local
// assistant. This test never records or invokes a provider.
func TestRecapEditorCLIIntegration(t *testing.T) {
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("Neovim unavailable")
	}
	p := testPaths(t)
	t.Setenv("TRAILMIX_HOME", p.home)
	cli := filepath.Join(p.home, "trailmix")
	build := exec.Command("go", "build", "-o", cli, "./cmd/trailmix")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	write := func(path, text string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	id := "0123456789abcdef0123456789abcdef"
	marker := "<!-- trailmix: meeting " + id + " -->\n"
	notePath := filepath.Join(p.home, "notes with spaces", "meeting.md")
	dir := filepath.Join(p.home, "recording")
	original := marker + "# Meeting\n\n## Rough notes\n\nOriginal saved note.\n"
	write(notePath, original, 0600)
	write(filepath.Join(dir, "meeting.json"), `{"id":"`+id+`"}`, 0600)
	write(filepath.Join(dir, "session.json"), `{"meeting_id":"`+id+`","state":"stopped","elapsed_seconds":120}`, 0600)
	write(filepath.Join(dir, "transcription", "status.json"), `{"meeting_id":"`+id+`","state":"complete","tracks":{"microphone":{"state":"complete"},"system":{"state":"complete"}}}`, 0600)
	write(filepath.Join(dir, "transcription", "system-1.passage.json"), `{"id":"system-1","meeting_id":"`+id+`","track":"system","start_seconds":62,"end_seconds":65,"text":"Friday is tentative."}`, 0600)
	if err := os.MkdirAll(filepath.Join(p.home, "meetings"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := meeting.WriteJSON(filepath.Join(p.home, "meetings", id+".json"), map[string]string{"id": id, "path": notePath, "directory": dir, "date": "2026-09-24", "session": "trailmix-2026-09-24"}); err != nil {
		t.Fatal(err)
	}
	assistantPath := filepath.Join(p.home, "fake-assistant")
	write(assistantPath, "#!/bin/sh\ncat > received.txt\nsleep 0.25\nprintf '%s\\n' '{\"suggested_title\":\"Friday planning\",\"summary\":\"Friday remains tentative. [[passage:system-1]]\",\"observations\":\"No confirmed date. [[passage:system-1]]\\n\\n~~~`\\ncode example\\n~~~\",\"questions_to_follow_up\":\"Agent suggestion: who will confirm Friday?\",\"my_commitments\":\"None clearly accepted.\"}'\n", 0700)
	argv, _ := json.Marshal([]string{assistantPath})
	for _, pair := range [][2]string{{"assistant.command", string(argv)}, {"assistant.format", "markdown"}} {
		if out, err := exec.Command(cli, "config", "set", pair[0], pair[1]).CombinedOutput(); err != nil {
			t.Fatalf("config: %v %s", err, out)
		}
	}
	t.Setenv("TRAILMIX_CLI", cli)
	t.Setenv("TRAILMIX_TEST_NOTE", notePath)
	script, _ := filepath.Abs("../../tests/nvim/recap_cli.lua")
	cmd := exec.Command(nvim, "--headless", "-u", "NONE", "--noplugin", "-i", "NONE", "-l", script)
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recap editor/CLI integration: %v %s", err, out)
	}
	data, _ := os.ReadFile(notePath)
	if string(data) != original {
		t.Fatal("recap saved or rewrote source note on disk")
	}
	prompts, _ := filepath.Glob(filepath.Join(dir, "recaps", "*", "received.txt"))
	if len(prompts) == 0 {
		t.Fatal("no recap request retained")
	}
	prompt, _ := os.ReadFile(prompts[0])
	if !strings.Contains(string(prompt), "Unsaved editor question") || !strings.Contains(string(prompt), "Friday is tentative.") {
		t.Fatalf("recap omitted live notes or transcript: %s", prompt)
	}
}
