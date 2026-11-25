package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cwebley/trailmix/internal/assistant"
	"github.com/cwebley/trailmix/internal/meeting"
	"github.com/cwebley/trailmix/internal/settings"
	"github.com/cwebley/trailmix/internal/transcription"
)

func recapTestWrite(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func recapTestResponse(summary string) string {
	data, _ := json.Marshal(map[string]string{
		"suggested_title": "Friday planning", "summary": summary,
		"observations":           "Friday is tentative. [[passage:system-1]]",
		"questions_to_follow_up": "Notes question: who will confirm the date?",
		"my_commitments":         "No personal commitments are supported by the supplied evidence.",
	})
	return string(data)
}

func recapTestRunner(t *testing.T, p paths, response, commands string, timeout int) {
	t.Helper()
	responsePath := filepath.Join(p.home, "response.json")
	recapTestWrite(t, responsePath, response, 0600)
	program := filepath.Join(p.home, "recap-assistant")
	// Paths come from a controlled temporary directory, not from user input.
	recapTestWrite(t, program, "#!/bin/sh\ncat > received.txt\n"+commands+"\ncat '"+responsePath+"'\n", 0700)
	path, err := settings.Path()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"recap": settings.Recap{Enabled: false, AutoName: false}, "assistant": settings.Assistant{Command: []string{program}, Format: "markdown", TimeoutSeconds: timeout}})
	recapTestWrite(t, path, string(data), 0600)
}

func recapTestFixture(t *testing.T) (paths, meetingNote) {
	t.Helper()
	p, note := titleFixture(t)
	state := transcription.Status{MeetingID: note.ID, State: "complete", Tracks: map[string]transcription.TrackStatus{
		"microphone": {State: "complete"}, "system": {State: "complete"},
	}}
	if err := meeting.WriteJSON(filepath.Join(note.Directory, "transcription", "status.json"), state); err != nil {
		t.Fatal(err)
	}
	for _, passage := range []transcription.Passage{
		{Range: transcription.Range{ID: "microphone-1", Track: "microphone", Start: 1, End: 3}, MeetingID: note.ID, Text: "Can we confirm a date?"},
		{Range: transcription.Range{ID: "system-1", Track: "system", Start: 62, End: 65}, MeetingID: note.ID, Text: "Friday is tentative."},
	} {
		if err := meeting.WriteJSON(filepath.Join(note.Directory, "transcription", passage.ID+".passage.json"), passage); err != nil {
			t.Fatal(err)
		}
	}
	end := 120.0
	if err := meeting.WriteJSON(filepath.Join(note.Directory, "session.json"), status{
		MeetingID: note.ID, State: "stopped", Capture: &captureHealth{State: "stopped", LastCapturedSeconds: &end},
	}); err != nil {
		t.Fatal(err)
	}
	recapTestRunner(t, p, recapTestResponse("Friday remains tentative. [[passage:system-1]]"), "", 5)
	return p, note
}

