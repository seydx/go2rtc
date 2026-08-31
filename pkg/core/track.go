package core

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

var ErrCantGetTrack = errors.New("can't get track")

type Receiver struct {
	Node

	// If set, Bind() will use this Node pointer instead of the embedded Node.
	// This avoids Node copying for backchannel mixers where we need to bind to an existing Node.
	ParentNode *Node `json:"-"`

	// Deprecated: should be removed
	Media *Media `json:"-"`
	// Deprecated: should be removed
	ID byte `json:"-"` // Channel for RTSP, PayloadType for MPEG-TS

	Bytes   int `json:"bytes,omitempty"`
	Packets int `json:"packets,omitempty"`

	lastPacket atomic.Int64 // UnixNano of the last received packet (for staleness detection)

	// gop is the GOP cache (nil = disabled). gopMu makes "update cache, then
	// forward to children" atomic against a Sender taking a cache snapshot,
	// so a sender never sees a packet twice or misses one.
	gop   *GopCache
	gopMu sync.Mutex
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
		r.lastPacket.Store(time.Now().UnixNano())

		r.gopMu.Lock()
		if r.gop != nil {
			r.gop.Input(packet)
		}
		r.forward(packet)
		r.gopMu.Unlock()
	}
	return r
}

func (r *Receiver) forward(packet *Packet) {
	// Use custom Forward function if set (e.g., by mixer), otherwise forward to children
	if r.Forward != nil {
		r.Forward(packet)
		return
	}
	for _, child := range r.childs {
		child.Input(packet)
	}
}

// SetupGOP enables the GOP cache. No-op for audio and unsupported codecs.
func (r *Receiver) SetupGOP() {
	if !r.Codec.IsVideo() {
		return
	}

	r.gopMu.Lock()
	if r.gop == nil {
		r.gop = NewGopCache(r.Codec)
	}
	r.gopMu.Unlock()
}

// attachGOP hands the sender a snapshot of the GOP cache. If the cache has
// content it covers everything the sender buffered before now, so that
// buffer is discarded to avoid delivering those packets twice.
func (r *Receiver) attachGOP(s *Sender) (frames, pending []*Packet) {
	r.gopMu.Lock()
	defer r.gopMu.Unlock()

	if r.gop == nil {
		return nil, nil
	}
	frames, pending = r.gop.Snapshot()
	if len(frames) > 0 {
		s.discard()
	}
	return
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

// Retire moves the consumers onto target and marks this receiver as gone
// for good, so a consumer attach that lost the race against the swap still
// reaches the successor. Only for receivers that are discarded.
func (r *Receiver) Retire(target *Receiver) {
	MoveNode(&target.Node, &r.Node)
}

func (r *Receiver) Close() {
	r.gopMu.Lock()
	if r.gop != nil {
		r.gop.Clear()
	}
	r.gopMu.Unlock()

	// Before closing, check if this receiver has any mixer nodes as children
	// If so, call RemoveParent on those mixers
	r.Node.mu.Lock()
	children := r.Node.childs
	r.Node.mu.Unlock()

	for _, child := range children {
		// Check if this child is a mixer node (has RTPMixer as owner)
		if mixer, ok := child.owner.(*RTPMixer); ok {
			mixer.RemoveParent(&r.Node)
		}
	}

	r.Node.Close()
}

// IsActive returns true if the receiver has received packets recently (within maxAge).
// Used to detect stale tracks (e.g. camera stopped sending audio).
func (r *Receiver) IsActive(maxAge time.Duration) bool {
	last := r.lastPacket.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(0, last)) < maxAge
}

type Sender struct {
	Node

	// Deprecated:
	Media *Media `json:"-"`
	// Deprecated:
	Handler HandlerFunc `json:"-"`

	Bytes   int `json:"bytes,omitempty"`
	Packets int `json:"packets,omitempty"`
	Drops   int `json:"drops,omitempty"`

	// UseGOP replays the receiver's GOP cache (if the producer has one) on Start.
	UseGOP bool `json:"-"`

	buf  chan *Packet
	done chan struct{}
}

func NewSender(media *Media, codec *Codec) *Sender {
	var bufSize uint16

	if GetKind(codec.Name) == KindVideo {
		if codec.IsRTP() {
			// in my tests 40Mbit/s 4K-video can generate up to 1500 items
			// for the h264.RTPDepay => RTPPay queue
			bufSize = 4096
		} else {
			// live frames queue up here while a GOP cache is replayed
			bufSize = 128
		}
	} else {
		bufSize = 128
	}

	buf := make(chan *Packet, bufSize)
	s := &Sender{
		Node:   Node{id: NewID(), Codec: codec},
		Media:  media,
		UseGOP: true,
		buf:    buf,
	}

	s.SetOwner(s)

	s.Input = func(packet *Packet) {
		s.mu.Lock()
		select {
		case s.buf <- packet:
			s.Bytes += len(packet.Payload)
			s.Packets++
		default:
			s.Drops++
		}
		s.mu.Unlock()
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
	if parent.ParentNode != nil {
		s.Node.WithParent(parent.ParentNode)
	} else {
		s.Node.WithParent(&parent.Node)
	}
	return s
}

func (s *Sender) Start() {
	s.mu.Lock()
	if s.buf == nil || s.done != nil {
		s.mu.Unlock()
		return
	}
	s.done = make(chan struct{})
	buf := s.buf // pass buf to goroutine so that it's impossible for buf to be nil
	s.mu.Unlock()

	var frames, pending []*Packet
	if s.UseGOP && s.parent != nil {
		if receiver, ok := s.parent.owner.(*Receiver); ok {
			frames, pending = receiver.attachGOP(s)
		}
	}

	go func() {
		// the cached GOP goes first, live packets queue up in buf meanwhile
		replayGOP(frames, pending, s.writeCached, s.closed)

		for packet := range buf {
			s.Output(packet)
		}
		close(s.done)
	}()
}

func (s *Sender) writeCached(packet *Packet) {
	s.mu.Lock()
	s.Bytes += len(packet.Payload)
	s.Packets++
	s.mu.Unlock()

	s.Output(packet)
}

func (s *Sender) closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf == nil
}

// discard drops everything buffered so far.
func (s *Sender) discard() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for {
		select {
		case packet := <-s.buf:
			s.Bytes -= len(packet.Payload)
			s.Packets--
		default:
			return
		}
	}
}

func (s *Sender) Wait() {
	if done := s.done; done != nil {
		<-done
	}
}

func (s *Sender) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()

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
