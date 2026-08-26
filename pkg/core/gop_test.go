package core

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// pkg/h264 registers the real codec but core can't import it, so the tests
// use a stub: a frame is a keyframe when its payload starts with 'K', and an
// RTP frame ends with the marker bit.
func init() {
	RegisterGopCodec(CodecH264, func(payload []byte) bool {
		return len(payload) > 0 && payload[0] == 'K'
	}, func(_ *Codec, handler HandlerFunc) HandlerFunc {
		var buf []byte
		return func(packet *Packet) {
			if packet.Version == RTPPacketVersionAVC {
				handler(packet)
				return
			}
			buf = append(buf, packet.Payload...)
			if packet.Marker {
				handler(&Packet{Header: rtp.Header{Timestamp: packet.Timestamp}, Payload: buf})
				buf = nil
			}
		}
	})
}

func avcFrame(payload string, ts uint32) *Packet {
	return &Packet{Header: rtp.Header{Timestamp: ts}, Payload: []byte(payload)}
}

func rtpFragment(payload string, ts uint32, seq uint16, marker bool) *Packet {
	return &Packet{
		Header:  rtp.Header{Version: 2, Marker: marker, SequenceNumber: seq, Timestamp: ts},
		Payload: []byte(payload),
	}
}

func payloads(packets []*Packet) []string {
	s := make([]string, len(packets))
	for i, p := range packets {
		s[i] = string(p.Payload)
	}
	return s
}

func TestGopCacheAVCC(t *testing.T) {
	cache := NewGopCache(&Codec{Name: CodecH264, PayloadType: PayloadTypeRAW})
	require.NotNil(t, cache)

	// nothing is decodable before the first keyframe
	cache.Input(avcFrame("P", 0))
	frames, _ := cache.Snapshot()
	require.Nil(t, frames)

	cache.Input(avcFrame("K1", 100))
	cache.Input(avcFrame("P1", 200))
	cache.Input(avcFrame("P2", 300))
	frames, pending := cache.Snapshot()
	require.Equal(t, []string{"K1", "P1", "P2"}, payloads(frames))
	require.Empty(t, pending)

	// next keyframe starts a new GOP and must not touch an older snapshot
	cache.Input(avcFrame("K2", 400))
	cache.Input(avcFrame("P3", 500))
	require.Equal(t, []string{"K1", "P1", "P2"}, payloads(frames))
	frames, _ = cache.Snapshot()
	require.Equal(t, []string{"K2", "P3"}, payloads(frames))

	cache.Clear()
	frames, _ = cache.Snapshot()
	require.Nil(t, frames)

	require.Nil(t, NewGopCache(&Codec{Name: CodecOpus}))
}

func TestGopCacheRTP(t *testing.T) {
	cache := NewGopCache(&Codec{Name: CodecH264, PayloadType: 96})
	require.NotNil(t, cache)

	cache.Input(rtpFragment("K", 100, 1, false))
	cache.Input(rtpFragment("1", 100, 2, true))
	cache.Input(rtpFragment("P", 200, 3, false))

	frames, pending := cache.Snapshot()
	require.Equal(t, []string{"K1"}, payloads(frames))
	require.Equal(t, []string{"P"}, payloads(pending))
	require.Equal(t, uint8(2), pending[0].Version)

	// completing the frame moves it from pending to frames
	cache.Input(rtpFragment("1", 200, 4, true))
	frames, pending = cache.Snapshot()
	require.Equal(t, []string{"K1", "P1"}, payloads(frames))
	require.Empty(t, pending)
}

func TestGopCacheClonesPayload(t *testing.T) {
	cache := NewGopCache(&Codec{Name: CodecH264, PayloadType: PayloadTypeRAW})
	packet := avcFrame("K1", 100)
	cache.Input(packet)
	packet.Payload[0] = 'X' // producer reuses its buffer

	frames, _ := cache.Snapshot()
	require.Equal(t, "K1", string(frames[0].Payload))
}

func TestReplayGOP(t *testing.T) {
	frames := []*Packet{avcFrame("K", 3600), avcFrame("P1", 7200), avcFrame("P2", 10800)}
	pending := []*Packet{rtpFragment("P3", 14400, 9, false)}

	var out []*Packet
	replayGOP(frames, pending, func(p *Packet) { out = append(out, p) }, func() bool { return false })

	require.Equal(t, []string{"K", "P1", "P2", "P3"}, payloads(out))
	// frames are re-timed to the replay pacing, ending at the last frame's
	// original timestamp; pending fragments keep theirs
	step := uint32(gopReplayInterval * videoClockRate / time.Second)
	require.Equal(t, uint32(10800-2*step), out[0].Timestamp)
	require.Equal(t, uint32(10800-step), out[1].Timestamp)
	require.Equal(t, uint32(10800), out[2].Timestamp)
	require.Equal(t, uint32(14400), out[3].Timestamp)
	// cached packets are never handed out directly
	require.NotSame(t, frames[0], out[0])
	require.NotSame(t, pending[0], out[3])
}

