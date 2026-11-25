package recorder

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// Capture health is independent of transcription: all captured audio can be
// transcribed successfully even when recording ended unexpectedly. Times are
// observed buffer boundaries, not claims that every sample before them exists.
type captureHealth struct {
	State               string                         `json:"state"`
	Error               string                         `json:"error,omitempty"`
	LastCapturedSeconds *float64                       `json:"last_captured_seconds,omitempty"`
	LastCapturedAt      *time.Time                     `json:"last_captured_at,omitempty"`
	CaptureOriginAt     captureOrigin                  `json:"capture_origin_at,omitempty"`
	Tracks              map[string]capturedTrackHealth `json:"tracks"`
	Warnings            []string                       `json:"warnings,omitempty"`
}

type capturedTrackHealth struct {
	LastCapturedSeconds float64    `json:"last_captured_seconds"`
	LastCapturedAt      *time.Time `json:"last_captured_at,omitempty"`
}

func (h *captureHealth) UnmarshalJSON(data []byte) error {
	type decodedHealth captureHealth
	var decoded decodedHealth
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*h = captureHealth(decoded)
	// Retained transcript/recap snapshots also decode health without status.
	// Normalize only the in-memory value, never the source artifact.
	h.normalizeWallTimes()
	return nil
}

func (h *captureHealth) normalizeWallTimes() {
	h.LastCapturedAt = nil
	if h.LastCapturedSeconds != nil {
		h.LastCapturedAt = h.CaptureOriginAt.capturedAt(*h.LastCapturedSeconds)
	}
	for name, observed := range h.Tracks {
		observed.LastCapturedAt = h.CaptureOriginAt.capturedAt(observed.LastCapturedSeconds)
		h.Tracks[name] = observed
	}
}

// captureOrigin is a native wall-clock association for relative capture times.
// Invalid optional telemetry must leave wall time unknown, not reject otherwise
// usable status or fall back to the recorder's earlier StartedAt.
type captureOrigin string

func (o *captureOrigin) UnmarshalJSON(data []byte) error {
	*o = ""
	var value string
	if json.Unmarshal(data, &value) == nil {
		if at, err := time.Parse(time.RFC3339Nano, value); err == nil && !at.IsZero() {
			*o = captureOrigin(at.UTC().Format(time.RFC3339Nano))
		}
	}
	return nil
}

func (o captureOrigin) capturedAt(seconds float64) *time.Time {
	// Reject values that cannot safely become a nonnegative time.Duration.
	// float64 rounds MaxInt64 up, so the upper comparison is inclusive.
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return nil
	}
	origin, err := time.Parse(time.RFC3339Nano, string(o))
	if err != nil || origin.IsZero() {
		return nil
	}
	at := origin.UTC().Add(time.Duration(seconds * float64(time.Second)))
	return &at
}

func captureHealthFor(s status) *captureHealth {
	if s.Capture != nil {
		result := *s.Capture
		result.CaptureOriginAt = s.CaptureOriginAt
		result.Warnings = append([]string(nil), s.Warnings...)
		// Older cached health used Go startup as its origin. Preserve the frozen
		// capture outcome and relative boundaries, but never trust cached wall
		// times without the native association. Do not mutate persisted health.
		result.Tracks = make(map[string]capturedTrackHealth, len(s.Capture.Tracks))
		for name, observed := range s.Capture.Tracks {
			result.Tracks[name] = observed
		}
		result.normalizeWallTimes()
		return &result
	}
	h := &captureHealth{State: s.State, Error: s.Error, CaptureOriginAt: s.CaptureOriginAt, Tracks: map[string]capturedTrackHealth{}, Warnings: append([]string(nil), s.Warnings...)}
	if active(h.State) && h.Error != "" {
		h.State = "failed" // Native errors remain visible while it finalizes.
	}
	for _, name := range []string{"microphone", "system"} {
		t := s.Tracks[name]
		if t.Frames <= 0 {
			continue // An elapsed clock alone is not evidence of captured audio.
		}
		end := t.EndSeconds
		observed := capturedTrackHealth{LastCapturedSeconds: end, LastCapturedAt: s.CaptureOriginAt.capturedAt(end)}
		h.Tracks[name] = observed
		if h.LastCapturedSeconds == nil || end > *h.LastCapturedSeconds {
			h.LastCapturedSeconds = &end
			h.LastCapturedAt = observed.LastCapturedAt
		}
	}
	return h
}

func captureSummary(h *captureHealth) string {
	if h == nil {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Capture: %s.\n\n", h.State)
	if h.Error != "" {
		fmt.Fprintf(&out, "Capture error: %s\n\n", h.Error)
	}
	if h.LastCapturedSeconds == nil {
		out.WriteString("Last observed audio: unknown, no buffer telemetry.\n\n")
	} else {
		fmt.Fprintf(&out, "Last observed audio: %s", timestamp(*h.LastCapturedSeconds))
		// Summaries can receive standalone cached health. Require its native
		// association too, rather than displaying legacy cached wall times.
		if at := h.CaptureOriginAt.capturedAt(*h.LastCapturedSeconds); at != nil {
			fmt.Fprintf(&out, " · approximately %s", at.Format(time.RFC3339))
		}
		out.WriteString(".\n\n")
	}
	for _, name := range []string{"microphone", "system"} {
		if track, ok := h.Tracks[name]; ok {
			fmt.Fprintf(&out, "%s last observed audio: %s.\n\n", name, timestamp(track.LastCapturedSeconds))
		}
	}
	for _, warning := range h.Warnings {
		fmt.Fprintf(&out, "Capture warning: %s\n\n", warning)
	}
	return out.String()
}

func captureFailed(s status) bool {
	h := captureHealthFor(s)
	return h.State == "failed" || h.State == "interrupted"
}

func interruptStatus(s *status) {
	if !active(s.State) {
		return
	}
	h := captureHealthFor(*s)
	s.State = "interrupted"
	s.Error = "recorder exited without finalizing; completed local writes remain in the recording directory"
	if h.State != "failed" {
		h.State, h.Error = s.State, s.Error
	}
	s.Capture = h
}
