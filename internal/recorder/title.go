package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/cwebley/trailmix/internal/cliout"
	"github.com/cwebley/trailmix/internal/meeting"
	"github.com/cwebley/trailmix/internal/navigation"
)

func titleCommand(p paths, args []string) error {
	flags := flag.NewFlagSet("title", flag.ContinueOnError)
	id := flags.String("meeting", "", "meeting ID from the note")
	pending := flags.Bool("pending", false, "retry the saved automatic title without an assistant request")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !meeting.ValidID(*id) || (*pending && flags.NArg() != 0) || (!*pending && flags.NArg() != 1) {
		return errors.New("usage: trailmix title --meeting ID 'Meeting title' | --pending")
	}
	if *pending {
		// Run already holds workspace.lock for public title commands.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		intent, err := retryPendingTitleLocked(ctx, p, *id)
		if err != nil {
			return err
		}
		return cliout.JSON(intent, false)
	}
	note, err := titleMeeting(p, *id, flags.Arg(0))
	if err != nil {
		return err
	}
	return cliout.JSON(note, false)
}

func cleanTitle(title string) (string, error) {
	for _, r := range title {
		if unicode.IsControl(r) {
			return "", errors.New("title must be a single line without control characters")
		}
	}
	title = strings.Join(strings.Fields(title), " ")
	if title == "" || len([]rune(title)) > 200 {
		return "", errors.New("title must contain 1–200 characters")
	}
	return title, nil
}

func titleSlug(title string) string {
	var out strings.Builder
	separator := false
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separator && out.Len() > 0 {
				out.WriteByte('-')
			}
			if out.Len()+len(string(r)) > 120 {
				break
			}
			out.WriteRune(r)
			separator = false
		} else {
			separator = true
		}
	}
	slug := strings.Trim(out.String(), "-")
	if slug == "" {
		return "meeting"
	}
	return slug
}

var noteClock = regexp.MustCompile(`^(\d{2})-(\d{2})(?:-|\.)`)
var windowClock = regexp.MustCompile(`^([0-2][0-9]):([0-5][0-9])(?: |$)`)

func titleClock(note meetingNote) (string, error) {
	if match := noteClock.FindStringSubmatch(filepath.Base(note.Path)); match != nil && match[1] <= "23" && match[2] <= "59" {
		return match[1] + "-" + match[2], nil
	}
	if match := windowClock.FindStringSubmatch(note.Window); match != nil && match[1] <= "23" {
		return match[1] + "-" + match[2], nil
	}
	if !note.StartedAt.IsZero() {
		return note.StartedAt.Format("15-04"), nil
	}
	return "", errors.New("meeting has no original start time; refusing to invent one")
}

func titleHeading(data []byte, title string) []byte {
	lines := strings.Split(string(data), "\n")
	var fence byte
	var fenceSize int
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if indent <= 3 && len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			count := 0
			for count < len(trimmed) && trimmed[count] == trimmed[0] {
				count++
			}
			if count >= 3 {
				if fence == 0 {
					fence, fenceSize = trimmed[0], count
					continue
				}
				if trimmed[0] == fence && count >= fenceSize && strings.TrimSpace(trimmed[count:]) == "" {
					fence = 0
					continue
				}
			}
		}
		if fence == 0 && strings.HasPrefix(line, "# ") {
			lines[i] = "# " + title
			return []byte(strings.Join(lines, "\n"))
		}
	}
	// Keep the identity marker as the first line, including for custom templates.
	lines = append(lines[:1], append([]string{"# " + title, ""}, lines[1:]...)...)
	return []byte(strings.Join(lines, "\n"))
}

type titleFile struct {
	path string
	data []byte
	mode os.FileMode
	info os.FileInfo
}

func readTitleFile(path string) (titleFile, error) {
	file := titleFile{path: path}
	info, err := os.Lstat(path)
	if err != nil {
		return file, err
	}
	if !info.Mode().IsRegular() {
		return file, fmt.Errorf("refusing non-regular file or symlink: %s", path)
	}
	file.mode = info.Mode().Perm()
	file.info = info
	file.data, err = os.ReadFile(path)
	return file, err
}