func TestRecapCLIContract(t *testing.T) {
	p, note := recapTestFixture(t)
	t.Setenv("TRAILMIX_HOME", p.home)
	cli := filepath.Join(p.home, "trailmix")
	build := exec.Command("go", "build", "-o", cli, "./cmd/trailmix")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	run := func(input string, args ...string) ([]byte, error) {
		cmd := exec.Command(cli, append([]string{"recap", "--meeting", note.ID}, args...)...)
		cmd.Stdin = strings.NewReader(input)
		return cmd.CombinedOutput()
	}
	for _, mode := range []string{"--status", "--cancel"} {
		out, err := run("", mode)
		var status recapStatusView
		var fields map[string]json.RawMessage
		if err != nil || json.Unmarshal(out, &status) != nil || json.Unmarshal(out, &fields) != nil || status.State != "none" || status.NotePath != note.Path {
			t.Fatalf("initial %s: %v %s", mode, err, out)
		}
		for _, key := range []string{"schema_version", "meeting_id", "note_path", "state", "editor_conflict"} {
			if _, ok := fields[key]; !ok {
				t.Fatalf("missing status field %s: %s", key, out)
			}
		}
		if recapRequestCount(t, note) != 0 {
			t.Fatal("status or cancel generated a recap")
		}
	}
	if out, err := run("", "--latest"); err == nil || !strings.Contains(string(out), "no saved recap") {
		t.Fatalf("missing latest: %v %s", err, out)
	}
	original := string(readTitleTest(t, note.Path))
	out, err := run("")
	if err != nil {
		t.Fatalf("saved recap: %v %s", err, out)
	}
	var saved recapView
	if err := json.Unmarshal(out, &saved); err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	_ = json.Unmarshal(out, &envelope)
	for _, key := range []string{"schema_version", "meeting_id", "note_path", "recap_id", "markdown", "suggested_title", "notes_snapshot", "path"} {
		if _, ok := envelope[key]; !ok {
			t.Fatalf("missing CLI field %s: %s", key, out)
		}
	}
	if saved.NotesSnapshot != original || saved.MeetingID != note.ID || saved.NotePath != note.Path || !meeting.ValidID(saved.RecapID) {
		t.Fatalf("wrong saved snapshot: %+v", saved)
	}
	unsaved := original + "\n- unsaved question with trailing spaces  \n\n"
	payload, _ := json.Marshal(map[string]string{"notes": unsaved, "note_path": note.Path})
	out, err = run(string(payload), "--stdin")
	if err != nil {
		t.Fatalf("stdin recap: %v %s", err, out)
	}
	var got recapView
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.NotesSnapshot != unsaved || got.RecapID == saved.RecapID {
		t.Fatalf("lost exact snapshot or manual regeneration: %+v", got)
	}
	if !strings.Contains(got.Markdown, "[01:02](trailmix://meetings/"+note.ID+"/passages/system-1)") || strings.Contains(got.Markdown, "## Rough notes") || strings.Contains(got.Markdown, "# Meeting") {
		t.Fatalf("bad recap body: %s", got.Markdown)
	}
	headings := []string{}
	for _, line := range strings.Split(got.Markdown, "\n") {
		if strings.HasPrefix(line, "#") {
			headings = append(headings, line)
		}
	}
	if !reflect.DeepEqual(headings, []string{"## Summary", "## Observations", "## Questions to follow up", "## My commitments"}) {
		t.Fatalf("headings: %v", headings)
	}
	if string(readTitleTest(t, note.Path)) != original {
		t.Fatal("recap changed source note")
	}
	for _, path := range []string{got.Path, filepath.Dir(got.Path), filepath.Join(note.Directory, "recaps", "latest.json")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("not private: %s %v", path, err)
		}
	}
	var persisted recapView
	if err := json.Unmarshal(readTitleTest(t, got.Path), &persisted); err != nil || persisted != got {
		t.Fatalf("persisted view: %+v %v", persisted, err)
	}
	requestDirectory := filepath.Dir(got.Path)
	for _, name := range []string{"inputs.json", "transcript.json", "prompt.txt", "runner.json", "request.json"} {
		if _, err := os.Stat(filepath.Join(requestDirectory, name)); err != nil {
			t.Fatal(name, err)
		}
	}
	prompt := string(readTitleTest(t, filepath.Join(requestDirectory, "received.txt")))
	for _, text := range []string{"unsaved question", "Friday is tentative.", "Can we confirm a date?", "capture", "my_commitments"} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("prompt omitted %q: %s", text, prompt)
		}
	}
	// Neither latest nor auto invokes a provider, even if the command now fails.
	recapTestRunner(t, p, "", "exit 23", 5)
	for _, args := range [][]string{{"--latest"}, {"--stdin", "--auto"}} {
		out, err := run(string(payload), args...)
		var latest recapView
		if err != nil || json.Unmarshal(out, &latest) != nil || latest != got {
			t.Fatalf("saved reuse: %v %s", err, out)
		}
	}
	for _, mode := range []string{"--status", "--enqueue", "--cancel"} {
		out, err := run("", mode)
		var status recapStatusView
		if err != nil || json.Unmarshal(out, &status) != nil || status.State != "ready" || status.RecapID != got.RecapID || status.NotePath != note.Path {
			t.Fatalf("ready %s: %v %s", mode, err, out)
		}
	}
	before, _ := filepath.Glob(filepath.Join(note.Directory, "recaps", "*", "request.json"))
	for _, input := range []string{
		`{}`, `null`, `{"notes":null,"note_path":"x"}`, `{"notes":"x","note_path":"x","extra":true}`,
		`{"notes":"x","notes":"y","note_path":"x"}`, `{"notes":"x","note_path":"x"} {}`,
		`{"notes":"x","note_path":4}`, `{"notes":"x","note_path":""}`, strings.Repeat("x", assistant.MaxContextBytes+1),
	} {
		if out, err := run(input, "--stdin"); err == nil {
			t.Fatalf("accepted malformed input: %.100s %s", input, out)
		}
	}
	for _, args := range [][]string{{"--stdin", "--latest"}, {"--auto"}, {"extra"}} {
		if out, err := run("", args...); err == nil {
			t.Fatalf("accepted flags %v: %s", args, out)
		}
	}
	after, _ := filepath.Glob(filepath.Join(note.Directory, "recaps", "*", "request.json"))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("bad CLI requests reached provider")
	}
}

