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

	"github.com/cwebley/trailmix/internal/assistant"
	"github.com/cwebley/trailmix/internal/meeting"
)

// Model the old adjacent-citation validation failure using one local fake
// assistant call. Its retained request, runner output and evidence are real
// generation artifacts, but neither result nor latest was published then.
func recapRecoveryFixture(t *testing.T) (paths, meetingNote, recapRecord) {
	t.Helper()
	p, note := recapTestFixture(t)
	recapTestRunner(t, p, recapTestResponse("Tentative planning. [[passage:microphone-1]][[passage:system-1]]"), "", 5)
	notes := string(readTitleTest(t, note.Path)) + "Exact unsaved notes supplied to the original request.\n"
	view, err := generateRecap(context.Background(), p, note.ID, recapInput{Notes: &notes, NotePath: note.Path, SourceWarnings: []string{"Original saved-note fallback <notice>."}}, false)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(view.Path)
	var record recapRecord
	if err := json.Unmarshal(readTitleTest(t, filepath.Join(directory, "request.json")), &record); err != nil {
		t.Fatal(err)
	}
	record.State, record.Error = "failed", "recap summary contains headings, links, or reserved metadata"
	if err := meeting.WriteJSON(filepath.Join(directory, "request.json"), record); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{view.Path, filepath.Join(note.Directory, "recaps", "latest.json")} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	job := recapJob{MeetingID: note.ID, State: "failed", Error: record.Error}
	if err := saveRecapJob(note, &job); err != nil {
		t.Fatal(err)
	}
	return p, note, record
}

