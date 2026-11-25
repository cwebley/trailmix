package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cwebley/trailmix/internal/meeting"
	"github.com/cwebley/trailmix/internal/navigation"
	"github.com/cwebley/trailmix/internal/settings"
)

func autoTitleTestNavigation(t *testing.T) {
	t.Helper()
	check, retitle, start := autoTitleCheck, autoTitleRetitle, startTitleWorker
	autoTitleCheck, autoTitleRetitle = noTitleEditors, noTitlePanes
	startTitleWorker = func(paths, string) error { return nil }
	t.Cleanup(func() { autoTitleCheck, autoTitleRetitle, startTitleWorker = check, retitle, start })
}

func autoTitleTestEnable(t *testing.T) {
	t.Helper()
	cfg, err := settings.Read()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Recap.AutoName = true
	path, err := settings.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := meeting.WriteJSON(path, cfg); err != nil {
		t.Fatal(err)
	}
}

func autoTitleTestView(note meetingNote, title string) recapView {
	id := meeting.NewID()
	return recapView{MeetingID: note.ID, RecapID: id, SuggestedTitle: title, NotePath: note.Path, Path: filepath.Join(note.Directory, "recaps", id, "result.json")}
}

func autoTitleTestRead(t *testing.T, note meetingNote) autoTitleIntent {
	t.Helper()
	intent, err := readAutoTitle(note)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func TestAutoTitleEligibility(t *testing.T) {
	_, note := titleFixture(t)
	original := string(readTitleTest(t, note.Path))
	for _, tt := range []struct {
		name string
		edit func(*meetingNote, string) string
		want bool
	}{
		{"default", func(_ *meetingNote, s string) string { return s }, true},
		{"collision suffix", func(n *meetingNote, s string) string { n.Path = strings.TrimSuffix(n.Path, ".md") + "-12.md"; return s }, true},
		{"stored title", func(n *meetingNote, s string) string { n.Title = "Custom"; return s }, false},
		{"named basename", func(n *meetingNote, s string) string {
			n.Path = filepath.Join(filepath.Dir(n.Path), "09-07-my-name.md")
			return s
		}, false},
		{"custom heading", func(_ *meetingNote, s string) string {
			return strings.Replace(s, "# Meeting 2026-11-18 09:07:43", "# Planning", 1)
		}, false},
		{"changed time", func(_ *meetingNote, s string) string { return strings.Replace(s, "09:07:43", "09:07:44", 1) }, false},
		{"heading whitespace", func(_ *meetingNote, s string) string { return strings.Replace(s, "# Meeting", " # Meeting", 1) }, false},
		{"two headings", func(_ *meetingNote, s string) string { return s + "\n# Custom\n" }, false},
		{"setext heading", func(_ *meetingNote, s string) string { return s + "\nCustom\n======\n" }, false},
		{"html heading", func(_ *meetingNote, s string) string { return s + "\n<h1>Custom</h1>\n" }, false},
		{"fenced headings ignored", func(_ *meetingNote, s string) string { return s + "\n```md\n# Custom\n```\n~~~\n# Also literal\n~~~\n" }, true},
		{"only fenced default", func(_ *meetingNote, s string) string { return "```\n" + s + "```\n" }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := note
			data := tt.edit(&n, original)
			if got := autoTitleEligible(n, []byte(data)); got != tt.want {
				t.Fatalf("eligible=%v want %v", got, tt.want)
			}
		})
	}
}

