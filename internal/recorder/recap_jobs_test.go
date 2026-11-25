package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cwebley/trailmix/internal/meeting"
	"github.com/cwebley/trailmix/internal/settings"
)

func queuedRecapTestJob(t *testing.T, note meetingNote) {
	t.Helper()
	job := recapJob{MeetingID: note.ID, State: "queued"}
	if err := saveRecapJob(note, &job); err != nil {
		t.Fatal(err)
	}
}

func mustRecapJob(t *testing.T, note meetingNote) recapJob {
	t.Helper()
	job, err := readRecapJob(note)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func recapRequestCount(t *testing.T, note meetingNote) int {
	t.Helper()
	requests, err := filepath.Glob(filepath.Join(note.Directory, "recaps", "*", "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	return len(requests)
}

func TestRecapJobStatusIsObservationalAndRepairsDeadWorkers(t *testing.T) {
	p, note := recapTestFixture(t)
	view, err := recapJobStatus(p, note.ID)
	if err != nil || view.State != "none" || view.NotePath != note.Path {
		t.Fatalf("status: %+v %v", view, err)
	}
	if recapRequestCount(t, note) != 0 {
		t.Fatal("status generated a recap")
	}
	for _, state := range []string{"queued", "generating"} {
		job := recapJob{MeetingID: note.ID, State: state}
		if err := saveRecapJob(note, &job); err != nil {
			t.Fatal(err)
		}
		guard, err := recapWorkerLock(p, note.ID)
		if err != nil {
			t.Fatal(err)
		}
		view, err = recapJobStatus(p, note.ID)
		guard.Close()
		if err != nil || view.State != state {
			t.Fatalf("live worker status: %+v %v", view, err)
		}
		view, err = recapJobStatus(p, note.ID)
		if err != nil || view.State != "failed" || !strings.Contains(view.Error, "--enqueue") {
			t.Fatalf("dead worker status: %+v %v", view, err)
		}
		if job := mustRecapJob(t, note); job.State != "failed" {
			t.Fatalf("not persisted: %+v", job)
		}
	}
	if recapRequestCount(t, note) != 0 {
		t.Fatal("status retried a dead worker")
	}
}

func TestRecapJobQueuedGeneratingReadyAndEnqueueDeduplicates(t *testing.T) {
	p, note := recapTestFixture(t)
	before := readTitleTest(t, note.Path)
	started, release := filepath.Join(p.home, "started"), filepath.Join(p.home, "release")
	recapTestRunner(t, p, recapTestResponse("Friday is tentative. [[passage:system-1]]"), "touch '"+started+"'\nwhile [ ! -f '"+release+"' ]; do sleep 0.02; done", 5)
	oldStart := startRecapWorker
	t.Cleanup(func() { startRecapWorker = oldStart })
	launches := 0
	var inherited *os.File
	startRecapWorker = func(_ paths, _ string, _ meetingNote, guard *os.File) error {
		launches++
		fd, err := syscall.Dup(int(guard.Fd()))
		if err != nil {
			return err
		}
		inherited = os.NewFile(uintptr(fd), "test-inherited-worker-lock")
		return nil
	}
	// recap.enabled is false in the fixture. Explicit enqueue still works.
	if err := enqueueRecap(p, note.ID); err != nil {
		t.Fatal(err)
	}
	if inherited == nil {
		t.Fatal("worker was not started")
	}
	defer inherited.Close()
	if job := mustRecapJob(t, note); job.State != "queued" {
		t.Fatalf("queue: %+v", job)
	}
	if err := enqueueRecap(p, note.ID); err != nil || launches != 1 {
		t.Fatalf("duplicate queued launch: %d %v", launches, err)
	}
	done := make(chan error, 1)
	go func() { done <- processRecapJob(context.Background(), p, note.ID) }()
	recapTestAwait(t, started)
	view, err := recapJobStatus(p, note.ID)
	if err != nil || view.State != "generating" {
		t.Fatalf("generating: %+v %v", view, err)
	}
	if err := enqueueRecap(p, note.ID); err != nil || launches != 1 {
		t.Fatalf("duplicate generating launch: %d %v", launches, err)
	}
	recapTestWrite(t, release, "", 0600)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	inherited.Close()
	job := mustRecapJob(t, note)
	if job.State != "ready" || job.Attempts != 1 || job.Input == nil || job.Input.Source != "saved_note" || job.RecapID == "" {
		t.Fatalf("ready: %+v", job)
	}
	if err := enqueueRecap(p, note.ID); err != nil || launches != 1 {
		t.Fatalf("duplicate ready launch: %d %v", launches, err)
	}
	if recapRequestCount(t, note) != 1 {
		t.Fatal("deduplication made another provider request")
	}
	if got := readTitleTest(t, note.Path); string(got) != string(before) {
		t.Fatal("worker changed source note")
	}
	for _, path := range []string{recapJobPath(note), filepath.Dir(recapJobPath(note)), filepath.Join(note.Directory, "recaps", "latest.json")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("private artifact: %s %v %v", path, info, err)
		}
	}
	// Returned associations track a rename, not the path saved with a result.
	renamed, err := retitleForTest(p, note, "Renamed after recap")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := latestRecap(context.Background(), p, note.ID)
	if err != nil || latest.NotePath != renamed.Path {
		t.Fatalf("latest association: %+v %v", latest, err)
	}
	view, err = recapJobStatus(p, note.ID)
	if err != nil || view.NotePath != renamed.Path || view.State != "ready" {
		t.Fatalf("status association: %+v %v", view, err)
	}
}

func TestRecapJobBoundsProviderAttemptsAndExplicitRetry(t *testing.T) {
	p, note := recapTestFixture(t)
	recapTestRunner(t, p, "", "exit 23", 3)
	queuedRecapTestJob(t, note)
	if err := runRecapWorker(p, []string{"--meeting", note.ID}); err == nil {
		t.Fatal("provider failure succeeded")
	}
	job := mustRecapJob(t, note)
	if job.State != "failed" || job.Attempts != recapMaxAttempts || !strings.Contains(job.Error, "--enqueue") || recapRequestCount(t, note) != recapMaxAttempts {
		t.Fatalf("unbounded or unrecorded failure: %+v", job)
	}
	if err := runRecapWorker(p, []string{"--meeting", note.ID}); err != nil {
		t.Fatal(err)
	}
	if recapRequestCount(t, note) != recapMaxAttempts {
		t.Fatal("failed job retried implicitly")
	}
	oldStart := startRecapWorker
	t.Cleanup(func() { startRecapWorker = oldStart })
	startRecapWorker = func(paths, string, meetingNote, *os.File) error { return nil }
	recapTestRunner(t, p, recapTestResponse("Friday is tentative. [[passage:system-1]]"), "", 3)
	if err := enqueueRecap(p, note.ID); err != nil {
		t.Fatal(err)
	}
	job = mustRecapJob(t, note)
	if job.State != "queued" || job.Attempts != 0 {
		t.Fatalf("explicit retry not reset: %+v", job)
	}
	if err := runRecapWorker(p, []string{"--meeting", note.ID}); err != nil {
		t.Fatal(err)
	}
	if job = mustRecapJob(t, note); job.State != "ready" || job.Attempts != 1 {
		t.Fatalf("retry: %+v", job)
	}
}

func TestRecapJobSnapshotFallbackWarningPersists(t *testing.T) {
	p, note := recapTestFixture(t)
	recapTestWrite(t, filepath.Join(p.home, "recap-editors", "bad.json"), `{}`, 0600)
	queuedRecapTestJob(t, note)
	if err := runRecapWorker(p, []string{"--meeting", note.ID}); err != nil {
		t.Fatal(err)
	}
	job := mustRecapJob(t, note)
	if job.State != "ready" || !job.EditorConflict || job.EditorReason == "" || job.Input == nil || job.Input.Source != "saved_note" || len(job.Input.SourceWarnings) != 1 {
		t.Fatalf("snapshot fallback: %+v", job)
	}
	view, err := latestRecap(context.Background(), p, note.ID)
	if err != nil || !strings.Contains(view.Markdown, "Notes source notice:") || view.NotesSnapshot != string(readTitleTest(t, note.Path)) {
		t.Fatalf("fallback result: %+v %v", view, err)
	}
	var record recapRecord
	data := readTitleTest(t, filepath.Join(filepath.Dir(view.Path), "request.json"))
	if err := json.Unmarshal(data, &record); err != nil || len(record.Input.SourceWarnings) != 1 {
		t.Fatalf("private fallback warning: %+v %v", record, err)
	}
	status, err := recapJobStatus(p, note.ID)
	if err != nil || !status.EditorConflict || status.EditorReason == "" {
		t.Fatalf("status conflict: %+v %v", status, err)
	}
}

func TestRecapJobCancelStopsProviderAndDoesNotRetry(t *testing.T) {
	p, note := recapTestFixture(t)
	started := filepath.Join(p.home, "started")
	recapTestRunner(t, p, recapTestResponse("Friday is tentative. [[passage:system-1]]"), "touch '"+started+"'\nsleep 30", 60)
	queuedRecapTestJob(t, note)
	done := make(chan error, 1)
	go func() { done <- runRecapWorker(p, []string{"--meeting", note.ID}) }()
	recapTestAwait(t, started)
	if err := cancelRecapJob(p, note.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errRecapCancelled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not stop the provider process group promptly")
	}
	job := mustRecapJob(t, note)
	if job.State != "cancelled" || job.Attempts != 1 || !strings.Contains(job.Error, "cancelled") || recapRequestCount(t, note) != 1 {
		t.Fatalf("cancel retried or lost state: %+v", job)
	}
	if err := runRecapWorker(p, []string{"--meeting", note.ID}); err != nil {
		t.Fatal(err)
	}
	if recapRequestCount(t, note) != 1 {
		t.Fatal("cancelled job retried implicitly")
	}
}

func TestRecapJobQueuedCancellationIsInstanceScoped(t *testing.T) {
	p, note := recapTestFixture(t)
	queuedRecapTestJob(t, note)
	original := mustRecapJob(t, note)
	if err := cancelRecapJob(p, note.ID); err != nil {
		t.Fatal(err)
	}
	if err := runRecapWorker(p, []string{"--meeting", note.ID}); !errors.Is(err, errRecapCancelled) {
		t.Fatalf("queued cancellation: %v", err)
	}
	job := mustRecapJob(t, note)
	if job.State != "cancelled" || job.Attempts != 0 || recapRequestCount(t, note) != 0 {
		t.Fatalf("cancel sent a request: %+v", job)
	}
	oldStart := startRecapWorker
	t.Cleanup(func() { startRecapWorker = oldStart })
	startRecapWorker = func(paths, string, meetingNote, *os.File) error { return nil }
	if err := enqueueRecap(p, note.ID); err != nil {
		t.Fatal(err)
	}
	job = mustRecapJob(t, note)
	if job.ID == original.ID {
		t.Fatal("retry reused cancelled job instance")
	}
	if err := runRecapWorker(p, []string{"--meeting", note.ID}); err != nil {
		t.Fatalf("old cancellation affected retry: %v", err)
	}
	ready := mustRecapJob(t, note)
	if ready.State != "ready" {
		t.Fatalf("retry: %+v", ready)
	}
	if err := cancelRecapJob(p, note.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustRecapJob(t, note); got.State != "ready" || got.RecapID != ready.RecapID {
		t.Fatalf("cancel removed success: %+v", got)
	}
}

func TestRecapSnapshotRefreshesRenamedAssociation(t *testing.T) {
	p, note := recapTestFixture(t)
	renamed, err := retitleForTest(p, note, "Renamed before snapshot")
	if err != nil {
		t.Fatal(err)
	}
	current, input, _, err := snapshotRecapJobInput(context.Background(), p, note)
	if err != nil || current.Path != renamed.Path || input.NotePath != renamed.Path || input.Notes == nil || *input.Notes != string(readTitleTest(t, renamed.Path)) {
		t.Fatalf("stale snapshot association: %+v %+v %v", current, input, err)
	}
	// Also allow a rename between the snapshot and the generation attempt.
	queuedRecapTestJob(t, renamed)
	job := mustRecapJob(t, renamed)
	job.Input = &input
	if err := saveRecapJob(renamed, &job); err != nil {
		t.Fatal(err)
	}
	again, err := retitleForTest(p, renamed, "Renamed before attempt")
	if err != nil {
		t.Fatal(err)
	}
	if err := runRecapWorker(p, []string{"--meeting", note.ID}); err != nil {
		t.Fatal(err)
	}
	latest, err := latestRecap(context.Background(), p, note.ID)
	if err != nil || latest.NotePath != again.Path || latest.NotesSnapshot != *input.Notes {
		t.Fatalf("renamed attempt changed snapshot: %+v %v", latest, err)
	}
}

func TestRecapSnapshotWaitsForWorkspaceLaunchAndReleasesBeforeProvider(t *testing.T) {
	p, note := recapTestFixture(t)
	started, release := filepath.Join(p.home, "provider-started"), filepath.Join(p.home, "provider-release")
	recapTestRunner(t, p, recapTestResponse("Friday is tentative. [[passage:system-1]]"), "touch '"+started+"'\nwhile [ ! -f '"+release+"' ]; do sleep 0.02; done", 5)
	workspace, err := lifecycleLock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	done := make(chan error, 1)
	go func() { _, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("snapshot interleaved with launch: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	recorder, err := lock(p)
	if err != nil {
		t.Fatalf("snapshot stole launch's recorder lock: %v", err)
	}
	recorder.Close()
	if recapRequestCount(t, note) != 0 {
		t.Fatal("provider started during workspace launch")
	}
	workspace.Close()
	recapTestAwait(t, started)
	workspace, err = lifecycleLock(p)
	if err != nil {
		t.Fatalf("provider kept workspace lock: %v", err)
	}
	workspace.Close()
	recapTestWrite(t, release, "", 0600)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRecapJobBoundsSourceWaitsWithoutProviderCalls(t *testing.T) {
	p, note := recapTestFixture(t)
	capture, err := recapExclusive(filepath.Join(note.Directory, "capture.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	// Seed the last wait to test the durable bound without a five-second sleep.
	job := recapJob{MeetingID: note.ID, State: "queued", Waits: recapMaxWaits}
	if err := saveRecapJob(note, &job); err != nil {
		t.Fatal(err)
	}
	if err := runRecapWorker(p, []string{"--meeting", note.ID}); !errors.Is(err, errRecapBusy) {
		t.Fatalf("expected busy failure: %v", err)
	}
	job = mustRecapJob(t, note)
	if job.State != "failed" || job.Attempts != 0 || job.Waits != recapMaxWaits || recapRequestCount(t, note) != 0 {
		t.Fatalf("busy request bound: %+v", job)
	}
}

func TestRecapTargetStartupGuardAndUnrelatedRecorder(t *testing.T) {
	p, note := recapTestFixture(t)
	recorder, err := lock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	for _, state := range []string{"starting", "recording", "stopping"} {
		if err := meeting.WriteJSON(filepath.Join(note.Directory, "session.json"), status{MeetingID: note.ID, State: state}); err != nil {
			t.Fatal(err)
		}
		if _, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false); !errors.Is(err, errRecapBusy) {
			t.Fatalf("startup %s was not excluded: %v", state, err)
		}
		if _, err := os.Stat(filepath.Join(note.Directory, "capture.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("startup guard created native lock: %v", err)
		}
	}
	if err := meeting.WriteJSON(filepath.Join(note.Directory, "session.json"), status{MeetingID: meeting.NewID(), State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if _, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false); !errors.Is(err, errRecapBusy) {
		t.Fatalf("mismatched session accepted: %v", err)
	}
	if err := meeting.WriteJSON(filepath.Join(note.Directory, "session.json"), status{MeetingID: note.ID, State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if _, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false); err != nil {
		t.Fatalf("unrelated recording blocked terminal target: %v", err)
	}
}

func TestRecapFailedCaptureWithoutTranscriptUsesNotes(t *testing.T) {
	p, note := recapTestFixture(t)
	if err := os.RemoveAll(filepath.Join(note.Directory, "transcription")); err != nil {
		t.Fatal(err)
	}
	if err := meeting.WriteJSON(filepath.Join(note.Directory, "session.json"), status{MeetingID: note.ID, State: "failed", Error: "microphone disconnected"}); err != nil {
		t.Fatal(err)
	}
	recapTestRunner(t, p, recapTestResponse("No finalized conversation evidence is available."), "", 3)
	// Remove the fixture's cited observation too, since no passages survived.
	response, _ := json.Marshal(map[string]string{"suggested_title": "Capture failed", "summary": "No finalized conversation evidence is available.", "observations": "The saved notes contain the agenda.", "questions_to_follow_up": "No follow-up question is supported.", "my_commitments": "No personal commitments are supported."})
	recapTestRunner(t, p, string(response), "", 3)
	queuedRecapTestJob(t, note)
	if err := runRecapWorker(p, []string{"--meeting", note.ID}); err != nil {
		t.Fatal(err)
	}
	view, err := latestRecap(context.Background(), p, note.ID)
	if err != nil || !strings.Contains(view.Markdown, "Coverage notice:") || !strings.Contains(view.Markdown, "missing or uncertain coverage") {
		t.Fatalf("failed capture: %+v %v", view, err)
	}
}

func TestAutomaticRecapFailureDoesNotOverwriteNewerSessionOrAssociation(t *testing.T) {
	t.Setenv("TRAILMIX_TEST_HELPER", "failure")
	p, note := titleFixture(t)
	p.meetingID, p.transcribe = note.ID, true
	if err := settings.Run([]string{"set", "recap.enabled", "true"}); err != nil {
		t.Fatal(err)
	}
	oldStart := startRecapWorker
	t.Cleanup(func() { startRecapWorker = oldStart })
	newerID := meeting.NewID()
	renamedPath := ""
	launches := 0
	startRecapWorker = func(got paths, id string, jobNote meetingNote, _ *os.File) error {
		launches++
		// Simulate a new recorder winning the global lock after serve released
		// it, before this failed background launch reports its warning.
		if err := meeting.WriteJSON(filepath.Join(got.home, "last.json"), status{MeetingID: newerID, State: "recording"}); err != nil {
			return err
		}
		renamed, err := retitleForTest(got, jobNote, "Renamed after capture")
		if err != nil {
			return err
		}
		renamedPath = renamed.Path
		return errors.New("fixture worker launch failure")
	}
	output := filepath.Join(p.home, "failed-recording")
	if err := serve(p, output, "test-mic"); err == nil {
		t.Fatal("capture failure was hidden")
	}
	if launches != 1 {
		t.Fatal("failed capture with saved notes was not queued")
	}
	var last, saved status
	if err := json.Unmarshal(readTitleTest(t, filepath.Join(p.home, "last.json")), &last); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(readTitleTest(t, filepath.Join(output, "session.json")), &saved); err != nil {
		t.Fatal(err)
	}
	if last.MeetingID != newerID || last.State != "recording" {
		t.Fatalf("overwrote newer global session: %+v", last)
	}
	if saved.MeetingID != note.ID || saved.Note == nil || renamedPath == "" || saved.Note.Path != renamedPath {
		t.Fatalf("overwrote renamed session association: %+v", saved)
	}
	if job := mustRecapJob(t, *saved.Note); job.State != "failed" || !strings.Contains(job.Error, "fixture worker launch failure") {
		t.Fatalf("job lost launch failure: %+v", job)
	}
}

func TestAutomaticRecapSchedulingAfterRecorderReleaseDoesNotDelayStop(t *testing.T) {
	for _, test := range []struct {
		name                           string
		enabled, transcribe, workspace bool
		want                           int
	}{
		{"workspace transcribed", true, true, true, 1},
		{"disabled", false, true, true, 0},
		{"raw capture", true, true, false, 0},
		{"without transcription", true, false, true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("TRAILMIX_TEST_HELPER", "normal")
			p, note := titleFixture(t)
			p.transcribe = test.transcribe
			if test.workspace {
				p.meetingID = note.ID
			}
			if err := settings.Run([]string{"set", "recap.enabled", map[bool]string{true: "true", false: "false"}[test.enabled]}); err != nil {
				t.Fatal(err)
			}
			oldStart := startRecapWorker
			t.Cleanup(func() { startRecapWorker = oldStart })
			started, release := make(chan struct{}), make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			launches := 0
			startRecapWorker = func(got paths, id string, jobNote meetingNote, _ *os.File) error {
				launches++
				guard, err := lock(got)
				if err != nil {
					return err
				}
				guard.Close()
				data, err := os.ReadFile(filepath.Join(jobNote.Directory, "session.json"))
				if err != nil {
					return err
				}
				var saved status
				if err := json.Unmarshal(data, &saved); err != nil {
					return err
				}
				if saved.MeetingID != id || saved.State != "stopped" {
					return errors.New("worker started before final capture status")
				}
				close(started)
				<-release // Even a stuck launcher must not delay the stop response.
				return nil
			}
			done := make(chan error, 1)
			go func() { done <- serve(p, filepath.Join(p.home, "new-recording"), "test-mic") }()
			awaitStatus(t, p, func(s status) bool { return s.State == "recording" })
			stopped := make(chan error, 1)
			go func() { _, err := request(p.socket, "stop"); stopped <- err }()
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("stop waited for background recap")
			}
			if test.want > 0 {
				select {
				case <-started:
				case err := <-done:
					t.Fatalf("worker not reached: %v", err)
				case <-time.After(3 * time.Second):
					t.Fatal("no automatic job")
				}
				if launches != test.want {
					t.Fatalf("launches %d, want %d", launches, test.want)
				}
				// Release explicitly so the goroutine finishes before fixture cleanup.
				close(release)
				released = true
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if launches != test.want {
				t.Fatalf("launches %d, want %d", launches, test.want)
			}
		})
	}
}
