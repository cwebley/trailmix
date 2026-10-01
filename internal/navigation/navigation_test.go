package navigation

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cwebley/trailmix/internal/settings"

	"github.com/creack/pty"
)

// Exercise real tmux -> Neovim startup with a fixture init.lua. Using -u NONE
// here would hide failures in the configuration lookup used by normal editors.
func TestEditorLoadsConfiguration(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("nvim is not installed")
	}
	for _, mode := range []string{"unset", "empty", "explicit"} {
		t.Run(mode, func(t *testing.T) {
			// Keep Unix socket paths short, including on macOS.
			home, err := os.MkdirTemp("/tmp", "tm-nav-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			write := func(path, contents string, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), mode); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", home)
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
			t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
			t.Setenv("XDG_CONFIG_DIRS", filepath.Join(home, "config-dirs"))
			t.Setenv("XDG_DATA_DIRS", filepath.Join(home, "data-dirs"))
			t.Setenv("NVIM_APPNAME", "nvim")
			for _, key := range []string{"VIMINIT", "EXINIT", "NVIM", "TMUX", "TMUX_PANE"} {
				t.Setenv(key, "")
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("TERM", "xterm-256color")
			// The server's stale XDG value must not replace the caller's choice,
			// including the caller choosing the default by leaving it unset.
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "stale"))
			socket := filepath.Join(home, "tmux.sock")
			if out, err := exec.Command(tmux, "-S", socket, "-f", "/dev/null", "new-session", "-d", "-s", "fixture", "/bin/sleep", "60").CombinedOutput(); err != nil {
				t.Fatalf("start private tmux: %v %s", err, out)
			}
			t.Cleanup(func() { _ = exec.Command(tmux, "-S", socket, "kill-server").Run() })
			configRoot := filepath.Join(home, ".config")
			switch mode {
			case "unset":
				if err := os.Unsetenv("XDG_CONFIG_HOME"); err != nil {
					t.Fatal(err)
				}
			case "empty":
				t.Setenv("XDG_CONFIG_HOME", "")
			case "explicit":
				configRoot = filepath.Join(home, "custom config")
				t.Setenv("XDG_CONFIG_HOME", configRoot)
			}
			write(filepath.Join(configRoot, "nvim", "init.lua"), `vim.g.mapleader = " "
vim.keymap.set("n", "<leader>mt", ":echo 'fixture timestamp'<CR>")
`, 0600)
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
			tmuxWrapper := filepath.Join(home, "bin", "tmux")
			write(tmuxWrapper, "#!/bin/sh\nexec "+quote(tmux)+" -S "+quote(socket)+" -f /dev/null \"$@\"\n", 0700)
			nvimWrapper := filepath.Join(home, "bin", "nvim")
			write(nvimWrapper, "#!/bin/sh\nexec "+quote(nvim)+" --noplugin -i NONE \"$@\"\n", 0700)
			n := &Navigator{tmux: tmuxWrapper, nvim: nvimWrapper, home: home}
			note := Target{ID: "config-fixture", Path: filepath.Join(home, "notes", "meeting.md"), Session: "trailmix-fixture", Window: "meeting"}
			write(note.Path, "<!-- trailmix: meeting "+note.ID+" -->\n", 0600)
			target, err := n.editor(note)
			if err != nil {
				t.Fatal(err)
			}
			editorSocket, err := run(tmuxWrapper, "display-message", "-p", "-t", target, "#{@trailmix_editor}")
			if err != nil {
				t.Fatal(err)
			}
			got, err := run(nvim, "--server", editorSocket, "--remote-expr", `get(g:, 'mapleader', '') ==# ' ' && !empty(maparg(' mt', 'n'))`)
			if err != nil || got != "1" {
				config, _ := run(nvim, "--server", editorSocket, "--remote-expr", `stdpath('config')`)
				t.Fatalf("leader mapping not loaded: result=%q error=%v config=%q; expected %s", got, err, config, filepath.Join(configRoot, "nvim"))
			}
		})
	}
}

func TestAppBundleAndAncestry(t *testing.T) {
	for command, want := range map[string]string{
		"/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal": "/System/Applications/Utilities/Terminal.app",
		"/Applications/Wez Term.app/Contents/MacOS/wezterm-gui":               "/Applications/Wez Term.app",
		"/opt/homebrew/bin/tmux": "",
		"-zsh":                   "",
	} {
		if got := appBundle(command); got != want {
			t.Errorf("appBundle(%q) = %q, want %q", command, got, want)
		}
	}
	table := map[int]process{40: {30, "tmux"}, 30: {20, "login"}, 20: {1, "Terminal"}}
	if got := ancestry(table, 40); len(got) != 3 || got[0] != 40 || got[2] != 20 {
		t.Fatalf("ancestry = %v", got)
	}
}

func TestProcessesKeepsExecutablePaths(t *testing.T) {
	table, err := processes()
	if err != nil {
		t.Fatal(err)
	}
	self, ok := table[os.Getpid()]
	if !ok || self.parent != os.Getppid() || !strings.HasPrefix(self.command, "/") {
		t.Fatalf("own process entry: %+v", self)
	}
}

