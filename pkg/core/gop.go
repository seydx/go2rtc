package core

import (
	"sync"
	"time"

	"github.com/pion/rtp"
)

// RTPPacketVersionAVC marks a packet whose payload is a complete AVCC frame
// instead of an RTP fragment (a real RTP packet always has Version == 2).
const RTPPacketVersionAVC = 0

const (
	// gopReplayInterval is the pause between two replayed frames.
	gopReplayInterval = 10 * time.Millisecond
	// gopReplayMinInterval bounds how fast a long GOP is replayed so a client
	// decoder is not asked for more than ~250 fps.
	gopReplayMinInterval = 4 * time.Millisecond
	// gopReplayBudget is the wall-clock time a whole replay should stay within.
	gopReplayBudget = time.Second
	// videoClockRate is the RTP clock of every video codec.
	videoClockRate = 90000
)

// gopCodec knows how to turn a video track's packets into complete frames.
type gopCodec struct {
	isKeyframe    func(payload []byte) bool
	rtpDepay      func(codec *Codec, handler HandlerFunc) HandlerFunc
	isKeyframeRTP func(payload []byte) bool
}

var (
	gopCodecs   = map[string]gopCodec{}
	gopCodecsMu sync.RWMutex
)

// RegisterGopCodec makes a video codec eligible for the GOP cache.
// isKeyframe inspects a complete AVCC frame, rtpDepay reassembles RTP
// fragments into such frames (marked with RTPPacketVersionAVC), and
// isKeyframeRTP tells the RTP packet a keyframe starts with.
func RegisterGopCodec(name string, isKeyframe func([]byte) bool, rtpDepay func(*Codec, HandlerFunc) HandlerFunc, isKeyframeRTP func([]byte) bool) {
	gopCodecsMu.Lock()
	gopCodecs[name] = gopCodec{isKeyframe: isKeyframe, rtpDepay: rtpDepay, isKeyframeRTP: isKeyframeRTP}
	gopCodecsMu.Unlock()
}

// nil for codecs without a gopCodec
func keyframeStart(codec *Codec) func(packet *Packet) bool {
	gopCodecsMu.RLock()
	gc, ok := gopCodecs[codec.Name]
	gopCodecsMu.RUnlock()
	if !ok {
		return nil
	}

	return func(packet *Packet) bool {
		if packet.Version == RTPPacketVersionAVC {
			return gc.isKeyframe(packet.Payload)
		}
		return gc.isKeyframeRTP != nil && gc.isKeyframeRTP(packet.Payload)
	}
}

// GopCache keeps the current group of pictures of a video track: every
// complete frame since the most recent keyframe, plus the RTP fragments of the
// frame that is still being received. A consumer attaching mid-GOP replays the
// cache and can decode right away instead of waiting for the next keyframe.
type GopCache struct {
	mu      sync.Mutex
	input   HandlerFunc
	frames  gopBuffer // complete AVCC frames since the last keyframe
	pending gopBuffer // RTP fragments received after the last complete frame
}

// NewGopCache returns nil for codecs without a registered gopCodec.
func NewGopCache(codec *Codec) *GopCache {
	gopCodecsMu.RLock()
	gc, ok := gopCodecs[codec.Name]
	gopCodecsMu.RUnlock()
	if !ok {
		return nil
	}

	c := &GopCache{}

	// runs under c.mu, from within c.Input
	onFrame := func(frame *Packet) {
		c.pending.reset(false)
		if gc.isKeyframe(frame.Payload) {
			c.frames.reset(true)
		} else if len(c.frames.ends) == 0 {
			return // nothing is decodable before the first keyframe
		}
		c.frames.add(frame)
	}

	if codec.IsRTP() {
		c.input = gc.rtpDepay(codec, onFrame)
	} else {
		c.input = onFrame
	}

	return c
}

// Input feeds a packet exactly as the receiver got it.
func (c *GopCache) Input(packet *Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if packet.Version != RTPPacketVersionAVC {
		c.pending.add(packet)
	}
	c.input(packet)
}

// Snapshot returns the cached frames and pending fragments. The cache never
// writes to them again. frames is nil while no keyframe has been seen.
func (c *GopCache) Snapshot() (frames, pending []*Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.frames.ends) == 0 {
		return nil, nil
	}
	return c.frames.packets(), c.pending.packets()
}

func (c *GopCache) Clear() {
	c.mu.Lock()
	c.frames = gopBuffer{}
	c.pending = gopBuffer{}
	c.mu.Unlock()
}

// a reset reuses data, unless a snapshot still points into it or, with trim,
// an outlier GOP grew it past twice the bytes it held
type gopBuffer struct {
	headers []rtp.Header
	ends    []int // the payload of packet i ends at data[ends[i]]
	data    []byte
	shared  bool
}

// producers reuse their read buffers after the packet has been handled
func (b *gopBuffer) add(packet *Packet) {
	b.headers = append(b.headers, packet.Header)
	b.data = append(b.data, packet.Payload...)
	b.ends = append(b.ends, len(b.data))
}

func (b *gopBuffer) reset(trim bool) {
	b.headers = b.headers[:0]
	b.ends = b.ends[:0]
	size, used := cap(b.data), len(b.data)
	if trim && size > 2*used {
		size = used + used/2
	}
	if b.shared || size < cap(b.data) {
		b.data = make([]byte, 0, size)
		b.shared = false
	} else {
		b.data = b.data[:0]
	}
}

// no copy: data is not reused after this
func (b *gopBuffer) packets() []*Packet {
	if len(b.ends) == 0 {
		return nil
	}
	b.shared = true

	packets := make([]Packet, len(b.ends))
	out := make([]*Packet, len(b.ends))
	var start int
	for i, end := range b.ends {
		packets[i] = Packet{Header: b.headers[i], Payload: b.data[start:end:end]}
		out[i] = &packets[i]
		start = end
	}
	return out
}

// replayGOP writes a cache snapshot to write, stopping early once closed
// reports true.
//
// Frames are paced (gopReplayInterval apart, faster for long GOPs but never
// below gopReplayMinInterval) so the burst doesn't overrun UDP transports.
// Their timestamps are rewritten to that same pacing, ending at the original
// timestamp of the last cached frame: the client fast-forwards through the
// GOP while its media clock advances at wall-clock speed, so audio (which is
// never cached) stays in sync and the live stream continues seamlessly.
// Pending fragments keep their timestamp, the rest of that frame arrives live.
func replayGOP(frames, pending []*Packet, write HandlerFunc, closed func() bool) {
	n := len(frames)
	if n == 0 {
		return
	}

	interval := gopReplayInterval
	if d := gopReplayBudget / time.Duration(n); d < interval {
		interval = max(d, gopReplayMinInterval)
	}
	step := uint32(interval * videoClockRate / time.Second)

	ts := frames[n-1].Timestamp - uint32(n-1)*step
	for i, frame := range frames {
		if closed() {
			return
		}
		clone := *frame
		clone.Timestamp = ts
		write(&clone)
		ts += step
		if i < n-1 {
			time.Sleep(interval)
		}
	}

	for _, fragment := range pending {
		if closed() {
			return
		}
		clone := *fragment
		write(&clone)
	}
}
