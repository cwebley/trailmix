package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/cwebley/trailmix/internal/meeting"
	"github.com/cwebley/trailmix/internal/navigation"
	"github.com/cwebley/trailmix/internal/settings"
)

// title.json is private recording state, not a generated note region. Once a
// suggestion is selected, subsequent recaps cannot replace it, even while an
// editor or capture keeps the rename pending.
type autoTitleIntent struct {
	MeetingID  string   `json:"meeting_id"`
	RecapID    string   `json:"recap_id,omitempty"`
	Title      string   `json:"title,omitempty"`
	State      string   `json:"state"`
	Error      string   `json:"error,omitempty"`
	SwapPaths  []string `json:"swap_paths,omitempty"`
	EditorPIDs []int    `json:"editor_pids,omitempty"`
}

var errAutoTitleIneligible = errors.New("meeting is no longer an unnamed default note")
var autoTitleBasename = regexp.MustCompile(`^([0-1][0-9]|2[0-3])-([0-5][0-9])-([0-5][0-9])(?:-[2-9][0-9]*|-[1-9][0-9]+)?\.md$`)
var startTitleWorker = launchTitleWorker
var autoTitleCheck = navigation.CheckTitleSafety
var autoTitleRetitle = navigation.Retitle

func cleanAutoTitle(raw string) (string, error) {
	title, err := cleanTitle(raw)
	if err != nil {
		return "", err
	}
	if strings.Contains(title, "<!--") || strings.Contains(title, "-->") || strings.Contains(strings.ToLower(title), "trailmix:") {
		return "", errors.New("automatic title must not contain HTML comments or TrailMix metadata")
	}
	return title, nil
}

func autoTitleEligible(note meetingNote, data []byte) bool {
	clock := autoTitleBasename.FindStringSubmatch(filepath.Base(note.Path))
	if note.Title != "" || clock == nil {
		return false
	}
	want := "# Meeting " + note.Date + " " + strings.Join(clock[1:4], ":")
	// Reuse the recap fence scanner's CommonMark-style boundaries. Exactly
	// one unfenced H1 must be the exact built-in heading, not a custom variant.
	var fence byte
	width, count := 0, 0
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) <= 3 && len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			n := 0
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			if n >= 3 {
				if fence == 0 && !(trimmed[0] == '`' && strings.ContainsRune(trimmed[n:], '`')) {
					fence, width = trimmed[0], n
					continue
				} else if fence == trimmed[0] && n >= width && strings.TrimSpace(trimmed[n:]) == "" {
					fence = 0
					continue
				}
			}
		}
		// Setext and HTML headings are custom headings too. Refuse rather
		// than guessing whether a mixed heading style was user-authored.
		if fence == 0 && len(line)-len(trimmed) <= 3 && trimmed != "" && (strings.Trim(trimmed, "= \t") == "" || strings.Contains(strings.ToLower(trimmed), "<h1")) {
			return false
		}
		if fence == 0 && len(line)-len(trimmed) <= 3 && (trimmed == "#" || strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "#\t")) {
			count++
			if line != want {
				return false
			}
		}
	}
	return count == 1
}

func autoTitlePath(note meetingNote) string {
	return filepath.Join(note.Directory, "recaps", "title.json")
}

// Caller holds the artifact lock and has re-resolved the recording association.
func readAutoTitle(note meetingNote) (autoTitleIntent, error) {
	intent := autoTitleIntent{MeetingID: note.ID, State: "none"}
	data, err := os.ReadFile(autoTitlePath(note))
	if errors.Is(err, os.ErrNotExist) {
		return intent, nil
	}
	if err != nil {
		return intent, err
	}
	if err := json.Unmarshal(data, &intent); err != nil {
		return intent, err
	}
	if intent.MeetingID != note.ID || !meeting.ValidID(intent.RecapID) {
		return intent, errors.New("automatic title identity mismatch")
	}
	if clean, err := cleanAutoTitle(intent.Title); err != nil || clean != intent.Title {
		return intent, errors.New("invalid saved automatic title")
	}
	for _, pid := range intent.EditorPIDs {
		if pid < 2 || pid > 1<<31-1 {
			return intent, errors.New("invalid saved source editor PID")
		}
	}
	for _, path := range intent.SwapPaths {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return intent, errors.New("invalid saved editor swap path")
		}
	}
	switch intent.State {
	case "pending", "applied", "skipped", "error":
	default:
		return intent, errors.New("invalid automatic title state")
	}
	return intent, nil
}

func rememberTitleSwaps(intent *autoTitleIntent, paths []string) {
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			continue
		}
		found := false
		for _, prior := range intent.SwapPaths {
			if prior == path {
				found = true
				break
			}
		}
		if !found {
			intent.SwapPaths = append(intent.SwapPaths, path)
		}
	}
}

