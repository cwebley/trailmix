package recorder

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestInterruptedTranscriptionDoesNotFailLiveCapture(t *testing.T) {
	p := testPaths(t)
	dir := healthTranscriptFixture(t, p, status{State: "recording"})
	f, err := os.OpenFile(filepath.Join(dir, "capture.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	data, err := os.ReadFile(filepath.Join(dir, "transcription", "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Preserve meeting identity while changing only the transcription outcome.
	text := strings.ReplaceAll(string(data), `"complete"`, `"interrupted"`)
	if err := os.WriteFile(filepath.Join(dir, "transcription", "status.json"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readTranscript(dir, "")
	if err != nil || snapshot.Capture.State != "recording" || snapshot.Capture.Error != "" || snapshot.Transcription.State != "interrupted" {
		t.Fatalf("transcription failure was presented as capture failure: %+v %v", snapshot, err)
	}
}
