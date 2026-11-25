package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Cross the real detached recap and name workers with a registered Neovim.
// All evidence and assistant responses are local fixtures. No live devices or
// real providers are used. The editor alone saves its previously unsaved text.
func TestAutomaticTitleAfterEditorExit(t *testing.T) {
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("Neovim unavailable")
	}
	p, before := recapTestFixture(t)
	t.Setenv("TRAILMIX_HOME", p.home)
	countPath := filepath.Join(p.home, "provider-count")
	recapTestRunner(t, p, recapTestResponse("Friday remains tentative. [[passage:system-1]]"), "printf 'request\\n' >> '"+countPath+"'", 5)
	autoTitleTestEnable(t)
	cli := filepath.Join(p.home, "trailmix")
	build := exec.Command("go", "build", "-o", cli, "./cmd/trailmix")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	answers := []byte(artifactMarker(before.ID, "answers") + "\nSaved answer.\n")
	if err := os.WriteFile(sidecarPath(before, "answers"), answers, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRAILMIX_CLI", cli)
	t.Setenv("TRAILMIX_TEST_NOTE", before.Path)
	editorReady := filepath.Join(p.home, "editor-ready")
	savedNote := filepath.Join(p.home, "saved-note")
	t.Setenv("TRAILMIX_TEST_EDITOR_READY", editorReady)
	t.Setenv("TRAILMIX_TEST_TITLE_INTENT", autoTitlePath(before))
	t.Setenv("TRAILMIX_TEST_SAVED_NOTE", savedNote)
	script, _ := filepath.Abs("../../tests/nvim/auto_title_cli.lua")
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nvim, "--headless", "-u", "NONE", "--noplugin", "-n", "-i", "NONE", "-l", script)
	cmd.Dir = "../.."
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(editorReady); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("editor did not register: %s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out, err := exec.Command(cli, "recap", "--meeting", before.ID, "--enqueue").CombinedOutput(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("enqueue: %v %s", err, out)
	} else {
		t.Logf("enqueue: %s", out)
	}
	if err := cmd.Wait(); err != nil {
		for _, file := range []string{"job.json", "title.json", "worker.log"} {
			data, _ := os.ReadFile(filepath.Join(before.Directory, "recaps", file))
			t.Logf("%s: %s", file, data)
		}
		current, _ := readMeeting(p, before.ID)
		t.Fatalf("editor workflow: %v %s; associated note: %+v", err, output.String(), current)
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		intent, err := readAutoTitle(before)
		if err == nil && intent.State == "applied" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached close retry did not apply title: %+v %v", intent, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	after, err := readMeeting(p, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != "Friday planning" || filepath.Base(after.Path) != "09-07-friday-planning.md" || after.ID != before.ID {
		t.Fatalf("unexpected automatically named association: %+v", after)
	}
	want := titleHeading(readTitleTest(t, savedNote), after.Title)
	if got := readTitleTest(t, after.Path); !bytes.Equal(got, want) {
		t.Fatalf("naming changed saved rough notes or recap:\ngot: %s\nwant: %s", got, want)
	}
	if !bytes.Equal(readTitleTest(t, sidecarPath(after, "answers")), answers) {
		t.Fatal("naming changed answer evidence")
	}
	assertTitleAbsent(t, before.Path)
	assertTitleAbsent(t, sidecarPath(before, "answers"))
	if count := strings.Count(string(readTitleTest(t, countPath)), "request"); count != 1 {
		t.Fatalf("naming or editor closure invoked the provider: %d", count)
	}
	latest, err := latestRecap(context.Background(), p, before.ID)
	if err != nil || latest.NotePath != after.Path || !strings.Contains(latest.NotesSnapshot, "Unsaved detail before the recap request.") {
		t.Fatalf("recap lost current association or live snapshot: %+v %v", latest, err)
	}
	var stored meetingNote
	data := readTitleTest(t, filepath.Join(p.home, "latest-meeting.json"))
	if err := json.Unmarshal(data, &stored); err != nil || stored.Path != after.Path {
		t.Fatalf("latest association was not renamed: %+v %v", stored, err)
	}
	if !strings.Contains(latest.Markdown, "trailmix://meetings/"+before.ID+"/passages/system-1") {
		t.Fatal("automatic naming changed stable citation identity")
	}
}
