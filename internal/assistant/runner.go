// Package assistant runs configurable headless commands against supplied context.
// It has no access to recorder lifecycle or the user's note-writing operations.
package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cwebley/trailmix/internal/settings"
)

const MaxContextBytes = 2 * 1024 * 1024
const maxOutputBytes = 4 * 1024 * 1024

type Result struct {
	Text        string `json:"text"`
	Diagnostics string `json:"diagnostics,omitempty"`
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > maxOutputBytes-b.buffer.Len() {
		b.overflow = true
		b.cancel()
		return 0, errors.New("assistant output exceeded 4 MiB")
	}
	return b.buffer.Write(p)
}

// Resolve finds the assistant executable the way Run will launch it.
func Resolve(cfg settings.Assistant) (string, error) {
	program := cfg.Command[0]
	if strings.ContainsRune(program, filepath.Separator) && !filepath.IsAbs(program) {
		return "", errors.New("assistant executable must be an absolute path or a PATH command")
	}
	if path, err := exec.LookPath(program); err == nil {
		return path, nil
	}
	// tmux and desktop launches may not inherit the interactive shell's PATH.
	if program == "opencode" {
		home, _ := os.UserHomeDir()
		for _, candidate := range []string{filepath.Join(home, ".opencode", "bin", "opencode"), "/opt/homebrew/bin/opencode", "/usr/local/bin/opencode"} {
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("assistant executable %q not found", program)
}

func Run(ctx context.Context, cfg settings.Assistant, directory, prompt string) (Result, error) {
	var result Result
	if err := cfg.Validate(); err != nil {
		return result, err
	}
	if len(prompt) > MaxContextBytes {
		return result, errors.New("meeting context exceeds 2 MiB; no request was sent")
	}
	program, err := Resolve(cfg)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, cfg.Command[1:]...)
	cmd.Dir = directory
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = os.Environ()
	if cfg.Format == "opencode-json" {
		env, err := openCodeEnvironment()
		if err != nil {
			return result, err
		}
		cmd.Env = env
	}
	// Cancel the whole subprocess group, including wrapper scripts and children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	stdout, stderr := boundedBuffer{cancel: cancel}, boundedBuffer{cancel: cancel}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	// A wrapper may exit before its children close inherited pipes. WaitDelay
	// bounds our wait, but does not terminate those remaining group members.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	result.Text, result.Diagnostics = stdout.buffer.String(), stderr.buffer.String()
	if stdout.overflow || stderr.overflow {
		return result, errors.New("assistant output exceeded 4 MiB")
	}
	if ctx.Err() != nil {
		return result, fmt.Errorf("assistant request: %w", ctx.Err())
	}
	if err != nil {
		return result, fmt.Errorf("assistant command failed: %w", err)
	}
	if cfg.Format == "opencode-json" {
		text, err := decodeOpenCode(stdout.buffer.Bytes())
		if err != nil {
			return result, err
		}
		result.Text = text
	}
	result.Text = strings.TrimSpace(result.Text)
	if result.Text == "" {
		return result, errors.New("assistant returned an empty answer")
	}
	return result, nil
}

func openCodeEnvironment() ([]string, error) {
	// A per-process overlay keeps provider/auth settings, without installing or
	// modifying any of the user's OpenCode files. All context arrives on stdin.
	config := map[string]any{}
	if prior := os.Getenv("OPENCODE_CONFIG_CONTENT"); prior != "" {
		if err := json.Unmarshal([]byte(prior), &config); err != nil || config == nil {
			return nil, errors.New("invalid inherited OPENCODE_CONFIG_CONTENT")
		}
	}
	agents, _ := config["agent"].(map[string]any)
	if agents == nil {
		agents = map[string]any{}
	}
	agents["trailmix-ask"] = map[string]any{
		"description": "Answer from the supplied meeting context",
		"mode":        "primary", "permission": map[string]string{"*": "deny"},
		"prompt": "Answer the supplied meeting question in Markdown. Use only the supplied context; do not use tools. Follow the requested citation format.",
	}
	config["agent"], config["share"] = agents, "disabled"
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	var env []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "OPENCODE_CONFIG_CONTENT=") && !strings.HasPrefix(value, "OPENCODE_DISABLE_PROJECT_CONFIG=") {
			env = append(env, value)
		}
	}
	return append(env, "OPENCODE_CONFIG_CONTENT="+string(encoded), "OPENCODE_DISABLE_PROJECT_CONFIG=1"), nil
}

func decodeOpenCode(data []byte) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxOutputBytes)
	var parts []string
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Part struct {
				Text      string `json:"text"`
				Synthetic bool   `json:"synthetic"`
			} `json:"part"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return "", errors.New("invalid OpenCode JSON event output")
		}
		if event.Type == "error" {
			return "", errors.New("OpenCode reported an error; see saved runner output")
		}
		if event.Type == "text" && !event.Part.Synthetic {
			parts = append(parts, event.Part.Text)
		}
	}
	return strings.Join(parts, "\n\n"), scanner.Err()
}
