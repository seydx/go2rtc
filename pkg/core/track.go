package core

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/pion/rtp"
)

var ErrCantGetTrack = errors.New("can't get track")

type Receiver struct {
	Node

	// Deprecated: should be removed
	Media *Media `json:"-"`
	// Deprecated: should be removed
	ID byte `json:"-"` // Channel for RTSP, PayloadType for MPEG-TS

	Bytes   int `json:"bytes,omitempty"`
	Packets int `json:"packets,omitempty"`

	LastPacketTime time.Time `json:"-"` // Time of last received packet (for staleness detection)

	codecHandler CodecHandler
}

func NewReceiver(media *Media, codec *Codec) *Receiver {
	r := &Receiver{
		Node:  Node{id: NewID(), Codec: codec},
		Media: media,
	}

	r.SetOwner(r)

	r.Input = func(packet *Packet) {
		r.Bytes += len(packet.Payload)
		r.Packets++
		r.LastPacketTime = time.Now()

		if r.codecHandler != nil {
			r.codecHandler.ProcessPacket(packet)
		}

		// Forward to all children
		for _, child := range r.childs {
			child.Input(packet)
		}
	}
	return r
}

func (r *Receiver) SetupGOP() {
	// GOP is only for video codecs
	if !r.Codec.IsVideo() {
		return
	}

	// Create codec handler if needed
	if r.codecHandler == nil {
		if handler := CreateCodecHandler(r.Codec); handler != nil {
			r.codecHandler = handler
		}
	}

	// Setup GOP cache
	if r.codecHandler != nil {
		r.codecHandler.SetupGOP()
	}
}

// Deprecated: should be removed
func (r *Receiver) WriteRTP(packet *rtp.Packet) {
	r.Input(packet)
}

// Deprecated: should be removed
func (r *Receiver) Senders() []*Sender {
	if len(r.childs) > 0 {
		return []*Sender{{}}
	} else {
		return nil
	}
}

// Deprecated: should be removed
func (r *Receiver) Replace(target *Receiver) {
	MoveNode(&target.Node, &r.Node)
}

func (r *Receiver) Close() {
	if r.codecHandler != nil {
		r.codecHandler.ClearCache()
	}

	r.Node.Close()
}

// IsActive returns true if the receiver has received packets recently (within maxAge).
// Used to detect stale tracks (e.g. camera stopped sending audio).
func (r *Receiver) IsActive(maxAge time.Duration) bool {
	if r.LastPacketTime.IsZero() {
		return false
	}
	return time.Since(r.LastPacketTime) < maxAge
}

type Sender struct {
	Node

	InputCache HandlerFunc

	// Deprecated:
	Media *Media `json:"-"`
	// Deprecated:
	Handler HandlerFunc `json:"-"`

	Bytes   int `json:"bytes,omitempty"`
	Packets int `json:"packets,omitempty"`
	Drops   int `json:"drops,omitempty"`

	UseGOP bool `json:"-"`

	buf  chan *Packet
	done chan struct{}

	started         bool
	waitingForCache bool
	liveQueue       chan *Packet
}

func NewSender(media *Media, codec *Codec) *Sender {
	var bufSize uint16

	if GetKind(codec.Name) == KindVideo {
		if codec.IsRTP() {
			// in my tests 40Mbit/s 4K-video can generate up to 1500 items
			// for the h264.RTPDepay => RTPPay queue
			bufSize = 4096
		} else {
			bufSize = 64
		}
	} else {
		bufSize = 128
	}

	buf := make(chan *Packet, bufSize)
	s := &Sender{
		Node:            Node{id: NewID(), Codec: codec},
		Media:           media,
		UseGOP:          true,
		buf:             buf,
		liveQueue:       make(chan *Packet, 512),
		waitingForCache: true,
	}

	s.SetOwner(s)

	s.Input = func(packet *Packet) {
		if s.UseGOP {
			if !s.started && s.Codec.IsVideo() {
				return
			}

			if s.Codec.IsVideo() {
				if s.waitingForCache {
					select {
					case s.liveQueue <- packet:
					default:
					}
					return
				}
			}
		}

		s.processPacket(packet)
	}

	s.InputCache = func(packet *Packet) {
		s.processPacket(packet)
	}
	s.Output = func(packet *Packet) {
		s.Handler(packet)
	}
	return s
}

