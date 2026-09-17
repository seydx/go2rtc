package streams

import (
	"github.com/AlexxIT/go2rtc/pkg/core"
)

// State is what camera.ui mirrors of a stream: how its source is doing, what
// the camera sends, who is attached and whether the preload holds.
type State struct {
	Status    string          `json:"status"`
	Error     string          `json:"error,omitempty"`
	Producers []*Producer     `json:"producers"`
	Consumers []ConsumerState `json:"consumers"`
	Preload   *PreloadState   `json:"preload"`
	Offers    *Offers         `json:"offers"`
}

type ConsumerState struct {
	ID         uint32 `json:"id,omitempty"`
	FormatName string `json:"format_name,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	RemoteAddr string `json:"remote_addr,omitempty"`
	UserAgent  string `json:"user_agent,omitempty"`
	Tag        string `json:"tag,omitempty"`
}

type PreloadState struct {
	Attached bool   `json:"attached"`
	Error    string `json:"error,omitempty"`
}

// Stats holds the raw traffic counters of a stream. They start at zero again
// when a reconnect replaces a receiver, which then has a new id.
type Stats struct {
	Producers []ProducerStats `json:"producers"`
	Consumers []ConsumerStats `json:"consumers"`
}

type ProducerStats struct {
	ID        uint32           `json:"id,omitempty"`
	Receivers []*core.Receiver `json:"receivers"`
}

type ConsumerStats struct {
	ID      uint32         `json:"id,omitempty"`
	Senders []*core.Sender `json:"senders"`
}

// StateOf builds the state of the stream registered under name. It takes the
// stream and producer locks itself, never call it with them held.
func StateOf(name string, s *Stream) *State {
	s.mu.Lock()
	producers := append([]*Producer{}, s.producers...)
	consumers := append([]core.Consumer(nil), s.consumers...)
	s.mu.Unlock()

	state := &State{
		Status:    s.Status(),
		Producers: producers,
		Consumers: make([]ConsumerState, 0, len(consumers)),
		Offers:    OffersOf(s),
	}

	if len(producers) > 0 {
		if err := producers[0].Err(); err != nil {
			state.Error = err.Error()
		}
	}

	for _, cons := range consumers {
		state.Consumers = append(state.Consumers, consumerState(cons))
	}

	if p := GetPreload(name); p != nil && p.stream == s {
		state.Preload = &PreloadState{Attached: p.Attached()}
		if err := p.Err(); err != nil {
			state.Preload.Error = err.Error()
		}
	}

	return state
}

// StatsOf reads the traffic counters of a stream.
func StatsOf(s *Stream) *Stats {
	s.mu.Lock()
	producers := append([]*Producer(nil), s.producers...)
	consumers := append([]core.Consumer(nil), s.consumers...)
	s.mu.Unlock()

	stats := &Stats{
		Producers: make([]ProducerStats, 0, len(producers)),
		Consumers: make([]ConsumerStats, 0, len(consumers)),
	}

	for _, prod := range producers {
		prod.mu.RLock()
		conn := prod.conn
		receivers := append([]*core.Receiver(nil), prod.receivers...)
		prod.mu.RUnlock()

		entry := ProducerStats{Receivers: receivers}
		if c, ok := conn.(interface{ GetID() uint32 }); ok {
			entry.ID = c.GetID()
		}
		stats.Producers = append(stats.Producers, entry)
	}

	for _, cons := range consumers {
		entry := ConsumerStats{}
		if c, ok := cons.(interface{ GetConnection() *core.Connection }); ok {
			conn := c.GetConnection()
			entry.ID = conn.ID
			entry.Senders = conn.Senders
		}
		stats.Consumers = append(stats.Consumers, entry)
	}

	return stats
}

func consumerState(cons core.Consumer) ConsumerState {
	c, ok := cons.(interface{ GetConnection() *core.Connection })
	if !ok {
		return ConsumerState{}
	}
	conn := c.GetConnection()
	return ConsumerState{
		ID:         conn.ID,
		FormatName: conn.FormatName,
		Protocol:   conn.Protocol,
		RemoteAddr: conn.RemoteAddr,
		UserAgent:  conn.UserAgent,
		Tag:        conn.Tag,
	}
}
