package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/cwebley/trailmix/internal/assistant"
	"github.com/cwebley/trailmix/internal/meeting"
	"github.com/cwebley/trailmix/internal/settings"
)

// Preserve the original failure, exact source snapshot and job metadata before
// repairing any publication state. A retry never replaces this audit record.
type recapRecovery struct {
	StartedAt       time.Time   `json:"started_at"`
	PreviousRequest recapRecord `json:"previous_request"`
	PreviousJob     recapJob    `json:"previous_job"`
}

func readRecoveryJSON(path string, value any) error {
	file, err := readTitleFile(path)
	if err != nil {
		return err
	}
	if len(file.data) > 16*1024*1024 {
		return errors.New("retained recap artifact exceeds 16 MiB")
	}
	if err := json.Unmarshal(file.data, value); err != nil {
		return fmt.Errorf("read retained %s: %w", filepath.Base(path), err)
	}
	return nil
}

func recoveryDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing non-directory or symlink: %s", path)
	}
	return nil
}

func validateRecoveryEvidence(id string, record recapRecord, snapshot transcriptSnapshot) error {
	if record.MeetingID != id || record.Input.Notes == nil || strings.Count(*record.Input.Notes, "<!-- trailmix: meeting ") != 1 || !strings.Contains(*record.Input.Notes, "<!-- trailmix: meeting "+id+" -->") {
		return errors.New("retained request notes identity mismatch")
	}
	if record.Input.EditorPID != 0 && (record.Input.EditorPID < 2 || record.Input.EditorPID > 1<<31-1) {
		return errors.New("invalid retained source editor PID")
	}
	for _, path := range record.Input.SwapPaths {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return errors.New("invalid retained editor swap path")
		}
	}
	if snapshot.MeetingID != id || snapshot.Transcription.MeetingID != id {
		return errors.New("retained transcript identity mismatch")
	}
	seen := map[string]bool{}
	for _, passage := range snapshot.Passages {
		if passage.MeetingID != id || !passageID.MatchString(passage.ID) || seen[passage.ID] || passage.Start < 0 || passage.End < passage.Start || (passage.Track != "microphone" && passage.Track != "system") {
			return errors.New("invalid or duplicate retained finalized passage")
		}
		seen[passage.ID] = true
	}
	return nil
}

func sameRecoveredView(a, b recapView) bool {
	// A manual title can change NotePath. All content and durable identities
	// must still match the revalidated retained response exactly.
	a.NotePath, b.NotePath = "", ""
	return a == b
}

