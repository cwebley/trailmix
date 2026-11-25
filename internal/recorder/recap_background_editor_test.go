package recorder

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Queue with no editor, wait for a real detached CLI worker using a fake
// assistant, then open the note and let the plugin discover the saved result.
func TestBackgroundRecapEditorPickup(t *testing.T) {
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("Neovim unavailable")
	}
	p, note := recapTestFixture(t)
	t.Setenv("TRAILMIX_HOME", p.home)
	original := string(readTitleTest(t, note.Path))
	countPath := filepath.Join(p.home, "provider-count")
	recapTestRunner(t, p, recapTestResponse("Friday remains tentative. [[passage:system-1]]"), "printf 'request\\n' >> '"+countPath+"'", 5)
	cli := filepath.Join(p.home, "trailmix")
	build := exec.Command("go", "build", "-o", cli, "./cmd/trailmix")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	if out, err := exec.Command(cli, "recap", "--meeting", note.ID, "--enqueue").CombinedOutput(); err != nil {
		t.Fatalf("enqueue: %v %s", err, out)
	}
	deadline := time.Now().Add(15 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		out, err := exec.Command(cli, "recap", "--meeting", note.ID, "--status").CombinedOutput()
		if err != nil {
			t.Fatalf("status: %v %s", err, out)
		}
		var state struct {
			State string `json:"state"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(out, &state); err != nil {
			t.Fatal(err)
		}
		if state.State == "failed" {
			t.Fatal("background job failed: ", state.Error)
		}
		if state.State == "ready" {
			ready = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatal("detached worker did not finish")
	}
	if string(readTitleTest(t, note.Path)) != original {
		t.Fatal("background worker rewrote the note")
	}
	t.Setenv("TRAILMIX_CLI", cli)
	t.Setenv("TRAILMIX_TEST_NOTE", note.Path)
	script, _ := filepath.Abs("../../tests/nvim/recap_background_cli.lua")
	cmd := exec.Command(nvim, "--headless", "-u", "NONE", "--noplugin", "-n", "-i", "NONE", "-l", script)
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("background editor pickup: %v %s", err, out)
	}
	if string(readTitleTest(t, note.Path)) != original {
		t.Fatal("automatic insertion saved the main note")
	}
	if count := strings.Count(string(readTitleTest(t, countPath)), "request"); count != 1 {
		t.Fatalf("opening a note invoked the provider again: %d", count)
	}
}
