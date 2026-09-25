package transcription

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type row struct {
	Offset        int64   `json:"offset"`
	Bytes         int     `json:"bytes"`
	Start         float64 `json:"start_seconds"`
	End           float64 `json:"end_seconds"`
	Discontinuity bool    `json:"discontinuity"`
}

// Segmentation is deterministic over published PCM rows, including during retry.
// The simple energy detector bounds turns, but does not claim to recognize words.
type segmenter struct {
	current turn
	quiet   float64
	voiced  bool
}

func (s *segmenter) add(track string, r row, pcm []byte) {
	if s.current.Bytes == 0 {
		s.current.Range = Range{ID: fmt.Sprintf("%s-%012d", track, r.Offset), Track: track, Offset: r.Offset, Start: r.Start}
	}
	s.current.Bytes += int64(r.Bytes)
	s.current.End = r.End
	var power float64
	for i := 0; i < len(pcm); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i:]))) / 32768
		power += v * v
	}
	if math.Sqrt(power/float64(len(pcm)/2)) >= 0.008 {
		s.voiced = true
		s.quiet = 0
	} else {
		s.quiet += float64(r.Bytes) / 48000
	}
}

func (s *segmenter) boundary() bool {
	seconds := float64(s.current.Bytes) / 48000
	return seconds >= 15 || (seconds >= 0.1 && s.quiet >= 0.6)
}

func (s *segmenter) take() turn { t := s.current; t.Silence = !s.voiced; *s = segmenter{}; return t }

type Runner struct {
	store         *store
	dir, key, url string
	ctx           context.Context
	cancel        context.CancelFunc
	captureDone   chan struct{}
	done          chan struct{}
	once          sync.Once
	finishOnce    sync.Once
	finishErr     error
}

// Start tails the two local PCM queues. Finish signals that the helper has closed
// them and allows at most twelve seconds for remaining network work.
func Start(dir, key string) (*Runner, error) { return start(dir, key, endpoint, false) }

func start(dir, key, url string, finished bool) (*Runner, error) {
	s, err := openStore(dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{store: s, dir: dir, key: key, url: url, ctx: ctx, cancel: cancel, captureDone: make(chan struct{}), done: make(chan struct{})}
	if finished {
		r.once.Do(func() { close(r.captureDone) })
	}
	go func() {
		var workers sync.WaitGroup
		for _, name := range []string{"microphone", "system"} {
			workers.Add(1)
			go func() {
				defer workers.Done()
				if err := r.track(name); err != nil {
					r.retainUnread(name)
					s.state(name, "degraded", err)
				}
			}()
		}
		workers.Wait()
		close(r.done)
	}()
	return r, nil
}

func (r *Runner) Snapshot() Status { return r.store.snapshot() }

// Capture timestamps determine evidence. This monotonic local estimate is used
// only for latency measurements; helper-to-Go delivery adds a small uncertainty.
func (r *Runner) CaptureClock(elapsed float64) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if r.store.origin.IsZero() {
		r.store.origin = time.Now().Add(-time.Duration(elapsed * float64(time.Second)))
	}
}

func (r *Runner) Fail(err error) {
	for _, name := range []string{"microphone", "system"} {
		r.store.state(name, "degraded", err)
	}
}

func (r *Runner) Persist() error {
	return atomicJSON(filepath.Join(r.store.dir, "status.json"), r.Snapshot())
}

func (r *Runner) Finish() error {
	r.finishOnce.Do(func() {
		started := time.Now()
		r.once.Do(func() { close(r.captureDone) })
		timer := time.AfterFunc(12*time.Second, r.cancel)
		<-r.done
		timer.Stop()
		r.cancel()
		r.store.mu.Lock()
		r.store.finalizationSeconds = time.Since(started).Seconds()
		r.store.mu.Unlock()
		r.finishErr = r.Persist()
		r.store.guard.Close()
	})
	return r.finishErr
}