func TestRecapResponseValidation(t *testing.T) {
	_, note := recapTestFixture(t)
	snapshot, err := readTranscript(note.Directory, note.ID)
	if err != nil {
		t.Fatal(err)
	}
	good := recapTestResponse("Friday is tentative. [[passage:system-1]]")
	for name, response := range map[string]string{
		"unknown field":   strings.Replace(good, `"summary":`, `"extra":"bad","summary":`, 1),
		"duplicate field": strings.Replace(good, `"summary":`, `"summary":"bad","summary":`, 1),
		"missing":         `{}`, "trailing": good + ` {}`, "fence": "```json\n" + good + "\n```",
		"null":          strings.Replace(good, `"Friday planning"`, "null", 1),
		"title newline": strings.Replace(good, `"Friday planning"`, `"Friday\nplanning"`, 1),
		"heading":       recapTestResponse("## Unwanted\ntext"), "setext": recapTestResponse("Extra heading\n===="),
		"unclosed backticks":  recapTestResponse("Some summary\n```"),
		"unclosed tildes":     recapTestResponse("Some summary\n~~~text\ncode"),
		"short closing fence": recapTestResponse("````text\ncode\n```"),
		"identity":            recapTestResponse("<!-- trailmix: meeting other -->"), "region": recapTestResponse("<!-- trailmix: recap end -->"),
		"raw citation link": recapTestResponse("[01:02](trailmix://meetings/x/passages/x)"),
		"web link":          recapTestResponse("https://example.com"), "other link": recapTestResponse("[a](relative.md)"),
		"reference link":  recapTestResponse("A claim\n[x]: relative.md"),
		"missing passage": recapTestResponse("Claim [[passage:missing]]"), "malformed citation": recapTestResponse("Claim [[passage:system-1"),
		"empty": recapTestResponse("   "),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := renderRecap(response, snapshot); err == nil {
				t.Fatal("accepted", response)
			}
		})
	}
	title, markdown, err := renderRecap(good, snapshot)
	if err != nil || title != "Friday planning" || strings.Contains(markdown, "Coverage notice") {
		t.Fatalf("valid recap: %s %s %v", title, markdown, err)
	}
}

func TestRecapAdjacentCitationsAreNotReferenceLinks(t *testing.T) {
	_, note := recapTestFixture(t)
	snapshot, err := readTranscript(note.Directory, note.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, separator := range []string{"", " ", "  ", "\n", "\n\n"} {
		t.Run(fmt.Sprintf("separator-%q", separator), func(t *testing.T) {
			var fields map[string]string
			if err := json.Unmarshal([]byte(recapTestResponse("A supported claim.")), &fields); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"summary", "observations", "questions_to_follow_up", "my_commitments"} {
				fields[key] = "Supported text. [[passage:system-1]]" + separator + "[[passage:microphone-1]]"
			}
			response, _ := json.Marshal(fields)
			_, markdown, err := renderRecap(string(response), snapshot)
			if err != nil {
				t.Fatalf("adjacent valid citations rejected: %v", err)
			}
			for _, id := range []string{"system-1", "microphone-1"} {
				if strings.Count(markdown, "trailmix://meetings/"+note.ID+"/passages/"+id) != 4 {
					t.Fatalf("lost evidence links for %s: %s", id, markdown)
				}
			}
			for _, invalid := range []string{
				"[[passage:missing]]", "[[passage:bad/id]]", "[[passage:system-1",
				"[label][reference]", "[label](relative.md)", "\n[reference]: relative.md", "https://example.com",
				"[label][[passage:system-1]]", "[[passage:system-1]][reference]", "[[passage:system-1]](relative.md)",
			} {
				fields["questions_to_follow_up"] = "[[passage:system-1]] " + invalid
				response, _ = json.Marshal(fields)
				if _, _, err := renderRecap(string(response), snapshot); err == nil {
					t.Fatalf("adjacent-citation exception allowed invalid content: %s", invalid)
				}
			}
		})
	}
}