// Publish by linking a fully synced temporary file. Unlike Rename, Link never
// replaces an existing file, directory, or symlink, including a racing creator.
func createTitleFile(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".trailmix-title-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := errors.Join(temp.Chmod(mode), writeSyncClose(temp, data)); err != nil {
		return err
	}
	return os.Link(temp.Name(), path)
}

func writeSyncClose(f *os.File, data []byte) error {
	_, err := f.Write(data)
	return errors.Join(err, f.Sync(), f.Close())
}

// The CLI holds the lifecycle lock. Refuse capture/retry rather than attempting
// to coordinate note writes with an active recorder. Artifact writers must
// resolve the association after obtaining the same meeting-ID lock.
func titleMeeting(p paths, id, rawTitle string) (meetingNote, error) {
	return titleMeetingWithNavigation(p, id, rawTitle, navigation.CheckTitleSafety, navigation.Retitle)
}

func titleMeetingWithNavigation(p paths, id, rawTitle string, check func(string, navigation.Target) error, retitle func(string, navigation.Target, navigation.Target) error) (meetingNote, error) {
	return titleMeetingGuarded(p, id, rawTitle, check, retitle, nil)
}

// eligible runs under the existing recorder and artifact transaction locks,
// after re-reading both the association and the exact source bytes.
func titleMeetingGuarded(p paths, id, rawTitle string, check func(string, navigation.Target) error, retitle func(string, navigation.Target, navigation.Target) error, eligible func(meetingNote, []byte) error) (meetingNote, error) {
	return titleMeetingGuardedContext(context.Background(), p, id, rawTitle, check, retitle, eligible)
}

