package assistant

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cwebley/trailmix/internal/settings"
)

// A same-named agent in the user's OpenCode config deep-merges into ours, and
// its allowances can outrank our deny. Each request must select an agent name
// that no configuration can already define.
func TestOpenCodeSelectsAFreshlyNamedAgent(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "opencode")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$TRAILMIX_FAKE_DIR/args.$$"
printf '%s' "$OPENCODE_CONFIG_CONTENT" > "$TRAILMIX_FAKE_DIR/config.$$"
echo '{"type":"text","part":{"text":"answer"}}'
`
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRAILMIX_FAKE_DIR", dir)
	inherited := `{"agent":{"trailmix-ask":{"permission":{"*":"allow","bash":"allow"}}}}`
	t.Setenv("OPENCODE_CONFIG_CONTENT", inherited)
	cfg := settings.Assistant{Command: []string{fake, "run", "--format", "json", "--agent", "trailmix-ask"}, Format: "opencode-json", TimeoutSeconds: 10}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		result, err := Run(context.Background(), cfg, dir, "question")
		if err != nil || result.Text != "answer" {
			t.Fatalf("run: %v %q", err, result.Text)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "args.*"))
	if len(matches) != 2 {
		t.Fatalf("expected two recorded runs, got %d", len(matches))
	}
	for _, path := range matches {
		args, _ := os.ReadFile(path)
		lines := strings.Split(strings.TrimSpace(string(args)), "\n")
		agent := lines[len(lines)-1]
		if lines[len(lines)-2] != "--agent" || agent == "trailmix-ask" || !strings.HasPrefix(agent, "trailmix-ask-") {
			t.Fatalf("agent not renamed: %q", lines)
		}
		if seen[agent] {
			t.Fatalf("agent name reused: %s", agent)
		}
		seen[agent] = true
		data, _ := os.ReadFile(strings.Replace(path, "args.", "config.", 1))
		var config struct {
			Agent map[string]struct {
				Permission map[string]string `json:"permission"`
			} `json:"agent"`
			Share string `json:"share"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		if got := config.Agent[agent].Permission; len(got) != 1 || got["*"] != "deny" || config.Share != "disabled" {
			t.Fatalf("injected agent is not deny-all: %s", data)
		}
		// The user's own agent is passed through untouched, just not selected.
		if config.Agent["trailmix-ask"].Permission["bash"] != "allow" {
			t.Fatalf("inherited agent changed: %s", data)
		}
	}
}

// Checks the real merge against installed OpenCode without a model request.
func TestOpenCodeAgentHasNoToolsDespiteUserConfig(t *testing.T) {
	program, err := Resolve(settings.Assistant{Command: []string{"opencode"}})
	if err != nil {
		t.Skip("opencode is not installed")
	}
	home := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, "xdg", "opencode", "opencode.json"),
		`{"permission":{"*":"allow","bash":"allow","edit":"allow"},"agent":{"trailmix-ask":{"permission":{"*":"allow","bash":"allow","edit":"allow","read":"allow"}}}}`)
	for key, value := range map[string]string{"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, "xdg"), "XDG_DATA_HOME": filepath.Join(home, "data"),
		"XDG_STATE_HOME": filepath.Join(home, "state"), "XDG_CACHE_HOME": filepath.Join(home, "cache")} {
		t.Setenv(key, value)
	}
	t.Setenv("OPENCODE_CONFIG_CONTENT", "")
	args, env, err := openCodeCommand([]string{"--agent", "trailmix-ask"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(program, "debug", "agent", args[1])
	cmd.Dir, cmd.Env = home, env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("opencode debug agent: %v %s", err, out)
	}
	start := strings.IndexByte(string(out), '{')
	var agent struct {
		Name  string          `json:"name"`
		Tools map[string]bool `json:"tools"`
	}
	if start < 0 || json.Unmarshal(out[start:], &agent) != nil {
		t.Fatalf("unexpected debug output: %s", out)
	}
	if agent.Name != args[1] || len(agent.Tools) == 0 {
		t.Fatalf("injected agent not resolved: %s", out)
	}
	for tool, enabled := range agent.Tools {
		if enabled {
			t.Errorf("tool %s enabled", tool)
		}
	}
}