func TestRecapPromptExcludesUnfinalizedText(t *testing.T) {
	_, note := recapTestFixture(t)
	snapshot, err := readTranscript(note.Directory, note.ID)
	if err != nil {
		t.Fatal(err)
	}
	track := snapshot.Transcription.Tracks["microphone"]
	track.Provisional = map[string]string{"unfinished": "UNFINALIZED_COMMITMENT_DO_NOT_USE"}
	snapshot.Transcription.Tracks["microphone"] = track
	notes := string(readTitleTest(t, note.Path))
	prompt, err := recapPrompt(recapInput{Notes: &notes}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "UNFINALIZED_COMMITMENT_DO_NOT_USE") || strings.Contains(prompt, `"provisional"`) {
		t.Fatal("unfinished provider text leaked into recap evidence")
	}
	if !strings.Contains(prompt, "Friday is tentative.") || !strings.Contains(prompt, "Transcription coverage is incomplete") {
		t.Fatal("filtering lost finalized evidence or missing-coverage metadata")
	}
	if snapshot.Transcription.Tracks["microphone"].Provisional["unfinished"] != "UNFINALIZED_COMMITMENT_DO_NOT_USE" {
		t.Fatal("prompt filtering changed the retained source snapshot")
	}
}

func TestRecapBalancedCodeFences(t *testing.T) {
	for _, text := range []string{"ordinary text", "```go\nfmt.Println(1)\n```", "~~~text\nnotes\n~~~~", "~~~`\ncode\n~~~", "````\n```\n````"} {
		if !recapBalancedFences(text) {
			t.Fatalf("rejected balanced section: %q", text)
		}
	}
	for _, text := range []string{"```", "  ~~~text\nunfinished", "````\ncode\n```", "```\ncode\n```invalid"} {
		if recapBalancedFences(text) {
			t.Fatalf("accepted unclosed section: %q", text)
		}
	}
}

func TestRecapCoverage(t *testing.T) {
	p, note := recapTestFixture(t)
	snapshot, err := readTranscript(note.Directory, note.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, condition := range []string{"failed", "missing", "unknown boundary", "incomplete", "missing track", "pending", "no passages", "track error"} {
		t.Run(condition, func(t *testing.T) {
			data, _ := json.Marshal(snapshot)
			var degraded transcriptSnapshot
			_ = json.Unmarshal(data, &degraded)
			switch condition {
			case "failed":
				degraded.Capture.State = "failed"
			case "missing":
				degraded.Capture = nil
			case "unknown boundary":
				degraded.Capture.LastCapturedSeconds = nil
			case "incomplete":
				degraded.Transcription.State = "degraded"
			case "missing track":
				delete(degraded.Transcription.Tracks, "system")
			case "no passages":
				degraded.Passages = nil
			case "pending", "track error":
				track := degraded.Transcription.Tracks["system"]
				if condition == "pending" {
					track.Pending = []transcription.Range{{Start: 20, End: 30}}
				} else {
					track.Error = "failed"
				}
				degraded.Transcription.Tracks["system"] = track
			}
			warning := recapCoverage(degraded)
			if warning == "" || !strings.Contains(warning, "Missing evidence") {
				t.Fatalf("no warning: %s", warning)
			}
			if recapCoverage(degraded) != warning {
				t.Fatal("nondeterministic warning")
			}
		})
	}
	// A failed capture with fully transcribed saved audio is still usable manually.
	if err := meeting.WriteJSON(filepath.Join(note.Directory, "session.json"), status{MeetingID: note.ID, State: "failed", Error: "microphone unplugged"}); err != nil {
		t.Fatal(err)
	}
	view, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false)
	if err != nil || !strings.HasPrefix(view.Markdown, "## Summary\n\nCoverage notice:") {
		t.Fatalf("degraded recap: %v %s", err, view.Markdown)
	}
}

