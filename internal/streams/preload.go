package streams

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/probe"
)

// preloadCheckInterval is how often a healthy preload verifies that its
// consumer is still attached to a live producer.
const preloadCheckInterval = 5 * time.Second

// Preload keeps a stream's producer running with the tracks from its query,
// so the camera session is always negotiated the way the config says and
// clients can attach instantly. A Preload is supervised: if the consumer
// can't be attached (camera offline at boot) or loses its tracks (producer
// stopped), it re-attaches with the same backoff the producer uses for
// reconnects, until DelPreload stops it.
type Preload struct {
	name   string
	stream *Stream // Don't include the stream in JSON to avoid leaking secrets.
	query  url.Values
	Query  string

	mu   sync.Mutex
	cons *probe.Probe
	err  error
	stop chan struct{}

	attachMu sync.Mutex // serializes attach attempts
}

var preloads = map[string]*Preload{}
var preloadsMu sync.Mutex

// AddPreload registers the preload and attaches it. An attach failure is not
// an error: the preload stays registered and keeps retrying in background.
func AddPreload(name, rawQuery string) error {
	if rawQuery == "" {
		rawQuery = "video&audio"
	}

	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return err
	}

	stream := Get(name)
	if stream == nil {
		return fmt.Errorf("streams: stream not found: %s", name)
	}

	p := &Preload{
		name:   name,
		stream: stream,
		query:  query,
		Query:  rawQuery,
		stop:   make(chan struct{}),
	}

	preloadsMu.Lock()
	old := preloads[name]
	preloads[name] = p
	preloadsMu.Unlock()

	if old != nil {
		old.close()
	}

	// Don't hold preloadsMu during this call to avoid blocking API
	if err = p.tryAttach(); err != nil {
		log.Warn().Err(err).Str("name", name).Msg("[preload] attach failed, retrying")
	}

	go p.supervise()

	return nil
}

func DelPreload(name string) error {
	preloadsMu.Lock()
	p := preloads[name]
	delete(preloads, name)
	preloadsMu.Unlock()

	if p == nil {
		return fmt.Errorf("streams: preload not found: %s", name)
	}

	p.close()
	return nil
}

func GetPreload(name string) *Preload {
	preloadsMu.Lock()
	defer preloadsMu.Unlock()
	return preloads[name]
}

func GetPreloads() map[string]*Preload {
	preloadsMu.Lock()
	defer preloadsMu.Unlock()

	return maps.Clone(preloads)
}

func HasPreload(name string) bool {
	return GetPreload(name) != nil
}

// preloadOf returns the preload registered for the stream, if any.
func preloadOf(stream *Stream) *Preload {
	preloadsMu.Lock()
	defer preloadsMu.Unlock()

	for _, p := range preloads {
		if p.stream == stream {
			return p
		}
	}
	return nil
}

// isPreloadConsumer tells the preload's own probe apart from real clients.
func isPreloadConsumer(cons core.Consumer) bool {
	p, ok := cons.(*probe.Probe)
	return ok && p.FormatName == "preload"
}

// ensurePreload attaches the stream's preload before a real client
// negotiates, so the camera session always starts from the preload's query.
// A client arriving first would otherwise dictate a smaller session that
// the preload has to widen later — for RTSP that means a reconnect.
//
// The preload owns the source: if it can't attach, the client gets that
// error instead of dialing the camera a second time itself.
func ensurePreload(stream *Stream, cons core.Consumer) error {
	if isPreloadConsumer(cons) {
		return nil
	}
	p := preloadOf(stream)
	if p == nil {
		return nil
	}
	if err := p.attachIfIdle(); err != nil {
		return fmt.Errorf("streams: preload: %w", err)
	}
	return nil
}

// Attached reports whether the preload consumer currently holds live tracks.
func (p *Preload) Attached() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cons != nil && p.cons.IsActive()
}

// Err returns the last attach error, nil while attached.
func (p *Preload) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *Preload) MarshalJSON() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	v := struct {
		Cons     *probe.Probe `json:"consumer,omitempty"`
		Query    string       `json:"query"`
		Attached bool         `json:"attached"`
		Error    string       `json:"error,omitempty"`
	}{
		Cons:     p.cons,
		Query:    p.Query,
		Attached: p.cons != nil && p.cons.IsActive(),
	}
	if p.err != nil {
		v.Error = p.err.Error()
	}
	return json.Marshal(v)
}

func (p *Preload) stopped() bool {
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

// attach negotiates a fresh consumer and swaps it in. The old consumer (if
// any) is removed only after the new one holds the producer, so the producer
// never loses its last reader in between.
func (p *Preload) attach() error {
	cons := probe.Create("preload", p.query)
	err := p.stream.AddConsumer(cons)

	p.mu.Lock()
	if err != nil {
		p.err = err
		p.mu.Unlock()
		return err
	}
	if p.stopped() {
		p.mu.Unlock()
		p.stream.RemoveConsumer(cons)
		return nil
	}
	old := p.cons
	p.cons = cons
	p.err = nil
	p.mu.Unlock()

	if old != nil {
		p.stream.RemoveConsumer(old)
	}
	return nil
}

// tryAttach attaches the preload unless it's already attached or stopped.
func (p *Preload) tryAttach() error {
	p.attachMu.Lock()
	defer p.attachMu.Unlock()

	if p.Attached() || p.stopped() {
		return nil
	}
	return p.attach()
}

// an attach in flight already owns the camera session; waiting for it would
// deadlock when the client is the stream's own ffmpeg companion looping back
// through the RTSP server while that attach dials it
func (p *Preload) attachIfIdle() error {
	if !p.attachMu.TryLock() {
		return nil
	}
	defer p.attachMu.Unlock()

	if p.Attached() || p.stopped() {
		return nil
	}
	return p.attach()
}

func (p *Preload) supervise() {
	retry := 0
	for {
		delay := preloadCheckInterval
		if !p.Attached() {
			delay = reconnectDelay(retry)
		}

		select {
		case <-p.stop:
			return
		case <-time.After(delay):
		}

		if p.Attached() {
			retry = 0
			continue
		}

		log.Debug().Str("name", p.name).Int("retry", retry).Msg("[preload] re-attaching")

		if err := p.tryAttach(); err != nil {
			retry++
			log.Debug().Err(err).Str("name", p.name).Msg("[preload] attach failed")
		} else {
			retry = 0
		}
	}
}

func (p *Preload) close() {
	p.mu.Lock()
	if p.stopped() {
		p.mu.Unlock()
		return
	}
	close(p.stop)
	cons := p.cons
	p.cons = nil
	p.mu.Unlock()

	if cons != nil {
		p.stream.RemoveConsumer(cons)
	}
}
