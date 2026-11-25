package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cwebley/trailmix/internal/meeting"
	"github.com/cwebley/trailmix/internal/navigation"
	"github.com/cwebley/trailmix/internal/settings"
)

func TestAutoTitleRejectsMetadataInSuggestedTitle(t *testing.T) {
	p, note := titleFixture(t)
	before := string(readTitleTest(t, note.Path))
	for _, title := range []string{
		"Planning <!-- trailmix: meeting " + note.ID + " -->",
		"Planning <!-- trailmix: recap begin -->",
		"Planning -->",
		"Planning TRAILMIX: meeting",
	} {
		if _, err := selectRecapTitle(context.Background(), p, autoTitleTestView(note, title)); err == nil {
			t.Fatalf("accepted metadata title %q", title)
		}
		assertTitleAbsent(t, autoTitlePath(note))
		if string(readTitleTest(t, note.Path)) != before {
			t.Fatal("malicious suggestion modified note")
		}
		var response map[string]string
		if err := json.Unmarshal([]byte(recapTestResponse("Summary.")), &response); err != nil {
			t.Fatal(err)
		}
		response["suggested_title"] = title
		data, _ := json.Marshal(response)
		if _, _, err := renderRecap(string(data), transcriptSnapshot{}); err == nil {
			t.Fatalf("render accepted metadata title %q", title)
		}
	}
}

func TestAutoTitleSelectedDuringFirstPublicationBeforeOutsideHook(t *testing.T) {
	p, note := recapTestFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	first, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false)
	if err != nil {
		t.Fatal(err)
	}
	intent := autoTitleTestRead(t, note)
	if intent.State != "pending" || intent.RecapID != first.RecapID {
		t.Fatalf("first publication did not select: %+v", intent)
	}
	// No outside hook ran for the first request. A second successful recap
	// cannot win merely because its caller schedules application sooner.
	second, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false)
	if err != nil {
		t.Fatal(err)
	}
	scheduleRecapTitle(p, second)
	intent = autoTitleTestRead(t, note)
	if intent.State != "applied" || intent.RecapID != first.RecapID {
		t.Fatalf("later publication replaced first: %+v", intent)
	}
}

func TestAutoTitleCarriesPreProviderSwapEvidence(t *testing.T) {
	p, note := recapTestFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	swap := filepath.Join(p.home, "outside-swaps", "recovery.swp")
	recapTestWrite(t, swap, "unsaved recovery", 0600)
	view, err := generateRecap(context.Background(), p, note.ID, recapInput{SwapPaths: []string{swap}}, false)
	if err != nil {
		t.Fatal(err)
	}
	var saved recapInput
	if err := json.Unmarshal(readTitleTest(t, filepath.Join(filepath.Dir(view.Path), "inputs.json")), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.SwapPaths) != 1 || saved.SwapPaths[0] != swap {
		t.Fatalf("lost private pre-provider evidence: %+v", saved)
	}
	intent := autoTitleTestRead(t, note)
	if len(intent.SwapPaths) != 1 || intent.SwapPaths[0] != swap {
		t.Fatalf("lost selected recovery path: %+v", intent)
	}
	intent, err = retryPendingTitle(context.Background(), p, note.ID)
	if err != nil || intent.State != "pending" || !strings.Contains(intent.Error, "swap") {
		t.Fatalf("crash recovery was not protected: %+v %v", intent, err)
	}
}

func TestAutoTitleSettingDisabledRetainsPendingForLaterOptIn(t *testing.T) {
	p, note := titleFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	if _, err := selectRecapTitle(context.Background(), p, autoTitleTestView(note, "Planning")); err != nil {
		t.Fatal(err)
	}
	if err := settings.Run([]string{"set", "recap.auto_name", "false"}); err != nil {
		t.Fatal(err)
	}
	intent, err := retryPendingTitle(context.Background(), p, note.ID)
	if err != nil || intent.State != "pending" || !strings.Contains(intent.Error, "disabled") {
		t.Fatalf("opt-out: %+v %v", intent, err)
	}
	if _, err := os.Stat(note.Path); err != nil {
		t.Fatal("opt-out renamed note", err)
	}
	autoTitleTestEnable(t)
	intent, err = retryPendingTitle(context.Background(), p, note.ID)
	if err != nil || intent.State != "applied" {
		t.Fatalf("opt-in retry: %+v %v", intent, err)
	}
}