func TestReplayGOPStopsWhenClosed(t *testing.T) {
	frames := []*Packet{avcFrame("K", 100), avcFrame("P1", 200)}

	var n int
	replayGOP(frames, nil, func(*Packet) { n++ }, func() bool { return n > 0 })
	require.Equal(t, 1, n)
}

func TestReplayGOPPacing(t *testing.T) {
	long := make([]*Packet, 300)
	for i := range long {
		long[i] = avcFrame("P", uint32(i)*3600)
	}
	long[0].Payload[0] = 'K'

	start := time.Now()
	replayGOP(long, nil, func(*Packet) {}, func() bool { return false })
	elapsed := time.Since(start)

	// a long GOP is replayed faster than gopReplayInterval per frame...
	require.Less(t, elapsed, time.Duration(len(long))*gopReplayInterval)
	// ...but never faster than gopReplayMinInterval per frame
	require.GreaterOrEqual(t, elapsed, time.Duration(len(long)-1)*gopReplayMinInterval)
}

func newVideoReceiver() *Receiver {
	return NewReceiver(nil, &Codec{Name: CodecH264, ClockRate: 90000, PayloadType: PayloadTypeRAW})
}

func collect(t *testing.T, recv chan *Packet, n int) []*Packet {
	var out []*Packet
	for len(out) < n {
		select {
		case p := <-recv:
			out = append(out, p)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d of %d packets", len(out), n)
		}
	}
	return out
}

func TestSenderReplaysGOP(t *testing.T) {
	receiver := newVideoReceiver()
	receiver.SetupGOP()
	receiver.Input(avcFrame("K", 90000))
	receiver.Input(avcFrame("P1", 93600))

	recv := make(chan *Packet, 16)
	sender := NewSender(nil, receiver.Codec)
	sender.Output = func(p *Packet) { recv <- p }
	sender.WithParent(receiver)

	// arrives after bind but before start: buffered by the sender and cached
	receiver.Input(avcFrame("P2", 97200))

	sender.Start()
	receiver.Input(avcFrame("P3", 100800))

	out := collect(t, recv, 4)
	// every packet exactly once, in order: cache first, then live
	require.Equal(t, []string{"K", "P1", "P2", "P3"}, payloads(out))
	for i := 1; i < len(out); i++ {
		require.Less(t, out[i-1].Timestamp, out[i].Timestamp)
	}
	require.Equal(t, uint32(97200), out[2].Timestamp, "last cached frame keeps its timestamp")
	require.Equal(t, uint32(100800), out[3].Timestamp, "live packets are untouched")

	require.Equal(t, 4, sender.Packets)
	require.Equal(t, 0, sender.Drops)

	sender.Close()
	sender.Wait()
}

func TestSenderWithoutGOP(t *testing.T) {
	receiver := newVideoReceiver()
	receiver.SetupGOP()
	receiver.Input(avcFrame("K", 100))

	recv := make(chan *Packet, 16)
	sender := NewSender(nil, receiver.Codec)
	sender.UseGOP = false
	sender.Output = func(p *Packet) { recv <- p }
	sender.WithParent(receiver)

	receiver.Input(avcFrame("P1", 200))
	sender.Start()
	receiver.Input(avcFrame("P2", 300))

	// plain sender: what was buffered since bind, then live
	require.Equal(t, []string{"P1", "P2"}, payloads(collect(t, recv, 2)))

	sender.Close()
	sender.Wait()
}

func TestSenderCloseDuringReplay(t *testing.T) {
	receiver := newVideoReceiver()
	receiver.SetupGOP()
	for i := range 50 {
		p := avcFrame("P", uint32(i)*3600)
		if i == 0 {
			p.Payload[0] = 'K'
		}
		receiver.Input(p)
	}

	recv := make(chan *Packet, 1)
	sender := NewSender(nil, receiver.Codec)
	sender.Output = func(p *Packet) { recv <- p }
	sender.WithParent(receiver)
	sender.Start()

	<-recv
	sender.Close()
	go func() {
		for range recv { // drain whatever was still in flight
		}
	}()
	sender.Wait()
	close(recv)
}
