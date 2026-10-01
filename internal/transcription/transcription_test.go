package transcription

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Exercise actual files and WebSocket frames, including server events arriving
// while the client is still submitting audio. No credentials or devices needed.
func testServer(t *testing.T, serve func(*websocket.Conn)) string {
	return testServerExpiry(t, 0, serve)
}

func testServerExpiry(t *testing.T, expiry int64, serve func(*websocket.Conn)) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing authentication")
		}
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		var config struct {
			Type    string
			Session struct {
				Type  string
				Audio struct {
					Input struct {
						Format struct {
							Type string
							Rate int
						}
						Transcription struct{ Model string }
						TurnDetection any `json:"turn_detection"`
					}
				}
			}
		}
		if err := c.ReadJSON(&config); err != nil {
			return
		}
		if config.Type != "session.update" || config.Session.Type != "transcription" || config.Session.Audio.Input.Format.Rate != 24000 || config.Session.Audio.Input.Transcription.Model != Model || config.Session.Audio.Input.TurnDetection != nil {
			t.Errorf("wrong session config: %+v", config)
		}
		_ = c.WriteJSON(map[string]any{"type": "session.updated", "session": map[string]any{"id": "test-session", "expires_at": expiry}})
		serve(c)
	}))
	t.Cleanup(s.Close)
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

type queueWriter struct {
	audio, journal *os.File
	offset         int64
}

