// Package recorder controls one native capture process independently of the CLI.
package recorder

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cwebley/trailmix/internal/auth"
	"github.com/cwebley/trailmix/internal/meeting"
	"github.com/cwebley/trailmix/internal/settings"
	"github.com/cwebley/trailmix/internal/transcription"
)

type track struct {
	Frames       int64   `json:"frames"`
	SampleRate   float64 `json:"sample_rate"`
	Channels     int     `json:"channels"`
	FirstSeconds float64 `json:"first_seconds"`
	EndSeconds   float64 `json:"end_seconds"`
	Gaps         int     `json:"discontinuities"`
	Peak         float64 `json:"peak"`
	PeakMax      float64 `json:"peak_max"`
}

type event struct {
	Type           string           `json:"type"`
	Error          string           `json:"error,omitempty"`
	ElapsedSeconds float64          `json:"elapsed_seconds"`
	Tracks         map[string]track `json:"tracks,omitempty"`
}

type status struct {
	Note            *meetingNote          `json:"note,omitempty"`
	MeetingID       string                `json:"meeting_id,omitempty"`
	Live            bool                  `json:"live"`
	MicrophoneName  string                `json:"microphone_name,omitempty"`
	State           string                `json:"state"`
	Directory       string                `json:"directory,omitempty"`
	Microphone      string                `json:"microphone,omitempty"`
	StartedAt       time.Time             `json:"started_at"`
	ElapsedSeconds  float64               `json:"elapsed_seconds"`
	ClockAgeSeconds *float64              `json:"clock_age_seconds,omitempty"`
	Tracks          map[string]track      `json:"tracks,omitempty"`
	Error           string                `json:"error,omitempty"`
	Warnings        []string              `json:"warnings,omitempty"`
	Transcription   *transcription.Status `json:"transcription,omitempty"`
}

type paths struct {
	home, socket, helper string
	transcribe           bool
	meetingID            string
	maxDuration          time.Duration
}

// Recordings stop on their own so a forgotten session cannot run overnight.
const defaultMaxDuration = 3 * time.Hour

func config() (paths, error) {
	executable, err := os.Executable()
	if err != nil {
		return paths{}, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return paths{}, err
	}
	home := os.Getenv("TRAILMIX_HOME")
	if home == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return paths{}, err
		}
		home = filepath.Join(base, "TrailMix")
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return paths{}, err
	}
	helper := os.Getenv("TRAILMIX_CAPTURE_HELPER")
	if helper == "" {
		helper = filepath.Join(filepath.Dir(executable), "TrailMixCapture.app", "Contents", "MacOS", "trailmix-capture")
	}
	helper, err = filepath.Abs(helper)
	if err != nil {
		return paths{}, err
	}
	p := paths{home: home, socket: filepath.Join(home, "recorder.sock"), helper: helper, maxDuration: defaultMaxDuration}
	if len(p.socket) >= 104 {
		return paths{}, errors.New("socket path is too long; set TRAILMIX_HOME to a shorter absolute path")
	}
	return p, nil
}

const usage = `TrailMix meeting capture, transcription, and notes (macOS 15+)

  trailmix auth                         Enter or replace the saved OpenAI API key
  trailmix auth status                  Show whether a key is saved
  trailmix auth logout                  Remove the saved key
  trailmix devices                      List microphone IDs
  trailmix start                        Record with transcription and open new meeting notes
  trailmix record [--mic ID] [--output DIR] [--transcribe] [--max-duration 3h]  Low-level background capture
  trailmix toggle                       Start a meeting, or stop active capture
  trailmix open                         Open active or most recent meeting notes
  trailmix config [show | set KEY VALUE] Configure notes_root, template, microphone
  trailmix status                       Show active or last recording as JSON
  trailmix status --tmux                Short recording label for a tmux status line
  trailmix stop                         Stop and finalize local tracks
  trailmix transcript [--meeting ID | --output DIR] [--json]  Read a meeting's transcript
  trailmix transcript --meeting ID --export  Write the transcript beside its note
  trailmix ask --meeting ID [--question TEXT]  Ask the configured assistant; otherwise reads JSON stdin
  trailmix answers --meeting ID          Read saved AI answers as JSON
  trailmix retry [--output DIR]          Retry incomplete transcription after stop
  trailmix version                      Print the version

For record, --output must be a new directory. The default is under TrailMix's recordings directory.
The first recording may ask for Microphone and Screen & System Audio Recording access.
--transcribe sends both tracks to OpenAI using the key saved by trailmix auth.
Credentials use $XDG_CONFIG_HOME/trailmix or ~/.config/trailmix on macOS and Linux.
`