func titleMeetingGuardedContext(ctx context.Context, p paths, id, rawTitle string, check func(string, navigation.Target) error, retitle func(string, navigation.Target, navigation.Target) error, eligible func(meetingNote, []byte) error) (result meetingNote, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	title, err := cleanTitle(rawTitle)
	if err != nil {
		return result, err
	}
	recorder, err := lock(p)
	if err != nil {
		return result, errors.New("stop active capture with trailmix stop and wait for transcription retry to finish before changing a title")
	}
	defer recorder.Close()
	artifactCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	guard, err := artifactLock(artifactCtx, p, id)
	if err != nil {
		return result, fmt.Errorf("meeting artifacts are busy; retry title shortly: %w", err)
	}
	defer guard.Close()
	before, err := readMeeting(p, id)
	if err != nil {
		return result, err
	}
	if err := validateNote(before); err != nil {
		return result, err
	}
	original, err := readTitleFile(before.Path)
	if err != nil {
		return result, err
	}
	if eligible != nil {
		if err := eligible(before, original.data); err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := check(p.home, before.Target); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	clock, err := titleClock(before)
	if err != nil {
		return result, err
	}
	after := before
	after.Title, after.Window = title, strings.ReplaceAll(clock, "-", ":")+" "+title
	originals := []titleFile{original}
	for _, kind := range []string{"transcript", "answers"} {
		path := sidecarPath(before, kind)
		data, err := readArtifact(path, artifactMarker(id, kind))
		if err != nil {
			return result, err
		}
		if data != nil {
			f, err := readTitleFile(path)
			if err != nil {
				return result, err
			}
			originals = append(originals, f)
		}
	}
	// Reserve a basename only when the note and both sidecar names are free.
	// A sidecar collision also advances the suffix, even if it is not exported yet.
	for i := 1; ; i++ {
		name := clock + "-" + titleSlug(title)
		if i > 1 {
			name += fmt.Sprintf("-%d", i)
		}
		after.Path = filepath.Join(filepath.Dir(before.Path), name+".md")
		available := true
		for _, pair := range [][2]string{{after.Path, before.Path}, {sidecarPath(after, "transcript"), sidecarPath(before, "transcript")}, {sidecarPath(after, "answers"), sidecarPath(before, "answers")}} {
			if pair[0] == pair[1] {
				continue
			}
			if _, err := os.Lstat(pair[0]); err == nil {
				available = false
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return result, err
			}
		}
		if available {
			break
		}
	}
	// Build every metadata update before changing any files. Preserve unknown
	// fields and never change last/latest when they refer to another meeting.
	updates, err := titleMetadata(p, before, after)
	if err != nil {
		return result, err
	}
	var created, deleted []titleFile
	var replaced []titleReplacement
	navigationAttempted := false
	defer func() {
		if err == nil {
			return
		}
		undo := func() error {
			if navigationAttempted {
				return retitle(p.home, after.Target, before.Target)
			}
			return nil
		}
		if restoreErr := rollbackTitle(created, replaced, deleted, undo); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("title rollback incomplete; original note was %s: %w", before.Path, restoreErr))
		}
	}()
	for i, f := range originals {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		path := after.Path
		data := f.data
		if i == 0 {
			data = titleHeading(data, title)
		} else {
			path = strings.TrimSuffix(after.Path, ".md") + strings.TrimPrefix(f.path, strings.TrimSuffix(before.Path, ".md"))
		}
		if path == f.path {
			if bytes.Equal(data, f.data) {
				continue
			}
			var replacement titleReplacement
			replacement, err = replaceTitleFile(f, data)
			replaced = append(replaced, replacement)
			if err != nil {
				return result, err
			}
		} else {
			if err = createTitleFile(path, data, f.mode); err != nil {
				return result, err
			}
			published := titleFile{path: path, data: data, mode: f.mode}
			published.info, err = os.Lstat(path)
			created = append(created, published)
			if err != nil {
				return result, err
			}
		}
	}
	for _, update := range updates {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		var replacement titleReplacement
		replacement, err = replaceTitleFile(update.original, update.data)
		replaced = append(replaced, replacement)
		if err != nil {
			return result, err
		}
	}
	// Recheck after publication, before deleting any original. This narrows,
	// but cannot eliminate, races with editors launched outside TrailMix.
	if err = check(p.home, before.Target); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	navigationAttempted = true
	if err = retitle(p.home, before.Target, after.Target); err != nil {
		return result, err
	}
	if before.Path != after.Path {
		for _, f := range originals {
			if err = ctx.Err(); err != nil {
				return result, err
			}
			current, readErr := readTitleFile(f.path)
			if readErr != nil || !sameTitleFile(current, f) {
				err = fmt.Errorf("source changed during title update; close all editors and retry: %s", f.path)
				return result, err
			}
			if err = os.Remove(f.path); err != nil {
				return result, err
			}
			deleted = append(deleted, f)
		}
	}
	dir, err := os.Open(filepath.Dir(after.Path))
	if err != nil {
		return result, err
	}
	if err = errors.Join(dir.Sync(), dir.Close()); err != nil {
		return result, err
	}
	return after, nil
}

// A failed atomic write can have replaced the destination before directory
// sync fails. Record the expected bytes and observed inode even on failure.
type titleReplacement struct {
	original titleFile
	expected []byte
	written  os.FileInfo
}

func replaceTitleFile(original titleFile, data []byte) (titleReplacement, error) {
	replacement := titleReplacement{original: original, expected: data}
	prior, err := readTitleFile(original.path)
	if err != nil {
		return replacement, err
	}
	if !sameTitleFile(prior, original) {
		return replacement, fmt.Errorf("file changed outside title; not overwriting %s", original.path)
	}
	err = meeting.WriteFile(original.path, data)
	current, readErr := readTitleFile(original.path)
	if readErr == nil && bytes.Equal(current.data, data) {
		replacement.written = current.info
	}
	return replacement, err
}

func sameTitleFile(a, b titleFile) bool {
	return a.info != nil && b.info != nil && os.SameFile(a.info, b.info) && bytes.Equal(a.data, b.data)
}