func queue(t *testing.T, dir, track string) *queueWriter {
	t.Helper()
	a, err := os.Create(filepath.Join(dir, track+".stream.pcm"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := os.Create(filepath.Join(dir, track+".stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); j.Close() })
	return &queueWriter{audio: a, journal: j}
}

func (q *queueWriter) chunk(t *testing.T, start, seconds float64, voice, gap bool) {
	t.Helper()
	pcm := make([]byte, int(seconds*24000)*2)
	if voice {
		for i := 0; i < len(pcm); i += 2 {
			binary.LittleEndian.PutUint16(pcm[i:], 4000)
		}
	}
	if _, err := q.audio.Write(pcm); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(q.journal).Encode(row{Offset: q.offset, Bytes: len(pcm), Start: start, End: start + seconds, Discontinuity: gap}); err != nil {
		t.Fatal(err)
	}
	q.offset += int64(len(pcm))
}

func await(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for transcription")
}

func TestTwoTracksOutOfOrderFinalsAndRetryDeduplication(t *testing.T) {
	var connections atomic.Int32
	url := testServer(t, func(c *websocket.Conn) {
		connections.Add(1)
		var ids []string
		for {
			var e map[string]any
			if c.ReadJSON(&e) != nil {
				return
			}
			if e["type"] == "input_audio_buffer.commit" {
				id := fmt.Sprintf("item-%d", len(ids))
				ids = append(ids, id)
				_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": id})
				if len(ids) == 2 {
					for _, i := range []int{1, 0, 1} {
						_ = c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": ids[i], "transcript": fmt.Sprintf("turn %d", i), "usage": map[string]any{"type": "duration", "seconds": 1}})
					}
				}
			}
		}
	})
	dir := t.TempDir()
	for n, name := range []string{"microphone", "system"} {
		q := queue(t, dir, name)
		base := float64(n) + 2
		q.chunk(t, base, .2, true, false)
		q.chunk(t, base+.2, .6, false, false)
		// A capture gap must split ranges without compressing meeting time.
		q.chunk(t, base+10, .2, true, true)
		q.chunk(t, base+10.2, .6, false, false)
	}
	r, err := start(dir, "test-key", url, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(); err != nil {
		t.Fatal(err)
	}
	passages, state, err := Read(dir)
	if err != nil || state.State != "complete" || len(passages) != 4 {
		t.Fatalf("finals: %+v, %+v, %v", passages, state, err)
	}
	for i, p := range passages {
		if p.Start != []float64{2, 3, 12, 13}[i] || p.End-p.Start > .801 || p.MeetingID == "" || p.SessionID != "test-session" || p.Version != 1 {
			t.Fatalf("incorrect evidence mapping: %+v", p)
		}
	}
	before, _ := os.ReadFile(filepath.Join(dir, "transcription", passages[0].ID+".passage.json"))
	r, err = start(dir, "test-key", url, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "transcription", passages[0].ID+".passage.json"))
	if string(before) != string(after) || connections.Load() != 2 {
		t.Fatalf("retry changed evidence or uploaded completed audio, connections=%d", connections.Load())
	}
}

func TestLiveDeltasBeforeCommitAndTrailingFlush(t *testing.T) {
	url := testServer(t, func(c *websocket.Conn) {
		for {
			var e map[string]any
			if c.ReadJSON(&e) != nil {
				return
			}
			switch e["type"] {
			case "input_audio_buffer.append":
				pcm, _ := base64.StdEncoding.DecodeString(e["audio"].(string))
				if len(pcm) == 0 || len(pcm)%2 != 0 {
					t.Error("bad PCM payload")
				}
				_ = c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.delta", "item_id": "live", "delta": "counting "})
			case "input_audio_buffer.commit":
				// Even a completion preceding its acknowledgment must bind by ID.
				_ = c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": "live", "transcript": "counting one two"})
				_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "live"})
			}
		}
	})
	dir := t.TempDir()
	q := queue(t, dir, "microphone")
	queue(t, dir, "system")
	r, err := start(dir, "test-key", url, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Finish() })
	q.chunk(t, 42.25, .05, true, false)
	await(t, func() bool { return r.Snapshot().Tracks["microphone"].Provisional["live"] != "" })
	if r.Snapshot().Tracks["microphone"].Finals != 0 {
		t.Fatal("provisional text became evidence")
	}
	if err := r.Finish(); err != nil {
		t.Fatal(err)
	}
	passages, state, err := Read(dir)
	if err != nil || state.State != "complete" || len(passages) != 1 || passages[0].Start != 42.25 || passages[0].End != 42.3 || passages[0].Bytes != 2400 {
		t.Fatalf("trailing padded turn lost source range: %+v %+v %v", passages, state, err)
	}
}

func TestDisconnectRetainsFutureAudioAndRetry(t *testing.T) {
	url := testServer(t, func(c *websocket.Conn) { var e any; _ = c.ReadJSON(&e) })
	dir := t.TempDir()
	q := queue(t, dir, "microphone")
	queue(t, dir, "system")
	r, err := start(dir, "test-key", url, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Finish() })
	q.chunk(t, 1, .2, true, false)
	await(t, func() bool { return r.Snapshot().State == "degraded" })
	q.chunk(t, 1.2, .6, false, false)
	q.chunk(t, 5, .2, true, true)
	r.Finish()
	state := r.Snapshot()
	if len(state.Tracks["microphone"].Pending) != 2 || state.Tracks["microphone"].Pending[1].Start != 5 {
		t.Fatalf("incomplete ranges lost after disconnect: %+v", state)
	}
	url = testServer(t, func(c *websocket.Conn) {
		var item int
		for {
			var e map[string]any
			if c.ReadJSON(&e) != nil {
				return
			}
			if e["type"] == "input_audio_buffer.commit" {
				item++
				id := fmt.Sprint(item)
				_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": id})
				_ = c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": id, "transcript": "recovered"})
			}
		}
	})
	r, err = start(dir, "test-key", url, true)
	if err != nil {
		t.Fatal(err)
	}
	r.Finish()
	if r.Snapshot().State != "complete" || r.Snapshot().Tracks["microphone"].Finals != 2 {
		t.Fatalf("retry failed: %+v", r.Snapshot())
	}
}