// Selection takes only the artifact lock. Applying is a separate transaction,
// after publication and selection locks have been released.
func selectRecapTitle(ctx context.Context, p paths, view recapView, editorPIDs ...int) (autoTitleIntent, error) {
	guard, err := artifactLock(ctx, p, view.MeetingID)
	if err != nil {
		return autoTitleIntent{}, err
	}
	defer guard.Close()
	note, err := readRecapJobNote(p, view.MeetingID)
	if err != nil {
		return autoTitleIntent{}, err
	}
	return selectRecapTitleLocked(p, note, view, nil, editorPIDs...)
}

// Caller holds both the generation and artifact publication locks when choosing
// a new title. The outside scheduling hook may only merge an existing intent.
func selectRecapTitleLocked(p paths, note meetingNote, view recapView, swapPaths []string, editorPIDs ...int) (autoTitleIntent, error) {
	intent, err := readAutoTitle(note)
	if err != nil || (intent.State != "none" && intent.State != "pending") {
		return intent, err
	}
	if intent.State == "none" {
		title, err := cleanAutoTitle(view.SuggestedTitle)
		if err != nil || !meeting.ValidID(view.RecapID) {
			return intent, errors.New("invalid recap title suggestion")
		}
		data, err := os.ReadFile(note.Path)
		if err != nil {
			return intent, err
		}
		intent = autoTitleIntent{MeetingID: note.ID, RecapID: view.RecapID, Title: title, State: "pending"}
		if !autoTitleEligible(note, data) {
			intent.State = "skipped"
		}
	}
	// A later manual recap may supply another unregistered live source editor.
	// Add its blocker, but never replace the first selected suggestion.
	for _, pid := range editorPIDs {
		if pid < 2 || pid > 1<<31-1 {
			continue
		}
		found := false
		for _, prior := range intent.EditorPIDs {
			if prior == pid {
				found = true
				break
			}
		}
		if !found {
			intent.EditorPIDs = append(intent.EditorPIDs, pid)
		}
	}
	rememberTitleSwaps(&intent, swapPaths)
	editors, editorErr := navigation.RecapEditors(p.home, note.Target)
	rememberTitleSwaps(&intent, editors.SwapPaths)
	if editorErr != nil {
		intent.Error = editorErr.Error()
	}
	if err := os.MkdirAll(filepath.Dir(autoTitlePath(note)), 0700); err != nil {
		return intent, err
	}
	return intent, meeting.WriteJSON(autoTitlePath(note), intent)
}

// Cached --auto requests still choose under the generation lock held by their
// caller. --latest and --status never call this helper.
func prepareRecapTitle(ctx context.Context, p paths, view recapView, input recapInput) error {
	cfg, err := settings.Read()
	if err != nil || !cfg.Recap.AutoName {
		return err
	}
	guard, err := artifactLock(ctx, p, view.MeetingID)
	if err != nil {
		return err
	}
	defer guard.Close()
	note, err := readRecapJobNote(p, view.MeetingID)
	if err != nil {
		return err
	}
	_, err = selectRecapTitleLocked(p, note, view, input.SwapPaths, input.EditorPID)
	return err
}

