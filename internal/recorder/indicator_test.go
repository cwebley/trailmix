package recorder

import (
	"context"
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