func TestCancellationBoundsFinalizationAndPreservesPending(t *testing.T) {
	url := testServer(t, func(c *websocket.Conn) {
		for {
			var e map[string]any
			if c.ReadJSON(&e) != nil {
				return
			}
			if e["type"] == "input_audio_buffer.commit" {
				_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "never-final"})
			}
		}
	})
	dir := t.TempDir()
	q := queue(t, dir, "microphone")
	queue(t, dir, "system")
	q.chunk(t, 0, .2, true, false)
	q.chunk(t, .2, .6, false, false)
	r, err := start(dir, "test-key", url, true)
	if err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { r.store.mu.Lock(); defer r.store.mu.Unlock(); return len(r.store.turns) == 1 })
	r.cancel()
	done := make(chan struct{})
	go func() { r.Finish(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock provider")
	}
	if state := r.Snapshot(); state.State != "degraded" || len(state.Tracks["microphone"].Pending) != 1 {
		t.Fatalf("timeout looked complete: %+v", state)
	}
}

func TestSessionRolloverPreservesClock(t *testing.T) {
	var connections atomic.Int32
	url := testServerExpiry(t, time.Now().Add(30*time.Second).Unix(), func(c *websocket.Conn) {
		connections.Add(1)
		for {
			var e map[string]any
			if c.ReadJSON(&e) != nil {
				return
			}
			if e["type"] == "input_audio_buffer.commit" {
				_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "reused-on-new-session"})
				_ = c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": "reused-on-new-session", "transcript": "rollover"})
			}
		}
	})
	dir := t.TempDir()
	q := queue(t, dir, "microphone")
	queue(t, dir, "system")
	q.chunk(t, 3500, .2, true, false)
	q.chunk(t, 3500.2, .6, false, false)
	q.chunk(t, 3501, .2, true, true)
	q.chunk(t, 3501.2, .6, false, false)
	r, err := start(dir, "test-key", url, true)
	if err != nil {
		t.Fatal(err)
	}
	r.Finish()
	passages, state, err := Read(dir)
	if err != nil || state.State != "complete" || connections.Load() != 2 || len(passages) != 2 || passages[1].Start != 3501 {
		t.Fatalf("rollover lost identity or timing: %+v %+v %v", passages, state, err)
	}
}