func TestRecoverRecapReusesRetainedResponseAndEvidence(t *testing.T) {
	p, note, record := recapRecoveryFixture(t)
	directory := filepath.Join(note.Directory, "recaps", record.ID)
	// A fresh note or transcript must not replace the original source snapshot.
	current := string(readTitleTest(t, note.Path)) + "Newer notes not sent to the assistant.\n"
	if err := os.WriteFile(note.Path, []byte(current), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(note.Directory, "transcription", "system-1.passage.json")); err != nil {
		t.Fatal(err)
	}
	runnerBefore := string(readTitleTest(t, filepath.Join(directory, "runner.json")))
	transcriptBefore := string(readTitleTest(t, filepath.Join(directory, "transcript.json")))
	view, err := recoverRecap(context.Background(), p, note.ID, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.RecapID != record.ID || view.NotePath != note.Path || view.Path != filepath.Join(directory, "result.json") || view.NotesSnapshot != *record.Input.Notes {
		t.Fatalf("recovered identity or notes changed: %+v", view)
	}
	if !strings.Contains(view.Markdown, "Notes source notice: Original saved&#45;note fallback &#60;notice&#62;&#46;") || !strings.Contains(view.Markdown, "/passages/microphone-1)") || !strings.Contains(view.Markdown, "/passages/system-1)") {
		t.Fatalf("lost warnings or adjacent citations: %s", view.Markdown)
	}
	if string(readTitleTest(t, note.Path)) != current {
		t.Fatal("recovery wrote current note")
	}
	var audit recapRecovery
	if err := readRecoveryJSON(filepath.Join(directory, "recovered.json"), &audit); err != nil {
		t.Fatal(err)
	}
	if audit.PreviousRequest.Error != record.Error || audit.PreviousRequest.State != "failed" || *audit.PreviousRequest.Input.Notes != *record.Input.Notes {
		t.Fatalf("failure audit lost: %+v", audit)
	}
	var completed recapRecord
	if err := readRecoveryJSON(filepath.Join(directory, "request.json"), &completed); err != nil {
		t.Fatal(err)
	}
	if completed.State != "complete" || completed.Error != "" {
		t.Fatalf("request not complete: %+v", completed)
	}
	if job := mustRecapJob(t, note); job.State != "ready" || job.RecapID != record.ID {
		t.Fatalf("job not ready: %+v", job)
	}
	latest, err := latestRecap(context.Background(), p, note.ID)
	if err != nil || latest != view {
		t.Fatalf("latest: %+v %v", latest, err)
	}
	auditBefore := string(readTitleTest(t, filepath.Join(directory, "recovered.json")))
	again, err := recoverRecap(context.Background(), p, note.ID, record.ID)
	if err != nil || again != view {
		t.Fatalf("idempotent retry: %+v %v", again, err)
	}
	if string(readTitleTest(t, filepath.Join(directory, "recovered.json"))) != auditBefore || string(readTitleTest(t, filepath.Join(directory, "runner.json"))) != runnerBefore || string(readTitleTest(t, filepath.Join(directory, "transcript.json"))) != transcriptBefore {
		t.Fatal("recovery changed retained evidence or its audit")
	}
	if recapRequestCount(t, note) != 1 {
		t.Fatal("recovery called the assistant")
	}
}

func TestRecoverRecapRejectsInvalidRetainedOutputWithoutPublication(t *testing.T) {
	for _, response := range []string{
		recapTestResponse("Unknown. [[passage:absent]]"),
		recapTestResponse("Read [source](https://example.com)."),
		recapTestResponse("Read [[passage:system-1]](target)."),
		`{"suggested_title":"Planning","summary":null}`,
	} {
		t.Run(response[:min(len(response), 30)], func(t *testing.T) {
			p, note, record := recapRecoveryFixture(t)
			directory := filepath.Join(note.Directory, "recaps", record.ID)
			if err := meeting.WriteJSON(filepath.Join(directory, "runner.json"), assistant.Result{Text: response}); err != nil {
				t.Fatal(err)
			}
			requestBefore := string(readTitleTest(t, filepath.Join(directory, "request.json")))
			noteBefore := string(readTitleTest(t, note.Path))
			if _, err := recoverRecap(context.Background(), p, note.ID, record.ID); err == nil {
				t.Fatal("accepted invalid retained response")
			}
			for _, path := range []string{filepath.Join(directory, "result.json"), filepath.Join(directory, "recovered.json"), filepath.Join(note.Directory, "recaps", "latest.json")} {
				assertTitleAbsent(t, path)
			}
			if string(readTitleTest(t, filepath.Join(directory, "request.json"))) != requestBefore || string(readTitleTest(t, note.Path)) != noteBefore {
				t.Fatal("invalid recovery changed request or note")
			}
			if recapRequestCount(t, note) != 1 {
				t.Fatal("invalid recovery called assistant")
			}
		})
	}
}

func TestRecoverRecapRefusesOtherLatestAndActiveOrCancelledWork(t *testing.T) {
	t.Run("other latest", func(t *testing.T) {
		p, note, record := recapRecoveryFixture(t)
		newer, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false)
		if err != nil {
			t.Fatal(err)
		}
		before := string(readTitleTest(t, filepath.Join(note.Directory, "recaps", "latest.json")))
		if _, err := recoverRecap(context.Background(), p, note.ID, record.ID); err == nil {
			t.Fatal("replaced another latest")
		}
		if string(readTitleTest(t, filepath.Join(note.Directory, "recaps", "latest.json"))) != before {
			t.Fatal("overwrote newer latest")
		}
		if latest, err := latestRecap(context.Background(), p, note.ID); err != nil || latest.RecapID != newer.RecapID {
			t.Fatalf("lost newer recap: %+v %v", latest, err)
		}
	})
	for _, state := range []string{"worker lock", "queued", "generating", "cancelled", "cancellation token"} {
		t.Run(state, func(t *testing.T) {
			p, note, record := recapRecoveryFixture(t)
			if state == "worker lock" {
				guard, err := recapWorkerLock(p, note.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Close()
			} else {
				job := mustRecapJob(t, note)
				if state == "cancellation token" {
					if err := meeting.WriteJSON(filepath.Join(note.Directory, "recaps", "cancel.json"), recapCancellation{JobID: job.ID}); err != nil {
						t.Fatal(err)
					}
				} else {
					job.State = state
					if err := saveRecapJob(note, &job); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := recoverRecap(context.Background(), p, note.ID, record.ID); err == nil {
				t.Fatal("recovered active or cancelled work")
			}
			assertTitleAbsent(t, filepath.Join(note.Directory, "recaps", "latest.json"))
		})
	}
}

func TestRecoverRecapRejectsIdentityAndPathEscapes(t *testing.T) {
	for _, kind := range []string{"request ID", "request meeting", "notes", "transcript meeting", "passage meeting", "cancelled request", "directory symlink", "runner symlink"} {
		t.Run(kind, func(t *testing.T) {
			p, note, record := recapRecoveryFixture(t)
			directory := filepath.Join(note.Directory, "recaps", record.ID)
			switch kind {
			case "request ID", "request meeting", "notes":
				if kind == "request ID" {
					record.ID = meeting.NewID()
				} else if kind == "request meeting" {
					record.MeetingID = meeting.NewID()
				} else {
					value := "unassociated notes"
					record.Input.Notes = &value
				}
				if err := meeting.WriteJSON(filepath.Join(directory, "request.json"), record); err != nil {
					t.Fatal(err)
				}
			case "transcript meeting", "passage meeting":
				var snapshot transcriptSnapshot
				if err := readRecoveryJSON(filepath.Join(directory, "transcript.json"), &snapshot); err != nil {
					t.Fatal(err)
				}
				if kind == "transcript meeting" {
					snapshot.MeetingID = meeting.NewID()
				} else {
					snapshot.Passages[0].MeetingID = meeting.NewID()
				}
				if err := meeting.WriteJSON(filepath.Join(directory, "transcript.json"), snapshot); err != nil {
					t.Fatal(err)
				}
			case "cancelled request":
				record.Error = "context canceled"
				if err := meeting.WriteJSON(filepath.Join(directory, "request.json"), record); err != nil {
					t.Fatal(err)
				}
			case "directory symlink":
				outside := filepath.Join(p.home, "outside-request")
				if err := os.Rename(directory, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, directory); err != nil {
					t.Fatal(err)
				}
			case "runner symlink":
				path := filepath.Join(directory, "runner.json")
				outside := filepath.Join(p.home, "outside-runner.json")
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := recoverRecap(context.Background(), p, note.ID, filepath.Base(directory)); err == nil {
				t.Fatal("accepted mismatched or escaped evidence")
			}
			assertTitleAbsent(t, filepath.Join(note.Directory, "recaps", "latest.json"))
		})
	}
	p, note, _ := recapRecoveryFixture(t)
	for _, id := range []string{"", "../request", strings.Repeat("g", 32), strings.Repeat("a", 32) + "/.."} {
		if _, err := recoverRecap(context.Background(), p, note.ID, id); err == nil {
			t.Fatalf("accepted request ID %q", id)
		}
		if err := runRecap(p, []string{"--meeting", note.ID, "--recover", id}); err == nil {
			t.Fatalf("CLI accepted request ID %q", id)
		}
	}
}

func TestRecoverRecapContextAndTitleHooks(t *testing.T) {
	p, note, record := recapRecoveryFixture(t)
	generation, err := recapExclusive(filepath.Join(p.home, "meetings", note.ID+".recap.lock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := recoverRecap(ctx, p, note.ID, record.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ignored generation deadline: %v", err)
	}
	generation.Close()
	autoTitleTestNavigation(t)
	autoTitleTestEnable(t)
	// Public CLI dispatch schedules only after all recovery locks are released.
	if err := runRecap(p, []string{"--meeting", note.ID, "--recover", record.ID}); err != nil {
		t.Fatal(err)
	}
	intent := autoTitleTestRead(t, note)
	if intent.State != "applied" || intent.RecapID != record.ID {
		t.Fatalf("recovery naming hook: %+v", intent)
	}
	if recapRequestCount(t, note) != 1 {
		t.Fatal("recovery naming called assistant")
	}
	for _, mode := range []string{"--latest", "--enqueue", "--status", "--cancel", "--stdin", "--auto"} {
		if err := runRecap(p, []string{"--meeting", note.ID, "--recover", record.ID, mode}); err == nil {
			t.Fatalf("accepted recovery plus %s", mode)
		}
	}
}
