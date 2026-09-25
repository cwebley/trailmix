package transcription

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const endpoint = "wss://api.openai.com/v1/realtime?intent=transcription"

type serverEvent struct {
	Type           string          `json:"type"`
	ItemID         string          `json:"item_id"`
	PreviousItemID string          `json:"previous_item_id"`
	Delta          string          `json:"delta"`
	Transcript     string          `json:"transcript"`
	Usage          json.RawMessage `json:"usage"`
	Session        struct {
		ID        string `json:"id"`
		ExpiresAt int64  `json:"expires_at"`
	} `json:"session"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

type pendingTurn struct {
	turn turn
	at   time.Time
}

// One writer, one reader. The mutex protects item reconciliation and persistence;
// network writes never hold it. Commit acknowledgments bind locally ordered turns
// to provider item IDs; final events may then arrive in any order.
type provider struct {
	conn       *websocket.Conn
	mu         sync.Mutex
	store      *store
	track      string
	key        string
	session    string
	expires    time.Time
	err        error
	waiting    *pendingTurn
	items      map[string]pendingTurn
	early      map[string][]serverEvent
	seen       map[string]bool
	ack        chan struct{}
	done       chan struct{}
	stopCancel func() bool
}

func connect(ctx context.Context, url, key, track string, s *store) (*provider, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, Proxy: http.ProxyFromEnvironment}
	c, response, err := dialer.DialContext(ctx, url, http.Header{"Authorization": {"Bearer " + key}})
	if err != nil {
		if response != nil {
			response.Body.Close()
			return nil, fmt.Errorf("transcription handshake: HTTP %d", response.StatusCode)
		}
		return nil, errors.New(strings.ReplaceAll(err.Error(), key, "[redacted]"))
	}
	p := &provider{conn: c, store: s, track: track, key: key, expires: time.Now().Add(55 * time.Minute), items: map[string]pendingTurn{}, early: map[string][]serverEvent{}, seen: map[string]bool{}, ack: make(chan struct{}, 1), done: make(chan struct{})}
	p.stopCancel = context.AfterFunc(ctx, func() { c.Close() })
	c.SetReadLimit(2 * 1024 * 1024)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err = p.write(map[string]any{"type": "session.update", "session": map[string]any{"type": "transcription", "audio": map[string]any{"input": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "transcription": map[string]any{"model": Model, "delay": "low"}, "turn_detection": nil}}}}); err == nil {
		for {
			var e serverEvent
			if err = c.ReadJSON(&e); err != nil {
				break
			}
			p.sessionInfo(e)
			if e.Type == "error" {
				err = p.eventError(e)
				break
			}
			if e.Type == "session.updated" {
				break
			}
		}
	}
	if err != nil {
		p.stopCancel()
		c.Close()
		return nil, err
	}
	_ = c.SetReadDeadline(time.Time{})
	go p.receive()
	return p, nil
}

func (p *provider) eventError(e serverEvent) error {
	return fmt.Errorf("provider %s: %s", e.Type, strings.ReplaceAll(e.Error.Message, p.key, "[redacted]"))
}

func (p *provider) sessionInfo(e serverEvent) {
	if e.Session.ID != "" {
		p.session = e.Session.ID
	}
	if e.Session.ExpiresAt > 0 {
		deadline := time.Unix(e.Session.ExpiresAt, 0).Add(-time.Minute)
		if deadline.Before(p.expires) {
			p.expires = deadline
		}
	}
}

func (p *provider) write(value any) error {
	_ = p.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	return p.conn.WriteJSON(value)
}

func (p *provider) health() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	for _, item := range p.items {
		if time.Since(item.at) > 30*time.Second {
			return errors.New("transcription final result timed out; audio retained for retry")
		}
	}
	return nil
}

func (p *provider) append(pcm []byte) error {
	if err := p.health(); err != nil {
		return err
	}
	if err := p.write(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(pcm)}); err != nil {
		return err
	}
	p.store.mu.Lock()
	t := p.store.tracks[p.track]
	t.SubmittedSeconds += float64(len(pcm)) / 48000
	p.store.tracks[p.track] = t
	p.store.mu.Unlock()
	return nil
}

func (p *provider) commit(t turn) error {
	if err := p.health(); err != nil {
		return err
	}
	if t.Silence {
		return p.write(map[string]any{"type": "input_audio_buffer.clear"})
	}
	p.mu.Lock()
	p.waiting = &pendingTurn{t, time.Now()}
	p.mu.Unlock()
	if err := p.write(map[string]any{"type": "input_audio_buffer.commit", "event_id": t.ID}); err != nil {
		return err
	}
	select {
	case <-p.ack:
		return p.health()
	case <-p.done:
		return p.health()
	case <-time.After(5 * time.Second):
		return errors.New("transcription commit acknowledgment timed out")
	}
}

func (p *provider) receive() {
	defer close(p.done)
	for {
		var e serverEvent
		err := p.conn.ReadJSON(&e)
		p.mu.Lock()
		if err == nil {
			err = p.handle(e)
		}
		if err != nil {
			p.err = err
		}
		p.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (p *provider) handle(e serverEvent) error {
	switch e.Type {
	case "error", "conversation.item.input_audio_transcription.failed":
		return p.eventError(e)
	case "input_audio_buffer.committed":
		if p.seen[e.ItemID] {
			return nil
		}
		if p.waiting == nil || e.ItemID == "" {
			return errors.New("unexpected provider commit identity")
		}
		p.items[e.ItemID] = *p.waiting
		p.waiting = nil
		p.seen[e.ItemID] = true
		for _, early := range p.early[e.ItemID] {
			if err := p.handle(early); err != nil {
				return err
			}
		}
		delete(p.early, e.ItemID)
		p.ack <- struct{}{}
	case "conversation.item.input_audio_transcription.delta", "conversation.item.input_audio_transcription.completed":
		if e.ItemID == "" {
			return errors.New("transcript event has no item identity")
		}
		item, ok := p.items[e.ItemID]
		if e.Type == "conversation.item.input_audio_transcription.delta" {
			if p.seen[e.ItemID] && !ok {
				return nil
			}
			p.store.mu.Lock()
			texts := p.store.tracks[p.track].Provisional
			if len(texts) > 32 || len(texts[e.ItemID])+len(e.Delta) > 1024*1024 {
				p.store.mu.Unlock()
				return errors.New("provisional transcript exceeded limit")
			}
			texts[e.ItemID] += e.Delta
			p.store.mu.Unlock()
			return nil
		}
		if !ok {
			if p.seen[e.ItemID] {
				return nil
			} // Duplicate final or late delta.
			if len(p.early) >= 32 || len(p.early[e.ItemID]) >= 1024 {
				return errors.New("too many unmatched provider events")
			}
			p.early[e.ItemID] = append(p.early[e.ItemID], e)
			return nil
		}
		if e.Type == "conversation.item.input_audio_transcription.completed" {
			if err := p.store.finalize(item.turn, e, p.session, item.at); err != nil {
				return err
			}
			delete(p.items, e.ItemID)
		}
	}
	return nil
}

func (p *provider) drain(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := p.health(); err != nil {
			return err
		}
		p.mu.Lock()
		pending := len(p.items)
		p.mu.Unlock()
		if pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("transcription finalization timed out; audio retained for retry")
		case <-ticker.C:
		}
	}
}

func (p *provider) close() {
	p.stopCancel()
	p.conn.Close()
	<-p.done
	p.store.mu.Lock()
	t := p.store.tracks[p.track]
	t.Provisional = map[string]string{}
	p.store.tracks[p.track] = t
	p.store.mu.Unlock()
}
