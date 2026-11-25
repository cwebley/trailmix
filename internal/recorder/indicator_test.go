package recorder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cwebley/trailmix/internal/transcription"
)

func TestTmuxStatusLabels(t *testing.T) {
	started := time.Date(2026, 9, 25, 16, 39, 0, 0, time.UTC)
	at := started.Local().Format("15:04")
	for _, c := range []struct {
		name string
		s    status
		want string
	}{
		{"idle", status{}, ""},
		{"stopped", status{Live: true, State: "stopped"}, ""},
		{"stale disk state", status{State: "recording"}, ""},
		{"persisted failure", status{State: "failed"}, "#[fg=yellow]!rec failed#[default]"},
		{"persisted interruption", status{State: "interrupted"}, "#[fg=yellow]!rec interrupted#[default]"},
		{"complete transcript with failed capture", status{State: "failed", Transcription: &transcription.Status{State: "complete"}}, "#[fg=yellow]!rec failed#[default]"},
		{"audio warning", status{Live: true, State: "recording", StartedAt: started, Warnings: []string{"microphone: no audio buffers received"}}, "#[fg=red]● " + at + " #[fg=yellow]!audio#[default]"},
		{"starting", status{Live: true, State: "starting"}, "#[fg=colour244]○ …#[default]"},
		{"recording", status{Live: true, State: "recording", StartedAt: started}, "#[fg=red]● " + at + "#[default]"},
		{"stopping", status{Live: true, State: "stopping", StartedAt: started}, "#[fg=red]● " + at + "#[default]"},
		{"degraded", status{Live: true, State: "recording", StartedAt: started, Transcription: &transcription.Status{State: "degraded"}}, "#[fg=red]● " + at + " #[fg=yellow]!txt#[default]"},
	} {
		if got := tmuxStatus(c.s); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTmuxFlagPersistsForCaptureFailureAndClearsForStop(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "commands")
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TRAILMIX_TMUX_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("TRAILMIX_TMUX_LOG", log)
	for _, state := range []string{"failed", "stopped"} {
		if err := tmuxListener(context.Background(), status{State: state}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 || lines[0] != "set-option -g @trailmix_rec 1" || lines[2] != "set-option -gu @trailmix_rec" {
		t.Fatalf("unexpected tmux flag updates: %s", data)
	}
}

func TestIndicatorPublishesHealthChangesWithoutCaptureTransition(t *testing.T) {
	updates := make(chan status, 3)
	i := newIndicator(func(_ context.Context, s status) error { updates <- s; return nil })
	defer i.close()
	s := status{State: "recording"}
	for _, change := range []func(){
		func() {},
		func() { s.Warnings = []string{"microphone: no audio buffers received"} },
		func() { s.Transcription = &transcription.Status{State: "degraded"} },
	} {
		change()
		i.publish(s)
		select {
		case <-updates:
		case <-time.After(time.Second):
			t.Fatal("health change was hidden by state-only deduplication")
		}
	}
}

func TestIndicatorDeliversTransitionsAndFinalState(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	busy, release := make(chan struct{}), make(chan struct{})
	i := newIndicator(func(ctx context.Context, s status) error {
		if s.State == "starting" {
			close(busy)
			<-release // A slow listener must not block publishers.
		}
		mu.Lock()
		seen = append(seen, s.State)
		mu.Unlock()
		return nil
	})
	i.publish(status{State: "starting"})
	<-busy
	done := make(chan struct{})
	go func() {
		for _, state := range []string{"recording", "recording", "stopping", "stopped"} {
			i.publish(status{State: state})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish blocked on a slow listener")
	}
	close(release)
	i.close()
	mu.Lock()
	defer mu.Unlock()
	// Intermediate states may coalesce; the first and final states must arrive.
	if len(seen) < 2 || seen[0] != "starting" || seen[len(seen)-1] != "stopped" {
		t.Fatalf("delivered %v", seen)
	}
}
