package recorder

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cwebley/trailmix/internal/meeting"
)

func TestCaptureOriginSurvivesNativeAndPersistedStatus(t *testing.T) {
	for _, test := range []struct {
		name, stage, raw string
		known            bool
	}{
		{"ready", "ready", `"2026-11-18T14:05:00.125+02:00"`, true},
		{"started", "started", `"2026-11-18T14:05:00.125+02:00"`, true},
		{"progress", "progress", `"2026-11-18T14:05:00.125+02:00"`, true},
		{"stopped", "stopped", `"2026-11-18T14:05:00.125+02:00"`, true},
		{"legacy", "ready", "", false},
		{"malformed", "ready", `"not a time"`, false},
		{"missing timezone", "started", `"2026-11-18T12:05:00"`, false},
		{"null", "progress", `null`, false},
		{"number", "stopped", `123`, false},
		{"zero", "ready", `"0001-01-01T00:00:00Z"`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := testPaths(t)
			t.Setenv("TRAILMIX_TEST_HELPER", "origin-"+test.stage)
			t.Setenv("TRAILMIX_TEST_ORIGIN", test.raw)
			dir := filepath.Join(p.home, "recording")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			id, err := meeting.Ensure(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			// Go starts five minutes before the native origin. The ready and
			// started events are themselves delayed by another twenty seconds.
			started := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
			indicator := newIndicator()
			defer indicator.close()
			s := &session{status: status{State: "starting", MeetingID: id, Directory: dir, StartedAt: started}, stop: make(chan struct{}), indicator: indicator}
			if err := capture(p, s); err != nil {
				t.Fatal(err)
			}
			s.status.State = "stopped"
			s.status.Capture = captureHealthFor(s.status)
			frozen := s.snapshot()
			if !frozen.StartedAt.Equal(started) {
				t.Fatal("native origin changed the meeting start time")
			}
			assertOriginHealth(t, frozen.Capture, test.known)
			if err := saveStatus(p, frozen); err != nil {
				t.Fatal(err)
			}
			last, err := lastStatus(p)
			if err != nil {
				t.Fatal(err)
			}
			assertOriginHealth(t, last.Capture, test.known)
			if test.known && last.CaptureOriginAt != "2026-11-18T12:05:00.125Z" {
				t.Fatalf("origin not normalized/persisted: %q", last.CaptureOriginAt)
			}
			// Retry replaces transcription status, never the capture clock.
			// Read both its retrying and completed states through the same
			// stopped-transcript read path without a provider or API key.
			if err := os.Mkdir(filepath.Join(dir, "transcription"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, state := range []string{"retrying", "complete"} {
				data := `{"meeting_id":"` + id + `","state":"` + state + `","tracks":{}}`
				if err := os.WriteFile(filepath.Join(dir, "transcription", "status.json"), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				snapshot, err := readTranscript(dir, "")
				if err != nil {
					t.Fatal(err)
				}
				assertOriginHealth(t, snapshot.Capture, test.known)
			}
		})
	}
}

func assertOriginHealth(t *testing.T, h *captureHealth, known bool) {
	t.Helper()
	if h == nil || h.LastCapturedSeconds == nil || *h.LastCapturedSeconds != 64 || h.Tracks["system"].LastCapturedSeconds != 61 {
		t.Fatalf("lost relative audio boundaries: %+v", h)
	}
	want := time.Date(2026, 11, 18, 12, 6, 4, 125_000_000, time.UTC)
	if known {
		if h.LastCapturedAt == nil || !h.LastCapturedAt.Equal(want) || h.LastCapturedAt.Location() != time.UTC {
			t.Fatalf("aggregate did not use native origin + 64 seconds: %+v", h)
		}
		for name, delta := range map[string]time.Duration{"microphone": 0, "system": -3 * time.Second} {
			at := h.Tracks[name].LastCapturedAt
			if at == nil || !at.Equal(want.Add(delta)) {
				t.Fatalf("%s boundary used the wrong origin: %+v", name, h.Tracks[name])
			}
		}
	} else {
		if h.LastCapturedAt != nil || h.Tracks["microphone"].LastCapturedAt != nil || h.Tracks["system"].LastCapturedAt != nil {
			t.Fatalf("unknown native origin became a wall time: %+v", h)
		}
		data, err := json.Marshal(h)
		if err != nil || strings.Contains(string(data), "last_captured_at") || strings.Contains(captureSummary(h), "approximately") {
			t.Fatalf("unknown wall time serialized or displayed: %s, %v", data, err)
		}
	}
}

func TestLegacyCachedCaptureWallTimesAreNotTrustedOrMutated(t *testing.T) {
	end := 64.0
	wrong := time.Date(2026, 11, 18, 12, 1, 4, 0, time.UTC)
	cached := &captureHealth{State: "stopped", LastCapturedSeconds: &end, LastCapturedAt: &wrong, Tracks: map[string]capturedTrackHealth{
		"microphone": {LastCapturedSeconds: 64, LastCapturedAt: &wrong},
		"system":     {LastCapturedSeconds: 61, LastCapturedAt: &wrong},
	}}
	if strings.Contains(captureSummary(cached), "approximately") {
		t.Fatal("standalone legacy summary trusted an unproven wall time")
	}
	before, _ := json.Marshal(cached)
	for _, origin := range []captureOrigin{"", "bad", "2026-11-18T12:05:00.125Z"} {
		s := status{State: "failed", Error: "new transcription failure", StartedAt: wrong.Add(-64 * time.Second), Capture: cached, CaptureOriginAt: origin}
		h := captureHealthFor(s)
		assertOriginHealth(t, h, origin == "2026-11-18T12:05:00.125Z")
		if h.State != "stopped" || h.Error != "" {
			t.Fatalf("lost frozen capture outcome: %+v", h)
		}
	}
	after, _ := json.Marshal(cached)
	if string(before) != string(after) {
		t.Fatalf("sanitization mutated cached health: %s => %s", before, after)
	}
}

func TestRetainedSnapshotCaptureJSONNormalizesWallTimesWithoutRewritingSource(t *testing.T) {
	for _, test := range []struct {
		name, origin string
		known        bool
	}{
		{"legacy", "", false},
		{"malformed", `,"capture_origin_at":"bad"`, false},
		{"native", `,"capture_origin_at":"2026-11-18T14:05:00.125+02:00"`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snapshot.json")
			source := []byte(`{"meeting_id":"fixture","capture":{"state":"stopped","last_captured_seconds":64,"last_captured_at":"2026-11-18T12:01:04Z"` + test.origin + `,"tracks":{"microphone":{"last_captured_seconds":64,"last_captured_at":"2026-11-18T12:01:04Z"},"system":{"last_captured_seconds":61,"last_captured_at":"2026-11-18T12:01:01Z"}}}}`)
			if err := os.WriteFile(path, source, 0600); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			before := append([]byte(nil), data...)
			var snapshot transcriptSnapshot
			if err := json.Unmarshal(data, &snapshot); err != nil {
				t.Fatal(err)
			}
			assertOriginHealth(t, snapshot.Capture, test.known)
			forwarded, err := json.Marshal(snapshot)
			if err != nil || bytes.Contains(forwarded, []byte("2026-11-18T12:01:04Z")) || bytes.Contains(forwarded, []byte("2026-11-18T12:01:01Z")) {
				t.Fatalf("assistant JSON retained legacy wall time: %s, %v", forwarded, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(source, after) || !bytes.Equal(before, data) {
				t.Fatalf("normalization changed source bytes: %s, %v", after, err)
			}
		})
	}
}

func TestCaptureOriginRejectsInvalidRelativeWallTimes(t *testing.T) {
	origin := captureOrigin("2026-11-18T12:05:00Z")
	for _, seconds := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, float64(math.MaxInt64) / float64(time.Second), math.MaxFloat64} {
		if at := origin.capturedAt(seconds); at != nil {
			t.Fatalf("invalid relative seconds %v fabricated wall time %s", seconds, at)
		}
	}
	if at := origin.capturedAt(0); at == nil || at.Format(time.RFC3339Nano) != string(origin) {
		t.Fatalf("valid zero offset rejected: %v", at)
	}
}

func TestMalformedPersistedCaptureOriginLeavesRelativeHealthUsable(t *testing.T) {
	for _, raw := range []string{`"bad"`, `null`, `[]`, `{}`, `123`, `""`} {
		var s status
		data := `{"state":"stopped","started_at":"2026-11-18T12:00:00Z","capture_origin_at":` + raw + `,"tracks":{"microphone":{"frames":48000,"end_seconds":64},"system":{"frames":48000,"end_seconds":61}},"capture":{"state":"stopped","last_captured_seconds":64,"last_captured_at":"2026-11-18T12:01:04Z","tracks":{"microphone":{"last_captured_seconds":64,"last_captured_at":"2026-11-18T12:01:04Z"},"system":{"last_captured_seconds":61}}}}`
		if err := json.Unmarshal([]byte(data), &s); err != nil {
			t.Fatal(err)
		}
		assertOriginHealth(t, captureHealthFor(s), false)
	}
}
