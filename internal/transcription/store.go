// Package transcription streams the native helper's durable PCM queue. Capture
// never calls into this package and never waits for the provider.
package transcription

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/cwebley/trailmix/internal/meeting"
)

const Model = "gpt-live-transcribe"

type Range struct {
	ID     string  `json:"id"`
	Track  string  `json:"track"`
	Offset int64   `json:"byte_offset"`
	Bytes  int64   `json:"bytes"`
	Start  float64 `json:"start_seconds"`
	End    float64 `json:"end_seconds"`
}

type Passage struct {
	Range
	MeetingID             string          `json:"meeting_id"`
	Version               int             `json:"version"`
	Model                 string          `json:"model"`
	ItemID                string          `json:"provider_item_id"`
	SessionID             string          `json:"provider_session_id"`
	Text                  string          `json:"text"`
	FinalizedAt           time.Time       `json:"finalized_at"`
	CommitToFinalSeconds  float64         `json:"commit_to_final_seconds"`
	CaptureToFinalSeconds *float64        `json:"capture_to_final_seconds,omitempty"`
	Usage                 json.RawMessage `json:"usage,omitempty"`
}

type TrackStatus struct {
	State            string            `json:"state"`
	Error            string            `json:"error,omitempty"`
	Finals           int               `json:"finals"`
	EmptyFinals      int               `json:"empty_finals"`
	Pending          []Range           `json:"incomplete_ranges,omitempty"`
	Provisional      map[string]string `json:"provisional,omitempty"`
	SubmittedSeconds float64           `json:"submitted_seconds"`
	EstimatedUSD     float64           `json:"estimated_usd"`
}

type Status struct {
	MeetingID           string                 `json:"meeting_id"`
	UpdatedAt           time.Time              `json:"updated_at"`
	FinalizationSeconds float64                `json:"finalization_seconds,omitempty"`
	Model               string                 `json:"model"`
	State               string                 `json:"state"`
	Tracks              map[string]TrackStatus `json:"tracks"`
}

type turn struct {
	Range
	Silence bool `json:"silence"`
}

type store struct {
	mu                  sync.Mutex
	dir                 string
	meeting             string
	turns               map[string]turn
	finals              map[string]Passage
	tracks              map[string]TrackStatus
	active              map[string]Range
	guard               *os.File
	origin              time.Time
	finalizationSeconds float64
}

