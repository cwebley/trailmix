package recorder

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cwebley/trailmix/internal/settings"

	"github.com/creack/pty"
)

// This crosses the actual CLI, detached recorder, socket, tmux server and clean
// Neovim processes. Only native audio and desktop focus are substituted.
func TestMeetingWorkspaceIntegration(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("nvim is not installed")
	}
	p := testPaths(t)
	bin := filepath.Join(p.home, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(bin, "trailmix")
	build := exec.Command("go", "build", "-o", cli, "./cmd/trailmix")
	build.Dir = "../.."
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, data)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	socket := filepath.Join(p.home, "tmux.sock")
	write := func(path, data string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "tmux"), "#!/bin/sh\nexec "+quote(tmux)+" -S "+quote(socket)+" -f /dev/null \"$@\"\n", 0700)
	write(filepath.Join(bin, "nvim"), "#!/bin/sh\nexec "+quote(nvim)+" -u NONE --noplugin -i NONE \"$@\"\n", 0700)
	focusLog := filepath.Join(p.home, "focus.log")
	terminalPID := filepath.Join(p.home, "terminal.pid")
	t.Setenv("TRAILMIX_TEST_TERMINAL_PID", terminalPID)
	write(filepath.Join(bin, "alacritty"), "#!/bin/sh\nexec "+quote(p.helper)+" trailmix-test-terminal \"$@\"\n", 0700)
	write(filepath.Join(bin, "aerospace"), fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = list-windows ]; then\n pid=%d\n if [ -f %s ]; then pid=$(cat %s); fi\n printf '[{\"window-id\":17,\"app-pid\":%%s,\"app-bundle-id\":\"org.alacritty\"}]\\n' \"$pid\"\nelse\n printf '%%s\\n' \"$*\" >> %s\nfi\n", os.Getpid(), quote(terminalPID), quote(terminalPID), quote(focusLog)), 0700)
	// App activation must never raise the developer's real terminal.
	openLog := filepath.Join(p.home, "open.log")
	write(filepath.Join(bin, "open"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+quote(openLog)+"\n", 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TRAILMIX_HOME", p.home)
	t.Setenv("TRAILMIX_CAPTURE_HELPER", p.helper)
	t.Setenv("TRAILMIX_TEST_HELPER", "normal")
	t.Setenv("XDG_STATE_HOME", filepath.Join(p.home, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(p.home, "data"))
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TERM", "xterm-256color")
	client := exec.Command(tmux, "-S", socket, "-f", "/dev/null", "new-session", "-s", "elsewhere", "/bin/sleep", "300")
	terminal, err := pty.Start(client)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, terminal) }()
	t.Cleanup(func() {
		_, _ = request(p.socket, "stop")
		_ = exec.Command(tmux, "-S", socket, "kill-server").Run()
		terminal.Close()
		_ = client.Wait()
	})
	for deadline := time.Now().Add(3 * time.Second); ; {
		if data, err := exec.Command(tmux, "-S", socket, "list-clients").Output(); err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test tmux client did not attach")
		}
		time.Sleep(10 * time.Millisecond)
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(cli, args...)
		cmd.Dir = p.home
		return cmd.CombinedOutput()
	}
	require := func(args ...string) []byte {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	root := filepath.Join(p.home, "notes 'quoted' ; $literal")
	template := filepath.Join(p.home, "template.md")
	write(template, "# {{date}} {{time}}\n\nTemplate kept\n", 0600)
	require("config", "set", "notes_root", root)
	require("config", "set", "template", template)
	require("config", "set", "terminal.command", `["alacritty", "-e", "{tmux}", "attach-session", "-t", "{target}"]`)
	// An unavailable pin fails before a note or capture directory is created.
	require("config", "set", "microphone", "missing-device")
	if out, err := run("start"); err == nil || !strings.Contains(string(out), "unavailable") {
		t.Fatalf("missing pin: %v %s", err, out)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("missing microphone created a note")
	}
	require("config", "set", "microphone", "")
	// Concurrent starts must converge on the same note, recorder and editor.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := run("start"); err != nil {
				t.Errorf("concurrent start: %v %s", err, out)
			}
		}()
	}
	wg.Wait()
	first := awaitStatus(t, p, func(s status) bool { return s.State == "recording" })
	if first.Note == nil || first.Note.ID != first.MeetingID || first.Transcription == nil {
		t.Fatalf("missing meeting association/transcription: %+v", first)
	}
	// The recorder pushes its state to the invoking tmux server's status line.
	recFlag := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			out, _ := exec.Command(tmux, "-S", socket, "show-options", "-gqv", "@trailmix_rec").Output()
			if strings.TrimSpace(string(out)) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("@trailmix_rec = %q, want %q", out, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	recFlag("1")
	if out := require("status", "--tmux"); !strings.Contains(string(out), "● ") {
		t.Fatalf("status --tmux while recording: %q", out)
	}
	panes := func() string {
		t.Helper()
		out, err := exec.Command(tmux, "-S", socket, "list-panes", "-a", "-F", "#{pane_id} #{pane_current_command} #{@trailmix_meeting} #{@trailmix_note}").Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if count := strings.Count(panes(), first.MeetingID); count != 1 {
		t.Fatalf("duplicate editor targets: %s", panes())
	}
	// Reusing an editor must restore the note even after :edit, without losing
	// unsaved text in the unrelated buffer.
	editorData, err := exec.Command(tmux, "-S", socket, "list-panes", "-a", "-F", "#{@trailmix_editor}").Output()
	if err != nil {
		t.Fatal(err)
	}
	editorSocket := strings.TrimSpace(string(editorData))
	remote := func(expression string) string {
		t.Helper()
		out, err := exec.Command(nvim, "-u", "NONE", "--noplugin", "-i", "NONE", "--server", editorSocket, "--remote-expr", expression).CombinedOutput()
		if err != nil {
			t.Fatalf("remote editor: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	other := filepath.Join(p.home, "other.md")
	remote("execute('edit " + other + "')")
	remote("setline(1, 'unsaved unrelated text')")
	require("open")
	canonicalNote, _ := filepath.EvalSymlinks(first.Note.Path)
	if got := remote("expand('%:p')"); got != canonicalNote {
		t.Fatalf("open did not restore meeting buffer: %s", got)
	}
	if got := remote("getbufline('" + other + "', 1)[0]"); got != "unsaved unrelated text" {
		t.Fatalf("unrelated edits lost: %s", got)
	}
	files, _ := filepath.Glob(filepath.Join(root, "*", "*.md"))
	if len(files) != 1 {
		t.Fatalf("duplicate notes: %v", files)
	}
	data, _ := os.ReadFile(first.Note.Path)
	if !strings.Contains(string(data), "Template kept") || strings.Contains(string(data), "{{date}}") {
		t.Fatalf("template not rendered: %s", data)
	}
	original, _ := os.ReadFile(template)
	if string(original) != "# {{date}} {{time}}\n\nTemplate kept\n" {
		t.Fatal("template overwritten")
	}
	// Navigation back to the original meeting and recreation after editor close.
	_ = exec.Command(tmux, "-S", socket, "switch-client", "-t", "elsewhere").Run()
	require("open")
	var pane string
	for _, line := range strings.Split(panes(), "\n") {
		if strings.Contains(line, first.MeetingID) {
			pane = strings.Fields(line)[0]
		}
	}
	if err := exec.Command(tmux, "-S", socket, "kill-pane", "-t", pane).Run(); err != nil {
		t.Fatal(err)
	}
	require("open")
	if !strings.Contains(panes(), first.MeetingID) {
		t.Fatal("editor not recreated")
	}
	focusBefore, _ := os.ReadFile(focusLog)
	require("toggle")
	focusAfter, _ := os.ReadFile(focusLog)
	if string(focusBefore) != string(focusAfter) {
		t.Fatal("stop stole focus")
	}
	if !strings.Contains(panes(), first.MeetingID) {
		t.Fatal("stop closed the editor")
	}
	require("open")
	require("start")
	second := awaitStatus(t, p, func(s status) bool { return s.State == "recording" && s.MeetingID != first.MeetingID })
	if second.Note.Path == first.Note.Path {
		t.Fatal("new meeting reused original note")
	}
	require("stop")
	recFlag("")
	if out := require("status", "--tmux"); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("status --tmux after stop: %q", out)
	}
	// A recorder that dies cannot clear its flag; status --tmux clears it instead.
	if err := exec.Command(tmux, "-S", socket, "set-option", "-g", "@trailmix_rec", "1").Run(); err != nil {
		t.Fatal(err)
	}
	require("status", "--tmux")
	recFlag("")
	require("record")
	if out, err := run("start"); err == nil || !strings.Contains(string(out), "no meeting note") {
		t.Fatalf("low-level capture acquired note: %v %s", err, out)
	}
	require("toggle")
	out := require("open")
	if !strings.Contains(string(out), second.MeetingID) {
		t.Fatalf("latest meeting lost after low-level record: %s", out)
	}
	// A restarted server may recycle pane/window IDs. An unrelated target with
	// those IDs must not be adopted as the meeting's editor.
	if err := exec.Command(tmux, "-S", socket, "kill-server").Run(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		if err := exec.Command(tmux, "-S", socket, "has-session").Run(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tmux server did not stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if data, err := exec.Command(tmux, "-S", socket, "-f", "/dev/null", "new-session", "-d", "-s", "unrelated", "/bin/sleep", "300").CombinedOutput(); err != nil {
		t.Fatalf("restart: %v %s", err, data)
	}
	require("open")
	if count := strings.Count(panes(), second.MeetingID); count != 1 {
		t.Fatalf("recycled tmux IDs lost note: %s", panes())
	}
	// Simulate opening the saved meeting on a later date. Its stored daily
	// session remains authoritative, independent of today's clock.
	past := *second.Note
	past.Date = "2001-02-03"
	past.Session = "trailmix-2001-02-03"
	encoded, _ := json.Marshal(past)
	write(filepath.Join(p.home, "latest-meeting.json"), string(encoded), 0600)
	require("open")
	if data, err := exec.Command(tmux, "-S", socket, "has-session", "-t", "=trailmix-2001-02-03").CombinedOutput(); err != nil {
		t.Fatalf("original date ignored: %v %s", err, data)
	}
	// Focus failure occurs after successful capture and is recoverable through open.
	write(filepath.Join(bin, "aerospace"), "#!/bin/sh\nexit 1\n", 0700)
	if out, err := run("start"); err == nil || !strings.Contains(string(out), "capture remains recording") {
		t.Fatalf("focus failure: %v %s", err, out)
	}
	third := awaitStatus(t, p, func(s status) bool { return s.State == "recording" })
	if third.Note == nil {
		t.Fatal("focus failure lost note")
	}
	// Restore exact focus and retry without creating another meeting or pane.
	write(filepath.Join(bin, "aerospace"), fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = list-windows ]; then\n printf '[{\"window-id\":17,\"app-pid\":%%s,\"app-bundle-id\":\"org.alacritty\"}]\\n' \"$(cat %s)\"\nfi\n", quote(terminalPID)), 0700)
	require("open")
	if count := strings.Count(panes(), third.MeetingID); count != 1 {
		t.Fatalf("retry duplicated editor: %s", panes())
	}
	require("stop")
	var historical status
	if err := json.Unmarshal(require("status"), &historical); err != nil || historical.Live || historical.MeetingID != third.MeetingID {
		t.Fatalf("historical state: %+v %v", historical, err)
	}
	write(filepath.Join(bin, "nvim"), "#!/bin/sh\nexit 1\n", 0700)
	if out, err := run("start"); err == nil || !strings.Contains(string(out), "capture remains recording") {
		t.Fatalf("editor failure lost capture: %v %s", err, out)
	}
	failedEditor := awaitStatus(t, p, func(s status) bool { return s.State == "recording" })
	write(filepath.Join(bin, "nvim"), "#!/bin/sh\nexec "+quote(nvim)+" -u NONE --noplugin -i NONE \"$@\"\n", 0700)
	require("open")
	if count := strings.Count(panes(), failedEditor.MeetingID); count != 1 {
		t.Fatalf("editor recovery: %s", panes())
	}
	require("stop")
}

func TestMeetingNotesNeverOverwriteSameSecondOrTemplate(t *testing.T) {
	p := testPaths(t)
	cfg := settings.Settings{NotesRoot: filepath.Join(p.home, "notes")}
	now := time.Date(2026, 9, 24, 9, 30, 0, 0, time.FixedZone("local", -5*60*60))
	first, err := newNote(p, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(first.Path)
	second, err := newNote(p, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == second.Path || first.ID == second.ID || first.Date != "2026-09-24" || first.Session != "trailmix-2026-09-24" {
		t.Fatalf("collision: %+v %+v", first, second)
	}
	if filepath.Base(second.Path) != "09-30-00-2.md" {
		t.Fatalf("unexpected collision suffix: %s", second.Path)
	}
	got, _ := os.ReadFile(first.Path)
	if string(got) != string(original) {
		t.Fatal("existing note overwritten")
	}
	if err := validateNote(first); err != nil {
		t.Fatal(err)
	}
	cfg.Template = first.Path
	if _, err := newNote(p, cfg, now); err == nil {
		t.Fatal("accepted a template carrying an existing meeting identity")
	}
}

func TestStartWaitsForFinalRecorderSocketBeforePreflight(t *testing.T) {
	p := testPaths(t)
	t.Setenv("TRAILMIX_HOME", p.home)
	t.Setenv("TRAILMIX_CAPTURE_HELPER", p.helper)
	settingsPath, err := settings.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"notes_root":"invalid-relative-path"}`), 0600); err != nil {
		t.Fatal(err)
	}
	final := status{State: "stopped", Directory: p.home}
	if err := saveStatus(p, final); err != nil {
		t.Fatal(err)
	}
	guard, err := lock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	listener, err := net.Listen("unix", p.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			var req map[string]string
			_ = json.NewDecoder(conn).Decode(&req)
			_ = json.NewEncoder(conn).Encode(final)
			conn.Close()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- Run([]string{"start"}) }()
	select {
	case err := <-done:
		t.Fatalf("start passed terminal recorder before teardown: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	listener.Close()
	guard.Close()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "settings paths must be absolute") {
			t.Fatalf("start did not resume preflight after teardown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start did not resume after recorder teardown")
	}
}