// Retry replays only ranges without finalized evidence. It takes the same writer
// lock as live transcription, and refuses to run while the recorder is active.
func Retry(ctx context.Context, dir, key string) (Status, error) {
	r, err := start(dir, key, endpoint, true)
	if err != nil {
		return Status{}, err
	}
	select {
	case <-ctx.Done():
		r.cancel()
	case <-r.done:
	}
	err = r.Finish()
	state := r.Snapshot()
	if err == nil && state.State != "complete" {
		err = errors.New("transcription remains incomplete; inspect trailmix transcript --output DIR")
	}
	return state, err
}

func (r *Runner) stopped() bool {
	select {
	case <-r.captureDone:
		return true
	default:
		return false
	}
}

// A canceled replay or native queue failure can leave a suffix that never
// reached the segmenter. Report its capture range without writing thousands of
// turn files after cancellation. Retry will segment it normally.
func (r *Runner) retainUnread(name string) {
	r.store.mu.Lock()
	var consumed int64
	for _, turn := range r.store.turns {
		if turn.Track == name {
			consumed = max(consumed, turn.Offset+turn.Bytes)
		}
	}
	r.store.mu.Unlock()
	f, err := os.Open(filepath.Join(r.dir, name+".stream.jsonl"))
	var remaining Range
	if err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var row row
			if json.Unmarshal(scanner.Bytes(), &row) != nil || row.Offset < consumed {
				continue
			}
			if remaining.ID == "" {
				remaining = Range{ID: name + "-unprocessed", Track: name, Offset: row.Offset, Start: row.Start}
			}
			remaining.Bytes += int64(row.Bytes)
			remaining.End = row.End
		}
	}
	// CAF capture may extend beyond a failed conversion queue. Its original
	// timing journal is enough to report the missing suffix for prepare-stream.
	if timeline, err := os.Open(filepath.Join(r.dir, name+".timeline.jsonl")); err == nil {
		defer timeline.Close()
		scanner := bufio.NewScanner(timeline)
		var end float64
		for scanner.Scan() {
			var item struct {
				End float64 `json:"end_seconds"`
			}
			if json.Unmarshal(scanner.Bytes(), &item) == nil {
				end = item.End
			}
		}
		if remaining.ID == "" {
			r.store.mu.Lock()
			var start float64
			for _, turn := range r.store.turns {
				if turn.Track == name {
					start = max(start, turn.End)
				}
			}
			r.store.mu.Unlock()
			if end > start+0.002 {
				remaining = Range{ID: name + "-unprocessed", Track: name, Offset: consumed, Start: start, End: end}
			}
		} else {
			remaining.End = max(remaining.End, end)
		}
	}
	if remaining.ID != "" {
		r.store.mu.Lock()
		r.store.active[name] = remaining
		r.store.mu.Unlock()
	}
}

