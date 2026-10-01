package recorder

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cwebley/trailmix/internal/cliout"
)

// Machine-readable commands print one object led by the schema version the
// Neovim companion checks, whether or not the command then exits nonzero.
func TestJSONCommandsCarryTheSchemaVersion(t *testing.T) {
	p := testPaths(t)
	cli := filepath.Join(p.home, "trailmix")
	build := exec.Command("go", "build", "-o", cli, "./cmd/trailmix")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	t.Setenv("TRAILMIX_HOME", p.home)
	t.Setenv("TRAILMIX_CAPTURE_HELPER", p.helper)
	t.Setenv("TRAILMIX_TEST_HELPER", "normal")
	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(cli, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	for _, args := range [][]string{{"devices"}, {"config", "show"}, {"auth", "status"}, {"doctor", "--json"}} {
		stdout, stderr, _ := run(args...)
		decoder := json.NewDecoder(strings.NewReader(stdout))
		var document map[string]any
		if err := decoder.Decode(&document); err != nil || decoder.More() {
			t.Errorf("%v: not one JSON object: %v\nstdout: %s\nstderr: %s", args, err, stdout, stderr)
			continue
		}
		if document["schema_version"] != float64(cliout.SchemaVersion) || len(document) < 2 {
			t.Errorf("%v: missing schema version or content: %s", args, stdout)
		}
	}
	t.Setenv("TRAILMIX_TEST_HELPER", "devices-failure")
	if stdout, stderr, err := run("devices"); err == nil || stdout != "" || !strings.Contains(stderr, "no audio devices") {
		t.Fatalf("helper failure: %v\nstdout: %q\nstderr: %q", err, stdout, stderr)
	}
}