func Run(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(usage)
		return nil
	}
	if args[0] == "auth" {
		return auth.Run(args[1:])
	}
	if args[0] == "config" {
		return settings.Run(args[1:])
	}
	p, err := config()
	if err != nil {
		return err
	}
	switch args[0] {
	case "start", "record", "toggle", "open", "stop":
		guard, err := lifecycleLock(p)
		if err != nil {
			return err
		}
		defer guard.Close()
	}
	switch args[0] {
	case "ask", "answers":
		return runAnswers(p, args[0], args[1:])
	case "start", "toggle", "open":
		if len(args) != 1 {
			return fmt.Errorf("usage: trailmix %s", args[0])
		}
		return workspace(p, args[0])
	case "devices":
		if len(args) != 1 {
			return errors.New("usage: trailmix devices")
		}
		cmd := exec.Command(p.helper, "devices")
		cmd.Env = captureEnvironment()
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	case "record", "_record":
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		mic := flags.String("mic", "", "microphone unique ID; defaults to the system input selected at start")
		output := flags.String("output", "", "new recording directory")
		transcribe := flags.Bool("transcribe", false, "stream both tracks to hosted transcription")
		flags.DurationVar(&p.maxDuration, "max-duration", defaultMaxDuration, "stop automatically after this long")
		if args[0] == "_record" {
			flags.StringVar(&p.meetingID, "meeting-id", "", "internal meeting association")
		}
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		p.transcribe = *transcribe
		if p.maxDuration <= 0 {
			return errors.New("--max-duration must be positive")
		}
		if *output != "" {
			*output, err = filepath.Abs(*output)
			if err != nil {
				return err
			}
		}
		if args[0] == "_record" {
			return serve(p, *output, *mic)
		}
		preferences, err := settings.Read()
		if err != nil {
			return err
		}
		micProvided := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "mic" {
				micProvided = true
			}
		})
		if !micProvided {
			*mic = preferences.Microphone
		}
		s, err := launch(p, *output, *mic)
		if err != nil {
			return err
		}
		return printStatus(s)
	case "transcript", "retry":
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		output := flags.String("output", "", "recording directory; defaults to current or last")
		var meetingID string
		var asJSON bool
		var export bool
		if args[0] == "transcript" {
			flags.StringVar(&meetingID, "meeting", "", "read the transcript associated with this meeting ID")
			flags.BoolVar(&asJSON, "json", false, "return a structured transcript snapshot")
			flags.BoolVar(&export, "export", false, "write a transcript Markdown file alongside the associated note")
		}
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		meetingProvided := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "meeting" {
				meetingProvided = true
			}
		})
		if meetingProvided {
			if *output != "" {
				return errors.New("choose --meeting or --output, not both")
			}
			note, err := readMeeting(p, meetingID)
			if err != nil {
				return fmt.Errorf("resolve meeting transcript: %w", err)
			}
			if !filepath.IsAbs(note.Directory) {
				return errors.New("meeting has no associated recording directory")
			}
			*output = note.Directory
		}
		if export && !meetingProvided {
			return errors.New("--export requires --meeting")
		}
		if *output == "" {
			s, err := request(p.socket, "status")
			if unavailable(err) {
				s, err = lastStatus(p)
			}
			if err != nil {
				return err
			}
			if s.Directory == "" {
				return errors.New("no recording available")
			}
			*output = s.Directory
		}
		if args[0] == "retry" {
			// Keep the recorder lock for the whole retry so capture cannot restart
			// while this command is reading its queues.
			guard, err := lock(p)
			if err != nil {
				return errors.New("stop the active recorder before retrying transcription")
			}
			defer guard.Close()
			key, err := auth.APIKey()
			if err != nil {
				return err
			}
			if key == "" {
				return errors.New("no saved OpenAI API key; run trailmix auth before retrying")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			nativeGuard, err := os.OpenFile(filepath.Join(*output, "capture.lock"), os.O_RDWR, 0600)
			if err != nil {
				return fmt.Errorf("capture lock unavailable: %w", err)
			}
			defer nativeGuard.Close()
			if err := syscall.Flock(int(nativeGuard.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				return errors.New("native capture is still finalizing; retry shortly")
			}
			prepare := exec.CommandContext(ctx, p.helper, "prepare-stream", "--output", *output)
			prepare.Env = captureEnvironment()
			prepare.Stdout, prepare.Stderr = os.Stdout, os.Stderr
			if err := prepare.Run(); err != nil {
				return fmt.Errorf("prepare transcription audio: %w", err)
			}
			state, err := transcription.Retry(ctx, *output, key)
			if _, readErr := readMeeting(p, state.MeetingID); readErr == nil {
				if exportErr := exportMeetingTranscript(p, state.MeetingID); exportErr != nil {
					fmt.Fprintln(os.Stderr, "Transcript export:", exportErr)
				}
			}
			_ = json.NewEncoder(os.Stdout).Encode(state)
			return err
		}
		snapshot, err := readTranscript(*output, meetingID)
		if err != nil {
			return err
		}
		if export {
			note, err := readMeeting(p, meetingID)
			if err != nil {
				return err
			}
			path, err := exportTranscript(p, note)
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "Transcript exported to", path)
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(snapshot)
		}
		passages, state := snapshot.Passages, snapshot.Transcription
		fmt.Printf("Transcription: %s (%s)\n", state.State, state.Model)
		for _, passage := range passages {
			fmt.Printf("[%.2f–%.2f %s] %s\n  %s\n", passage.Start, passage.End, passage.Track, passage.Text, passage.ID)
		}
		for _, name := range []string{"microphone", "system"} {
			t := state.Tracks[name]
			if t.Error != "" {
				fmt.Printf("%s: %s\n", name, t.Error)
			}
			for _, gap := range t.Pending {
				fmt.Printf("[incomplete %.2f–%.2f %s]\n", gap.Start, gap.End, name)
			}
			for _, text := range t.Provisional {
				fmt.Printf("[provisional %s] %s\n", name, text)
			}
		}
		return nil
	case "status", "stop":
		if len(args) == 2 && args[0] == "status" && args[1] == "--tmux" {
			// Runs from a tmux status line; it must stay fast and never fail loudly.
			s, err := request(p.socket, "status")
			if err != nil {
				s = status{}
			}
			label := tmuxStatus(s)
			if label == "" {
				// A crashed recorder cannot clear its flag; clear it here.
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_ = tmuxListener(ctx, s)
				cancel()
			}
			fmt.Println(label)
			return nil
		}
		if len(args) != 1 {
			return fmt.Errorf("usage: trailmix %s", args[0])
		}
		s, err := request(p.socket, args[0])
		fromDisk := false
		if err != nil {
			if !unavailable(err) {
				return err
			}
			s, err = lastStatus(p)
			fromDisk = true
			if err != nil {
				return err
			}
		}
		if fromDisk {
			if _, latest, readErr := transcription.Read(s.Directory); readErr == nil {
				if s.State == "interrupted" {
					latest.State = "interrupted"
				}
				// Persist errors attached to the final session remain visible even
				// when transcription/status.json is an older successful write.
				if s.Transcription == nil || latest.UpdatedAt.After(s.Transcription.UpdatedAt) {
					s.Transcription = &latest
				}
			}
		}
		return printStatus(s)
	default:
		return fmt.Errorf("unknown command %q; see trailmix help", args[0])
	}
}