func TestAutoTitleFirstSuggestionSurvivesPendingAndRegeneration(t *testing.T) {
	p, note := titleFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	refuse := true
	autoTitleCheck = func(string, navigation.Target) error {
		if refuse {
			return errors.New("editor is open")
		}
		return nil
	}
	starts := 0
	startTitleWorker = func(paths, string) error { starts++; return nil }
	first := autoTitleTestView(note, "  Project   planning  ")
	scheduleRecapTitle(p, first)
	pending := autoTitleTestRead(t, note)
	if pending.State != "pending" || pending.Title != "Project planning" || pending.RecapID != first.RecapID || starts != 1 {
		t.Fatalf("pending: %+v, starts %d", pending, starts)
	}
	if _, err := os.Stat(note.Path); err != nil {
		t.Fatal("renamed with editor open", err)
	}
	scheduleRecapTitle(p, autoTitleTestView(note, "A later suggestion"))
	pending = autoTitleTestRead(t, note)
	if pending.Title != "Project planning" || pending.RecapID != first.RecapID {
		t.Fatalf("replaced first choice: %+v", pending)
	}
	refuse = false
	intent, err := retryPendingTitle(context.Background(), p, note.ID)
	if err != nil || intent.State != "applied" {
		t.Fatalf("retry: %+v %v", intent, err)
	}
	after, err := readMeeting(p, note.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != "Project planning" || filepath.Base(after.Path) != "09-07-project-planning.md" {
		t.Fatalf("after: %+v", after)
	}
	if !strings.Contains(string(readTitleTest(t, after.Path)), "# Project planning\n") {
		t.Fatal("heading not renamed")
	}
	scheduleRecapTitle(p, autoTitleTestView(after, "Do not replace this"))
	stable, _ := readMeeting(p, note.ID)
	if stable.Path != after.Path || stable.Title != after.Title {
		t.Fatalf("regeneration renamed: %+v", stable)
	}
}

func TestAutoTitleGuardRechecksManualEditsAfterSelection(t *testing.T) {
	for _, change := range []string{"heading", "title", "basename"} {
		t.Run(change, func(t *testing.T) {
			p, note := titleFixture(t)
			autoTitleTestNavigation(t)
			autoTitleTestEnable(t)
			_, err := selectRecapTitle(context.Background(), p, autoTitleTestView(note, "Automatic name"))
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "heading":
				data := strings.Replace(string(readTitleTest(t, note.Path)), "# Meeting 2026-11-18 09:07:43", "# My custom heading", 1)
				if err := os.WriteFile(note.Path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			case "title":
				note.Title = "Manual metadata title"
				if err := saveMeeting(p, note); err != nil {
					t.Fatal(err)
				}
			case "basename":
				path := filepath.Join(filepath.Dir(note.Path), "manually-named.md")
				if err := os.Rename(note.Path, path); err != nil {
					t.Fatal(err)
				}
				note.Path = path
				if err := saveMeeting(p, note); err != nil {
					t.Fatal(err)
				}
			}
			before := readTitleTest(t, note.Path)
			intent, err := retryPendingTitle(context.Background(), p, note.ID)
			if err != nil || intent.State != "skipped" {
				t.Fatalf("intent: %+v %v", intent, err)
			}
			if string(readTitleTest(t, note.Path)) != string(before) {
				t.Fatal("modified custom note")
			}
			current, _ := readMeeting(p, note.ID)
			if current.Path != note.Path || current.Title != note.Title {
				t.Fatal("modified manual association")
			}
		})
	}
}

func TestAutoTitleRecordedCrashSwapBlocksUntilRemoved(t *testing.T) {
	p, note := titleFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	intent, err := selectRecapTitle(context.Background(), p, autoTitleTestView(note, "Planning"))
	if err != nil {
		t.Fatal(err)
	}
	// A swap may be outside the note directory and its editor registration may
	// already be gone. Retain its exact path, not just currently live editors.
	swap := filepath.Join(p.home, "global-swaps", "meeting.swp")
	recapTestWrite(t, swap, "unsaved recovery bytes", 0600)
	intent.SwapPaths = []string{swap}
	if err := meeting.WriteJSON(autoTitlePath(note), intent); err != nil {
		t.Fatal(err)
	}
	intent, err = retryPendingTitle(context.Background(), p, note.ID)
	if err != nil || intent.State != "pending" || !strings.Contains(intent.Error, "swap") {
		t.Fatalf("swap refusal: %+v %v", intent, err)
	}
	if string(readTitleTest(t, swap)) != "unsaved recovery bytes" {
		t.Fatal("modified recovery swap")
	}
	if err := os.Remove(swap); err != nil {
		t.Fatal(err)
	}
	intent, err = retryPendingTitle(context.Background(), p, note.ID)
	if err != nil || intent.State != "applied" {
		t.Fatalf("after closure: %+v %v", intent, err)
	}
}

func TestAutoTitleUsesExistingCollisionAndRollbackTransaction(t *testing.T) {
	p, note := titleFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	_, err := selectRecapTitle(context.Background(), p, autoTitleTestView(note, "Planning"))
	if err != nil {
		t.Fatal(err)
	}
	collision := filepath.Join(filepath.Dir(note.Path), "09-07-planning.answers.md")
	recapTestWrite(t, collision, "not this meeting", 0600)
	navigationCalls := 0
	autoTitleRetitle = func(string, navigation.Target, navigation.Target) error {
		navigationCalls++
		if navigationCalls == 1 {
			return errors.New("injected navigation failure")
		}
		return nil
	}
	intent, err := retryPendingTitle(context.Background(), p, note.ID)
	if err != nil || intent.State != "pending" {
		t.Fatalf("failed rename: %+v %v", intent, err)
	}
	if _, err := os.Stat(note.Path); err != nil {
		t.Fatal("original not restored", err)
	}
	assertTitleAbsent(t, filepath.Join(filepath.Dir(note.Path), "09-07-planning-2.md"))
	current, _ := readMeeting(p, note.ID)
	if current.Path != note.Path || current.Title != "" {
		t.Fatalf("association not rolled back: %+v", current)
	}
	autoTitleRetitle = noTitlePanes
	intent, err = retryPendingTitle(context.Background(), p, note.ID)
	if err != nil || intent.State != "applied" {
		t.Fatalf("retry: %+v %v", intent, err)
	}
	current, _ = readMeeting(p, note.ID)
	if filepath.Base(current.Path) != "09-07-planning-2.md" {
		t.Fatalf("collision ignored: %s", current.Path)
	}
	if string(readTitleTest(t, collision)) != "not this meeting" {
		t.Fatal("collision overwritten")
	}
}

func TestAutoTitleCaptureBusyRetainsIntent(t *testing.T) {
	p, note := titleFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	_, err := selectRecapTitle(context.Background(), p, autoTitleTestView(note, "Planning"))
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := lock(p)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := retryPendingTitle(context.Background(), p, note.ID)
	recorder.Close()
	if err != nil || intent.State != "pending" || intent.Error == "" {
		t.Fatalf("capture busy: %+v %v", intent, err)
	}
	intent, err = retryPendingTitle(context.Background(), p, note.ID)
	if err != nil || intent.State != "applied" {
		t.Fatalf("idle retry: %+v %v", intent, err)
	}
}

func TestAutoTitleReadOnlyAndNoIntentWorkerNeverSelect(t *testing.T) {
	p, note := recapTestFixture(t)
	autoTitleTestNavigation(t)
	view, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false)
	if err != nil {
		t.Fatal(err)
	}
	autoTitleTestEnable(t)
	for _, flag := range []string{"--status", "--latest"} {
		if err := runRecap(p, []string{"--meeting", note.ID, flag}); err != nil {
			t.Fatal(err)
		}
		assertTitleAbsent(t, autoTitlePath(note))
	}
	if err := runTitleWorker(p, []string{"--meeting", note.ID}); err != nil {
		t.Fatal(err)
	}
	assertTitleAbsent(t, autoTitlePath(note))
	if recapRequestCount(t, note) != 1 {
		t.Fatal("read-only calls made a provider request")
	}
	// --enqueue is an explicit successful generation intent, including when it
	// reuses a cached latest. Unlike --latest, it schedules the saved title.
	if err := enqueueRecap(p, note.ID); err != nil {
		t.Fatal(err)
	}
	intent := autoTitleTestRead(t, note)
	if intent.State != "applied" || intent.RecapID != view.RecapID {
		t.Fatalf("cached enqueue: %+v", intent)
	}
	if recapRequestCount(t, note) != 1 {
		t.Fatal("cached enqueue called provider")
	}
}

