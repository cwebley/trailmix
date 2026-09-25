package recorder

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cwebley/trailmix/internal/meeting"
	"github.com/cwebley/trailmix/internal/transcription"
)

// A read-only snapshot for editor clients. Meeting selection never falls back
// to the latest recording, and finalized passage identities survive reopening.
type transcriptSnapshot struct {
	MeetingID     string                  `json:"meeting_id"`
	Duration      *float64                `json:"duration_seconds,omitempty"`
	Passages      []transcription.Passage `json:"passages"`
	Transcription transcription.Status    `json:"transcription"`
}

func readTranscript(dir, expectedID string) (transcriptSnapshot, error) {
	var result transcriptSnapshot
	id, err := meeting.Read(dir)
	if err != nil {
		return result, err
	}
	if id == "" || (expectedID != "" && id != expectedID) {
		return result, errors.New("recording meeting identity mismatch")
	}
	passages, state, err := transcription.Read(dir)
	if err != nil {
		return result, fmt.Errorf("read transcript: %w", err)
	}
	if state.MeetingID != id {
		return result, errors.New("transcript meeting identity mismatch")
	}
	seen := make(map[string]bool, len(passages))
	for _, passage := range passages {
		if passage.MeetingID != id {
			return result, errors.New("passage meeting identity mismatch")
		}
		if !passageID.MatchString(passage.ID) || seen[passage.ID] || passage.Start < 0 || passage.End < passage.Start || (passage.Track != "microphone" && passage.Track != "system") {
			return result, errors.New("invalid or duplicate finalized passage")
		}
		seen[passage.ID] = true
	}
	if passages == nil {
		passages = []transcription.Passage{}
	}
	result.MeetingID, result.Passages, result.Transcription = id, passages, state
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil // Older transcription-only fixtures may have no session.
	}
	if err != nil {
		return result, err
	}
	var session status
	if err := json.Unmarshal(data, &session); err != nil {
		return result, err
	}
	if session.MeetingID != "" && session.MeetingID != id {
		return result, errors.New("session meeting identity mismatch")
	}
	result.Duration = &session.ElapsedSeconds
	// A final session can retain a persistence error newer than status.json.
	if session.Transcription != nil && session.Transcription.UpdatedAt.After(state.UpdatedAt) {
		if session.Transcription.MeetingID != "" && session.Transcription.MeetingID != id {
			return result, errors.New("session transcript identity mismatch")
		}
		result.Transcription = *session.Transcription
		result.Transcription.MeetingID = id
	}
	if state.State == "interrupted" {
		result.Transcription.State = "interrupted"
	}
	return result, nil
}
