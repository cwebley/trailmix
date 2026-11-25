package navigation

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func titleTestNavigator(t *testing.T, output string) (*Navigator, string) {
	t.Helper()
	root := t.TempDir()
	tmux := filepath.Join(root, "tmux")
	log := filepath.Join(root, "log")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
	script := "#!/bin/sh\nif [ \"$1\" = -N ]; then printf '%s\\n' " + quote(output) + "; exit; fi\nprintf '%s\\n' \"$@\" >> " + quote(log) + "\n"
	if err := os.WriteFile(tmux, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return &Navigator{tmux: tmux}, log
}

func TestTitleRefusesLiveTaggedPaneRegardlessOfBufferState(t *testing.T) {
	n, _ := titleTestNavigator(t, "$1:@2\t%3\tmeeting-id\t/old.md\t0\t/missing-editor.sock")
	// The dead socket is uncertainty, not proof that hidden or clean buffers are gone.
	if err := n.checkTitlePanes(Target{ID: "meeting-id"}); err == nil || !strings.Contains(err.Error(), "close") {
		t.Fatalf("live pane accepted: %v", err)
	}
}

func TestRetitleUpdatesTagsAndWindowWithoutOpeningEditor(t *testing.T) {
	n, log := titleTestNavigator(t, "$1:@2\t%3\tmeeting-id\t/old.md\t1\t/stale.sock")
	before := Target{ID: "meeting-id", Path: "/old.md", Window: "09:07"}
	after := before
	after.Path = "/new's note.md"
	after.Window = "09:07 Product standup"
	if err := n.retitle(before, after); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "set-option\n-p\n-t\n%3\n@trailmix_note\n/new's note.md\nrename-window\n-t\n$1:@2\n09:07 Product standup\n"
	if string(data) != want {
		t.Fatalf("unexpected commands: %s", data)
	}
}

func TestTitlePaneInspectionFailsClosed(t *testing.T) {
	n, _ := titleTestNavigator(t, "bad pane output")
	if err := n.checkTitlePanes(Target{ID: "meeting-id"}); err == nil {
		t.Fatal("malformed inspection accepted")
	}
	n, log := titleTestNavigator(t, "$1:@2\t%3\tmeeting-id\t/unexpected.md\t1\t/stale.sock")
	if err := n.retitle(Target{ID: "meeting-id", Path: "/old.md"}, Target{Path: "/new.md"}); err == nil {
		t.Fatal("retagged unrelated path")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("wrote tags before validation: %v", err)
	}
}

func TestTitleInspectsOtherManagedEditorsHiddenBuffers(t *testing.T) {
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("nvim unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "tm-title-[literal]-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	socket := filepath.Join(root, "e-test.sock")
	note := Target{ID: "0123456789abcdef0123456789abcdef", Path: filepath.Join(root, "note.md")}
	if err := os.WriteFile(note.Path, []byte("<!-- trailmix: meeting "+note.ID+" -->\n# Note\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(nvim, "--headless", "-u", "NONE", "-i", "NONE", "-n", "--listen", socket, "--", note.Path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Lstat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("editor socket did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := run(nvim, "--server", socket, "--remote-expr", `execute('hide enew')`); err != nil {
		t.Fatal(err)
	}
	n := &Navigator{home: root, nvim: nvim}
	if err := n.checkManagedTitleSockets(note, nil); err == nil || !strings.Contains(err.Error(), "hidden or clean") {
		t.Fatalf("hidden clean buffer accepted: %v", err)
	}
	// The same live editor is allowed once only unrelated buffers remain.
	if _, err := run(nvim, "--server", socket, "--remote-expr", `execute('silent! bwipeout 1')`); err != nil {
		t.Fatal(err)
	}
	if err := n.checkManagedTitleSockets(note, nil); err != nil {
		t.Fatalf("unrelated editor blocked title: %v", err)
	}
}

func TestTitleRefusesMatchingPaneWithoutControlSocket(t *testing.T) {
	n, _ := titleTestNavigator(t, "$1:@2\t%3\tmeeting-id\t/old.md\t0\t")
	if err := n.checkTitlePanes(Target{ID: "meeting-id"}); err == nil {
		t.Fatal("matching live pane without socket accepted")
	}
}
