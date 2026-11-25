package settings_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cwebley/trailmix/internal/settings"
)

func TestRecapAutoNameDefaultsAndConfigSet(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := settings.Read()
	if err != nil || !cfg.Recap.AutoName {
		t.Fatalf("default naming: %+v %v", cfg.Recap, err)
	}
	path, err := settings.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// Disabling automatic generation does not implicitly disable naming of an
	// explicitly requested recap. Old configurations gain the naming default.
	if err := os.WriteFile(path, []byte(`{"microphone":"existing","recap":{"enabled":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = settings.Read()
	if err != nil || !cfg.Recap.AutoName || cfg.Recap.Enabled {
		t.Fatalf("existing config: %+v %v", cfg.Recap, err)
	}
	for _, value := range []string{"false", "true", "false"} {
		if err := settings.Run([]string{"set", "recap.auto_name", value}); err != nil {
			t.Fatal(err)
		}
		cfg, err = settings.Read()
		if err != nil || cfg.Recap.AutoName != (value == "true") || cfg.Recap.Enabled || cfg.Microphone != "existing" {
			t.Fatalf("recap.auto_name=%s: %+v %v", value, cfg, err)
		}
	}
	for _, value := range []string{"", "1", "yes", "False", "null"} {
		if err := settings.Run([]string{"set", "recap.auto_name", value}); err == nil {
			t.Errorf("accepted recap.auto_name=%q", value)
		}
	}
}