func TestAutoTitlePublicGenerationAndDisabledSetting(t *testing.T) {
	p, note := recapTestFixture(t)
	autoTitleTestNavigation(t)
	if err := runRecap(p, []string{"--meeting", note.ID}); err != nil {
		t.Fatal(err)
	}
	assertTitleAbsent(t, autoTitlePath(note))
	autoTitleTestEnable(t)
	if err := runRecap(p, []string{"--meeting", note.ID}); err != nil {
		t.Fatal(err)
	}
	intent := autoTitleTestRead(t, note)
	if intent.State != "applied" || intent.Title != "Friday planning" {
		t.Fatalf("public generation: %+v", intent)
	}
}

func TestAutoTitlePublicStdinRetainsUnregisteredEditorPID(t *testing.T) {
	p, note := recapTestFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	notes := string(readTitleTest(t, note.Path)) + "Unsaved caller notes.\n"
	data, _ := json.Marshal(map[string]any{"notes": notes, "note_path": note.Path, "editor_pid": os.Getpid()})
	path := filepath.Join(p.home, "stdin.json")
	recapTestWrite(t, path, string(data), 0600)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	stdin := os.Stdin
	os.Stdin = file
	defer func() { os.Stdin = stdin }()
	if err := runRecap(p, []string{"--meeting", note.ID, "--stdin"}); err != nil {
		t.Fatal(err)
	}
	intent := autoTitleTestRead(t, note)
	if intent.State != "pending" || len(intent.EditorPIDs) != 1 || intent.EditorPIDs[0] != os.Getpid() {
		t.Fatalf("source editor not retained: %+v", intent)
	}
	if strings.Contains(string(readTitleTest(t, note.Path)), "Unsaved caller notes") {
		t.Fatal("autosaved caller buffer")
	}
}

