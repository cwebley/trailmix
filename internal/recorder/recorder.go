// Package recorder controls one native capture process independently of the CLI.
package recorder

import (
	"bufio"
	"encoding/json"
	"errors"
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