func printStatus(s status) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return err
	}
	if s.Error != "" {
		return errors.New(s.Error)
	}
	return nil
}

func unavailable(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func request(socket, command string) (status, error) {
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return status{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(40 * time.Second))
	if err := json.NewEncoder(conn).Encode(map[string]string{"command": command}); err != nil {
		return status{}, err
	}
	var s status
	err = json.NewDecoder(conn).Decode(&s)
	s.Live = err == nil
	return s, err
}

func launch(p paths, output, mic string) (status, error) {
	s, err := recorderState(p)
	if err != nil {
		return status{}, err
	}
	if s.Live {
		return s, nil
	}
	if _, err := os.Stat(p.helper); err != nil {
		return status{}, fmt.Errorf("capture helper not found; run make build: %w", err)
	}
	if err := os.MkdirAll(p.home, 0700); err != nil {
		return status{}, err
	}
	// A separate log per launch lets concurrent starts report their own failures.
	log, err := os.CreateTemp(p.home, "launch-*.log")
	if err != nil {
		return status{}, err
	}
	defer log.Close()
	executable, err := os.Executable()
	if err != nil {
		return status{}, err
	}
	cmd := exec.Command(executable, "_record", "--output", output, "--mic", mic, fmt.Sprintf("--transcribe=%t", p.transcribe), "--max-duration", p.maxDuration.String(), "--meeting-id", p.meetingID)
	cmd.Env = append(os.Environ(), "TRAILMIX_HOME="+p.home, "TRAILMIX_CAPTURE_HELPER="+p.helper)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return status{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-ticker.C:
			if s, err := request(p.socket, "status"); err == nil {
				fmt.Fprintln(os.Stderr, "Recorder running. Use trailmix status to check both tracks; trailmix stop to finish.")
				return s, nil
			}
		case err := <-done:
			// Another simultaneous start may have acquired the recording lock.
			if s, requestErr := request(p.socket, "status"); requestErr == nil {
				return s, nil
			}
			data, _ := os.ReadFile(log.Name())
			return status{}, fmt.Errorf("recorder exited (%v): %s", err, data)
		case <-timeout.C:
			return status{}, fmt.Errorf("recorder startup is taking too long; check %s and run trailmix status", log.Name())
		}
	}
}

