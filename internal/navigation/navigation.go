// Package navigation opens meeting notes through tmux, launches a configured
// terminal when no client is attached, and brings that client's terminal
// forward. Focus follows the tmux client's process ancestry to its terminal.
package navigation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cwebley/trailmix/internal/settings"
)

type Target struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Date    string `json:"date"`
	Session string `json:"session"`
	Window  string `json:"window"`
}

type Navigator struct {
	tmux, nvim, home string
	terminal         []string // Launch command with placeholders.
	focusMode        string   // aerospace, macos, or none.
	focusFallback    bool     // auto: try app activation when Aerospace cannot.
	aerospace        string
	preferred        *client
}

// Hotkeys need not inherit an interactive shell's Homebrew PATH.
// Find resolves an executable the way navigation launches it, including
// Homebrew locations that hotkey launches may lack in PATH.
func Find(name string) (string, error) { return executable(name) }

func executable(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return filepath.Abs(path)
	}
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/Applications/Alacritty.app/Contents/MacOS"} {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s is required and could not be found", name)
}

func run(program string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.WaitDelay = time.Second
	data, err := cmd.CombinedOutput()
	if err != nil {
		return string(data), fmt.Errorf("%s: %w: %s", filepath.Base(program), err, strings.TrimSpace(string(data)))
	}
	return strings.TrimSpace(string(data)), nil
}

func Preflight(home string, terminal settings.Terminal) (*Navigator, error) {
	if err := terminal.Validate(); err != nil {
		return nil, err
	}
	n := &Navigator{home: home, focusMode: terminal.Focus}
	if len(filepath.Join(home, "e-0123456789abcdef.sock")) >= 104 {
		return nil, errors.New("editor socket path is too long; set TRAILMIX_HOME to a shorter absolute path")
	}
	var err error
	n.tmux, err = executable("tmux")
	if err != nil {
		return nil, err
	}
	// TMUX selects the server for our own commands, but launched terminals drop
	// it to avoid nesting and would attach through the default server. Pin the
	// server in a wrapper, which also works where {tmux} sits in a shell string.
	if socket, _, _ := strings.Cut(os.Getenv("TMUX"), ","); filepath.IsAbs(socket) {
		if n.tmux, err = pinnedTmux(home, n.tmux, socket); err != nil {
			return nil, err
		}
	}
	n.nvim, err = executable("nvim")
	if err != nil {
		return nil, err
	}
	clients, err := n.clients()
	if err != nil {
		return nil, err
	}
	name := os.Getenv("TRAILMIX_TMUX_CLIENT")
	if name == "" && os.Getenv("TMUX") != "" && os.Getenv("TMUX_PANE") != "" {
		name, _ = run(n.tmux, "display-message", "-p", "-t", os.Getenv("TMUX_PANE"), "#{client_name}")
	}
	for _, c := range clients {
		if c.name == name {
			selected := c
			n.preferred = &selected
			break
		}
	}
	n.terminal = terminal.Command
	if len(clients) == 0 {
		if _, err := executable(n.terminal[0]); err != nil {
			return nil, fmt.Errorf("terminal.command: %w", err)
		}
	}
	// Missing focus support must not stop capture; focus reports it later.
	n.aerospace, _ = executable("aerospace")
	if n.focusMode == "auto" {
		n.focusMode, n.focusFallback = "macos", true
		if n.aerospace != "" {
			n.focusMode = "aerospace"
		}
	}
	return n, nil
}

func pinnedTmux(home, tmux, socket string) (string, error) {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
	sum := sha256.Sum256([]byte(tmux + "\x00" + socket))
	path := filepath.Join(home, "tmux-"+hex.EncodeToString(sum[:8]))
	temp, err := os.CreateTemp(home, ".tmux-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(temp.Name())
	_, err = temp.WriteString("#!/bin/sh\nexec " + quote(tmux) + " -S " + quote(socket) + " \"$@\"\n")
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(temp.Name(), 0700)
	}
	if err == nil {
		err = os.Rename(temp.Name(), path)
	}
	return path, err
}

type client struct {
	name     string
	pid      int
	activity int64
	session  string
}

