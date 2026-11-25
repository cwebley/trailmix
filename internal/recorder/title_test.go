package recorder

import (
	"bytes"
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
	"github.com/cwebley/trailmix/internal/navigation"
	"github.com/cwebley/trailmix/internal/settings"
)

func titleFixture(t *testing.T) (paths, meetingNote) {
	t.Helper()
	p := testPaths(t)
	now := time.Date(2026, 11, 18, 9, 7, 43, 123456789, time.FixedZone("meeting", -7*3600))
	note, err := newNote(p, settings.Settings{NotesRoot: filepath.Join(p.home, "notes")}, now)
	if err != nil {
		t.Fatal(err)
	}
	note.Directory = filepath.Join(p.home, "recording")
	if err := os.MkdirAll(note.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveMeeting(p, note); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(p.home, "latest-meeting.json")} {
		if err := meeting.WriteJSON(path, note); err != nil {
			t.Fatal(err)
		}
	}
	s := status{Note: &note, MeetingID: note.ID, Directory: note.Directory, State: "stopped", StartedAt: now.Add(time.Second), ElapsedSeconds: 123.5}
	for _, path := range []string{filepath.Join(note.Directory, "session.json"), filepath.Join(p.home, "last.json")} {
		if err := meeting.WriteJSON(path, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := meeting.Ensure(note.Directory, note.ID); err != nil {
		t.Fatal(err)
	}
	transcriptDir := filepath.Join(note.Directory, "transcription")
	if err := os.MkdirAll(transcriptDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := meeting.WriteJSON(filepath.Join(transcriptDir, "status.json"), map[string]any{"meeting_id": note.ID, "state": "complete", "tracks": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	return p, note
}

func noTitleEditors(string, navigation.Target) error                  { return nil }
func noTitlePanes(string, navigation.Target, navigation.Target) error { return nil }
func retitleForTest(p paths, note meetingNote, title string) (meetingNote, error) {
	return titleMeetingWithNavigation(p, note.ID, title, noTitleEditors, noTitlePanes)
}
func readTitleTest(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func assertTitleAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected file %s: %v", path, err)
	}
}

func TestTitleRenamesBundleAndMetadata(t *testing.T) {
	p, before := titleFixture(t)
	body := "<!-- trailmix: meeting " + before.ID + " -->\n# Meeting 2026-11-18 09:07:43\n\nKeep my notes.\n"
	if err := os.WriteFile(before.Path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	evidence := "[00:02](trailmix://meetings/" + before.ID + "/passages/system-1)\n"
	for _, kind := range []string{"transcript", "answers"} {
		if err := os.WriteFile(sidecarPath(before, kind), []byte(artifactMarker(before.ID, kind)+evidence), 0600); err != nil {
			t.Fatal(err)
		}
	}
	after, err := retitleForTest(p, before, "Engineering product standup")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(after.Path) != "09-07-engineering-product-standup.md" || after.ID != before.ID || after.Directory != before.Directory || !after.StartedAt.Equal(before.StartedAt) || after.StartedAt.Nanosecond() != 123456789 {
		t.Fatalf("lost identity/start: %+v", after)
	}
	if after.Window != "09:07 Engineering product standup" || after.Session != before.Session {
		t.Fatalf("bad navigation: %+v", after)
	}
	if got := string(readTitleTest(t, after.Path)); got != strings.Replace(body, "# Meeting 2026-11-18 09:07:43", "# Engineering product standup", 1) {
		t.Fatalf("changed note body: %s", got)
	}
	assertTitleAbsent(t, before.Path)
	for _, kind := range []string{"transcript", "answers"} {
		if got := string(readTitleTest(t, sidecarPath(after, kind))); got != artifactMarker(before.ID, kind)+evidence {
			t.Fatalf("changed citations: %s", got)
		}
		assertTitleAbsent(t, sidecarPath(before, kind))
	}
	for _, path := range []string{filepath.Join(p.home, "meetings", before.ID+".json"), filepath.Join(p.home, "latest-meeting.json")} {
		var note meetingNote
		if err := json.Unmarshal(readTitleTest(t, path), &note); err != nil || note.Path != after.Path || note.Title != after.Title {
			t.Fatalf("stale %s: %+v %v", path, note, err)
		}
	}
	for _, path := range []string{filepath.Join(p.home, "last.json"), filepath.Join(before.Directory, "session.json")} {
		var s status
		if err := json.Unmarshal(readTitleTest(t, path), &s); err != nil || s.Note == nil || s.Note.Path != after.Path || !s.StartedAt.Equal(before.StartedAt.Add(time.Second)) || s.ElapsedSeconds != 123.5 {
			t.Fatalf("changed status %s: %+v %v", path, s, err)
		}
	}
	id, err := meeting.Read(before.Directory)
	if err != nil || id != before.ID {
		t.Fatalf("identity changed: %s %v", id, err)
	}
	// Repeating the same title changes no basename and needs no suffix.
	again, err := retitleForTest(p, after, after.Title)
	if err != nil || again.Path != after.Path {
		t.Fatalf("non-idempotent title: %+v %v", again, err)
	}
}

func TestTitleBundleCollisionsNeverOverwrite(t *testing.T) {
	for _, collision := range []string{"note", "answers", "transcript", "directory", "symlink"} {
		t.Run(collision, func(t *testing.T) {
			p, before := titleFixture(t)
			dest := filepath.Join(filepath.Dir(before.Path), "09-07-standup.md")
			switch collision {
			case "answers":
				dest = strings.TrimSuffix(dest, ".md") + ".answers.md"
			case "transcript":
				dest = strings.TrimSuffix(dest, ".md") + ".transcript.md"
			}
			if collision == "directory" {
				if err := os.Mkdir(dest, 0700); err != nil {
					t.Fatal(err)
				}
			} else if collision == "symlink" {
				if err := os.Symlink(filepath.Join(p.home, "missing"), dest); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(dest, []byte("user-owned"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			after, err := retitleForTest(p, before, "Standup")
			if err != nil || filepath.Base(after.Path) != "09-07-standup-2.md" {
				t.Fatalf("collision not skipped: %+v %v", after, err)
			}
			info, err := os.Lstat(dest)
			if err != nil {
				t.Fatal(err)
			}
			if collision == "symlink" && info.Mode()&os.ModeSymlink == 0 {
				t.Fatal("replaced symlink")
			}
			if collision != "directory" && collision != "symlink" && string(readTitleTest(t, dest)) != "user-owned" {
				t.Fatal("overwrote unrelated file")
			}
		})
	}
}

func TestTitleRejectsUnsafeSourcesAndMetadata(t *testing.T) {
	for _, kind := range []string{"note-symlink", "answers", "transcript", "metadata-symlink"} {
		t.Run(kind, func(t *testing.T) {
			p, before := titleFixture(t)
			original := readTitleTest(t, before.Path)
			path := before.Path
			switch kind {
			case "note-symlink":
				target := filepath.Join(p.home, "real.md")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "metadata-symlink":
				path = filepath.Join(p.home, "last.json")
				target := filepath.Join(p.home, "real-last.json")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			default:
				path = sidecarPath(before, kind)
				if err := os.WriteFile(path, []byte("not app-owned"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := retitleForTest(p, before, "Standup"); err == nil {
				t.Fatal("unsafe rename accepted")
			}
			if !bytes.Equal(readTitleTest(t, before.Path), original) {
				t.Fatal("note changed on refusal")
			}
			assertTitleAbsent(t, filepath.Join(filepath.Dir(before.Path), "09-07-standup.md"))
		})
	}
}

func TestTitleRefusesRecorderWriterAndEditors(t *testing.T) {
	p, note := titleFixture(t)
	guard, err := lock(p)
	if err != nil {
		t.Fatal(err)
	}
	// Covers a teardown writer even if status already says stopped and socket is gone.
	if _, err := retitleForTest(p, note, "Standup"); err == nil || !strings.Contains(err.Error(), "trailmix stop") {
		t.Fatalf("writer not refused: %v", err)
	}
	guard.Close()
	_, err = titleMeetingWithNavigation(p, note.ID, "Standup", func(string, navigation.Target) error { return errors.New("close hidden editor buffers") }, noTitlePanes)
	if err == nil || !strings.Contains(err.Error(), "close hidden") {
		t.Fatalf("editor not refused: %v", err)
	}
	if _, err := os.Stat(note.Path); err != nil {
		t.Fatal(err)
	}
}

func TestTitleRollsBackNavigationFailure(t *testing.T) {
	p, note := titleFixture(t)
	original := readTitleTest(t, note.Path)
	canonical := readTitleTest(t, filepath.Join(p.home, "meetings", note.ID+".json"))
	calls := 0
	_, err := titleMeetingWithNavigation(p, note.ID, "Standup", noTitleEditors, func(_ string, before, after navigation.Target) error {
		calls++
		if calls == 1 {
			return errors.New("tmux failed after mutation")
		}
		return nil
	})
	if err == nil || calls != 2 {
		t.Fatalf("did not undo navigation: calls=%d %v", calls, err)
	}
	if !bytes.Equal(original, readTitleTest(t, note.Path)) || !bytes.Equal(canonical, readTitleTest(t, filepath.Join(p.home, "meetings", note.ID+".json"))) {
		t.Fatal("rollback lost originals")
	}
	assertTitleAbsent(t, filepath.Join(filepath.Dir(note.Path), "09-07-standup.md"))
}

func TestTitleExclusivePublication(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "new.md")
	target := filepath.Join(dir, "other.md")
	if err := os.WriteFile(target, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dest); err != nil {
		t.Fatal(err)
	}
	if err := createTitleFile(dest, []byte("new"), 0600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrote symlink: %v", err)
	}
	if string(readTitleTest(t, target)) != "other" {
		t.Fatal("followed destination symlink")
	}
}

func TestTitlePreservesUnrelatedPointersAndUnknownFields(t *testing.T) {
	p, note := titleFixture(t)
	other := []byte(`{"id":"ffffffffffffffffffffffffffffffff","custom":true}`)
	if err := os.WriteFile(filepath.Join(p.home, "latest-meeting.json"), other, 0600); err != nil {
		t.Fatal(err)
	}
	last := []byte(`{"meeting_id":"ffffffffffffffffffffffffffffffff","custom":true}`)
	if err := os.WriteFile(filepath.Join(p.home, "last.json"), last, 0600); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(note.Directory, "session.json")
	var session map[string]any
	if err := json.Unmarshal(readTitleTest(t, sessionPath), &session); err != nil {
		t.Fatal(err)
	}
	session["custom"] = "preserve"
	session["note"].(map[string]any)["extension"] = "keep"
	if err := meeting.WriteJSON(sessionPath, session); err != nil {
		t.Fatal(err)
	}
	if _, err := retitleForTest(p, note, "Standup"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(other, readTitleTest(t, filepath.Join(p.home, "latest-meeting.json"))) || !bytes.Equal(last, readTitleTest(t, filepath.Join(p.home, "last.json"))) {
		t.Fatal("changed newer pointers")
	}
	data := string(readTitleTest(t, sessionPath))
	if !strings.Contains(data, `"custom": "preserve"`) || !strings.Contains(data, `"extension": "keep"`) {
		t.Fatalf("lost unknown fields: %s", data)
	}
}

func TestTitleInputAndOriginalClock(t *testing.T) {
	for _, title := range []string{"", "   ", "line\nbreak", "tab\tbreak", strings.Repeat("a", 201)} {
		if _, err := cleanTitle(title); err == nil {
			t.Fatalf("accepted %q", title)
		}
	}
	if got := titleSlug(" .answers / Engineering & PRODUCT standup!!!"); got != "answers-engineering-product-standup" {
		t.Fatal(got)
	}
	if got := titleSlug("???"); got != "meeting" {
		t.Fatal(got)
	}
	p, note := titleFixture(t)
	note.StartedAt = time.Time{} // Old associations still retain their seconds filename.
	if err := saveMeeting(p, note); err != nil {
		t.Fatal(err)
	}
	after, err := retitleForTest(p, note, "Legacy")
	if err != nil || filepath.Base(after.Path) != "09-07-legacy.md" {
		t.Fatalf("lost legacy clock: %+v %v", after, err)
	}
}

func TestExportResolvesAssociationAfterTitle(t *testing.T) {
	p, before := titleFixture(t)
	after, err := retitleForTest(p, before, "Standup")
	if err != nil {
		t.Fatal(err)
	}
	// Export was handed a stale association before waiting for the artifact lock.
	path, err := exportTranscript(p, before)
	if err != nil || path != sidecarPath(after, "transcript") {
		t.Fatalf("stale export: %s %v", path, err)
	}
	assertTitleAbsent(t, sidecarPath(before, "transcript"))
}

func TestAskPublishesToCurrentTitleAfterProviderWait(t *testing.T) {
	p, before := titleFixture(t)
	gate := filepath.Join(p.home, "gate")
	if err := syscall.Mkfifo(gate, 0600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(p.home, "ready")
	cfg, err := settings.Read()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Assistant = settings.Assistant{Command: []string{"/bin/sh", "-c", `printf ready > "$1"; cat "$2" >/dev/null; printf 'No decision yet.'`, "fixture", ready, gate}, Format: "markdown", TimeoutSeconds: 10}
	configPath, err := settings.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := meeting.WriteJSON(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type outcome struct {
		view answersView
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		view, err := ask(ctx, p, before, askInput{Question: "What happened?"})
		done <- outcome{view, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	after, err := retitleForTest(p, before, "Standup")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(gate, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	got := <-done
	if got.err != nil || got.view.Path != sidecarPath(after, "answers") || got.view.TranscriptPath != sidecarPath(after, "transcript") {
		t.Fatalf("stale answer publication: %+v %v", got.view, got.err)
	}
	assertTitleAbsent(t, sidecarPath(before, "answers"))
	assertTitleAbsent(t, sidecarPath(before, "transcript"))
}

func TestTitleHeadingSkipsFencedExamples(t *testing.T) {
	for _, fence := range []string{"```markdown", "~~~markdown"} {
		close := fence[:3]
		input := "<!-- trailmix: meeting id -->\n" + fence + "\n# Example, not the title\n" + close + "\n# Real title\nKeep me.\n"
		got := string(titleHeading([]byte(input), "Standup"))
		if got != strings.Replace(input, "# Real title", "# Standup", 1) {
			t.Fatalf("changed fenced heading: %s", got)
		}
	}
}

func TestTitleRollbackPreservesChangedDestinationAndMetadata(t *testing.T) {
	for _, changed := range []string{"destination", "metadata"} {
		t.Run(changed, func(t *testing.T) {
			p, note := titleFixture(t)
			dest := filepath.Join(filepath.Dir(note.Path), "09-07-standup.md")
			canonical := filepath.Join(p.home, "meetings", note.ID+".json")
			calls := 0
			_, err := titleMeetingWithNavigation(p, note.ID, "Standup", noTitleEditors, func(_ string, _, after navigation.Target) error {
				calls++
				if calls > 1 {
					return nil
				}
				target := dest
				if changed == "metadata" {
					target = canonical
				}
				if err := os.WriteFile(target, []byte("external text"), 0600); err != nil {
					t.Fatal(err)
				}
				return errors.New("tmux failed")
			})
			if err == nil || !strings.Contains(err.Error(), "retained title files") || !strings.Contains(err.Error(), dest) {
				t.Fatalf("missing recovery details: %v", err)
			}
			target := dest
			if changed == "metadata" {
				target = canonical
			}
			if got := string(readTitleTest(t, target)); got != "external text" {
				t.Fatalf("lost external text: %s", got)
			}
			if _, err := os.Stat(dest); err != nil {
				t.Fatal("removed recovery copy", err)
			}
			if _, err := os.Stat(note.Path); err != nil {
				t.Fatal("lost original", err)
			}
		})
	}
}

func TestTitleRollbackRetainsOnlyCopyWhenSourceCannotRestore(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "old.md")
	dest := filepath.Join(root, "new.md")
	metadata := filepath.Join(root, "meeting.json")
	for path, text := range map[string]string{source: "external replacement", dest: "only remaining original", metadata: "old metadata"} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	originalMetadata, err := readTitleFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := replaceTitleFile(originalMetadata, []byte("new metadata"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := readTitleFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	undone := false
	err = rollbackTitle([]titleFile{created}, []titleReplacement{replacement}, []titleFile{{path: source, data: []byte("only remaining original"), mode: 0600}}, func() error { undone = true; return nil })
	if err == nil || !strings.Contains(err.Error(), dest) {
		t.Fatalf("restoration collision ignored: %v", err)
	}
	if undone || string(readTitleTest(t, metadata)) != "new metadata" {
		t.Fatal("moved association back before source restored")
	}
	if string(readTitleTest(t, dest)) != "only remaining original" || string(readTitleTest(t, source)) != "external replacement" {
		t.Fatal("rollback lost a copy")
	}
}