// Naming failure never turns a successfully published recap into a failed job.
// The durable pending record is the retry mechanism, not a daemon.
func scheduleRecapTitle(p paths, view recapView, editorPIDs ...int) {
	cfg, err := settings.Read()
	if err != nil || !cfg.Recap.AutoName {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	intent, err := selectRecapTitle(ctx, p, view, editorPIDs...)
	if err != nil || intent.State != "pending" {
		return
	}
	intent, err = retryPendingTitle(ctx, p, view.MeetingID)
	if err != nil || intent.State == "pending" {
		_ = startTitleWorker(p, view.MeetingID)
	}
}

func titleWorkerLock(p paths, id string) (*os.File, error) {
	if !meeting.ValidID(id) {
		return nil, errors.New("invalid meeting ID")
	}
	root := filepath.Join(p.home, "meetings")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return recapExclusive(filepath.Join(root, id+".title-worker.lock"))
}

func retryPendingTitle(ctx context.Context, p paths, id string) (autoTitleIntent, error) {
	worker, err := titleWorkerLock(p, id)
	if err != nil {
		return autoTitleIntent{}, err
	}
	defer worker.Close()
	return retryTitleWithLifecycle(ctx, p, id)
}

func retryTitleWithLifecycle(ctx context.Context, p paths, id string) (autoTitleIntent, error) {
	lifecycle, err := recapGenerationLock(ctx, filepath.Join(p.home, "workspace.lock"))
	if err != nil {
		return autoTitleIntent{}, err
	}
	defer lifecycle.Close()
	return attemptPendingTitle(ctx, p, id)
}

// Public title dispatch already holds the lifecycle lock. Never reacquire it.
func retryPendingTitleLocked(ctx context.Context, p paths, id string) (autoTitleIntent, error) {
	worker, err := titleWorkerLock(p, id)
	if err != nil {
		return autoTitleIntent{}, err
	}
	defer worker.Close()
	return attemptPendingTitle(ctx, p, id)
}

func attemptPendingTitle(ctx context.Context, p paths, id string) (autoTitleIntent, error) {
	guard, err := artifactLock(ctx, p, id)
	if err != nil {
		return autoTitleIntent{}, err
	}
	note, err := readRecapJobNote(p, id)
	if err != nil {
		guard.Close()
		return autoTitleIntent{}, err
	}
	intent, err := readAutoTitle(note)
	if err == nil && intent.State == "pending" {
		cfg, readErr := settings.Read()
		if readErr != nil {
			guard.Close()
			return intent, readErr
		}
		if !cfg.Recap.AutoName {
			intent.Error = "automatic naming is disabled by recap.auto_name"
			err = meeting.WriteJSON(autoTitlePath(note), intent)
			guard.Close()
			return intent, err
		}
	}
	guard.Close()
	if err != nil || intent.State != "pending" {
		return intent, err
	}
	check := func(home string, target navigation.Target) error {
		cfg, err := settings.Read()
		if err != nil {
			return err
		}
		if !cfg.Recap.AutoName {
			return errors.New("automatic naming is disabled by recap.auto_name")
		}
		for _, pid := range intent.EditorPIDs {
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("source editor process %d is still running or cannot be inspected", pid)
			}
		}
		for _, path := range intent.SwapPaths {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("recorded editor swap remains or cannot be inspected: %s", path)
			}
		}
		return autoTitleCheck(home, target)
	}
	_, applyErr := titleMeetingGuardedContext(ctx, p, id, intent.Title, check, autoTitleRetitle, func(current meetingNote, data []byte) error {
		if current.Directory != note.Directory {
			return errors.New("recording association changed before automatic title")
		}
		// Selection can merge source-editor blockers while this worker waits
		// for recorder/artifact locks. Read them again inside the transaction.
		latest, err := readAutoTitle(current)
		if err != nil {
			return err
		}
		if latest.State != "pending" || latest.RecapID != intent.RecapID || latest.Title != intent.Title {
			return errors.New("automatic title intent changed before application")
		}
		intent = latest
		if !autoTitleEligible(current, data) {
			return errAutoTitleIneligible
		}
		editors, err := navigation.RecapEditors(p.home, current.Target)
		rememberTitleSwaps(&intent, editors.SwapPaths)
		// Persist known swap names before refusing, so a crashed editor's swap
		// still protects its unsaved work after its registration disappears.
		if saveErr := meeting.WriteJSON(autoTitlePath(current), intent); saveErr != nil {
			return saveErr
		}
		if err != nil {
			return err
		}
		if editors.Loaded > 0 || editors.Conflict {
			return errors.New("meeting editors are still open; automatic title remains pending")
		}
		return nil
	})
	if errors.Is(applyErr, errAutoTitleIneligible) {
		intent.State, intent.Error = "skipped", ""
	} else if applyErr != nil {
		intent.Error = applyErr.Error()
	} else {
		intent.State, intent.Error = "applied", ""
	}
	guard, err = artifactLock(ctx, p, id)
	if err != nil {
		return intent, err
	}
	defer guard.Close()
	current, err := readRecapJobNote(p, id)
	if err != nil {
		return intent, err
	}
	if current.Directory != note.Directory {
		return intent, errors.New("recording association changed after automatic title")
	}
	latest, err := readAutoTitle(current)
	if err != nil {
		return intent, err
	}
	rememberTitleSwaps(&intent, latest.SwapPaths)
	for _, pid := range latest.EditorPIDs {
		found := false
		for _, prior := range intent.EditorPIDs {
			if prior == pid {
				found = true
				break
			}
		}
		if !found {
			intent.EditorPIDs = append(intent.EditorPIDs, pid)
		}
	}
	return intent, meeting.WriteJSON(autoTitlePath(current), intent)
}

func launchTitleWorker(p paths, id string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, "_title-worker", "--meeting", id)
	cmd.Env = append(os.Environ(), "TRAILMIX_HOME="+p.home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func runTitleWorker(p paths, args []string) error {
	flags := flag.NewFlagSet("_title-worker", flag.ContinueOnError)
	id := flags.String("meeting", "", "meeting ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !meeting.ValidID(*id) {
		return errors.New("usage: trailmix _title-worker --meeting ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Closure retries must not disappear behind an older worker just as its
	// wait expires. Wait for its lock within this worker's own bounded context.
	worker, err := recapGenerationLock(ctx, filepath.Join(p.home, "meetings", *id+".title-worker.lock"))
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer worker.Close()
	for {
		intent, err := retryTitleWithLifecycle(ctx, p, *id)
		if err == nil && intent.State != "pending" {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		} // Leave pending for a later closure/retry.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}