// flock, rather than a PID file, prevents concurrent launches and stale-PID mistakes.
func lock(p paths) (*os.File, error) {
	if err := os.MkdirAll(p.home, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(p.home, "recorder.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another recorder is running: %w", err)
	}
	return f, nil
}

func lastStatus(p paths) (status, error) {
	// If a recorder holds the lock, a missing socket is a startup/shutdown interval.
	f, err := lock(p)
	if err != nil {
		return status{}, errRecorderTransition
	}
	defer f.Close()
	data, err := os.ReadFile(filepath.Join(p.home, "last.json"))
	if errors.Is(err, os.ErrNotExist) {
		return status{State: "idle"}, nil
	}
	if err != nil {
		return status{}, err
	}
	var s status
	if err := json.Unmarshal(data, &s); err != nil {
		return status{}, err
	}
	s.Live = false
	if s.MeetingID == "" && s.Directory != "" {
		s.MeetingID, err = meeting.Read(s.Directory)
		if err != nil {
			return status{}, err
		}
	}
	if s.State == "starting" || s.State == "recording" || s.State == "stopping" {
		s.State = "interrupted"
		s.Error = "recorder exited without finalizing; completed local writes remain in the recording directory"
	}
	return s, nil
}

type session struct {
	mu           sync.Mutex
	status       status
	stop         chan struct{}
	finished     chan struct{}
	once         sync.Once
	transcriber  *transcription.Runner
	clockUpdated time.Time
	indicator    *indicator
}

func (s *session) snapshot() status {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.status // Track maps are replaced, never mutated after publication.
	result.Warnings = append([]string(nil), s.status.Warnings...)
	if !s.clockUpdated.IsZero() {
		age := time.Since(s.clockUpdated).Seconds()
		result.ClockAgeSeconds = &age
	}
	if s.transcriber != nil {
		state := s.transcriber.Snapshot()
		result.Transcription = &state
	}
	if (result.State == "recording" || result.State == "stopped") && result.ElapsedSeconds >= 3 {
		for _, name := range []string{"microphone", "system"} {
			t := result.Tracks[name]
			if t.Frames == 0 {
				result.Warnings = append(result.Warnings, name+": no audio buffers received")
			} else if result.ElapsedSeconds-t.EndSeconds > 3 {
				result.Warnings = append(result.Warnings, name+": no recent audio buffers; source may be silent or capture may be stalled")
			} else if t.PeakMax == 0 {
				result.Warnings = append(result.Warnings, name+": only silent samples received; check the input/output route and mute settings")
			}
		}
	}
	return result
}