func recoverRecap(ctx context.Context, p paths, id, requestID string) (recapView, error) {
	var view recapView
	if !meeting.ValidID(id) || !meeting.ValidID(requestID) {
		return view, errors.New("invalid meeting or recap request ID")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	gate, err := recapScheduleLock(ctx, p, id)
	if err != nil {
		return view, err
	}
	worker, err := recapWorkerLock(p, id)
	gate.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return view, errors.New("recap worker is active; wait for it to finish before recovery")
	}
	if err != nil {
		return view, err
	}
	defer worker.Close()
	// Background workers take their worker lock before the generation lock.
	// Never hold an artifact lock while waiting for either of them.
	generation, err := recapGenerationLock(ctx, filepath.Join(p.home, "meetings", id+".recap.lock"))
	if err != nil {
		return view, fmt.Errorf("recap generation is busy: %w", err)
	}
	defer generation.Close()
	guard, err := artifactLock(ctx, p, id)
	if err != nil {
		return view, err
	}
	defer guard.Close()
	note, err := readRecapJobNote(p, id)
	if err != nil {
		return view, err
	}
	job, err := readRecapJob(note)
	if err != nil {
		return view, err
	}
	if job.State == "queued" || job.State == "generating" || job.State == "cancelled" {
		return view, errors.New("recap job is queued, generating, or cancelled; recovery refused")
	}
	if err := recapCancellationError(note, job.ID); err != nil {
		return view, fmt.Errorf("recap cancellation prevents recovery: %w", err)
	}
	root := filepath.Join(note.Directory, "recaps")
	directory := filepath.Join(root, requestID)
	for _, path := range []string{root, directory} {
		if err := recoveryDirectory(path); err != nil {
			return view, err
		}
	}
	var record recapRecord
	if err := readRecoveryJSON(filepath.Join(directory, "request.json"), &record); err != nil {
		return view, err
	}
	if record.ID != requestID || record.MeetingID != id || (record.Input.Directory != "" && record.Input.Directory != note.Directory) {
		return view, errors.New("retained recap request identity or recording mismatch")
	}
	var audit recapRecovery
	auditPath := filepath.Join(directory, "recovered.json")
	auditErr := readRecoveryJSON(auditPath, &audit)
	if auditErr != nil && !errors.Is(auditErr, os.ErrNotExist) {
		return view, auditErr
	}
	if auditErr == nil {
		prior := audit.PreviousRequest
		if prior.ID != requestID || prior.MeetingID != id || prior.State != "failed" || !reflect.DeepEqual(prior.Input, record.Input) {
			return view, errors.New("retained recovery audit identity or source mismatch")
		}
	}
	if record.State != "failed" && !(record.State == "complete" && auditErr == nil) {
		return view, errors.New("only a retained failed request or its completed recovery can be recovered")
	}
	failure := strings.ToLower(record.Error)
	if strings.Contains(failure, "canceled") || strings.Contains(failure, "cancelled") {
		return view, errors.New("cancelled recap requests cannot be recovered")
	}
	var runner assistant.Result
	var snapshot transcriptSnapshot
	if err := readRecoveryJSON(filepath.Join(directory, "runner.json"), &runner); err != nil {
		return view, err
	}
	if err := readRecoveryJSON(filepath.Join(directory, "transcript.json"), &snapshot); err != nil {
		return view, err
	}
	if err := validateRecoveryEvidence(id, record, snapshot); err != nil {
		return view, err
	}
	title, markdown, err := renderRecap(runner.Text, snapshot)
	if err != nil {
		return view, fmt.Errorf("retained recap remains invalid: %w", err)
	}
	if len(record.Input.SourceWarnings) > 0 {
		warnings := make([]string, 0, len(record.Input.SourceWarnings))
		for _, warning := range record.Input.SourceWarnings {
			warnings = append(warnings, recapCoverageText(warning))
		}
		markdown = strings.Replace(markdown, "## Summary\n\n", "## Summary\n\nNotes source notice: "+strings.Join(warnings, " ")+"\n\n", 1)
	}
	view = recapView{MeetingID: id, NotePath: note.Path, RecapID: requestID, Markdown: markdown, SuggestedTitle: title, NotesSnapshot: *record.Input.Notes, Path: filepath.Join(directory, "result.json")}
	var latest recapView
	latestErr := readRecoveryJSON(filepath.Join(root, "latest.json"), &latest)
	if latestErr != nil && !errors.Is(latestErr, os.ErrNotExist) {
		return recapView{}, latestErr
	}
	if latestErr == nil && !sameRecoveredView(latest, view) {
		return recapView{}, errors.New("another successful recap is already latest; recovery will not replace it")
	}
	var priorResult recapView
	resultErr := readRecoveryJSON(view.Path, &priorResult)
	if resultErr != nil && !errors.Is(resultErr, os.ErrNotExist) {
		return recapView{}, resultErr
	}
	if resultErr == nil && !sameRecoveredView(priorResult, view) {
		return recapView{}, errors.New("retained result differs from the validated response; recovery refused")
	}
	if record.State == "complete" && (latestErr != nil || resultErr != nil) {
		return recapView{}, errors.New("completed recovery is missing its published result or latest pointer")
	}
	if err := ctx.Err(); err != nil {
		return recapView{}, err
	}
	// No durable state changes precede validation of every retained input,
	// publication destination and existing latest pointer.
	if auditErr != nil {
		audit = recapRecovery{StartedAt: time.Now().UTC(), PreviousRequest: record, PreviousJob: job}
		data, err := json.MarshalIndent(audit, "", "  ")
		if err != nil {
			return recapView{}, err
		}
		if err := createTitleFile(auditPath, append(data, '\n'), 0600); err != nil {
			return recapView{}, err
		}
	}
	if err := meeting.WriteJSON(view.Path, view); err != nil {
		return recapView{}, err
	}
	if err := ctx.Err(); err != nil {
		return recapView{}, err
	}
	if err := meeting.WriteJSON(filepath.Join(root, "latest.json"), view); err != nil {
		return recapView{}, err
	}
	record.State, record.Error = "complete", ""
	if err := meeting.WriteJSON(filepath.Join(directory, "request.json"), record); err != nil {
		return view, err
	}
	job.State, job.RecapID, job.Error = "ready", requestID, ""
	if err := saveRecapJob(note, &job); err != nil {
		return view, err
	}
	if cfg, err := settings.Read(); err == nil && cfg.Recap.AutoName {
		_, _ = selectRecapTitleLocked(p, note, view, record.Input.SwapPaths, record.Input.EditorPID)
	}
	return view, nil
}
