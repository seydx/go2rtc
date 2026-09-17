package streams

import (
	"sync"
	"sync/atomic"
)

// Watcher collects the streams that changed since it was last drained.
// Marking a change never blocks: it is a set insert under the watcher's own
// mutex, which is never held together with a stream or producer lock, so
// hooks may call it with those locks held.
type Watcher struct {
	// C receives a signal whenever something changed. Changes that arrive
	// before the signal is read collapse into one.
	C chan struct{}

	mu      sync.Mutex
	changed map[*Stream]struct{}
}

var (
	watchers      []*Watcher
	watchersMu    sync.Mutex
	watchersCount atomic.Int32
)

// Watch registers a new watcher. Close it when done.
func Watch() *Watcher {
	w := &Watcher{C: make(chan struct{}, 1), changed: map[*Stream]struct{}{}}

	watchersMu.Lock()
	watchers = append(watchers, w)
	watchersCount.Store(int32(len(watchers)))
	watchersMu.Unlock()

	return w
}

func (w *Watcher) Close() {
	watchersMu.Lock()
	for i, other := range watchers {
		if other == w {
			watchers = append(watchers[:i], watchers[i+1:]...)
			break
		}
	}
	watchersCount.Store(int32(len(watchers)))
	watchersMu.Unlock()
}

// Take returns the streams that changed since the last call and resets the set.
func (w *Watcher) Take() map[*Stream]struct{} {
	w.mu.Lock()
	changed := w.changed
	w.changed = map[*Stream]struct{}{}
	w.mu.Unlock()
	return changed
}

// notify marks s as changed for every watcher. A nil stream only signals,
// for changes of the name registry.
func notify(s *Stream) {
	if watchersCount.Load() == 0 {
		return
	}

	watchersMu.Lock()
	for _, w := range watchers {
		if s != nil {
			w.mu.Lock()
			w.changed[s] = struct{}{}
			w.mu.Unlock()
		}
		select {
		case w.C <- struct{}{}:
		default:
		}
	}
	watchersMu.Unlock()
}

func (p *Producer) notify() {
	if p.stream != nil {
		notify(p.stream)
	}
}
