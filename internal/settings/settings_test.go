package settings_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cwebley/trailmix/internal/settings"
)

func TestSettingsUseXDGAndNormalizeInvocationPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "relative")
	path, err := settings.Path()
	if err != nil || path != filepath.Join(home, ".config", "trailmix", "settings.json") {
		t.Fatalf("relative XDG fallback: %s %v", path, err)
	}
	cfg, err := settings.Read()
	if err != nil || cfg.NotesRoot != filepath.Join(home, "Documents", "TrailMix") || cfg.Microphone != "" {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := settings.Run([]string{"set", "notes_root", "relative notes"}); err != nil {
		t.Fatal(err)
	}
	if err := settings.Run([]string{"set", "template", "~/template.md"}); err != nil {
		t.Fatal(err)
	}
	if err := settings.Run([]string{"set", "microphone", "pinned"}); err != nil {
		t.Fatal(err)
	}
	cfg, err = settings.Read()
	cwd, _ := os.Getwd()
	if err != nil || cfg.NotesRoot != filepath.Join(cwd, "relative notes") || cfg.Template != filepath.Join(home, "template.md") || cfg.Microphone != "pinned" {
		t.Fatalf("paths/config: %+v %v", cfg, err)
	}
	if err := settings.Run([]string{"set", "microphone", ""}); err != nil {
		t.Fatal(err)
	}
	if err := settings.Run([]string{"set", "template", ""}); err != nil {
		t.Fatal(err)
	}
	cfg, err = settings.Read()
	if err != nil || cfg.Microphone != "" || cfg.Template != "" {
		t.Fatalf("clear: %+v %v", cfg, err)
	}
	if err := settings.Run([]string{"set", "notes_root", ""}); err == nil {
		t.Fatal("empty notes root accepted")
	}
	info, err := os.Stat(filepath.Join(xdg, "trailmix", "settings.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("settings permissions: %v %v", info, err)
	}
}

func TestTerminalValidation(t *testing.T) {
	valid := settings.Terminal{Command: settings.DefaultTerminalCommand, Focus: "auto"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]settings.Terminal{
		"empty command":  {Focus: "auto"},
		"relative path":  {Command: []string{"bin/alacritty"}, Focus: "auto"},
		"unknown focus":  {Command: []string{"alacritty"}, Focus: "yabai"},
		"NUL in command": {Command: []string{"alacritty", "a\x00b"}, Focus: "none"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, bad)
		}
	}
}