func TestRecapCoverageDoesNotInjectMarkdown(t *testing.T) {
	_, note := recapTestFixture(t)
	snapshot, err := readTranscript(note.Directory, note.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Capture.State = "failed"
	snapshot.Capture.Error = "microphone unplugged\n\n## Injected\n<!-- trailmix: recap begin --> [[passage:missing]] trailmix://bad"
	snapshot.Capture.Warnings = []string{"[bad](https://example.com)"}
	_, markdown, err := renderRecap(recapTestResponse("Friday is tentative. [[passage:system-1]]"), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(markdown, "microphone unplugged") || !strings.Contains(markdown, "Last observed audio boundary: 02:00") {
		t.Fatalf("coverage lacks detail: %s", markdown)
	}
	for _, injected := range []string{"## Injected", "<!--", "[[passage:missing]]", "trailmix://bad", "[bad](https://"} {
		if strings.Contains(markdown, injected) {
			t.Fatalf("unsafe health text %q: %s", injected, markdown)
		}
	}
}

func TestRecapRoughNotes(t *testing.T) {
	begin, end := "<!-- trailmix: recap begin -->", "<!-- trailmix: recap end -->"
	for _, fence := range []string{"```", "~~~~"} {
		input := "before\n" + fence + "\n" + begin + "\nquoted\n" + end + "\n" + fence + "\n" + begin + "\nold unsupported claim\n" + end + "\nafter\n"
		got, err := recapRoughNotes(input)
		want := "before\n" + fence + "\n" + begin + "\nquoted\n" + end + "\n" + fence + "\nafter\n"
		if err != nil || got != want {
			t.Fatalf("fences: %v %q", err, got)
		}
	}
	for _, input := range []string{begin, end, end + "\n" + begin, begin + "\n" + begin + "\n" + end, begin + "\n" + end + "\n" + begin + "\n" + end, begin + "\n" + end + "\n" + end} {
		if _, err := recapRoughNotes(input); err == nil {
			t.Fatalf("accepted invalid regions: %s", input)
		}
	}
	p, note := recapTestFixture(t)
	original := string(readTitleTest(t, note.Path))
	input := original + begin + "\nAn old hallucination\n" + end + "\nMy new question\n"
	view, err := generateRecap(context.Background(), p, note.ID, recapInput{Notes: &input, NotePath: note.Path}, false)
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(readTitleTest(t, filepath.Join(filepath.Dir(view.Path), "received.txt")))
	if strings.Contains(prompt, "An old hallucination") || !strings.Contains(prompt, "My new question") || view.NotesSnapshot != input {
		t.Fatalf("bad regeneration snapshot: %s %+v", prompt, view)
	}
}

func TestRecapWriterLocksAndInputValidation(t *testing.T) {
	for _, kind := range []string{"capture", "transcription"} {
		t.Run(kind, func(t *testing.T) {
			p, note := recapTestFixture(t)
			path := filepath.Join(p.home, "recorder.lock")
			if kind == "capture" {
				path = filepath.Join(note.Directory, "capture.lock")
			}
			if kind == "transcription" {
				path = filepath.Join(note.Directory, "transcription", "writer.lock")
			}
			guard, err := recapExclusive(path)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			if _, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false); err == nil {
				t.Fatal("writer not blocked")
			}
			requests, _ := filepath.Glob(filepath.Join(note.Directory, "recaps", "*"))
			if len(requests) != 0 {
				t.Fatalf("provider request started: %v", requests)
			}
		})
	}
	p, note := recapTestFixture(t)
	for _, input := range []recapInput{
		{Notes: ptr("wrong identity")}, {NotePath: filepath.Join(p.home, "not-a-note")},
		{Notes: ptr(string(readTitleTest(t, note.Path)) + "<!-- trailmix: recap begin -->")},
		{Notes: ptr(string(readTitleTest(t, note.Path)) + strings.Repeat("a", assistant.MaxContextBytes))},
	} {
		if _, err := generateRecap(context.Background(), p, note.ID, input, false); err == nil {
			t.Fatal("accepted invalid snapshot")
		}
	}
	requests, _ := filepath.Glob(filepath.Join(note.Directory, "recaps", "*"))
	if len(requests) != 0 {
		t.Fatalf("bad snapshots reached provider: %v", requests)
	}
}

func TestRecapFailureRetentionAndLatest(t *testing.T) {
	p, note := recapTestFixture(t)
	prior, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false)
	if err != nil {
		t.Fatal(err)
	}
	for name, commands := range map[string]string{"runner": "exit 19", "timeout": "sleep 10", "citation": "", "unclosed fence": ""} {
		t.Run(name, func(t *testing.T) {
			response := recapTestResponse("Bad [[passage:missing]]")
			if name == "unclosed fence" {
				response = recapTestResponse("Some summary\n```")
			}
			recapTestRunner(t, p, response, commands, 1)
			if _, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false); err == nil || !strings.Contains(err.Error(), "request retained") {
				t.Fatalf("failure not retained: %v", err)
			}
			latest, err := latestRecap(context.Background(), p, note.ID)
			if err != nil || latest != prior {
				t.Fatalf("failure replaced prior success: %+v %v", latest, err)
			}
		})
	}
	requests, _ := filepath.Glob(filepath.Join(note.Directory, "recaps", "*", "request.json"))
	failed := 0
	for _, path := range requests {
		var record recapRecord
		if err := json.Unmarshal(readTitleTest(t, path), &record); err != nil {
			t.Fatal(err)
		}
		if record.State == "failed" {
			failed++
			if record.Error == "" {
				t.Fatal("missing saved error")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), "runner.json")); err != nil {
				t.Fatal("missing runner failure", err)
			}
		}
	}
	if failed != 4 {
		t.Fatalf("failed records: %d", failed)
	}
}