func (s *session) stopRecording() { s.once.Do(func() { close(s.stop) }) }

func serve(p paths, output, mic string) error {
	// Deferred before the lock so the final status-line update is delivered
	// after other commands can already read the stopped recording.
	indicator := newIndicator(tmuxListener)
	defer indicator.close()
	guard, err := lock(p)
	if err != nil {
		return err
	}
	defer guard.Close()
	if output == "" {
		root := filepath.Join(p.home, "recordings")
		if err := os.MkdirAll(root, 0700); err != nil {
			return err
		}
		output, err = os.MkdirTemp(root, time.Now().Format("2006-01-02T150405")+"-")
	} else {
		// Never reopen an existing directory, even an empty one.
		err = os.Mkdir(output, 0700)
	}
	if err != nil {
		return fmt.Errorf("create new recording directory: %w", err)
	}
	id, err := meeting.Ensure(output, p.meetingID)
	if err != nil {
		return err
	}
	s := &session{
		status: status{State: "starting", MeetingID: id, Directory: output, Microphone: mic, StartedAt: time.Now().UTC()},
		stop:   make(chan struct{}), finished: make(chan struct{}),
		indicator: indicator,
	}
	if p.meetingID != "" {
		note, err := readMeeting(p, p.meetingID)
		if err != nil {
			return err
		}
		note.Directory = output
		s.status.Note = &note
		if err := saveMeeting(p, note); err != nil {
			return err
		}
		if err := meeting.WriteJSON(filepath.Join(p.home, "latest-meeting.json"), note); err != nil {
			return err
		}
	}
	if err := saveStatus(p, s.snapshot()); err != nil {
		return err
	}
	s.indicator.publish(s.snapshot())
	if err := os.Remove(p.socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", p.socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(p.socket, 0600); err != nil {
		return err
	}
	var clients sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			clients.Add(1)
			go func() {
				defer clients.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(40 * time.Second))
				var req struct {
					Command string `json:"command"`
				}
				if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&req); err != nil {
					return
				}
				if req.Command == "stop" {
					s.stopRecording()
					<-s.finished
				} else if req.Command != "status" {
					_ = json.NewEncoder(conn).Encode(status{State: "error", Error: "unknown recorder command"})
					return
				}
				_ = json.NewEncoder(conn).Encode(s.snapshot())
			}()
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
			s.stopRecording()
		case <-s.finished:
		}
	}()
	limit := time.AfterFunc(p.maxDuration, func() {
		s.mu.Lock()
		s.status.Warnings = append(s.status.Warnings, fmt.Sprintf("stopped automatically at the %s recording limit", p.maxDuration))
		s.mu.Unlock()
		s.stopRecording()
	})
	defer limit.Stop()
	err = capture(p, s)
	if s.transcriber != nil {
		if finishErr := s.transcriber.Finish(); finishErr != nil {
			s.transcriber.Fail(fmt.Errorf("persist transcription: %w", finishErr))
		}
	}
	s.mu.Lock()
	if err != nil {
		s.status.State, s.status.Error = "failed", err.Error()
	} else {
		s.status.State = "stopped"
	}
	s.mu.Unlock()
	if saveErr := saveStatus(p, s.snapshot()); saveErr != nil {
		s.mu.Lock()
		s.status.State, s.status.Error = "failed", fmt.Sprintf("persist final status: %v", saveErr)
		s.mu.Unlock()
		err = errors.Join(err, saveErr)
	}
	s.indicator.publish(s.snapshot())
	if s.status.Note != nil && s.transcriber != nil {
		if exportErr := exportMeetingTranscript(p, s.status.MeetingID); exportErr != nil {
			s.mu.Lock()
			s.status.Warnings = append(s.status.Warnings, "transcript sidecar: "+exportErr.Error())
			s.mu.Unlock()
			_ = saveStatus(p, s.snapshot())
		}
	}
	close(s.finished)
	listener.Close()
	<-acceptDone
	clients.Wait()
	return err
}

