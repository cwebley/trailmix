package settings_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cwebley/trailmix/internal/settings"
)

func TestRecapDefaultsAndConfigSet(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := settings.Read()
	if err != nil || !cfg.Recap.Enabled {
		t.Fatalf("default automatic recaps: %+v %v", cfg.Recap, err)
	}
	path, err := settings.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// Existing settings acquire the new default without a migration.
	if err := os.WriteFile(path, []byte(`{"microphone":"existing"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = settings.Read()
	if err != nil || !cfg.Recap.Enabled {
		t.Fatalf("existing config default: %+v %v", cfg, err)
	}
	for _, value := range []string{"false", "true", "false"} {
		if err := settings.Run([]string{"set", "recap.enabled", value}); err != nil {
			t.Fatal(err)
		}
		cfg, err = settings.Read()
		if err != nil || cfg.Recap.Enabled != (value == "true") || cfg.Microphone != "existing" {
			t.Fatalf("recap.enabled=%s: %+v %v", value, cfg, err)
		}
	}
	for _, value := range []string{"", "1", "yes", "False", "null"} {
		if err := settings.Run([]string{"set", "recap.enabled", value}); err == nil {
			t.Errorf("accepted recap.enabled=%q", value)
		}
	}
}
