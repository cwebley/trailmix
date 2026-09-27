package recorder

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cwebley/trailmix/internal/settings"
)

func statuses(checks []check) map[string]string {
	result := map[string]string{}
	for _, c := range checks {
		result[c.Name] = c.Status
	}
	return result
}

func TestDoctorReportsProblemsWithoutPrompting(t *testing.T) {
	t.Setenv("TRAILMIX_TEST_HELPER", "normal")
	t.Setenv("TRAILMIX_TEST_MIC_PERMISSION", "denied")
	t.Setenv("TRAILMIX_TEST_SCREEN_PERMISSION", "false")
	p := testPaths(t)
	notes := filepath.Join(p.home, "notes")
	for _, args := range [][]string{
		{"set", "notes_root", notes},
		{"set", "template", filepath.Join(p.home, "missing-template.md")},
		{"set", "microphone", "unplugged-mic"},
		{"set", "terminal.command", `["trailmix-no-such-terminal"]`},
		{"set", "terminal.focus", "none"},
	} {
		if err := settings.Run(args); err != nil {
			t.Fatal(err)
		}
	}
	got := statuses(diagnose(p))
	for name, want := range map[string]string{
		"Capture helper":      "ok",
		"Microphone access":   "fail",
		"System audio access": "warn",
		"Settings":            "ok",
		"Notes folder":        "warn", // Created on first start.
		"Note template":       "fail",
		"Microphone":          "fail",
		"OpenAI key":          "fail", // Isolated config has no saved key.
		"Terminal":            "fail",
		"Focus":               "ok",
		"tmux indicator":      "warn", // Tests never reach the real tmux server.
	} {
		if got[name] != want {
			t.Errorf("%s: got %q, want %q (all: %v)", name, got[name], want, got)
		}
	}
	if err := doctor(p, nil); err == nil {
		t.Fatal("doctor succeeded despite failures")
	}
}

func TestDoctorStopsAtInvalidSettingsAndMissingHelper(t *testing.T) {
	p := testPaths(t)
	p.helper = filepath.Join(p.home, "TrailMixCapture.app", "Contents", "MacOS", "trailmix-capture")
	path, err := settings.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"unknown": true}`), 0600); err != nil {
		t.Fatal(err)
	}
	checks := diagnose(p)
	got := statuses(checks)
	if got["Capture helper"] != "fail" || got["Settings"] != "fail" {
		t.Fatalf("got %v", got)
	}
	if _, present := got["Microphone access"]; present {
		t.Fatal("permissions checked without a helper")
	}
	if last := checks[len(checks)-1]; last.Name != "Settings" {
		t.Fatalf("checks continued after invalid settings: %v", got)
	}
}

func TestAtLeast(t *testing.T) {
	for _, c := range []struct {
		version string
		minimum []int
		want    bool
	}{
		{"15.7.7", []int{15}, true},
		{"14.6", []int{15}, false},
		{"26.0", []int{15}, true},
		{"0.11.6", []int{0, 11}, true},
		{"0.10.4", []int{0, 11}, false},
		{"0.12.0-dev", []int{0, 11}, true},
	} {
		if got := atLeast(c.version, c.minimum...); got != c.want {
			t.Errorf("atLeast(%q, %v) = %v", c.version, c.minimum, got)
		}
	}
}