func capture(p paths, s *session) error {
	current := s.snapshot()
	log, err := os.OpenFile(filepath.Join(current.Directory, "capture.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(p.helper, "record", "--output", current.Directory, "--mic", current.Microphone)
	cmd.Env = captureEnvironment()
	if p.transcribe {
		cmd.Args = append(cmd.Args, "--stream", "true")
		key, streamErr := auth.APIKey()
		var runner *transcription.Runner
		if streamErr == nil {
			runner, streamErr = transcription.Start(current.Directory, key)
		}
		s.mu.Lock()
		if streamErr != nil {
			s.status.Transcription = &transcription.Status{Model: transcription.Model, State: "degraded", Tracks: map[string]transcription.TrackStatus{"microphone": {State: "degraded", Error: streamErr.Error()}, "system": {State: "degraded", Error: streamErr.Error()}}}
		} else {
			s.transcriber = runner
		}
		s.mu.Unlock()
	}
	cmd.Stderr = log
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Go owns the pipe. If Go crashes, EOF tells the helper to finalize and exit.
	processDone := make(chan struct{})
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		select {
		case <-s.stop:
			s.mu.Lock()
			s.status.State = "stopping"
			s.mu.Unlock()
			s.indicator.publish(s.snapshot())
			stdin.Close()
			select {
			case <-processDone:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
			}
		case <-processDone:
		}
	}()
	var protocolErr error
	stopped := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		var e event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			protocolErr = fmt.Errorf("invalid capture event: %w", err)
			s.stopRecording()
			break
		}
		s.mu.Lock()
		switch e.Type {
		case "started":
			s.clockUpdated = time.Now()
			s.status.ElapsedSeconds = e.ElapsedSeconds
			// Native metadata records the selected default, not merely the request.
			var selected struct {
				ID   string `json:"microphone_id"`
				Name string `json:"microphone_name"`
			}
			if data, err := os.ReadFile(filepath.Join(current.Directory, "capture.json")); err == nil && json.Unmarshal(data, &selected) == nil {
				s.status.Microphone, s.status.MicrophoneName = selected.ID, selected.Name
			}
			if s.transcriber != nil {
				s.transcriber.CaptureClock(e.ElapsedSeconds)
			}
			if s.status.State == "starting" {
				s.status.State = "recording"
			}
		case "progress", "stopped":
			s.clockUpdated = time.Now()
			s.status.ElapsedSeconds, s.status.Tracks = e.ElapsedSeconds, e.Tracks
			stopped = e.Type == "stopped"
		case "error":
			protocolErr = errors.New(e.Error)
		case "transcription_error":
			if s.transcriber != nil {
				s.transcriber.Fail(errors.New(e.Error))
			}
		}
		s.mu.Unlock()
		if s.transcriber != nil {
			if err := s.transcriber.Persist(); err != nil {
				s.transcriber.Fail(fmt.Errorf("persist transcription: %w", err))
			}
		}
		if err := saveStatus(p, s.snapshot()); err != nil {
			protocolErr = fmt.Errorf("persist recording status: %w", err)
			s.stopRecording()
		}
		s.indicator.publish(s.snapshot())
	}
	if err := scanner.Err(); err != nil {
		protocolErr = errors.Join(protocolErr, err)
		s.stopRecording()
	}
	err = cmd.Wait()
	close(processDone)
	<-stopDone
	if protocolErr != nil {
		return protocolErr
	}
	if err != nil {
		return fmt.Errorf("native capture failed: %w; see %s", err, log.Name())
	}
	if !stopped {
		return errors.New("native capture exited without a finalization event")
	}
	return nil
}

func saveStatus(p paths, s status) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	for _, path := range []string{filepath.Join(s.Directory, "session.json"), filepath.Join(p.home, "last.json")} {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

func captureEnvironment() []string {
	env := os.Environ()
	result := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, "OPENAI_API_KEY=") {
			result = append(result, entry)
		}
	}
	return result
}