func (n *Navigator) clients() ([]client, error) {
	text, err := run(n.tmux, "-N", "list-clients", "-F", "#{client_name}\t#{client_pid}\t#{client_activity}\t#{client_control_mode}\t#{client_readonly}\t#{client_session}")
	if err != nil {
		if strings.Contains(text, "no server running") || strings.Contains(text, "No such file or directory") {
			return nil, nil
		}
		return nil, err
	}
	var clients []client
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 6 || fields[3] != "0" || fields[4] != "0" {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, err
		}
		activity, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, err
		}
		clients = append(clients, client{fields[0], pid, activity, fields[5]})
	}
	sort.SliceStable(clients, func(i, j int) bool { return clients[i].activity > clients[j].activity })
	if n.preferred != nil {
		for i, c := range clients {
			if c.name == n.preferred.name && c.pid == n.preferred.pid {
				copy(clients[1:i+1], clients[:i])
				clients[0] = c
				break
			}
		}
	}
	return clients, nil
}

func (n *Navigator) Open(note Target) error {
	clients, err := n.clients()
	if err != nil {
		return err
	}
	target, err := n.editor(note)
	if err != nil {
		return err
	}
	if len(clients) > 0 {
		c := clients[0]
		if _, err := run(n.tmux, "switch-client", "-c", c.name, "-t", target); err != nil {
			return err
		}
		return n.focus(c.pid, false)
	}
	program, err := executable(n.terminal[0])
	if err != nil {
		return fmt.Errorf("terminal.command: %w", err)
	}
	log, err := os.CreateTemp(n.home, "terminal-*.log")
	if err != nil {
		return err
	}
	defer log.Close()
	replacer := strings.NewReplacer("{tmux}", n.tmux, "{target}", target, "{dir}", filepath.Dir(note.Path))
	args := make([]string, len(n.terminal)-1)
	for i, arg := range n.terminal[1:] {
		args[i] = replacer.Replace(arg)
	}
	cmd := exec.Command(program, args...)
	// A terminal launched from a stale tmux environment must attach, not nest.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TMUX=") && !strings.HasPrefix(entry, "TMUX_PANE=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	// The launched process may only ask another app to open a window, as
	// osascript does for Terminal.app, so focus follows the attached client.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		clients, err := n.clients()
		if err != nil {
			return err
		}
		for _, c := range clients {
			if c.session == note.Session {
				if err := n.focus(c.pid, true); err != nil {
					return fmt.Errorf("%w; terminal log: %s", err, log.Name())
				}
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("terminal did not attach to %s; check terminal.command; terminal log: %s", note.Session, log.Name())
}

func (n *Navigator) editor(note Target) (string, error) {
	// Rediscover by our pane tags every time. Numeric tmux IDs can be recycled
	// after a server restart; titles and window names can change while editing.
	text, err := run(n.tmux, "-N", "list-panes", "-a", "-F", "#{session_name}\t#{session_id}:#{window_id}\t#{pane_id}\t#{@trailmix_meeting}\t#{@trailmix_note}\t#{pane_dead}\t#{pane_current_command}\t#{@trailmix_editor}\t#{pane_pid}")
	if err != nil && !strings.Contains(text, "no server running") && !strings.Contains(text, "No such file or directory") {
		return "", err
	}
	sessionExists := false
	for _, line := range strings.Split(text, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 9 || f[0] != note.Session {
			continue
		}
		sessionExists = true
		if f[3] == note.ID && f[4] == note.Path && f[5] == "0" && f[6] == filepath.Base(n.nvim) {
			if err := n.showNote(note, f[7], f[8]); err != nil {
				return "", err
			}
			if _, err := run(n.tmux, "select-pane", "-t", f[2]); err != nil {
				return "", err
			}
			return f[1], nil
		}
	}
	args := []string{"new-session", "-d", "-s", note.Session}
	if sessionExists {
		args = []string{"new-window", "-d", "-t", "=" + note.Session + ":"}
	}
	args = append(args, "-n", note.Window, "-c", filepath.Dir(note.Path), "-P", "-F", "#{session_id}:#{window_id}\t#{pane_id}")
	cli, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Multiple command arguments make tmux exec directly, with no shell parsing.
	editorSocket := filepath.Join(n.home, "e-"+rand.Text()[:16]+".sock")
	args = append(args, "/usr/bin/env")
	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		args = append(args, "XDG_CONFIG_HOME="+configHome)
	} else {
		// Neovim treats an empty XDG_CONFIG_HOME as a relative config root.
		// Unset it to use ~/.config, also clearing stale tmux server values.
		args = append(args, "-u", "XDG_CONFIG_HOME")
	}
	args = append(args, "TRAILMIX_CLI="+cli, "TRAILMIX_HOME="+n.home, n.nvim, "--listen", editorSocket, "--", note.Path)
	created, err := run(n.tmux, args...)
	if err != nil {
		return "", err
	}
	f := strings.Split(created, "\t")
	if len(f) != 2 {
		return "", errors.New("tmux did not return an editor target")
	}
	for _, tag := range [][2]string{{"@trailmix_editor", editorSocket}, {"@trailmix_note", note.Path}, {"@trailmix_meeting", note.ID}} {
		if _, err := run(n.tmux, "set-option", "-p", "-t", f[1], tag[0], tag[1]); err != nil {
			return "", err
		}
	}
	// Wait for the exec into Neovim, not its configuration or plugin readiness.
	// Recording is already running. A shell/failed editor is not a valid target.
	deadline := time.Now().Add(3 * time.Second)
	for {
		command, err := run(n.tmux, "display-message", "-p", "-t", f[1], "#{pane_current_command}\t#{pane_dead}\t#{pane_pid}")
		if err != nil {
			return "", err
		}
		parts := strings.Split(command, "\t")
		if len(parts) == 3 && parts[0] == filepath.Base(n.nvim) && parts[1] == "0" {
			if err := n.showNote(note, editorSocket, parts[2]); err != nil {
				return "", err
			}
			break
		}
		if time.Now().After(deadline) {
			return "", errors.New("Neovim did not start in the meeting pane")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return f[0], nil
}

func (n *Navigator) showNote(note Target, socket, pid string) error {
	if socket == "" {
		return errors.New("meeting editor has no control socket; close its pane and retry trailmix open")
	}
	editorPID, err := run(n.nvim, "--server", socket, "--remote-expr", "getpid()")
	if err != nil {
		return fmt.Errorf("reach meeting editor: %w", err)
	}
	actual, err := strconv.Atoi(editorPID)
	if err != nil {
		return err
	}
	panePID, err := strconv.Atoi(pid)
	if err != nil {
		return err
	}
	table, err := processes()
	if err != nil {
		return err
	}
	belongs := false
	for _, current := range ancestry(table, actual) {
		if current == panePID {
			belongs = true
			break
		}
	}
	if !belongs {
		return errors.New("meeting editor no longer belongs to its tmux pane")
	}
	// Quote as Vim string literals, then pass argv directly. No path is evaluated
	// as shell or Ex code. The PID check rejects stale/recycled editor endpoints.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	code := `(function()
  if vim.fn.getpid() ~= tonumber(_A[3]) then error("meeting editor identity changed") end
  local buf = vim.fn.bufadd(_A[1])
  vim.fn.bufload(buf)
  local marker = "<!-- trailmix: meeting " .. _A[2] .. " -->"
  local found = false
  for _, line in ipairs(vim.api.nvim_buf_get_lines(buf, 0, -1, false)) do
    if line == marker then found = true end
  end
  if not found then error("meeting buffer identity is missing or changed") end
  -- Never restore into the answers/transcript panel: its next update would
  -- replace the note. Prefer a window already showing it, then an editing one.
  local windows, target = vim.fn.win_findbuf(buf), nil
  for _, win in ipairs(windows) do
    if vim.api.nvim_win_get_tabpage(win) == vim.api.nvim_get_current_tabpage() then target = win; break end
  end
  target = target or windows[1]
  if not target then
    local editing = function(win) return not vim.w[win].trailmix_panel and vim.api.nvim_win_get_config(win).relative == "" end
    if editing(vim.api.nvim_get_current_win()) then target = vim.api.nvim_get_current_win() end
    for _, win in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
      if not target and editing(win) then target = win end
    end
  end
  if target then vim.api.nvim_set_current_win(target) end
  vim.cmd("hide buffer " .. buf)
  return "ok"
end)()`
	expression := "luaeval(" + quote(code) + ", [" + quote(note.Path) + ", " + quote(note.ID) + ", " + quote(editorPID) + "])"
	result, err := run(n.nvim, "--server", socket, "--remote-expr", expression)
	if err != nil {
		return fmt.Errorf("show meeting note: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("meeting editor could not display its note: %s", result)
	}
	return nil
}

type window struct {
	ID     int    `json:"window-id"`
	PID    int    `json:"app-pid"`
	Bundle string `json:"app-bundle-id"`
}

type process struct {
	parent  int
	command string // Full executable path on macOS.
}

func processes() (map[int]process, error) {
	text, err := run("/bin/ps", "-axo", "pid=,ppid=,comm=")
	if err != nil {
		return nil, err
	}
	table := map[int]process{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		child, _ := strconv.Atoi(fields[0])
		parent, _ := strconv.Atoi(fields[1])
		// Executable paths can contain spaces; keep everything after the PIDs.
		command := strings.TrimSpace(line)
		for _, field := range fields[:2] {
			command = strings.TrimSpace(strings.TrimPrefix(command, field))
		}
		table[child] = process{parent, command}
	}
	return table, nil
}

// ancestry lists pid and its parents, nearest first.
func ancestry(table map[int]process, pid int) []int {
	var chain []int
	for current, depth := pid, 0; current > 1 && depth < 64; current, depth = table[current].parent, depth+1 {
		chain = append(chain, current)
	}
	return chain
}

// focus brings forward the terminal that hosts the tmux client with this pid.
// wait allows a just-launched terminal's window time to appear.
func (n *Navigator) focus(pid int, wait bool) error {
	switch n.focusMode {
	case "none":
		return nil
	case "macos":
		return n.focusApp(pid)
	}
	err := n.focusWindow(pid, wait)
	if err != nil && n.focusFallback {
		if appErr := n.focusApp(pid); appErr == nil {
			return nil
		}
	}
	return err
}

// focusWindow focuses one exact Aerospace window owned by the client's
// terminal process. It never guesses between several windows.
func (n *Navigator) focusWindow(pid int, wait bool) error {
	if n.aerospace == "" {
		return errors.New("notes selected in tmux, but Aerospace is unavailable for exact terminal focus")
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		text, err := run(n.aerospace, "list-windows", "--monitor", "all", "--format", "%{window-id} %{app-pid} %{app-bundle-id} %{workspace}", "--json")
		if err != nil {
			return err
		}
		var windows []window
		if err := json.Unmarshal([]byte(text), &windows); err != nil {
			return err
		}
		table, err := processes()
		if err != nil {
			return err
		}
		ids := map[int]bool{}
		for _, current := range ancestry(table, pid) {
			for _, w := range windows {
				if w.PID == current {
					ids[w.ID] = true
				}
			}
			if len(ids) > 0 {
				break // The nearest windowed ancestor is the terminal.
			}
		}
		if len(ids) > 1 {
			return errors.New("notes selected in tmux, but the terminal process owns multiple Aerospace windows; exact focus is ambiguous")
		}
		for id := range ids {
			_, err := run(n.aerospace, "focus", "--window-id", strconv.Itoa(id))
			return err
		}
		if !wait || time.Now().After(deadline) {
			return errors.New("notes selected in tmux, but no exact terminal window could be identified")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// focusApp activates the nearest app bundle in the client's ancestry. macOS
// raises that app's frontmost window, which may not be the meeting's window.
func (n *Navigator) focusApp(pid int) error {
	table, err := processes()
	if err != nil {
		return err
	}
	for _, current := range ancestry(table, pid) {
		if bundle := appBundle(table[current].command); bundle != "" {
			open, err := executable("open")
			if err != nil {
				return err
			}
			_, err = run(open, "-a", bundle)
			return err
		}
	}
	return errors.New("notes selected in tmux, but no terminal app could be identified to focus")
}

// appBundle returns the .app bundle containing an executable, if any.
func appBundle(command string) string {
	if i := strings.Index(command, ".app/Contents/MacOS/"); i >= 0 {
		return command[:i+len(".app")]
	}
	return ""
}
