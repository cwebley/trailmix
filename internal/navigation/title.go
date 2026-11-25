package navigation

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CheckTitleSafety refuses even clean buffers in managed or registered editors.
// A later save could recreate the old filename. Users must close copies in
// unregistered external editors themselves; those cannot be reliably inspected.
func CheckTitleSafety(home string, note Target) error {
	n, err := titleNavigator(home)
	if err != nil {
		return err
	}
	if n == nil {
		return (&Navigator{home: home}).checkManagedTitleSockets(note, nil)
	}
	return n.checkTitlePanes(note)
}

func (n *Navigator) checkTitlePanes(note Target) error {
	panes, err := n.titlePanes()
	if err != nil {
		return err
	}
	for _, p := range panes {
		if p.dead == "0" && (p.id == note.ID || p.path == note.Path) {
			return errors.New("close the meeting's editor pane before changing its title, then retry trailmix title")
		}
	}
	return n.checkManagedTitleSockets(note, panes)
}

func (n *Navigator) checkManagedTitleSockets(note Target, panes []titlePane) error {
	sockets := map[string]bool{} // true means a live pane requires a reachable endpoint.
	for _, pane := range panes {
		if pane.dead == "0" && pane.socket != "" {
			sockets[pane.socket] = true
		}
	}
	expectedPIDs := map[string]int{}
	if n.home != "" {
		registered, err := recapEditorSockets(n.home)
		if err != nil {
			return err
		}
		for socket, pid := range registered {
			expectedPIDs[socket] = pid
			sockets[socket] = sockets[socket] || pid != 0
		}
	}
	for socket, required := range sockets {
		connection, err := net.DialTimeout("unix", socket, 300*time.Millisecond)
		if err != nil {
			if !required && (errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist)) {
				continue
			}
			return fmt.Errorf("cannot inspect managed editor %s; close its editor pane and retry title: %w", socket, err)
		}
		connection.Close()
		nvim := n.nvim
		if nvim == "" {
			nvim, err = executable("nvim")
			if err != nil {
				return err
			}
		}
		quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
		code := `(function()
  local paths = {}
  for i = 1, 3 do paths[_A[i]] = true; local real = vim.uv.fs_realpath(_A[i]); if real then paths[real] = true end end
  local marker = "<!-- trailmix: meeting " .. _A[4] .. " -->"
  for _, buf in ipairs(vim.api.nvim_list_bufs()) do
    if vim.api.nvim_buf_is_loaded(buf) then
      local name = vim.api.nvim_buf_get_name(buf)
      if paths[name] or paths[vim.uv.fs_realpath(name) or ""] then return "loaded:" .. vim.fn.getpid() end
      for _, line in ipairs(vim.api.nvim_buf_get_lines(buf, 0, -1, false)) do
        if line:find(marker, 1, true) then return "loaded:" .. vim.fn.getpid() end
      end
    end
  end
  return "clear:" .. vim.fn.getpid()
end)()`
		base := strings.TrimSuffix(note.Path, filepath.Ext(note.Path))
		expression := "luaeval(" + quote(code) + ", [" + quote(note.Path) + ", " + quote(base+".transcript.md") + ", " + quote(base+".answers.md") + ", " + quote(note.ID) + "])"
		result, err := run(nvim, "--server", socket, "--remote-expr", expression)
		if err != nil {
			return fmt.Errorf("cannot inspect managed editor %s; close its pane and retry title: %w", socket, err)
		}
		state, pidText, ok := strings.Cut(result, ":")
		pid, parseErr := strconv.Atoi(pidText)
		if !ok || parseErr != nil || pid < 2 || (expectedPIDs[socket] != 0 && expectedPIDs[socket] != pid) {
			return fmt.Errorf("cannot inspect managed editor %s; editor identity changed or returned invalid state", socket)
		}
		if state == "loaded" {
			return fmt.Errorf("close loaded meeting note and sidecar buffers in managed editor %s, including hidden or clean buffers, then retry title", socket)
		}
		if state != "clear" {
			return fmt.Errorf("cannot inspect managed editor %s; unexpected buffer response; close its pane and retry title", socket)
		}
	}
	return nil
}

func titleNavigator(home string) (*Navigator, error) {
	tmux, err := executable("tmux")
	if err != nil {
		return nil, nil
	} // No tmux executable means no managed panes to update.
	if socket, _, _ := strings.Cut(os.Getenv("TMUX"), ","); filepath.IsAbs(socket) {
		tmux, err = pinnedTmux(home, tmux, socket)
		if err != nil {
			return nil, err
		}
	}
	return &Navigator{tmux: tmux, home: home}, nil
}

type titlePane struct{ window, pane, id, path, dead, socket string }

func (n *Navigator) titlePanes() ([]titlePane, error) {
	text, err := run(n.tmux, "-N", "list-panes", "-a", "-F", "#{session_id}:#{window_id}\t#{pane_id}\t#{@trailmix_meeting}\t#{@trailmix_note}\t#{pane_dead}\t#{@trailmix_editor}")
	if err != nil {
		if strings.Contains(text, "no server running") || strings.Contains(text, "No such file or directory") {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot inspect meeting panes: %w", err)
	}
	var result []titlePane
	if text == "" {
		return result, nil
	}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Split(line, "\t")
		// run trims trailing whitespace, including the empty final socket field.
		if len(f) == 5 {
			f = append(f, "")
		}
		if len(f) != 6 {
			return nil, errors.New("cannot inspect meeting panes: unexpected tmux response")
		}
		result = append(result, titlePane{f[0], f[1], f[2], f[3], f[4], f[5]})
	}
	return result, nil
}

// Retitle updates stale pane tags and window names without opening a buffer or
// launching an editor. The caller can call it in reverse when rolling back.
func Retitle(home string, before, after Target) error {
	n, err := titleNavigator(home)
	if err != nil || n == nil {
		return err
	}
	return n.retitle(before, after)
}

func (n *Navigator) retitle(before, after Target) error {
	panes, err := n.titlePanes()
	if err != nil {
		return err
	}
	for _, p := range panes {
		if p.id != before.ID {
			continue
		}
		if p.path != before.Path && p.path != after.Path {
			return errors.New("meeting pane path changed; close its pane and retry")
		}
		if _, err := run(n.tmux, "set-option", "-p", "-t", p.pane, "@trailmix_note", after.Path); err != nil {
			return err
		}
		if _, err := run(n.tmux, "rename-window", "-t", p.window, after.Window); err != nil {
			return err
		}
	}
	return nil
}
