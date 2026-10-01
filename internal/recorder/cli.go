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
	"syscall"
	"time"

	"github.com/cwebley/trailmix/internal/auth"
	"github.com/cwebley/trailmix/internal/settings"
	"github.com/cwebley/trailmix/internal/transcription"
)

const usage = `TrailMix meeting capture, transcription, and notes (macOS 15+)

  trailmix auth                         Enter or replace the saved OpenAI API key
  trailmix auth status                  Show whether a key is saved
  trailmix auth logout                  Remove the saved key
  trailmix devices                      List microphone IDs
  trailmix doctor [--json]              Check the installation, permissions, and settings
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
	case "doctor":
		return doctor(p, args[1:])
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
		return recordCommand(p, args)
	case "transcript", "retry":
		return transcriptCommand(p, args)
	case "status", "stop":
		return statusCommand(p, args)
	default:
		return fmt.Errorf("unknown command %q; see trailmix help", args[0])
	}
}

// recordCommand starts low-level capture, or serves it as the detached recorder.
func recordCommand(p paths, args []string) error {
	var err error
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
}

// transcriptCommand reads a transcript, or replays its incomplete ranges for retry.
func transcriptCommand(p paths, args []string) error {
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
		// Repair output is progress. Stdout carries only the final state.
		prepare.Stdout, prepare.Stderr = os.Stderr, os.Stderr
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
}

// statusCommand reports, or stops, the active or most recent recording.
func statusCommand(p paths, args []string) error {
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