// Restoring a note must not land in the answers/transcript panel, which would
// then replace the note on the next panel update and take its focus.
func TestShowNotePrefersTheNoteWindowOverThePanel(t *testing.T) {
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("nvim is not installed")
	}
	home, err := os.MkdirTemp("/tmp", "tm-show-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	note := Target{ID: "show-fixture", Path: filepath.Join(home, "meeting.md")}
	if err := os.WriteFile(note.Path, []byte("<!-- trailmix: meeting "+note.ID+" -->\n"), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(home, "e.sock")
	editor := exec.Command(nvim, "--headless", "--clean", "--listen", socket, note.Path)
	if err := editor.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = editor.Process.Kill(); _ = editor.Wait() })
	lua := func(code string) string {
		t.Helper()
		var out string
		for i := 0; i < 50; i++ {
			if out, err = run(nvim, "--server", socket, "--remote-expr", "luaeval('"+strings.ReplaceAll(code, "'", "''")+"')"); err == nil {
				return out
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("remote lua: %v %s", err, out)
		return ""
	}
	// The note sits on the left; a tagged panel on the right has focus.
	lua(`(function()
  _G.note_win = vim.api.nvim_get_current_win()
  vim.cmd("rightbelow vsplit | enew")
  vim.bo.buftype = "nofile"
  vim.b.trailmix_kind = "transcript"
  vim.w.trailmix_panel = true
  _G.panel_win, _G.panel_buf = vim.api.nvim_get_current_win(), vim.api.nvim_get_current_buf()
  return "ok"
end)()`)
	n := &Navigator{nvim: nvim, home: home}
	if err := n.showNote(note, socket, strconv.Itoa(editor.Process.Pid)); err != nil {
		t.Fatal(err)
	}
	if got := lua(`vim.api.nvim_get_current_win() == _G.note_win and vim.api.nvim_win_get_buf(_G.panel_win) == _G.panel_buf and "ok" or "moved"`); got != "ok" {
		t.Fatal("note was restored into the panel window")
	}
	// A note hidden behind another file returns to that editing window, not the panel.
	lua(`(function()
  vim.api.nvim_set_current_win(_G.note_win)
  vim.cmd("enew")
  vim.api.nvim_set_current_win(_G.panel_win)
  return "ok"
end)()`)
	if err := n.showNote(note, socket, strconv.Itoa(editor.Process.Pid)); err != nil {
		t.Fatal(err)
	}
	if got := lua(`vim.api.nvim_get_current_win() == _G.note_win and vim.api.nvim_win_get_buf(_G.panel_win) == _G.panel_buf and vim.fn.expand("%:t") == "meeting.md" and "ok" or "moved"`); got != "ok" {
		t.Fatal("hidden note was restored into the panel window")
	}
}

// The test binary doubles as a terminal that runs its -e command under a pty.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "trailmix-test-terminal" {
		cmd := exec.Command(os.Args[2], os.Args[3:]...)
		terminal, err := pty.Start(cmd)
		if err != nil {
			os.Exit(1)
		}
		go func() { _, _ = io.Copy(io.Discard, terminal) }()
		_ = cmd.Wait()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A meeting created on the server named by TMUX must be attached on that
// server, even though launched terminals drop TMUX to avoid nesting.
func TestLaunchedTerminalAttachesToTheSelectedServer(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("nvim is not installed")
	}
	home, err := os.MkdirTemp("/tmp", "tm-srv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	for name, script := range map[string]string{
		"nvim":          "#!/bin/sh\nexec " + quote(nvim) + " -u NONE --noplugin -i NONE \"$@\"\n",
		"fake-terminal": "#!/bin/sh\nexec " + quote(self) + " trailmix-test-terminal \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+filepath.Dir(tmux)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", home)
	t.Setenv("TERM", "xterm-256color")
	// Isolate the default server too, so a wrong attach cannot reach a real one.
	t.Setenv("TMUX_TMPDIR", filepath.Join(home, "default"))
	if err := os.Mkdir(filepath.Join(home, "default"), 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(home, "private.sock")
	if out, err := exec.Command(tmux, "-S", socket, "-f", "/dev/null", "new-session", "-d", "-s", "elsewhere", "/bin/sleep", "60").CombinedOutput(); err != nil {
		t.Fatalf("start private tmux: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(tmux, "-S", socket, "kill-server").Run() })
	t.Setenv("TMUX", socket+",1,0")
	t.Setenv("TMUX_PANE", "")
	n, err := Preflight(home, settings.Terminal{Command: []string{"fake-terminal", "{tmux}", "attach-session", "-t", "{target}"}, Focus: "none"})
	if err != nil {
		t.Fatal(err)
	}
	note := Target{ID: "server-fixture", Path: filepath.Join(home, "meeting.md"), Session: "trailmix-server", Window: "meeting"}
	if err := os.WriteFile(note.Path, []byte("<!-- trailmix: meeting "+note.ID+" -->\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := n.Open(note); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(tmux, "-S", socket, "list-clients", "-F", "#{client_session}").Output()
	if err != nil || !strings.Contains(string(out), note.Session) {
		t.Fatalf("no client attached to the meeting on the selected server: %v %q", err, out)
	}
}
