// Package settings stores workspace preferences separately from credentials.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/cwebley/trailmix/internal/cliout"
	"github.com/cwebley/trailmix/internal/meeting"
)

type Settings struct {
	NotesRoot  string    `json:"notes_root"`
	Template   string    `json:"template,omitempty"`
	Microphone string    `json:"microphone,omitempty"`
	Assistant  Assistant `json:"assistant"`
	Terminal   Terminal  `json:"terminal"`
}

// Terminal chooses how a meeting's tmux session reaches the screen when no
// tmux client is attached, and how its window is brought forward.
type Terminal struct {
	// Command launches a terminal attached to tmux. {tmux}, {target}, and {dir}
	// are replaced with the tmux executable, attach target, and note directory.
	Command []string `json:"command"`
	// Focus is auto, aerospace, macos, or none.
	Focus string `json:"focus"`
}

// DefaultTerminalCommand uses Terminal.app, which every Mac has.
var DefaultTerminalCommand = []string{
	"osascript",
	"-e", `tell application "Terminal" to do script "exec '{tmux}' attach-session -t '{target}'"`,
	"-e", `tell application "Terminal" to activate`,
}

func (t Terminal) Validate() error {
	if len(t.Command) == 0 || strings.TrimSpace(t.Command[0]) == "" {
		return errors.New("terminal.command must be a nonempty executable/argument array")
	}
	for _, arg := range t.Command {
		if strings.ContainsRune(arg, 0) {
			return errors.New("terminal.command contains a NUL byte")
		}
	}
	if program := t.Command[0]; strings.ContainsRune(program, filepath.Separator) && !filepath.IsAbs(program) {
		return errors.New("terminal.command executable must be an absolute path or a PATH command")
	}
	switch t.Focus {
	case "auto", "aerospace", "macos", "none":
		return nil
	}
	return errors.New("terminal.focus must be auto, aerospace, macos, or none")
}

type Assistant struct {
	Command        []string `json:"command"`
	Format         string   `json:"format"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

func (a Assistant) Validate() error {
	if len(a.Command) == 0 || strings.TrimSpace(a.Command[0]) == "" {
		return errors.New("assistant.command must be a nonempty executable/argument array")
	}
	for _, arg := range a.Command {
		if strings.ContainsRune(arg, 0) {
			return errors.New("assistant.command contains a NUL byte")
		}
	}
	if a.Format != "markdown" && a.Format != "opencode-json" {
		return errors.New("assistant.format must be markdown or opencode-json")
	}
	if a.TimeoutSeconds < 1 || a.TimeoutSeconds > 1800 {
		return errors.New("assistant.timeout_seconds must be between 1 and 1800")
	}
	return nil
}

func Path() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(home) {
			return "", errors.New("cannot locate an absolute home directory for settings")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "trailmix", "settings.json"), nil
}

func absolute(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	}
	return filepath.Abs(path)
}

func Read() (Settings, error) {
	path, err := Path()
	if err != nil {
		return Settings{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Settings{}, err
	}
	s := Settings{NotesRoot: filepath.Join(home, "Documents", "TrailMix"), Assistant: Assistant{
		Command: []string{"opencode", "run", "--format", "json", "--agent", "trailmix-ask"},
		Format:  "opencode-json", TimeoutSeconds: 180,
	}, Terminal: Terminal{Command: DefaultTerminalCommand, Focus: "auto"}}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	if err == nil {
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&s); err != nil {
			return s, fmt.Errorf("invalid settings in %s: %w", path, err)
		}
	}
	if !filepath.IsAbs(s.NotesRoot) || (s.Template != "" && !filepath.IsAbs(s.Template)) {
		return s, errors.New("settings paths must be absolute; use trailmix config set")
	}
	if err := s.Assistant.Validate(); err != nil {
		return s, err
	}
	if err := s.Terminal.Validate(); err != nil {
		return s, err
	}
	return s, nil
}

func Run(args []string) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if len(args) == 0 || (len(args) == 1 && args[0] == "show") {
		s, err := Read()
		if err != nil {
			return err
		}
		return cliout.JSON(struct {
			Path string `json:"path"`
			Settings
		}{path, s}, true)
	}
	if len(args) != 3 || args[0] != "set" {
		return errors.New("usage: trailmix config [show | set notes_root|template|microphone|assistant.command|assistant.format|assistant.timeout_seconds|terminal.command|terminal.focus VALUE]; commands are JSON arrays")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	s, err := Read()
	if err != nil {
		return err
	}
	value := args[2]
	switch args[1] {
	case "notes_root", "template":
		if value == "" && args[1] == "notes_root" {
			return errors.New("notes_root cannot be empty")
		}
		if value != "" {
			value, err = absolute(value)
			if err != nil {
				return err
			}
		}
		if args[1] == "notes_root" {
			s.NotesRoot = value
		} else {
			s.Template = value
		}
	case "microphone":
		s.Microphone = value
	case "assistant.command":
		if err := json.Unmarshal([]byte(value), &s.Assistant.Command); err != nil {
			return fmt.Errorf("assistant.command: %w", err)
		}
	case "assistant.format":
		s.Assistant.Format = value
	case "assistant.timeout_seconds":
		s.Assistant.TimeoutSeconds, err = strconv.Atoi(value)
		if err != nil {
			return err
		}
	case "terminal.command":
		if err := json.Unmarshal([]byte(value), &s.Terminal.Command); err != nil {
			return fmt.Errorf("terminal.command: %w", err)
		}
	case "terminal.focus":
		s.Terminal.Focus = value
	default:
		return fmt.Errorf("unknown setting %q", args[1])
	}
	if err := s.Assistant.Validate(); err != nil {
		return err
	}
	if err := s.Terminal.Validate(); err != nil {
		return err
	}
	return meeting.WriteJSON(path, s)
}
