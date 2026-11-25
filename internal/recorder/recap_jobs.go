package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/cwebley/trailmix/internal/meeting"
	"github.com/cwebley/trailmix/internal/navigation"
	"github.com/cwebley/trailmix/internal/settings"
)

const recapMaxAttempts = 2
const recapMaxWaits = 5

var startRecapWorker = launchRecapWorker
var errRecapCancelled = errors.New("background recap cancelled")

type recapCancellation struct {
	JobID string `json:"job_id"`
}

// Cancellation is addressed to a job instance, never a PID. An old token cannot
// cancel a later explicit retry, even if the original worker has crashed.
func cancelRecapJob(p paths, id string) error {
	if !meeting.ValidID(id) {
		return errors.New("invalid meeting ID")
	}
	root := filepath.Join(p.home, "meetings")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	guard, err := recapGenerationLock(ctx, filepath.Join(root, id+".recap-cancel.lock"))
	if err != nil {
		return err
	}
	defer guard.Close()
	note, err := recapJobNote(p, id)
	if err != nil {
		return err
	}
	job, err := readRecapJob(note)
	if err != nil {
		return err
	}
	if job.State != "queued" && job.State != "generating" {
		return nil
	}
	if !meeting.ValidID(job.ID) {
		return errors.New("recap job has no cancellable instance identity; inspect --status and retry with --enqueue")
	}
	return meeting.WriteJSON(filepath.Join(note.Directory, "recaps", "cancel.json"), recapCancellation{JobID: job.ID})
}

