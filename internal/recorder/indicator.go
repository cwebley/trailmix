package recorder

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// A stateListener reacts to recorder state transitions, such as updating a
// status bar. Listeners are best effort: their failures never affect capture.
type stateListener func(ctx context.Context, s status) error

// indicator delivers state transitions to listeners on its own goroutine, so a
// slow or broken listener cannot delay capture. Only the latest state matters;
// pending updates are replaced rather than queued.
type indicator struct {
	listeners []stateListener
	mu        sync.Mutex
	last      string
	updates   chan status
	done      chan struct{}
}

func newIndicator(listeners ...stateListener) *indicator {
	i := &indicator{listeners: listeners, updates: make(chan status, 1), done: make(chan struct{})}
	go func() {
		defer close(i.done)
		for s := range i.updates {
			for _, listener := range i.listeners {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = listener(ctx, s)
				cancel()
			}
		}
	}()
	return i
}

func (i *indicator) publish(s status) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if s.State == i.last {
		return
	}
	i.last = s.State
	select {
	case <-i.updates: // Drop the stale pending update.
	default:
	}
	i.updates <- s
}

// close delivers the final state, waiting briefly so the recorder can exit.
func (i *indicator) close() {
	close(i.updates)
	select {
	case <-i.done:
	case <-time.After(3 * time.Second):
	}
}

func active(state string) bool {
	return state == "starting" || state == "recording" || state == "stopping"
}

// tmuxListener sets the @trailmix_rec user option while capture is active. A
// status line can show `#{?@trailmix_rec,#(trailmix status --tmux),}`, which
// costs nothing when idle. The recorder inherits TMUX from its launcher, so
// this targets the invoking server, or the default server from a hotkey.
func tmuxListener(ctx context.Context, s status) error {
	tmux, err := lookTmux()
	if err != nil {
		return nil // No tmux, no status line to update.
	}
	args := []string{"set-option", "-gu", "@trailmix_rec"}
	if active(s.State) {
		args = []string{"set-option", "-g", "@trailmix_rec", "1"}
	}
	if err := exec.CommandContext(ctx, tmux, args...).Run(); err != nil {
		return err
	}
	// Without -t, refresh-client redraws only one client; redraw them all.
	out, err := exec.CommandContext(ctx, tmux, "list-clients", "-F", "#{client_name}").Output()
	if err != nil {
		return err
	}
	for _, client := range strings.Fields(string(out)) {
		_ = exec.CommandContext(ctx, tmux, "refresh-client", "-S", "-t", client).Run()
	}
	return nil
}

// tmuxStatus renders a short tmux-styled label, or nothing when idle.
func tmuxStatus(s status) string {
	if !s.Live || !active(s.State) {
		return ""
	}
	if s.State == "starting" {
		return "#[fg=colour244]○ …#[default]"
	}
	label := "#[fg=red]● " + s.StartedAt.Local().Format("15:04")
	if s.Transcription != nil && s.Transcription.State == "degraded" {
		label += " #[fg=yellow]!txt"
	}
	return label + "#[default]"
}

// Hotkey launches may lack the interactive shell's PATH.
func lookTmux() (string, error) {
	if path, err := exec.LookPath("tmux"); err == nil {
		return path, nil
	}
	for _, path := range []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux"} {
		if _, err := exec.LookPath(path); err == nil {
			return path, nil
		}
	}
	return "", exec.ErrNotFound
}