func recapTestAwait(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestRecapAutoSerializesAndRenameDoesNotBlock(t *testing.T) {
	p, note := recapTestFixture(t)
	started, release := filepath.Join(p.home, "started"), filepath.Join(p.home, "release")
	recapTestRunner(t, p, recapTestResponse("Friday remains tentative. [[passage:system-1]]"), "touch '"+started+"'\nwhile [ ! -f '"+release+"' ]; do sleep 0.02; done", 5)
	var views [2]recapView
	var errs [2]error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		views[0], errs[0] = generateRecap(context.Background(), p, note.ID, recapInput{}, true)
	}()
	recapTestAwait(t, started)
	wg.Add(1)
	go func() {
		defer wg.Done()
		views[1], errs[1] = generateRecap(context.Background(), p, note.ID, recapInput{}, true)
	}()
	if _, err := latestRecap(context.Background(), p, note.ID); !errors.Is(err, errNoRecap) {
		t.Fatalf("unready latest published: %v", err)
	}
	recorder, err := lock(p)
	if err != nil {
		t.Fatal("global lock held during provider", err)
	}
	recorder.Close()
	renamed, err := retitleForTest(p, note, "Renamed while thinking")
	if err != nil {
		t.Fatal("rename blocked by provider", err)
	}
	recapTestWrite(t, release, "", 0600)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || views[0] != views[1] {
		t.Fatalf("duplicate autos: %+v %v", views, errs)
	}
	requests, _ := filepath.Glob(filepath.Join(note.Directory, "recaps", "*", "request.json"))
	if len(requests) != 1 {
		t.Fatalf("duplicate provider requests: %v", requests)
	}
	latest, err := latestRecap(context.Background(), p, note.ID)
	if err != nil || latest != views[0] {
		t.Fatalf("latest after rename: %+v %v", latest, err)
	}
	if _, err := os.Stat(renamed.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(note.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old note recreated")
	}
}

func TestRecapCancellationDoesNotPublish(t *testing.T) {
	p, note := recapTestFixture(t)
	prior, err := generateRecap(context.Background(), p, note.ID, recapInput{}, false)
	if err != nil {
		t.Fatal(err)
	}
	started, release := filepath.Join(p.home, "started"), filepath.Join(p.home, "release")
	recapTestRunner(t, p, recapTestResponse("Friday [[passage:system-1]]"), "touch '"+started+"'\nwhile [ ! -f '"+release+"' ]; do sleep 0.02; done", 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := generateRecap(ctx, p, note.ID, recapInput{}, false); done <- err }()
	recapTestAwait(t, started)
	guard, err := artifactLock(context.Background(), p, note.ID)
	if err != nil {
		t.Fatal(err)
	}
	recapTestWrite(t, release, "", 0600)
	// Wait for the runner result while blocking final publication.
	deadline := time.Now().Add(5 * time.Second)
	for {
		runners, _ := filepath.Glob(filepath.Join(note.Directory, "recaps", "*", "runner.json"))
		if len(runners) == 2 {
			break
		}
		if time.Now().After(deadline) {
			guard.Close()
			t.Fatal("runner did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	guard.Close()
	if err := <-done; !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "request retained") {
		t.Fatalf("cancellation: %v", err)
	}
	latest, err := latestRecap(context.Background(), p, note.ID)
	if err != nil || latest != prior {
		t.Fatalf("cancelled request published: %+v %v", latest, err)
	}
}