func TestAutoTitleWorkerWaitsForOlderWorkerHandoff(t *testing.T) {
	p, note := titleFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	if _, err := selectRecapTitle(context.Background(), p, autoTitleTestView(note, "Planning")); err != nil {
		t.Fatal(err)
	}
	older, err := titleWorkerLock(p, note.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer older.Close()
	done := make(chan error, 1)
	go func() { done <- runTitleWorker(p, []string{"--meeting", note.ID}) }()
	select {
	case err := <-done:
		t.Fatalf("closure retry disappeared behind older worker: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	older.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handoff did not finish")
	}
	if intent := autoTitleTestRead(t, note); intent.State != "applied" {
		t.Fatalf("handoff did not apply: %+v", intent)
	}
}

func TestAutoTitleContextCancellationBeforePublication(t *testing.T) {
	p, note := titleFixture(t)
	before := string(readTitleTest(t, note.Path))
	ctx, cancel := context.WithCancel(context.Background())
	check := func(string, navigation.Target) error { cancel(); return nil }
	_, err := titleMeetingGuardedContext(ctx, p, note.ID, "Planning", check, noTitlePanes, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored canceled caller context: %v", err)
	}
	if got := string(readTitleTest(t, note.Path)); got != before {
		t.Fatal("canceled transaction changed source")
	}
	current, _ := readMeeting(p, note.ID)
	if current.Path != note.Path || current.Title != "" {
		t.Fatalf("canceled transaction changed association: %+v", current)
	}
	assertTitleAbsent(t, filepath.Join(filepath.Dir(note.Path), "09-07-planning.md"))
}

func TestAutoTitleStatusIsReadOnlyAndCorruptionDoesNotHideRecap(t *testing.T) {
	p, note := recapTestFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	if _, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false); err != nil {
		t.Fatal(err)
	}
	before := readTitleTest(t, autoTitlePath(note))
	view, err := recapJobStatus(p, note.ID)
	if err != nil || view.State != "ready" || view.TitleState != "pending" || view.Title != "Friday planning" {
		t.Fatalf("title status: %+v %v", view, err)
	}
	if string(readTitleTest(t, autoTitlePath(note))) != string(before) {
		t.Fatal("status changed title intent")
	}
	if err := meeting.WriteFile(autoTitlePath(note), []byte("{broken")); err != nil {
		t.Fatal(err)
	}
	view, err = recapJobStatus(p, note.ID)
	if err != nil || view.State != "ready" || view.TitleState != "error" || view.TitleError == "" {
		t.Fatalf("corruption hid recap: %+v %v", view, err)
	}
	if _, err := latestRecap(context.Background(), p, note.ID); err != nil {
		t.Fatal("title corruption hid latest recap", err)
	}
}

func TestAutoTitleRejectsMalformedSavedSwapPaths(t *testing.T) {
	p, note := titleFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	intent := autoTitleIntent{MeetingID: note.ID, RecapID: meeting.NewID(), Title: "Planning", State: "pending"}
	for _, path := range []string{"", "relative/editor.swp", "/absolute/invalid\x00swap"} {
		intent.SwapPaths = []string{path}
		recapTestWrite(t, autoTitlePath(note), "", 0600)
		if err := meeting.WriteJSON(autoTitlePath(note), intent); err != nil {
			t.Fatal(err)
		}
		before := string(readTitleTest(t, autoTitlePath(note)))
		if _, err := readAutoTitle(note); err == nil {
			t.Fatalf("accepted malformed saved swap path %q", path)
		}
		if _, err := retryPendingTitle(context.Background(), p, note.ID); err == nil {
			t.Fatalf("retried malformed saved swap path %q", path)
		}
		if string(readTitleTest(t, autoTitlePath(note))) != before {
			t.Fatal("overwrote malformed recovery protection")
		}
		if current, err := readMeeting(p, note.ID); err != nil || current.Path != note.Path || current.Title != "" {
			t.Fatalf("malformed recovery protection allowed rename: %+v %v", current, err)
		}
	}
}