func rollbackTitle(created []titleFile, replaced []titleReplacement, deleted []titleFile, undo func() error) error {
	retained := func(cause error) error {
		paths := make([]string, 0, len(created))
		for _, f := range created {
			paths = append(paths, f.path)
		}
		return fmt.Errorf("recovery stopped to preserve data; inspect the meeting association and retained title files before retrying: %s: %w", strings.Join(paths, ", "), cause)
	}
	// Restore missing originals first. If any restoration fails, keep the new
	// association and every destination copy, which may be the only copy left.
	var failures []error
	for _, f := range deleted {
		if err := createTitleFile(f.path, f.data, f.mode); err != nil {
			failures = append(failures, err)
		}
	}
	if err := errors.Join(failures...); err != nil {
		return retained(err)
	}
	for i := len(replaced) - 1; i >= 0; i-- {
		r := replaced[i]
		current, err := readTitleFile(r.original.path)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if bytes.Equal(current.data, r.original.data) {
			continue
		}
		if r.written == nil || !os.SameFile(current.info, r.written) || !bytes.Equal(current.data, r.expected) {
			failures = append(failures, fmt.Errorf("metadata or note changed outside title; not overwriting %s", r.original.path))
			continue
		}
		if err := meeting.WriteFile(r.original.path, r.original.data); err != nil {
			failures = append(failures, err)
		}
	}
	if err := undo(); err != nil {
		failures = append(failures, err)
	}
	if err := errors.Join(failures...); err != nil {
		return retained(err)
	}
	// Do not delete a destination that another writer changed or replaced,
	// even if the original sources and association were successfully restored.
	for _, f := range created {
		current, err := readTitleFile(f.path)
		if err != nil {
			return retained(err)
		}
		if !sameTitleFile(current, f) {
			return retained(fmt.Errorf("destination changed outside title; not removing %s", f.path))
		}
	}
	for _, f := range created {
		if err := os.Remove(f.path); err != nil {
			return retained(err)
		}
	}
	return nil
}

type titleUpdate struct {
	original titleFile
	data     []byte
}

func titleMetadata(p paths, before, after meetingNote) ([]titleUpdate, error) {
	paths := []string{filepath.Join(p.home, "meetings", before.ID+".json"), filepath.Join(p.home, "latest-meeting.json"), filepath.Join(p.home, "last.json")}
	if before.Directory != "" {
		paths = append(paths, filepath.Join(before.Directory, "session.json"))
	}
	var result []titleUpdate
	encoded, err := json.Marshal(after)
	if err != nil {
		return nil, err
	}
	for i, path := range paths {
		file, err := readTitleFile(path)
		if errors.Is(err, os.ErrNotExist) && i != 0 {
			continue
		}
		if err != nil {
			return nil, err
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(file.data, &obj); err != nil {
			return nil, err
		}
		if obj == nil {
			return nil, fmt.Errorf("invalid metadata: %s", path)
		}
		if i < 2 {
			var id string
			if err := json.Unmarshal(obj["id"], &id); err != nil {
				return nil, err
			}
			if id != before.ID {
				if i == 0 {
					return nil, errors.New("meeting association identity changed")
				}
				continue
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(encoded, &fields)
			for key, value := range fields {
				obj[key] = value
			}
		} else {
			var embedded struct {
				ID string `json:"id"`
			}
			if len(obj["note"]) == 0 {
				continue
			}
			if err := json.Unmarshal(obj["note"], &embedded); err != nil {
				return nil, err
			}
			if embedded.ID != before.ID {
				continue
			}
			// Preserve unknown fields within embedded note metadata as well.
			var fields, prior map[string]json.RawMessage
			_ = json.Unmarshal(encoded, &fields)
			_ = json.Unmarshal(obj["note"], &prior)
			for key, value := range fields {
				prior[key] = value
			}
			obj["note"], err = json.Marshal(prior)
			if err != nil {
				return nil, err
			}
		}
		data, err := json.MarshalIndent(obj, "", "  ")
		if err != nil {
			return nil, err
		}
		result = append(result, titleUpdate{file, append(data, '\n')})
	}
	return result, nil
}
