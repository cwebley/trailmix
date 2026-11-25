package navigation

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRecapEditorsSnapshotsAndConflicts(t *testing.T) {
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("Neovim unavailable")
	}
	home, err := os.MkdirTemp("/tmp", "tm-editors-[work]-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	note := Target{ID: "0123456789abcdef0123456789abcdef", Path: filepath.Join(home, "meeting.md")}
	original := "<!-- trailmix: meeting " + note.ID + " -->\n# Meeting\n\n## Rough notes\n\nSaved notes.\n"
	if err := os.WriteFile(note.Path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := RecapEditors(home, note)
	if err != nil || state.Loaded != 0 || state.Notes != nil || state.Conflict {
		t.Fatalf("closed editors: %+v %v", state, err)
	}
	start := func(socket, path string, register bool) *exec.Cmd {
		t.Helper()
		cmd := exec.Command(nvim, "--headless", "--clean", "-n", "-i", "NONE", "--listen", socket, path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		n := Navigator{nvim: nvim}
		if err := n.waitEditor(socket); err != nil {
			t.Fatal(err)
		}
		if register {
			root := filepath.Join(home, "recap-editors")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(recapEditorRegistration{PID: cmd.Process.Pid, Socket: socket})
			if err := os.WriteFile(filepath.Join(root, strconv.Itoa(cmd.Process.Pid)+".json"), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		return cmd
	}
	socket := filepath.Join(home, "e-fixture.sock")
	start(socket, note.Path, true) // Both registry and managed discovery must deduplicate.
	if _, err := run(nvim, "--server", socket, "--remote-expr", `luaeval('vim.api.nvim_buf_set_lines(0, -1, -1, false, {"Unsaved rough notes."})')`); err != nil {
		t.Fatal(err)
	}
	state, err = RecapEditors(home, note)
	if err != nil || state.Loaded != 1 || state.Conflict || state.Notes == nil || !strings.Contains(*state.Notes, "Unsaved rough notes.\n") {
		t.Fatalf("live snapshot: %+v %v", state, err)
	}
	data, _ := os.ReadFile(note.Path)
	if string(data) != original {
		t.Fatal("snapshot saved or changed note")
	}
	// Whitespace around the stable marker is accepted by the generation
	// identity check and must not discard an otherwise valid live snapshot.
	indent := "luaeval('vim.api.nvim_buf_set_lines(0, 0, 1, false, {\"  <!-- trailmix: meeting " + note.ID + " -->\"})')"
	if _, err := run(nvim, "--server", socket, "--remote-expr", indent); err != nil {
		t.Fatal(err)
	}
	state, err = RecapEditors(home, note)
	if err != nil || state.Conflict || state.Notes == nil || !strings.HasPrefix(*state.Notes, "  <!--") {
		t.Fatalf("indented identity rejected: %+v %v", state, err)
	}
	extra := `luaeval('vim.api.nvim_buf_set_lines(0, -1, -1, false, {"<!-- trailmix: meeting fedcba9876543210fedcba9876543210 -->"})')`
	if _, err := run(nvim, "--server", socket, "--remote-expr", extra); err != nil {
		t.Fatal(err)
	}
	state, err = RecapEditors(home, note)
	if err != nil || !state.Conflict || state.Notes != nil {
		t.Fatalf("ambiguous live snapshot exposed: %+v %v", state, err)
	}
	if _, err := run(nvim, "--server", socket, "--remote-expr", `luaeval('vim.api.nvim_buf_set_lines(0, -2, -1, false, {})')`); err != nil {
		t.Fatal(err)
	}
	if _, err := run(nvim, "--server", socket, "--remote-expr", `luaeval('(function() vim.b.trailmix_recap_swap_pending = true; return 1 end)()')`); err != nil {
		t.Fatal(err)
	}
	state, err = RecapEditors(home, note)
	if err != nil || !state.Conflict || state.Notes != nil || !strings.Contains(state.Reason, "swap") {
		t.Fatalf("unresolved recovery exposed live snapshot: %+v %v", state, err)
	}
	if _, err := run(nvim, "--server", socket, "--remote-expr", `luaeval('(function() vim.b.trailmix_recap_swap_pending = nil; return 1 end)()')`); err != nil {
		t.Fatal(err)
	}
	// Generated sidecars carry meeting identities but aren't editing copies.
	answers := filepath.Join(home, "meeting.answers.md")
	if err := os.WriteFile(answers, []byte("<!-- trailmix: answers "+note.ID+" -->\n"+original), 0600); err != nil {
		t.Fatal(err)
	}
	code := "luaeval('vim.fn.bufload(vim.fn.bufadd(_A))', '" + answers + "')"
	if _, err := run(nvim, "--server", socket, "--remote-expr", code); err != nil {
		t.Fatal(err)
	}
	state, err = RecapEditors(home, note)
	if err != nil || state.Loaded != 1 || state.Conflict {
		t.Fatalf("sidecar mistaken for editor copy: %+v %v", state, err)
	}
	// A second process may have independent unsaved changes to the same file.
	start(filepath.Join(home, "other.sock"), note.Path, true)
	state, err = RecapEditors(home, note)
	if err != nil || state.Loaded != 2 || !state.Conflict || state.Notes != nil {
		t.Fatalf("independent editor conflict: %+v %v", state, err)
	}
}

func TestRecapEditorsRefusesCopiedIdentity(t *testing.T) {
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("Neovim unavailable")
	}
	home, err := os.MkdirTemp("/tmp", "tm-copy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	note := Target{ID: "0123456789abcdef0123456789abcdef", Path: filepath.Join(home, "original.md")}
	copyPath := filepath.Join(home, "copy.md")
	body := []byte("<!-- trailmix: meeting " + note.ID + " -->\n# Meeting\n")
	for _, path := range []string{note.Path, copyPath} {
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	socket := filepath.Join(home, "e-copy.sock")
	cmd := exec.Command(nvim, "--headless", "--clean", "-n", "-i", "NONE", "--listen", socket, copyPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if err := (&Navigator{nvim: nvim}).waitEditor(socket); err != nil {
		t.Fatal(err)
	}
	state, err := RecapEditors(home, note)
	if err != nil || !state.Conflict || state.Notes != nil || state.Loaded != 1 {
		t.Fatalf("copied identity accepted: %+v %v", state, err)
	}
}