func TestAutoTitleBackgroundSuccessSchedulesAfterLocksRelease(t *testing.T) {
	p, note := recapTestFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	job := recapJob{MeetingID: note.ID, State: "queued"}
	if err := saveRecapJob(note, &job); err != nil {
		t.Fatal(err)
	}
	if err := processRecapJob(context.Background(), p, note.ID); err != nil {
		t.Fatal(err)
	}
	if intent := autoTitleTestRead(t, note); intent.State != "applied" {
		t.Fatalf("background title: %+v", intent)
	}
	if recapRequestCount(t, note) != 1 {
		t.Fatal("unexpected provider calls")
	}
}

func TestAutoTitleUnregisteredSourceEditorPIDAndClosureWorker(t *testing.T) {
	p, note := titleFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	child := exec.Command("sh", "-c", "read line")
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); _ = child.Wait() })
	first := autoTitleTestView(note, "First choice")
	intent, err := selectRecapTitle(context.Background(), p, first, child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	intent, err = retryPendingTitle(context.Background(), p, note.ID)
	if err != nil || intent.State != "pending" || !strings.Contains(intent.Error, "source editor process") {
		t.Fatalf("live unregistered editor: %+v %v", intent, err)
	}
	// A later recap adds another caller blocker without replacing the title.
	intent, err = selectRecapTitle(context.Background(), p, autoTitleTestView(note, "Not selected"), os.Getpid())
	if err != nil || len(intent.EditorPIDs) != 2 || intent.RecapID != first.RecapID {
		t.Fatalf("merged callers: %+v %v", intent, err)
	}
	// Keep only the child for the closure retry; this test process stays live.
	intent.EditorPIDs = []int{child.Process.Pid}
	if err := meeting.WriteJSON(autoTitlePath(note), intent); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	_ = child.Wait()
	done := make(chan error, 1)
	go func() { done <- runTitleWorker(p, []string{"--meeting", note.ID}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("closure retry did not finish")
	}
	if intent := autoTitleTestRead(t, note); intent.State != "applied" {
		t.Fatalf("after editor exit: %+v", intent)
	}
}

func TestRecapRequestOptionalEditorPID(t *testing.T) {
	for _, pid := range []string{"null", "1", "0", "-1", "2.5", `"42"`, "2147483648"} {
		if _, err := recapRequest([]byte(`{"notes":"n","note_path":"p","editor_pid":` + pid + `}`)); err == nil {
			t.Fatalf("accepted PID %s", pid)
		}
	}
	for _, input := range []string{
		`{"notes":"n","note_path":"p","editor_pid":42,"editor_pid":43}`,
		`{"notes":"n","note_path":"p","extra":42}`,
		`{"notes":null,"note_path":"p"}`,
		`{"notes":"n","note_path":"p"} {}`,
	} {
		if _, err := recapRequest([]byte(input)); err == nil {
			t.Fatalf("accepted malformed request %s", input)
		}
	}
	input, err := recapRequest([]byte(`{"notes":"n","note_path":"p","editor_pid":42}`))
	if err != nil || input.EditorPID != 42 || *input.Notes != "n" || input.NotePath != "p" {
		t.Fatalf("valid request: %+v %v", input, err)
	}
	input, err = recapRequest([]byte(`{"notes":"n","note_path":"p"}`))
	if err != nil || input.EditorPID != 0 {
		t.Fatalf("shell request: %+v %v", input, err)
	}
}

func TestAutoTitlePendingCLIUsesHeldLifecycleAndWorkerSerializes(t *testing.T) {
	p, note := titleFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	_, err := selectRecapTitle(context.Background(), p, autoTitleTestView(note, "Planning"))
	if err != nil {
		t.Fatal(err)
	}
	guard, err := titleWorkerLock(p, note.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retryPendingTitle(context.Background(), p, note.ID); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("missing worker exclusion: %v", err)
	}
	guard.Close()
	lifecycle, err := lifecycleLock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Close()
	started := time.Now()
	if err := titleCommand(p, []string{"--meeting", note.ID, "--pending"}); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("pending title reacquired lifecycle lock")
	}
	intent := autoTitleTestRead(t, note)
	if intent.State != "applied" {
		t.Fatalf("pending CLI: %+v", intent)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(readTitleTest(t, autoTitlePath(note)), &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"meeting_id", "recap_id", "title", "state"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing saved field %s", key)
		}
	}
}