func TestPartialJournalAndConcurrentWriterProtection(t *testing.T) {
	dir := t.TempDir()
	q := queue(t, dir, "microphone")
	queue(t, dir, "system")
	r, err := start(dir, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := start(dir, "", "", false); err == nil {
		other.Finish()
		t.Fatal("allowed competing writer")
	}
	_, _ = q.journal.WriteString(`{"offset":0`)
	r.Finish()
	if !strings.Contains(r.Snapshot().Tracks["microphone"].Error, "incomplete PCM timing row") {
		t.Fatalf("truncated journal treated as complete: %+v", r.Snapshot())
	}
}

func TestLongSpeechIsBoundedAndSilenceIsNotEvidence(t *testing.T) {
	dir := t.TempDir()
	q := queue(t, dir, "microphone")
	silent := queue(t, dir, "system")
	for i := 0; i < 32; i++ {
		q.chunk(t, float64(i), 1, true, false)
	}
	silent.chunk(t, 0, 1, false, false)
	r, err := start(dir, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	r.Finish()
	state := r.Snapshot()
	if len(state.Tracks["microphone"].Pending) != 3 || len(state.Tracks["system"].Pending) != 0 {
		t.Fatalf("wrong speech/silence segmentation: %+v", state)
	}
}

func TestUnpublishedPCMTailCannotLookComplete(t *testing.T) {
	dir := t.TempDir()
	q := queue(t, dir, "microphone")
	queue(t, dir, "system")
	_, _ = q.audio.Write(make([]byte, 4800))
	r, err := start(dir, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	r.Finish()
	if state := r.Snapshot(); state.State != "degraded" || !strings.Contains(state.Tracks["microphone"].Error, "unpublished") {
		t.Fatalf("unpublished audio was ignored: %+v", state)
	}
}

func TestQueueFailureDoesNotFinalizeTruncatedTurn(t *testing.T) {
	dir := t.TempDir()
	q := queue(t, dir, "microphone")
	queue(t, dir, "system")
	q.chunk(t, 1, .2, true, false)
	_ = os.WriteFile(filepath.Join(dir, "microphone.stream.error"), []byte("converter failure"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "microphone.timeline.jsonl"), []byte("{\"end_seconds\":10}\n"), 0600)
	r, err := start(dir, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	r.Finish()
	state := r.Snapshot()
	if state.State != "degraded" || state.Tracks["microphone"].Pending[0].End != 10 {
		t.Fatalf("missing CAF suffix not retained: %+v", state)
	}
	if len(r.store.turns) != 0 {
		t.Fatal("truncated turn was closed before queue repair")
	}
}

func TestMissingQueueReportsOriginalCaptureRange(t *testing.T) {
	dir := t.TempDir()
	queue(t, dir, "system")
	_ = os.WriteFile(filepath.Join(dir, "microphone.timeline.jsonl"), []byte("{\"end_seconds\":42}\n"), 0600)
	r, err := start(dir, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	r.Finish()
	state := r.Snapshot()
	if len(state.Tracks["microphone"].Pending) != 1 || state.Tracks["microphone"].Pending[0].End != 42 {
		t.Fatalf("missing queue hid original audio: %+v", state)
	}
}

func TestIdleSessionRetiresBeforeMoreSpeech(t *testing.T) {
	var connections atomic.Int32
	url := testServerExpiry(t, time.Now().Add(61*time.Second).Unix(), func(c *websocket.Conn) {
		connections.Add(1)
		for {
			var e map[string]any
			if c.ReadJSON(&e) != nil {
				return
			}
			if e["type"] == "input_audio_buffer.commit" {
				_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "item"})
				_ = c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item", "transcript": "after idle"})
			}
		}
	})
	dir := t.TempDir()
	q := queue(t, dir, "microphone")
	queue(t, dir, "system")
	q.chunk(t, 2, .2, true, false)
	r, err := start(dir, "test-key", url, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Finish() })
	await(t, func() bool { return connections.Load() >= 2 })
	q.chunk(t, 2.2, .6, false, false)
	r.Finish()
	passages, state, err := Read(dir)
	if err != nil || state.State != "complete" || len(passages) != 1 || passages[0].Start != 2 {
		t.Fatalf("idle rollover dropped partial speech: %+v %+v %v", passages, state, err)
	}
}

// A provider may stream text for audio the local detector then clears as
// silence. That text has no committed item to finalize it, so clearing must
// retire it; otherwise it accumulates until the provisional limit degrades.
func TestClearedSilenceRetiresProvisionalText(t *testing.T) {
	var clears atomic.Int32
	url := testServer(t, func(c *websocket.Conn) {
		for {
			var e map[string]any
			if c.ReadJSON(&e) != nil {
				return
			}
			switch e["type"] {
			case "input_audio_buffer.append":
				_ = c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.delta", "item_id": fmt.Sprintf("noise-%d", clears.Load()), "delta": "hm "})
			case "input_audio_buffer.clear":
				clears.Add(1)
			case "input_audio_buffer.commit":
				_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "speech"})
				_ = c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": "speech", "transcript": "real words"})
			}
		}
	})
	dir := t.TempDir()
	q := queue(t, dir, "microphone")
	queue(t, dir, "system")
	r, err := start(dir, "test-key", url, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Finish() })
	at := 0.0
	for i := 0; i < 40; i++ {
		q.chunk(t, at, .7, false, false)
		at += .7
	}
	await(t, func() bool { return clears.Load() >= 39 || r.Snapshot().Tracks["microphone"].State == "degraded" })
	state := r.Snapshot()
	if track := state.Tracks["microphone"]; track.State == "degraded" || len(track.Provisional) > 1 {
		t.Fatalf("cleared silence left provisional text: %d items, state %s %q", len(track.Provisional), track.State, track.Error)
	}
	q.chunk(t, at, .5, true, false)
	q.chunk(t, at+.5, .7, false, false)
	if err := r.Finish(); err != nil {
		t.Fatal(err)
	}
	passages, final, err := Read(dir)
	if err != nil || len(passages) != 1 || passages[0].Text != "real words" || len(final.Tracks["microphone"].Provisional) != 0 {
		t.Fatalf("speech after cleared silence: %+v %+v %v", passages, final, err)
	}
}