func TestCorruptAutoTitleDoesNotFailPublishedOrCachedRecap(t *testing.T) {
	p, note := recapTestFixture(t)
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	// Retain every byte of damaged metadata. It may contain the only known
	// recovery path for an editor that crashed while the provider ran.
	corrupt := `{"meeting_id":"` + note.ID + `","swap_paths":["/recovery/editor.swp"],"state":`
	recapTestWrite(t, autoTitlePath(note), corrupt, 0600)
	view, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false)
	if err != nil {
		t.Fatalf("published recap failed because naming is corrupt: %v", err)
	}
	var request recapRecord
	if err := json.Unmarshal(readTitleTest(t, filepath.Join(filepath.Dir(view.Path), "request.json")), &request); err != nil {
		t.Fatal(err)
	}
	if request.State != "complete" || request.Error != "" {
		t.Fatalf("successful request marked failed: %+v", request)
	}
	cached, err := generateRecap(context.Background(), p, note.ID, recapInput{EditorPID: os.Getpid()}, true)
	if err != nil || cached.RecapID != view.RecapID {
		t.Fatalf("cached --auto hidden by corrupt title: %+v %v", cached, err)
	}
	if err := enqueueRecap(p, note.ID); err != nil {
		t.Fatalf("cached --enqueue failed because naming is corrupt: %v", err)
	}
	status, err := recapJobStatus(p, note.ID)
	if err != nil || status.State != "ready" || status.RecapID != view.RecapID || status.TitleState != "error" || status.TitleError == "" {
		t.Fatalf("corrupt title status: %+v %v", status, err)
	}
	// The cached background-worker path must also remain a successful job.
	job := recapJob{MeetingID: note.ID, State: "queued"}
	if err := saveRecapJob(note, &job); err != nil {
		t.Fatal(err)
	}
	if err := processRecapJob(context.Background(), p, note.ID); err != nil {
		t.Fatalf("cached background recap failed: %v", err)
	}
	if job := mustRecapJob(t, note); job.State != "ready" || job.RecapID != view.RecapID {
		t.Fatalf("cached job not ready: %+v", job)
	}
	if recapRequestCount(t, note) != 1 {
		t.Fatal("naming corruption provoked another provider request")
	}
	if string(readTitleTest(t, autoTitlePath(note))) != corrupt {
		t.Fatal("overwrote corrupt recovery metadata")
	}
	if current, err := readMeeting(p, note.ID); err != nil || current.Path != note.Path || current.Title != "" {
		t.Fatalf("corrupt intent changed note: %+v %v", current, err)
	}
}

func TestRecapEnqueueWaitsForStatusRepairOfStaleJob(t *testing.T) {
	for _, staleState := range []string{"queued", "generating"} {
		t.Run(staleState, func(t *testing.T) {
			p, note := recapTestFixture(t)
			job := recapJob{MeetingID: note.ID, State: staleState}
			if err := saveRecapJob(note, &job); err != nil {
				t.Fatal(err)
			}
			// Pause a status repair after it obtains both short-lived locks,
			// before it rewrites a dead worker's stale queued/generating state.
			gate, err := recapScheduleLock(context.Background(), p, note.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Close()
			statusGuard, err := recapWorkerLock(p, note.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer statusGuard.Close()
			oldStart := startRecapWorker
			defer func() { startRecapWorker = oldStart }()
			launches := 0
			startRecapWorker = func(paths, string, meetingNote, *os.File) error { launches++; return nil }
			done := make(chan error, 1)
			go func() { done <- enqueueRecap(p, note.ID) }()
			select {
			case err := <-done:
				t.Fatalf("stale %s falsely confirmed a live worker: %v", staleState, err)
			case <-time.After(75 * time.Millisecond):
			}
			job.State, job.Error = "failed", "previous worker exited"
			if err := saveRecapJob(note, &job); err != nil {
				t.Fatal(err)
			}
			statusGuard.Close()
			gate.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("explicit retry did not finish after status repair")
			}
			if launches != 1 {
				t.Fatalf("launched %d workers, want one", launches)
			}
			if retried := mustRecapJob(t, note); retried.State != "queued" || retried.ID == job.ID {
				t.Fatalf("did not create a fresh retry: %+v", retried)
			}
		})
	}
}

func TestRecapEnqueueDoesNotLoseRequestToStatusLock(t *testing.T) {
	p, note := recapTestFixture(t)
	statusGuard, err := recapWorkerLock(p, note.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer statusGuard.Close()
	oldStart := startRecapWorker
	defer func() { startRecapWorker = oldStart }()
	launches := 0
	startRecapWorker = func(paths, string, meetingNote, *os.File) error { launches++; return nil }
	done := make(chan error, 1)
	go func() { done <- enqueueRecap(p, note.ID) }()
	select {
	case err := <-done:
		t.Fatalf("enqueue silently lost while status held lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	statusGuard.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("enqueue did not survive status lock")
	}
	if launches != 1 || mustRecapJob(t, note).State != "queued" {
		t.Fatal("explicit enqueue did not queue exactly once")
	}
}
