package navigation

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
