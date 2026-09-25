package meeting_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cwebley/trailmix/internal/meeting"
)

func TestLegacyTranscriptionIdentitySurvivesPromotion(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "transcription"), 0700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "transcription", "meeting.json")
	data := []byte(`{"id":"old-immutable-evidence-id","segmentation_version":1}`)
	if err := os.WriteFile(legacy, data, 0600); err != nil {
		t.Fatal(err)
	}
	id, err := meeting.Ensure(dir, "")
	if err != nil || id != "old-immutable-evidence-id" {
		t.Fatalf("identity replaced: %q %v", id, err)
	}
	again, err := meeting.Read(dir)
	if err != nil || again != id {
		t.Fatalf("identity unreadable: %q %v", again, err)
	}
	got, _ := os.ReadFile(legacy)
	if string(got) != string(data) {
		t.Fatal("legacy evidence metadata rewritten")
	}
	if _, err := meeting.Ensure(dir, meeting.NewID()); err == nil {
		t.Fatal("accepted competing identity")
	}
}
