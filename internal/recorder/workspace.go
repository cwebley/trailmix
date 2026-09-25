package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cwebley/trailmix/internal/meeting"
	"github.com/cwebley/trailmix/internal/navigation"
	"github.com/cwebley/trailmix/internal/settings"
)

type meetingNote struct {
	navigation.Target
	Directory string `json:"directory,omitempty"`
}

var errRecorderTransition = errors.New("recorder is starting or stopping; retry shortly")

// A terminal state can be published before the old process releases its socket
// and writer lock. Do not allocate another note during that teardown interval.
func recorderState(p paths) (status, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		s, err := request(p.socket, "status")
		if err == nil && (s.State == "starting" || s.State == "recording" || s.State == "stopping") {
			return s, nil
		}
		if unavailable(err) {
			s, err = lastStatus(p)
			if err == nil {
				return s, nil
			}
		}
		if err != nil && !errors.Is(err, errRecorderTransition) {
			return status{}, err
		}
		if time.Now().After(deadline) {
			return status{}, errRecorderTransition
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// All public lifecycle commands share this lock, including low-level record.
// The detached recorder holds its separate writer lock for its entire lifetime.
func lifecycleLock(p paths) (*os.File, error) {
	if err := os.MkdirAll(p.home, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(p.home, "workspace.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(50 * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("meeting command is busy; retry shortly: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readMeeting(p paths, id string) (meetingNote, error) {
	var note meetingNote
	if !meeting.ValidID(id) {
		return note, errors.New("invalid meeting ID")
	}
	data, err := os.ReadFile(filepath.Join(p.home, "meetings", id+".json"))
	if err != nil {
		return note, err
	}
	err = json.Unmarshal(data, &note)
	if err == nil && note.ID != id {
		err = errors.New("meeting association ID mismatch")
	}
	return note, err
}

func saveMeeting(p paths, note meetingNote) error {
	root := filepath.Join(p.home, "meetings")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	return meeting.WriteJSON(filepath.Join(root, note.ID+".json"), note)
}

func newNote(p paths, cfg settings.Settings, now time.Time) (meetingNote, error) {
	note := meetingNote{Target: navigation.Target{
		ID: meeting.NewID(), Date: now.Format("2006-01-02"),
		Session: "trailmix-" + now.Format("2006-01-02"), Window: now.Format("15:04"),
	}}
	text := "# Meeting {{date}} {{time}}\n\n\n"
	if cfg.Template != "" {
		data, err := os.ReadFile(cfg.Template)
		if err != nil {
			return note, fmt.Errorf("read meeting template: %w", err)
		}
		text = string(data)
		if strings.Contains(text, "<!-- trailmix: meeting ") {
			return note, errors.New("template must not contain a meeting identity")
		}
	}
	text = strings.NewReplacer("{{date}}", note.Date, "{{time}}", now.Format("15:04:05")).Replace(text)
	text = "<!-- trailmix: meeting " + note.ID + " -->\n" + text
	daily := filepath.Join(cfg.NotesRoot, note.Date)
	if err := os.MkdirAll(daily, 0700); err != nil {
		return note, err
	}
	for i := 0; ; i++ {
		name := now.Format("15-04-05")
		if i > 0 {
			name += fmt.Sprintf("-%d", i+1)
		}
		note.Path = filepath.Join(daily, name+".md")
		f, err := os.OpenFile(note.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return note, err
		}
		_, writeErr := f.WriteString(text)
		if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
			return note, err
		}
		dir, err := os.Open(daily)
		if err != nil {
			return note, err
		}
		if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
			return note, err
		}
		break
	}
	return note, saveMeeting(p, note)
}

func validateNote(note meetingNote) error {
	if !meeting.ValidID(note.ID) || !filepath.IsAbs(note.Path) || note.Session != "trailmix-"+note.Date {
		return errors.New("invalid meeting association")
	}
	data, err := os.ReadFile(note.Path)
	if err != nil {
		return fmt.Errorf("open meeting note: %w", err)
	}
	if strings.Count(string(data), "<!-- trailmix: meeting ") != 1 || !strings.Contains(string(data), "<!-- trailmix: meeting "+note.ID+" -->") {
		return errors.New("meeting note identity is missing or changed")
	}
	return nil
}

func checkMicrophone(p paths, mic string) error {
	if mic == "" {
		return nil
	} // Native capture selects the current system default.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.helper, "devices")
	cmd.Env = captureEnvironment()
	data, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("check pinned microphone: %w", err)
	}
	var devices struct {
		Microphones []struct {
			ID string `json:"id"`
		} `json:"microphones"`
	}
	if err := json.Unmarshal(data, &devices); err != nil {
		return err
	}
	for _, device := range devices.Microphones {
		if device.ID == mic {
			return nil
		}
	}
	return fmt.Errorf("pinned microphone %q is unavailable; use trailmix devices or clear the microphone setting", mic)
}

func workspace(p paths, command string) error {
	s, err := recorderState(p)
	if err != nil {
		return err
	}
	active := s.Live && (s.State == "starting" || s.State == "recording" || s.State == "stopping")
	if active && command == "toggle" {
		stopped, err := request(p.socket, "stop")
		if err != nil {
			return err
		}
		return printStatus(stopped)
	}
	if active {
		if s.Note == nil {
			return errors.New("active low-level recording has no meeting note; use trailmix stop first")
		}
		return openNote(p, *s.Note, &s)
	}
	if command == "open" {
		data, err := os.ReadFile(filepath.Join(p.home, "latest-meeting.json"))
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("no meeting note available")
		}
		if err != nil {
			return err
		}
		var note meetingNote
		if err := json.Unmarshal(data, &note); err != nil {
			return err
		}
		return openNote(p, note, nil)
	}
	cfg, err := settings.Read()
	if err != nil {
		return err
	}
	nav, err := navigation.Preflight(p.home)
	if err != nil {
		return err
	}
	if info, err := os.Stat(p.helper); err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return errors.New("capture helper is unavailable; run make build")
	}
	if err := checkMicrophone(p, cfg.Microphone); err != nil {
		return err
	}
	note, err := newNote(p, cfg, time.Now())
	if err != nil {
		return err
	}
	p.transcribe, p.meetingID = true, note.ID
	s, err = launch(p, "", cfg.Microphone)
	if err != nil {
		return fmt.Errorf("%w; note retained at %s; check trailmix status before retrying", err, note.Path)
	}
	// A socket can be available before capture permissions and setup finish.
	deadline := time.Now().Add(15 * time.Second)
	for s.State == "starting" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		s, err = request(p.socket, "status")
		if unavailable(err) {
			s, err = lastStatus(p)
		}
		if err != nil {
			return fmt.Errorf("capture startup uncertain: %w; note: %s; use trailmix status/stop", err, note.Path)
		}
	}
	if s.MeetingID != note.ID {
		return errors.New("recorder identity changed during start; refusing to associate another recording")
	}
	if s.State != "recording" {
		_ = printStatus(s)
		return fmt.Errorf("capture is %s; note retained at %s; use trailmix status, open, or stop", s.State, note.Path)
	}
	if err := printStatus(s); err != nil {
		return err
	}
	if err := nav.Open(note.Target); err != nil {
		return navigationError(note, &s, err)
	}
	return nil
}

func openNote(p paths, note meetingNote, s *status) error {
	if err := validateNote(note); err != nil {
		return navigationError(note, s, err)
	}
	nav, err := navigation.Preflight(p.home)
	if err == nil {
		err = nav.Open(note.Target)
	}
	if err != nil {
		return navigationError(note, s, err)
	}
	if s != nil {
		return printStatus(*s)
	}
	return json.NewEncoder(os.Stdout).Encode(note)
}

func navigationError(note meetingNote, s *status, err error) error {
	state := "note retained"
	if s != nil {
		state = "capture remains " + s.State
	}
	return fmt.Errorf("%s; navigation failed: %w; note: %s; tmux attach -t %s; retry with trailmix open, finish with trailmix stop", state, err, note.Path, note.Session)
}
