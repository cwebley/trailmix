package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cwebley/trailmix/internal/assistant"
	"github.com/cwebley/trailmix/internal/auth"
	"github.com/cwebley/trailmix/internal/cliout"
	"github.com/cwebley/trailmix/internal/navigation"
	"github.com/cwebley/trailmix/internal/settings"
)

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, or fail
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func ok(name, detail string) check { return check{name, "ok", detail, ""} }
func warn(name, detail, fix string) check {
	return check{name, "warn", detail, fix}
}
func fail(name, detail, fix string) check {
	return check{name, "fail", detail, fix}
}

// output runs a short diagnostic command and returns trimmed stdout.
func output(program string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = captureEnvironment()
	data, err := cmd.Output()
	return strings.TrimSpace(string(data)), err
}

// atLeast compares dotted versions such as "15.7.7" against a minimum.
func atLeast(version string, minimum ...int) bool {
	parts := strings.Split(version, ".")
	for i, want := range minimum {
		got := 0
		if i < len(parts) {
			got, _ = strconv.Atoi(parts[i])
		}
		if got != want {
			return got > want
		}
	}
	return true
}

// diagnose inspects the installation without recording, prompting for
// permissions, or contacting a provider.
func diagnose(p paths) []check {
	var checks []check
	add := func(c check) { checks = append(checks, c) }

	if version, err := output("/usr/bin/sw_vers", "-productVersion"); err != nil {
		add(fail("macOS", err.Error(), ""))
	} else if !atLeast(version, 15) {
		add(fail("macOS", version, "TrailMix requires macOS 15 or newer"))
	} else {
		add(ok("macOS", version))
	}

	helperReady := false
	if info, err := os.Stat(p.helper); err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		add(fail("Capture helper", "missing at "+p.helper, "keep TrailMixCapture.app beside the trailmix executable; run make install"))
	} else {
		app := filepath.Dir(filepath.Dir(filepath.Dir(p.helper)))
		if _, err := output("/usr/bin/codesign", "--verify", app); err != nil && strings.HasSuffix(app, ".app") {
			add(fail("Capture helper", "signature invalid: "+app, "rebuild with make install"))
		} else {
			add(ok("Capture helper", app))
			helperReady = true
		}
	}

	if helperReady {
		text, err := output(p.helper, "permissions")
		var perms struct {
			Microphone    string `json:"microphone"`
			ScreenCapture bool   `json:"screen_capture"`
		}
		if err == nil {
			err = json.Unmarshal([]byte(text), &perms)
		}
		// macOS attributes permission to the app that launched TrailMix, so a
		// hotkey launch can differ from this terminal.
		const context = " (as launched from here)"
		switch {
		case err != nil:
			add(warn("Permissions", "could not read: "+err.Error(), ""))
		case perms.Microphone == "authorized":
			add(ok("Microphone access", "granted"+context))
		case perms.Microphone == "not_determined":
			add(warn("Microphone access", "not requested yet"+context, "macOS asks on the first recording"))
		default:
			add(fail("Microphone access", perms.Microphone+context, "enable it under System Settings > Privacy & Security > Microphone"))
		}
		if err == nil {
			if perms.ScreenCapture {
				add(ok("System audio access", "granted"+context))
			} else {
				add(warn("System audio access", "not granted"+context, "macOS asks on the first recording; if denied, enable it under System Settings > Privacy & Security > Screen & System Audio Recording"))
			}
		}
	}

	cfg, err := settings.Read()
	path, _ := settings.Path()
	if err != nil {
		add(fail("Settings", err.Error(), "fix or remove "+path))
		return checks // Remaining checks depend on valid settings.
	}
	add(ok("Settings", path))

	if info, err := os.Stat(cfg.NotesRoot); err == nil && info.IsDir() {
		add(ok("Notes folder", cfg.NotesRoot))
	} else if errors.Is(err, os.ErrNotExist) {
		add(warn("Notes folder", cfg.NotesRoot+" does not exist yet", "created on the first start, or choose another with trailmix config set notes_root"))
	} else {
		add(fail("Notes folder", cfg.NotesRoot+" is not a usable directory", "trailmix config set notes_root DIR"))
	}
	if cfg.Template != "" {
		if info, err := os.Stat(cfg.Template); err != nil || !info.Mode().IsRegular() {
			add(fail("Note template", cfg.Template+" is missing", "fix the file or run trailmix config set template ''"))
		} else {
			add(ok("Note template", cfg.Template))
		}
	}

	if cfg.Microphone == "" {
		add(ok("Microphone", "system default"))
	} else if !helperReady {
		add(warn("Microphone", "pinned to "+cfg.Microphone+"; cannot check without the capture helper", ""))
	} else if err := checkMicrophone(p, cfg.Microphone); err != nil {
		add(fail("Microphone", err.Error(), "connect it, or run trailmix config set microphone ''"))
	} else {
		add(ok("Microphone", "pinned to "+cfg.Microphone))
	}

	if key, err := auth.APIKey(); err != nil {
		add(fail("OpenAI key", err.Error(), "run trailmix auth"))
	} else if key == "" {
		add(fail("OpenAI key", "not saved; transcription would be unavailable", "run trailmix auth"))
	} else {
		add(ok("OpenAI key", "saved (not verified with OpenAI)"))
	}

	tmux, err := navigation.Find("tmux")
	if err != nil {
		add(fail("tmux", "not found", "brew install tmux"))
	} else {
		version, _ := output(tmux, "-V")
		add(ok("tmux", version))
	}
	if nvim, err := navigation.Find("nvim"); err != nil {
		add(fail("Neovim", "not found", "brew install neovim"))
	} else {
		version, _ := output(nvim, "--version")
		version, _, _ = strings.Cut(version, "\n")
		if !atLeast(strings.TrimPrefix(version, "NVIM v"), 0, 11) {
			add(fail("Neovim", version, "TrailMix requires Neovim 0.11 or newer"))
		} else {
			add(ok("Neovim", version))
		}
	}

	if program, err := navigation.Find(cfg.Terminal.Command[0]); err != nil {
		add(fail("Terminal", cfg.Terminal.Command[0]+" not found", "install it, or change terminal.command"))
	} else {
		add(ok("Terminal", program))
	}
	aerospace, aerospaceErr := navigation.Find("aerospace")
	switch cfg.Terminal.Focus {
	case "aerospace":
		if aerospaceErr != nil {
			add(fail("Focus", "aerospace not found", "install Aerospace or run trailmix config set terminal.focus auto"))
		} else {
			add(ok("Focus", "aerospace ("+aerospace+")"))
		}
	case "auto":
		if aerospaceErr == nil {
			add(ok("Focus", "auto: Aerospace, falling back to app activation"))
		} else {
			add(ok("Focus", "auto: app activation (Aerospace not installed)"))
		}
	default:
		add(ok("Focus", cfg.Terminal.Focus))
	}

	if program, err := assistant.Resolve(cfg.Assistant); err != nil {
		add(warn("Assistant", err.Error(), "needed only for ask; install it or change assistant.command"))
	} else {
		add(ok("Assistant", program))
	}

	if tmux != "" {
		left, errLeft := output(tmux, "show-options", "-gqv", "status-left")
		right, errRight := output(tmux, "show-options", "-gqv", "status-right")
		switch {
		case errLeft != nil || errRight != nil:
			add(warn("tmux indicator", "no tmux server running to check", "optional; see the README's recording indicator section"))
		case strings.Contains(left+right, "trailmix status --tmux"):
			add(ok("tmux indicator", "in status line"))
		default:
			add(warn("tmux indicator", "not in status line", "optional; see the README's recording indicator section"))
		}
	}
	return checks
}

func doctor(p paths, args []string) error {
	asJSON := len(args) == 1 && args[0] == "--json"
	if len(args) > 1 || (len(args) == 1 && !asJSON) {
		return errors.New("usage: trailmix doctor [--json]")
	}
	checks := diagnose(p)
	failed := 0
	for _, c := range checks {
		if c.Status == "fail" {
			failed++
		}
	}
	if asJSON {
		if err := cliout.JSON(struct {
			Checks []check `json:"checks"`
		}{checks}, true); err != nil {
			return err
		}
	} else {
		width := 0
		for _, c := range checks {
			width = max(width, len(c.Name))
		}
		marks := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}
		for _, c := range checks {
			fmt.Printf("%s %-*s  %s\n", marks[c.Status], width, c.Name, c.Detail)
			if c.Fix != "" {
				fmt.Printf("  %*s  → %s\n", width, "", c.Fix)
			}
		}
	}
	switch failed {
	case 0:
		return nil
	case 1:
		return errors.New("1 check failed")
	default:
		return fmt.Errorf("%d checks failed", failed)
	}
}
