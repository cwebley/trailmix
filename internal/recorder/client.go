package recorder

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cwebley/trailmix/internal/meeting"
)

// The CLI side of the recorder socket: launching, locking and querying it.

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