func openStore(dir string) (*store, error) {
	root := filepath.Join(dir, "transcription")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	guard, err := os.OpenFile(filepath.Join(root, "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		guard.Close()
		return nil, errors.New("transcription is already running for this recording")
	}
	s := &store{dir: root, meeting: filepath.Base(dir), turns: map[string]turn{}, finals: map[string]Passage{}, tracks: map[string]TrackStatus{}, active: map[string]Range{}, guard: guard}
	id, err := meeting.Ensure(dir, "")
	if err != nil {
		guard.Close()
		return nil, err
	}
	metadataPath := filepath.Join(root, "meeting.json")
	var metadata struct {
		ID                  string `json:"id"`
		SegmentationVersion int    `json:"segmentation_version"`
	}
	if data, err := os.ReadFile(metadataPath); err == nil {
		if err := json.Unmarshal(data, &metadata); err != nil || metadata.ID == "" || metadata.SegmentationVersion != 1 {
			guard.Close()
			return nil, errors.New("invalid or unsupported transcription metadata")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		metadata.ID, metadata.SegmentationVersion = id, 1
		if err := atomicJSON(metadataPath, metadata); err != nil {
			guard.Close()
			return nil, err
		}
	} else {
		guard.Close()
		return nil, err
	}
	s.meeting = metadata.ID
	for _, track := range []string{"microphone", "system"} {
		s.tracks[track] = TrackStatus{State: "starting", Provisional: map[string]string{}}
	}
	entries, err := os.ReadDir(root)
	if err == nil {
		for _, e := range entries {
			if filepath.Ext(e.Name()) != ".json" {
				continue
			}
			data, readErr := os.ReadFile(filepath.Join(root, e.Name()))
			if readErr != nil {
				err = readErr
				break
			}
			switch filepath.Ext(e.Name()[:len(e.Name())-5]) {
			case ".turn":
				var t turn
				if err = json.Unmarshal(data, &t); err == nil {
					s.turns[t.ID] = t
				}
			case ".passage":
				var p Passage
				if err = json.Unmarshal(data, &p); err == nil {
					s.finals[p.ID] = p
				}
			}
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		guard.Close()
		return nil, err
	}
	// Preserve submitted-duration estimates across explicit retries.
	if data, err := os.ReadFile(filepath.Join(root, "status.json")); err == nil {
		var prior Status
		if json.Unmarshal(data, &prior) == nil {
			for name, old := range prior.Tracks {
				if t, ok := s.tracks[name]; ok {
					t.SubmittedSeconds = old.SubmittedSeconds
					s.tracks[name] = t
				}
			}
		}
	}
	return s, nil
}

func atomicJSON(path string, value any) error {
	return meeting.WriteJSON(path, value)
}

func (s *store) saveTurn(t turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.turns[t.ID]; ok && old != t && s.finals[t.ID].ID != "" {
		return fmt.Errorf("turn %s changed; refusing to alter existing evidence", t.ID)
	}
	if err := atomicJSON(filepath.Join(s.dir, t.ID+".turn.json"), t); err != nil {
		return err
	}
	s.turns[t.ID] = t
	delete(s.active, t.Track)
	return nil
}

func (s *store) finalize(t turn, e serverEvent, session string, committed time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.finals[t.ID]; ok {
		return nil
	}
	p := Passage{Range: t.Range, MeetingID: s.meeting, Version: 1, Model: Model, ItemID: e.ItemID, SessionID: session, Text: e.Transcript, FinalizedAt: time.Now().UTC(), CommitToFinalSeconds: time.Since(committed).Seconds(), Usage: e.Usage}
	if !s.origin.IsZero() {
		latency := time.Since(s.origin).Seconds() - t.End
		p.CaptureToFinalSeconds = &latency
	}
	if err := atomicJSON(filepath.Join(s.dir, t.ID+".passage.json"), p); err != nil {
		return err
	}
	s.finals[t.ID] = p
	delete(s.tracks[t.Track].Provisional, e.ItemID)
	return nil
}

func (s *store) completed(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.finals[id]
	return ok || s.turns[id].Silence
}

func (s *store) state(track, state string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tracks[track]
	t.State = state
	if err != nil {
		t.Error = err.Error()
	}
	s.tracks[track] = t
}

func (s *store) snapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := Status{MeetingID: s.meeting, UpdatedAt: time.Now().UTC(), FinalizationSeconds: s.finalizationSeconds, Model: Model, State: "complete", Tracks: map[string]TrackStatus{}}
	for name, t := range s.tracks {
		t.Provisional = make(map[string]string, len(s.tracks[name].Provisional))
		for id, text := range s.tracks[name].Provisional {
			t.Provisional[id] = text
		}
		for id, turn := range s.turns {
			if turn.Track != name || turn.Silence {
				continue
			}
			if passage, ok := s.finals[id]; ok {
				t.Finals++
				if passage.Text == "" {
					t.EmptyFinals++
				}
			} else {
				t.Pending = append(t.Pending, turn.Range)
			}
		}
		if active, ok := s.active[name]; ok {
			t.Pending = append(t.Pending, active)
		}
		sort.Slice(t.Pending, func(i, j int) bool { return t.Pending[i].Offset < t.Pending[j].Offset })
		t.EstimatedUSD = t.SubmittedSeconds / 60 * 0.017
		result.Tracks[name] = t
		if t.Error != "" || t.State == "degraded" || (t.State == "complete" && len(t.Pending) > 0) {
			result.State = "degraded"
		} else if result.State != "degraded" && t.State != "complete" {
			result.State = "streaming"
		}
	}
	return result
}

// Read returns finalized evidence in capture order, never provider arrival order.
func Read(dir string) ([]Passage, Status, error) {
	var state Status
	data, err := os.ReadFile(filepath.Join(dir, "transcription", "status.json"))
	if err != nil {
		return nil, state, err
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, state, err
	}
	if state.State == "streaming" {
		guard, err := os.OpenFile(filepath.Join(dir, "transcription", "writer.lock"), os.O_RDWR, 0600)
		if err != nil {
			return nil, state, err
		}
		if syscall.Flock(int(guard.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			state.State = "interrupted"
		}
		guard.Close()
	}
	files, err := filepath.Glob(filepath.Join(dir, "transcription", "*.passage.json"))
	if err != nil {
		return nil, state, err
	}
	var passages []Passage
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, state, err
		}
		var p Passage
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, state, err
		}
		passages = append(passages, p)
	}
	sort.Slice(passages, func(i, j int) bool {
		if passages[i].Start == passages[j].Start {
			return passages[i].ID < passages[j].ID
		}
		return passages[i].Start < passages[j].Start
	})
	return passages, state, nil
}
