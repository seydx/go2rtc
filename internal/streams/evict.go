package streams

import (
	"errors"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// A consumer negotiates its codecs once: an MSE init segment, a WebRTC answer,
// an RTSP DESCRIBE. When a camera is reconfigured to a codec the consumer did
// not negotiate, the producer's reconnect drops the stale track, and nothing
// will ever feed the consumer again. It has to reconnect, so the stream
// evicts it: the consumer is removed and stopped, and the handler that owns
// its connection closes it, so the client sees the session end.

var errTrackGone = errors.New("streams: track was dropped while attaching")

// OnEvict registers fn to run when the stream evicts cons, so the handler can
// end the client connection its transport does not end by itself (a
// websocket survives the consumer being stopped). Register before
// AddConsumer: eviction can happen as soon as the consumer is attached. fn is
// not called when the consumer is removed any other way.
func (s *Stream) OnEvict(cons core.Consumer, fn func()) {
	s.mu.Lock()
	if s.evictHooks == nil {
		s.evictHooks = map[core.Consumer]func(){}
	}
	s.evictHooks[cons] = fn
	s.mu.Unlock()
}

// bind records the producer receivers cons was attached to.
func (s *Stream) bind(cons core.Consumer, tracks []*core.Receiver) {
	if len(tracks) == 0 {
		return
	}
	if s.bound == nil {
		s.bound = map[core.Consumer][]*core.Receiver{}
	}
	s.bound[cons] = tracks
}

// forget drops the bookkeeping of a consumer that is no longer attached.
// Callers hold s.mu.
func (s *Stream) forget(cons core.Consumer) {
	delete(s.bound, cons)
	delete(s.evictHooks, cons)
}

func anyGone(tracks []*core.Receiver) bool {
	for _, track := range tracks {
		if track.Gone() {
			return true
		}
	}
	return false
}

// evictStaleConsumers evicts every consumer bound to a receiver that is gone.
// A producer calls it after its reconnect dropped tracks.
func (s *Stream) evictStaleConsumers() {
	s.mu.Lock()
	var victims []core.Consumer
	for cons, tracks := range s.bound {
		if anyGone(tracks) {
			victims = append(victims, cons)
		}
	}
	s.mu.Unlock()

	for _, cons := range victims {
		s.evict(cons)
	}

	if len(victims) == 0 {
		return
	}

	// the preload may have been among them: renegotiate the new codec now
	// instead of waiting for the supervisor
	if p := preloadOf(s); p != nil {
		go func() { _ = p.tryAttach() }()
	}
}

func (s *Stream) evict(cons core.Consumer) {
	s.mu.Lock()
	hook := s.evictHooks[cons]
	s.mu.Unlock()

	log.Debug().Msgf("[streams] evict consumer, its track was dropped by a codec change")

	s.RemoveConsumer(cons)

	if hook != nil {
		hook()
	}
}