func (r *Runner) openQueue(track string) (*os.File, *os.File, error) {
	for {
		f, err := os.Open(filepath.Join(r.dir, track+".stream.jsonl"))
		if err == nil {
			pcm, err := os.Open(filepath.Join(r.dir, track+".stream.pcm"))
			if err != nil {
				f.Close()
				return nil, nil, err
			}
			return f, pcm, nil
		}
		if !errors.Is(err, os.ErrNotExist) || r.stopped() {
			return nil, nil, fmt.Errorf("%s PCM queue unavailable: %w", track, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (r *Runner) track(name string) error {
	timeline, audio, err := r.openQueue(name)
	if err != nil {
		return err
	}
	defer timeline.Close()
	defer audio.Close()
	var p *provider
	var networkErr error
	defer func() {
		if p != nil {
			p.close()
		}
	}()
	failNetwork := func(err error) {
		if networkErr == nil {
			networkErr = err
			r.store.state(name, "degraded", err)
		}
		if p != nil {
			p.close()
			p = nil
		}
	}
	ensureProvider := func() bool {
		if networkErr != nil {
			return false
		}
		if p == nil {
			if r.key == "" {
				failNetwork(errors.New("no saved OpenAI API key; run trailmix auth, then retry; local audio retained"))
				return false
			}
			p, err = connect(r.ctx, r.url, r.key, name, r.store)
			if err != nil {
				failNetwork(err)
				return false
			}
			r.store.state(name, "streaming", nil)
		}
		return true
	}
	var seg segmenter
	skipping := false
	flush := func() error {
		if seg.current.Bytes == 0 {
			return nil
		}
		t := seg.take()
		if err := r.store.saveTurn(t); err != nil {
			return err
		}
		if !skipping && p != nil {
			// Realtime commits require at least 100ms. Pad only the provider input;
			// evidence offsets still describe the captured audio, never the padding.
			if !t.Silence && t.Bytes < 4800 {
				if err := p.append(make([]byte, 4800-t.Bytes)); err != nil {
					failNetwork(err)
				}
			}
			if p != nil {
				if err := p.commit(t); err != nil {
					failNetwork(err)
				}
			}
		}
		if p != nil && time.Now().After(p.expires) {
			ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
			err := p.drain(ctx)
			cancel()
			if err != nil {
				failNetwork(err)
			} else {
				p.close()
				p = nil
			}
		}
		return nil
	}
	reader := bufio.NewReaderSize(timeline, 64*1024)
	var partial []byte
	var expected int64
	for {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		line, readErr := reader.ReadBytes('\n')
		partial = append(partial, line...)
		if len(partial) > 64*1024 {
			return errors.New("PCM timing row exceeds size limit")
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return readErr
			}
			if r.stopped() {
				// Read once more after observing capture completion. A final row can
				// be published between the EOF read and that observation.
				more, _ := reader.Peek(1)
				if len(more) > 0 {
					continue
				}
				if len(partial) > 0 {
					return errors.New("incomplete PCM timing row; queue retained")
				}
				info, err := audio.Stat()
				if err != nil {
					return err
				}
				if info.Size() != expected {
					return errors.New("PCM queue has unpublished audio bytes; retry rebuilds the queue from CAF")
				}
				if data, err := os.ReadFile(filepath.Join(r.dir, name+".stream.error")); err == nil {
					return fmt.Errorf("native transcription queue failed: %s; retry rebuilds from CAF", data)
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				break
			}
			// Retire an idle session before expiry. Any partial turn stays local
			// and is re-appended on the next session, with its original offsets.
			if p != nil && time.Now().After(p.expires) {
				ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
				err := p.drain(ctx)
				cancel()
				if err != nil {
					failNetwork(err)
				} else {
					p.close()
					p = nil
					if seg.current.Bytes > 0 && !skipping && ensureProvider() {
						pcm := make([]byte, seg.current.Bytes)
						if _, err := audio.ReadAt(pcm, seg.current.Offset); err != nil {
							return err
						}
						if err := p.append(pcm); err != nil {
							failNetwork(err)
						}
					}
				}
			}
			if p != nil {
				if err := p.health(); err != nil {
					failNetwork(err)
				}
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var row row
		if err := json.Unmarshal(partial, &row); err != nil {
			return err
		}
		partial = nil
		if row.Offset != expected || row.Bytes <= 0 || row.Bytes > 48000*2 || row.Bytes%2 != 0 || row.End <= row.Start || math.IsNaN(row.Start) || math.IsInf(row.Start, 0) || math.IsNaN(row.End) || math.IsInf(row.End, 0) {
			return errors.New("invalid or noncontiguous PCM queue mapping")
		}
		pcm := make([]byte, row.Bytes)
		if _, err := audio.ReadAt(pcm, row.Offset); err != nil {
			return fmt.Errorf("published PCM is missing: %w", err)
		}
		expected += int64(row.Bytes)
		if row.Discontinuity {
			if err := flush(); err != nil {
				return err
			}
		}
		if seg.current.Bytes == 0 {
			skipping = r.store.completed(fmt.Sprintf("%s-%012d", name, row.Offset))
		}
		seg.add(name, row, pcm)
		r.store.mu.Lock()
		r.store.active[name] = seg.current.Range
		r.store.mu.Unlock()
		if !skipping && ensureProvider() {
			if err := p.append(pcm); err != nil {
				failNetwork(err)
			}
		}
		if seg.boundary() {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if p != nil {
		ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
		err := p.drain(ctx)
		cancel()
		if err != nil {
			failNetwork(err)
		}
	}
	if networkErr != nil {
		return networkErr
	}
	r.store.state(name, "complete", nil)
	return nil
}
