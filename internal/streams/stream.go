package streams

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

type Stream struct {
	producers []*Producer
	consumers []core.Consumer
	mu        sync.Mutex
	pending   atomic.Int32
}

func NewStream(source any) *Stream {
	switch source := source.(type) {
	case string:
		return &Stream{
			producers: []*Producer{NewProducer(source)},
		}
	case []string:
		s := new(Stream)
		for _, str := range source {
			s.producers = append(s.producers, NewProducer(str))
		}
		return s
	case []any:
		s := new(Stream)
		for _, src := range source {
			str, ok := src.(string)
			if !ok {
				log.Error().Msgf("[stream] NewStream: Expected string, got %v", src)
				continue
			}
			s.producers = append(s.producers, NewProducer(str))
		}
		return s
	case map[string]any:
		return NewStream(source["url"])
	case nil:
		return new(Stream)
	default:
		panic(core.Caller())
	}
}

func (s *Stream) Sources() []string {
	sources := make([]string, 0, len(s.producers))
	for _, prod := range s.producers {
		sources = append(sources, prod.url)
	}
	return sources
}

func (s *Stream) SetSource(source string) {
	for _, prod := range s.producers {
		prod.SetSource(source)
	}
}

func (s *Stream) RemoveConsumer(cons core.Consumer) {
	_ = cons.Stop()

	s.mu.Lock()
	for i, consumer := range s.consumers {
		if consumer == cons {
			s.consumers = append(s.consumers[:i], s.consumers[i+1:]...)
			break
		}
	}
	s.mu.Unlock()

	s.stopProducers()
}

func (s *Stream) RemoveConsumerByID(id uint32) bool {
	s.mu.Lock()
	var target core.Consumer
	for _, cons := range s.consumers {
		if c, ok := cons.(interface{ GetID() uint32 }); ok && c.GetID() == id {
			target = cons
			break
		}
	}
	s.mu.Unlock()

	if target == nil {
		return false
	}

	s.RemoveConsumer(target)
	return true
}

func (s *Stream) RemoveConsumersByTag(tag string) int {
	if tag == "" {
		return 0
	}

	s.mu.Lock()
	var targets []core.Consumer
	for _, cons := range s.consumers {
		if c, ok := cons.(interface{ GetTag() string }); ok && c.GetTag() == tag {
			targets = append(targets, cons)
		}
	}
	s.mu.Unlock()

	for _, t := range targets {
		s.RemoveConsumer(t)
	}
	return len(targets)
}

func (s *Stream) AddProducer(prod core.Producer) {
	producer := &Producer{conn: prod, state: stateExternal, url: "external"}
	s.mu.Lock()
	s.producers = append(s.producers, producer)
	s.mu.Unlock()
}

func (s *Stream) RemoveProducer(prod core.Producer) {
	s.mu.Lock()
	for i, producer := range s.producers {
		if producer.conn == prod {
			s.producers = append(s.producers[:i], s.producers[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}

func (s *Stream) stopProducers() {
	if s.pending.Load() > 0 {
		log.Trace().Msg("[streams] skip stop pending producer")
		return
	}

	s.mu.Lock()
producers:
	for _, producer := range s.producers {
		for _, track := range producer.receivers {
			if len(track.Senders()) > 0 {
				continue producers
			}
		}
		for _, track := range producer.senders {
			if len(track.Senders()) > 0 {
				continue producers
			}
		}
		producer.stop()
	}
	s.mu.Unlock()
}

// isAudioStale returns true if all audio receivers across all started producers
// exist but have not received any packets recently. This detects cameras that
// advertise audio but stopped sending it (e.g. hardware issue).
func (s *Stream) isAudioStale() bool {
	hasAudioReceiver := false
	for _, prod := range s.producers {
		if prod.state < stateStart {
			continue
		}
		for _, recv := range prod.receivers {
			if recv.Codec != nil && recv.Codec.IsAudio() {
				hasAudioReceiver = true
				if recv.IsActive(audioStaleThreshold) {
					return false // at least one audio receiver is active
				}
			}
		}
	}
	return hasAudioReceiver // all audio receivers are stale
}

func (s *Stream) MarshalJSON() ([]byte, error) {
	s.mu.Lock()
	// Copy references while holding lock, then release before marshaling
	// to avoid blocking during slow producer operations
	producers := s.producers
	consumers := s.consumers
	s.mu.Unlock()

	var info = struct {
		Producers []*Producer     `json:"producers"`
		Consumers []core.Consumer `json:"consumers"`
	}{
		Producers: producers,
		Consumers: consumers,
	}
	return json.Marshal(info)
}