func recapCancellationError(note meetingNote, id string) error {
	data, err := os.ReadFile(filepath.Join(note.Directory, "recaps", "cancel.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var token recapCancellation
	if err := json.Unmarshal(data, &token); err != nil {
		return err
	}
	if token.JobID == id {
		return errRecapCancelled
	}
	return nil
}

func watchRecapCancellation(ctx context.Context, note meetingNote, id string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			if err := recapCancellationError(note, id); err != nil {
				cancel(err)
				return
			}
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return ctx, func() { close(done); <-stopped; cancel(nil) }
}

// Jobs and inputs are private recording artifacts. Nothing here writes the note.
type recapJob struct {
	ID             string      `json:"job_id"`
	MeetingID      string      `json:"meeting_id"`
	State          string      `json:"state"`
	Attempts       int         `json:"attempts"`
	Waits          int         `json:"waits"`
	UpdatedAt      time.Time   `json:"updated_at"`
	RecapID        string      `json:"recap_id,omitempty"`
	Error          string      `json:"error,omitempty"`
	Input          *recapInput `json:"input,omitempty"`
	EditorConflict bool        `json:"editor_conflict"`
	EditorReason   string      `json:"editor_reason,omitempty"`
}

type recapStatusView struct {
	MeetingID      string `json:"meeting_id"`
	NotePath       string `json:"note_path"`
	State          string `json:"state"`
	RecapID        string `json:"recap_id,omitempty"`
	Error          string `json:"error,omitempty"`
	EditorConflict bool   `json:"editor_conflict"`
	EditorReason   string `json:"editor_reason,omitempty"`
	TitleState     string `json:"title_state,omitempty"`
	Title          string `json:"title,omitempty"`
	TitleError     string `json:"title_error,omitempty"`
}

func recapWorkerLock(p paths, id string) (*os.File, error) {
	if !meeting.ValidID(id) {
		return nil, errors.New("invalid meeting ID")
	}
	root := filepath.Join(p.home, "meetings")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return recapExclusive(filepath.Join(root, id+".recap-worker.lock"))
}

// Status repair and explicit enqueue coordinate only their short ownership
// checks. Workers never take this gate, and it is released before editor RPC.
// Lock order is schedule gate, worker lock, then artifact lock.
func recapScheduleLock(ctx context.Context, p paths, id string) (*os.File, error) {
	if !meeting.ValidID(id) {
		return nil, errors.New("invalid meeting ID")
	}
	root := filepath.Join(p.home, "meetings")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return recapGenerationLock(ctx, filepath.Join(root, id+".recap-schedule.lock"))
}

func recapJobNote(p paths, id string) (meetingNote, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	guard, err := artifactLock(ctx, p, id)
	if err != nil {
		return meetingNote{}, err
	}
	defer guard.Close()
	return readRecapJobNote(p, id)
}

// Caller holds the meeting's artifact lock against title changes.
func readRecapJobNote(p paths, id string) (meetingNote, error) {
	note, err := readMeeting(p, id)
	if err != nil {
		return note, err
	}
	if err := validateNote(note); err != nil {
		return note, err
	}
	if !filepath.IsAbs(note.Directory) {
		return note, errors.New("meeting has no associated recording directory")
	}
	got, err := meeting.Read(note.Directory)
	if err != nil {
		return note, err
	}
	if got != id {
		return note, errors.New("recording meeting identity mismatch")
	}
	return note, nil
}

func recapJobPath(note meetingNote) string {
	return filepath.Join(note.Directory, "recaps", "job.json")
}

func readRecapJob(note meetingNote) (recapJob, error) {
	job := recapJob{MeetingID: note.ID, State: "none"}
	data, err := os.ReadFile(recapJobPath(note))
	if errors.Is(err, os.ErrNotExist) {
		return job, nil
	}
	if err != nil {
		return job, err
	}
	if err := json.Unmarshal(data, &job); err != nil {
		return job, err
	}
	if job.MeetingID != note.ID {
		return job, errors.New("recap job identity mismatch")
	}
	switch job.State {
	case "queued", "generating", "ready", "failed", "cancelled":
	default:
		return job, errors.New("invalid recap job state")
	}
	return job, nil
}

func saveRecapJob(note meetingNote, job *recapJob) error {
	if err := os.MkdirAll(filepath.Dir(recapJobPath(note)), 0700); err != nil {
		return err
	}
	if job.ID == "" {
		job.ID = meeting.NewID()
	}
	job.UpdatedAt = time.Now().UTC()
	return meeting.WriteJSON(recapJobPath(note), job)
}

// The lock is inherited by the detached child. There is no gap between queueing
// and worker startup in which status could mistake a queued job for a crash.
func enqueueRecap(p paths, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	gate, err := recapScheduleLock(ctx, p, id)
	if err != nil {
		return err
	}
	defer func() {
		if gate != nil {
			gate.Close()
		}
	}()
	var guard *os.File
	for {
		var err error
		guard, err = recapWorkerLock(p, id)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		// A status repair cannot own the worker lock while we hold the
		// scheduling gate. Only a worker or another queue builder can be
		// responsible, so durable queued/generating state confirms dedup.
		note, readErr := recapJobNote(p, id)
		if readErr != nil {
			return readErr
		}
		job, readErr := readRecapJob(note)
		if readErr != nil {
			return readErr
		}
		if job.State == "queued" || job.State == "generating" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("recap queue is busy; retry --enqueue: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer guard.Close()
	gate.Close()
	gate = nil
	note, err := recapJobNote(p, id)
	if err != nil {
		return err
	}
	job := recapJob{MeetingID: id, State: "queued"}
	latest, err := latestRecap(ctx, p, id)
	if err == nil {
		// Optional naming failure cannot make a cached recap unavailable.
		_ = prepareRecapTitle(ctx, p, latest, recapInput{})
		job.State, job.RecapID = "ready", latest.RecapID
		if err := saveRecapJob(note, &job); err != nil {
			return err
		}
		scheduleRecapTitle(p, latest)
		return nil
	}
	if !errors.Is(err, errNoRecap) {
		return err
	}
	if err := saveRecapJob(note, &job); err != nil {
		return err
	}
	if err := startRecapWorker(p, id, note, guard); err != nil {
		job.State, job.Error = "failed", "start recap worker: "+err.Error()+"; retry with trailmix recap --meeting "+id+" --enqueue"
		return errors.Join(err, saveRecapJob(note, &job))
	}
	return nil
}

func launchRecapWorker(p paths, id string, note meetingNote, guard *os.File) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(note.Directory, "recaps", "worker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	if err := log.Chmod(0600); err != nil {
		return err
	}
	cmd := exec.Command(executable, "_recap-worker", "--meeting", id, "--lock-fd", "3")
	// Assistant credentials and XDG configuration must match a foreground
	// recap. Only the native capture helper gets captureEnvironment's filter.
	cmd.Env = append(os.Environ(), "TRAILMIX_HOME="+p.home)
	cmd.ExtraFiles = []*os.File{guard}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func recapJobStatus(p paths, id string) (recapStatusView, error) {
	var view recapStatusView
	// Reading status may repair a dead worker's state, but never starts work.
	gateCtx, gateCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer gateCancel()
	gate, err := recapScheduleLock(gateCtx, p, id)
	if err != nil {
		return view, err
	}
	defer func() {
		if gate != nil {
			gate.Close()
		}
	}()
	guard, lockErr := recapWorkerLock(p, id)
	if lockErr != nil && !errors.Is(lockErr, syscall.EWOULDBLOCK) {
		return view, lockErr
	}
	if guard != nil {
		defer guard.Close()
	} else {
		// A live worker owns its own state. This status call is read-only.
		gate.Close()
		gate = nil
	}
	note, err := recapJobNote(p, id)
	if err != nil {
		return view, err
	}
	job, err := readRecapJob(note)
	if err != nil {
		return view, err
	}
	if guard != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		latest, latestErr := latestRecap(ctx, p, id)
		if latestErr == nil {
			if job.State != "ready" || job.RecapID != latest.RecapID {
				job.State, job.RecapID, job.Error = "ready", latest.RecapID, ""
				if err := saveRecapJob(note, &job); err != nil {
					return view, err
				}
			}
		} else if !errors.Is(latestErr, errNoRecap) {
			return view, latestErr
		} else if job.State == "queued" || job.State == "generating" {
			job.State, job.Error = "failed", "recap worker exited before completion; retry with trailmix recap --meeting "+id+" --enqueue"
			if errors.Is(recapCancellationError(note, job.ID), errRecapCancelled) {
				job.State, job.Error = "cancelled", errRecapCancelled.Error()+"; retry with trailmix recap --meeting "+id+" --enqueue"
			}
			if err := saveRecapJob(note, &job); err != nil {
				return view, err
			}
		}
	}
	// State repair is complete. Editor RPC inspection must not monopolize
	// the queue lock and cause an explicit --enqueue request to disappear.
	if guard != nil {
		guard.Close()
		guard = nil
	}
	if gate != nil {
		gate.Close()
		gate = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	association, err := artifactLock(ctx, p, id)
	if err != nil {
		return view, err
	}
	defer association.Close()
	current, err := readRecapJobNote(p, id)
	if err != nil {
		return view, err
	}
	if current.Directory != note.Directory {
		return view, errors.New("recording association changed during recap status")
	}
	note = current
	editors, editorErr := navigation.RecapEditors(p.home, note.Target)
	if editorErr != nil {
		editors.Conflict, editors.Reason = true, "editor inspection failed: "+editorErr.Error()
	}
	view = recapStatusView{MeetingID: id, NotePath: note.Path, State: job.State, RecapID: job.RecapID, Error: job.Error, EditorConflict: editors.Conflict, EditorReason: editors.Reason}
	if title, titleErr := readAutoTitle(note); titleErr != nil {
		view.TitleState, view.TitleError = "error", titleErr.Error()
	} else if title.State != "none" {
		view.TitleState, view.Title, view.TitleError = title.State, title.Title, title.Error
	}
	if (job.State == "queued" || job.State == "generating") && errors.Is(recapCancellationError(note, job.ID), errRecapCancelled) {
		view.Error = "background recap cancellation requested"
	}
	return view, nil
}

func runRecapWorker(p paths, args []string) error {
	flags := flag.NewFlagSet("_recap-worker", flag.ContinueOnError)
	id := flags.String("meeting", "", "meeting ID")
	fd := flags.Int("lock-fd", -1, "inherited worker lock")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !meeting.ValidID(*id) || (*fd != -1 && *fd != 3) {
		return errors.New("invalid recap worker arguments")
	}
	var guard *os.File
	var err error
	if *fd == 3 {
		guard = os.NewFile(uintptr(*fd), "recap-worker-lock")
		if guard == nil {
			return errors.New("missing inherited recap worker lock")
		}
		syscall.CloseOnExec(*fd)
		// Verify the descriptor names this meeting's lock before trusting it.
		info, statErr := guard.Stat()
		want, wantErr := os.Stat(filepath.Join(p.home, "meetings", *id+".recap-worker.lock"))
		if statErr != nil || wantErr != nil || !os.SameFile(info, want) {
			guard.Close()
			return errors.New("invalid inherited recap worker lock")
		}
		if err := syscall.Flock(*fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			guard.Close()
			return fmt.Errorf("inherited recap worker lock is busy: %w", err)
		}
	} else {
		guard, err = recapWorkerLock(p, *id)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	defer guard.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return processRecapJob(ctx, p, *id)
}

// Snapshot association, editor state and saved-note fallback under one artifact
// lock. A title change cannot remove the fallback path between resolution and
// ReadFile. No artifact lock survives this function or a provider request.
func snapshotRecapJobInput(ctx context.Context, p paths, original meetingNote) (meetingNote, recapInput, navigation.RecapEditorState, error) {
	var input recapInput
	var editors navigation.RecapEditorState
	guard, err := artifactLock(ctx, p, original.ID)
	if err != nil {
		return original, input, editors, err
	}
	defer guard.Close()
	note, err := readRecapJobNote(p, original.ID)
	if err != nil {
		return original, input, editors, err
	}
	if note.Directory != original.Directory {
		return note, input, editors, errors.New("recording association changed before recap snapshot")
	}
	editors, editorErr := navigation.RecapEditors(p.home, note.Target)
	input = recapInput{Notes: editors.Notes, NotePath: note.Path, Source: "editor", Directory: note.Directory, SwapPaths: editors.SwapPaths}
	if editorErr != nil {
		editors.Conflict, editors.Reason = true, "editor inspection failed: "+editorErr.Error()
	}
	if editors.Conflict || editorErr != nil || editors.Notes == nil {
		data, readErr := os.ReadFile(note.Path)
		if readErr != nil {
			return note, input, editors, readErr
		}
		text := string(data)
		input.Notes, input.Source = &text, "saved_note"
		if editors.Conflict || editorErr != nil {
			input.SourceWarnings = []string{"Live editor notes could not be used; this recap uses the saved note. " + editors.Reason}
		}
	}
	return note, input, editors, nil
}

// processRecapJob runs only while holding the meeting's worker lock. Provider
// attempts and temporary source waits have separate, durable bounds.
func processRecapJob(ctx context.Context, p paths, id string) (err error) {
	note, err := recapJobNote(p, id)
	if err != nil {
		return err
	}
	job, err := readRecapJob(note)
	if err != nil {
		return err
	}
	if job.State != "queued" && job.State != "generating" {
		return nil
	}
	defer func() {
		if errors.Is(context.Cause(ctx), errRecapCancelled) {
			err = errRecapCancelled
		}
		if err != nil {
			job.State, job.Error = "failed", err.Error()+"; retry with trailmix recap --meeting "+id+" --enqueue"
			if errors.Is(err, errRecapCancelled) {
				job.State = "cancelled"
			}
			err = errors.Join(err, saveRecapJob(note, &job))
		}
	}()
	cfg, err := settings.Read()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(2*cfg.Assistant.TimeoutSeconds+15)*time.Second)
	defer cancel()
	latest, latestErr := latestRecap(ctx, p, id)
	if latestErr == nil {
		// Optional naming failure cannot turn a published recap into a
		// failed background job or provoke another provider request.
		_ = prepareRecapTitle(ctx, p, latest, recapInput{})
		job.State, job.RecapID, job.Error = "ready", latest.RecapID, ""
		if err := saveRecapJob(note, &job); err != nil {
			return err
		}
		scheduleRecapTitle(p, latest)
		return nil
	}
	if !errors.Is(latestErr, errNoRecap) {
		return latestErr
	}
	job.State = "generating"
	if err := saveRecapJob(note, &job); err != nil {
		return err
	}
	ctx, stopCancellationWatch := watchRecapCancellation(ctx, note, job.ID)
	defer stopCancellationWatch()
	if err := recapCancellationError(note, job.ID); err != nil {
		return err
	}
	if job.Input == nil {
		current, input, editors, snapshotErr := snapshotRecapJobInput(ctx, p, note)
		if snapshotErr != nil {
			return snapshotErr
		}
		note = current
		job.EditorConflict, job.EditorReason = editors.Conflict, editors.Reason
		job.Input = &input
	}
	if err := saveRecapJob(note, &job); err != nil {
		return err
	}
	for job.Attempts < recapMaxAttempts {
		current, readErr := recapJobNote(p, id)
		if readErr != nil {
			return readErr
		}
		if current.Directory != note.Directory {
			return errors.New("recording association changed before recap generation")
		}
		job.Input.NotePath, job.Input.Directory = current.Path, note.Directory
		if err := recapCancellationError(note, job.ID); err != nil {
			return err
		}
		before := job.Attempts
		view, runErr := generateRecapAttempt(ctx, p, id, *job.Input, true, func() error {
			if err := recapCancellationError(note, job.ID); err != nil {
				return err
			}
			job.Attempts++
			return saveRecapJob(note, &job)
		})
		if runErr == nil {
			job.State, job.RecapID, job.Error = "ready", view.RecapID, ""
			if err := saveRecapJob(note, &job); err != nil {
				return err
			}
			scheduleRecapTitle(p, view)
			return nil
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if job.Attempts == before {
			if !errors.Is(runErr, errRecapBusy) || job.Waits >= recapMaxWaits {
				return runErr
			}
			job.Waits++
		} else if job.Attempts >= recapMaxAttempts {
			return runErr
		}
		job.Error = runErr.Error()
		if err := saveRecapJob(note, &job); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("recap provider attempt limit reached (%s)", strconv.Itoa(recapMaxAttempts))
}
