package cui

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api/ws"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/creds"
)

const streamPrefix = "cui_"

var (
	// a burst of changes (reconnect plus several consumers) goes out as one message per stream
	flushDelay    = 250 * time.Millisecond
	statsInterval = 5 * time.Second
)

type subscriberKey struct{}

// subscribe streams the state of every camera.ui stream to the client: a
// snapshot first, then the full state of a stream whenever it changed, plus
// traffic counters on a fixed interval.
func subscribe(tr *ws.Transport, _ *ws.Message) error {
	var subscribed bool
	tr.WithContext(func(ctx map[any]any) {
		_, subscribed = ctx[subscriberKey{}]
		ctx[subscriberKey{}] = true
	})
	if subscribed {
		return nil
	}

	done := make(chan struct{})
	tr.OnClose(func() { close(done) })

	// watch before the snapshot, so nothing that changes while it is built gets lost
	watcher := streams.Watch()
	go run(tr, watcher, done)

	return nil
}

func run(tr *ws.Transport, watcher *streams.Watcher, done chan struct{}) {
	defer watcher.Close()

	// sent: the stream object each name was last sent for
	sent := map[string]*streams.Stream{}
	snapshot := map[string]*streams.State{}
	for name, stream := range cuiStreams() {
		snapshot[name] = streams.StateOf(name, stream)
		sent[name] = stream
	}
	write(tr, "cui/snapshot", map[string]any{"streams": snapshot})

	stats := time.NewTicker(statsInterval)
	defer stats.Stop()

	for {
		select {
		case <-done:
			return
		case <-stats.C:
			writeStats(tr)
		case <-watcher.C:
			select {
			case <-done:
				return
			case <-time.After(flushDelay):
			}
			flush(tr, watcher, sent)
		}
	}
}

// flush sends the current state of every stream that changed, of every name
// that appeared or now points at another stream, and null for every name that
// is gone. A relinked name (alias patched to another stream, or deleted and
// created again within one flush) only signals the registry, it marks no
// stream as changed, so the stream object behind a name is compared too.
func flush(tr *ws.Transport, watcher *streams.Watcher, sent map[string]*streams.Stream) {
	changed := watcher.Take()
	current := cuiStreams()

	for name, stream := range current {
		if _, ok := changed[stream]; !ok && sent[name] == stream {
			continue
		}
		sent[name] = stream
		write(tr, "cui/stream", map[string]any{"name": name, "state": streams.StateOf(name, stream)})
	}

	for name := range sent {
		if _, ok := current[name]; ok {
			continue
		}
		delete(sent, name)
		write(tr, "cui/stream", map[string]any{"name": name, "state": nil})
	}
}

func writeStats(tr *ws.Transport) {
	all := map[string]*streams.Stats{}
	for name, stream := range cuiStreams() {
		all[name] = streams.StatsOf(stream)
	}

	b, err := json.Marshal(ws.Message{Type: "cui/stats", Value: map[string]any{"streams": all}})
	if err != nil {
		log.Warn().Err(err).Msg("[cui] marshal stats")
		return
	}
	tr.Write(json.RawMessage(b))
}

// write masks credentials in the whole message, producer urls included
func write(tr *ws.Transport, msgType string, value any) {
	b, err := json.Marshal(ws.Message{Type: msgType, Value: value})
	if err != nil {
		log.Warn().Err(err).Str("type", msgType).Msg("[cui] marshal state")
		return
	}
	tr.Write(json.RawMessage(creds.SecretString(string(b))))
}

func cuiStreams() map[string]*streams.Stream {
	all := streams.GetAll()
	for name := range all {
		if !strings.HasPrefix(name, streamPrefix) {
			delete(all, name)
		}
	}
	return all
}