// Deprecated: should be removed
func (s *Sender) HandleRTP(parent *Receiver) {
	s.WithParent(parent)
	s.Start()
}

// Deprecated: should be removed
func (s *Sender) Bind(parent *Receiver) {
	s.WithParent(parent)
}

func (s *Sender) WithParent(parent *Receiver) *Sender {
	s.Node.WithParent(&parent.Node)
	return s
}

func (s *Sender) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.buf == nil || s.done != nil {
		return
	}
	s.done = make(chan struct{})

	// pass buf directly so that it's impossible for buf to be nil
	go func(buf chan *Packet) {
		for packet := range buf {
			s.Output(packet)
		}
		close(s.done)
	}(s.buf)

	s.started = true

	// Get codecHandler for GOP cache (only needed for video)
	var codecHandler CodecHandler
	if s.parent != nil {
		if receiver, ok := s.parent.owner.(*Receiver); ok {
			if receiver.codecHandler != nil {
				codecHandler = receiver.codecHandler
			}
		}
	}

	if codecHandler == nil {
		s.waitingForCache = false
		return
	}

	// GOP only for video
	if !s.Codec.IsVideo() {
		s.waitingForCache = false
		return
	}

	if !s.UseGOP {
		s.waitingForCache = false
		return
	}

	go func() {
		nextTimestamp, lastSeq := codecHandler.SendCacheTo(s, 100)
		codecHandler.SendQueueTo(s, 100, nextTimestamp, lastSeq)
		s.waitingForCache = false
	}()
}

func (s *Sender) Wait() {
	if done := s.done; done != nil {
		<-done
	}
}

func (s *Sender) State() string {
	if s.buf == nil {
		return "closed"
	}
	if s.done == nil {
		return "new"
	}
	return "connected"
}

func (s *Sender) Close() {
	// close buffer if exists
	s.mu.Lock()
	if s.buf != nil {
		close(s.buf) // exit from for range loop
		s.buf = nil  // prevent writing to closed chan
	}
	if s.liveQueue != nil {
		close(s.liveQueue)
		s.liveQueue = nil
	}
	s.mu.Unlock()

	s.Node.Close()
}

func (r *Receiver) MarshalJSON() ([]byte, error) {
	v := struct {
		ID      uint32   `json:"id"`
		Codec   *Codec   `json:"codec"`
		Childs  []uint32 `json:"childs,omitempty"`
		Bytes   int      `json:"bytes,omitempty"`
		Packets int      `json:"packets,omitempty"`
	}{
		ID:      r.Node.id,
		Codec:   r.Node.Codec,
		Bytes:   r.Bytes,
		Packets: r.Packets,
	}
	for _, child := range r.childs {
		v.Childs = append(v.Childs, child.id)
	}
	return json.Marshal(v)
}

func (s *Sender) MarshalJSON() ([]byte, error) {
	v := struct {
		ID      uint32 `json:"id"`
		Codec   *Codec `json:"codec"`
		Parent  uint32 `json:"parent,omitempty"`
		Bytes   int    `json:"bytes,omitempty"`
		Packets int    `json:"packets,omitempty"`
		Drops   int    `json:"drops,omitempty"`
	}{
		ID:      s.Node.id,
		Codec:   s.Node.Codec,
		Bytes:   s.Bytes,
		Packets: s.Packets,
		Drops:   s.Drops,
	}
	if s.parent != nil {
		v.Parent = s.parent.id
	}
	return json.Marshal(v)
}

func (s *Sender) processPacket(packet *Packet) {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case s.buf <- packet:
		s.Bytes += len(packet.Payload)
		s.Packets++
	default:
		s.Drops++
	}
}
