package h265

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// Based on AlexxIT/go2rtc#2479. Unlike upstream, the fork never waits for a
// keyframe after a loss: only the damaged access unit is dropped, so NVRs and
// recorders don't get a gap of a whole GOP.

func TestRTPDepayIntactAccessUnit(t *testing.T) {
	var got [][]byte
	depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { got = append(got, bytes.Clone(p.Payload)) })
	// Include sequence rollover and the marked-SEI camera compatibility path.
	depay(lossPacket(65533, 100, true, []byte{78, 1, 5}))
	depay(lossPacket(65534, 99, true, []byte{64, 1, 7}))
	depay(lossPacket(65535, 100, false, []byte{98, 1, 147, 10}))
	depay(lossPacket(0, 100, false, []byte{98, 1, 19, 11}))
	depay(lossPacket(1, 100, true, []byte{98, 1, 83, 12}))
	want := lossNALs([]byte{64, 1, 7}, []byte{38, 1, 10, 11, 12})
	if len(got) != 1 || !bytes.Equal(got[0], want) {
		t.Fatalf("got %x, want %x", got, want)
	}
}

func TestRTPDepayLossDropsOnlyDamagedAccessUnit(t *testing.T) {
	tests := []struct {
		name    string
		packets []*rtp.Packet
		want    [][]byte
	}{
		{"missing middle fragment", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{98, 1, 129, 10}),
			lossPacket(12, 100, true, []byte{98, 1, 65, 12}),
		}, nil},
		{"missing start fragment", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{98, 1, 1, 10}),
			lossPacket(11, 100, true, []byte{98, 1, 65, 11}),
		}, nil},
		{"timestamp changes before end", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{98, 1, 129, 10}),
			lossPacket(11, 200, true, []byte{98, 1, 65, 11}),
		}, nil},
		{"new start before end", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{98, 1, 129, 10}),
			lossPacket(11, 100, false, []byte{98, 1, 129, 11}),
			lossPacket(12, 100, true, []byte{98, 1, 65, 12}),
		}, [][]byte{lossNALs([]byte{2, 1, 11, 12})}},
		{"whole frame lost between access units", []*rtp.Packet{
			lossPacket(10, 100, true, []byte{2, 1, 10}),
			lossPacket(12, 300, true, []byte{2, 1, 12}),
		}, [][]byte{lossNALs([]byte{2, 1, 10}), lossNALs([]byte{2, 1, 12})}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got [][]byte
			depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { got = append(got, bytes.Clone(p.Payload)) })
			for _, p := range tt.packets {
				depay(p)
			}
			require.Equal(t, tt.want, got, "only the damaged access unit may be dropped")

			// the next access unit is forwarded right away, no waiting for a keyframe
			seq := tt.packets[len(tt.packets)-1].SequenceNumber + 1
			depay(lossPacket(seq, 400, true, []byte{2, 1, 13}))
			require.Equal(t, append(tt.want, lossNALs([]byte{2, 1, 13})), got)
		})
	}
}

func lossPacket(seq uint16, ts uint32, marker bool, data []byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, Marker: marker}, Payload: data}
}
func lossNALs(nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		b = binary.BigEndian.AppendUint32(b, uint32(len(n)))
		b = append(b, n...)
	}
	return b
}

// Losing any single packet of a fragmented stream costs exactly the frame it
// belonged to, every other frame is forwarded intact.
func TestRTPDepayEveryFragmentLossDropsOneFrame(t *testing.T) {
	var packets []*rtp.Packet
	var expected [][]byte
	for frame := 0; frame < 8; frame++ {
		typ := byte(1)
		if frame%4 == 0 {
			typ = 19
		}
		expected = append(expected, lossNALs([]byte{typ << 1, 1, byte(frame), 10, 11}))
		packets = append(packets,
			lossPacket(uint16(len(packets)), uint32(frame*3000), false, []byte{98, 1, 128 | typ, byte(frame)}),
			lossPacket(uint16(len(packets)+1), uint32(frame*3000), false, []byte{98, 1, typ, 10}),
			lossPacket(uint16(len(packets)+2), uint32(frame*3000), true, []byte{98, 1, 64 | typ, 11}),
		)
	}
	for missing := range packets {
		var got [][]byte
		depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { got = append(got, bytes.Clone(p.Payload)) })
		for i, p := range packets {
			if i != missing {
				depay(p)
			}
		}
		var want [][]byte
		for frame, b := range expected {
			if frame != missing/3 {
				want = append(want, b)
			}
		}
		require.Equal(t, want, got, "missing packet %d", missing)
	}
}

func TestRTPDepayPreservesSDPParameterSets(t *testing.T) {
	var got []byte
	codec := &core.Codec{FmtpLine: "sprop-vps=QAEH;sprop-sps=QgEI;sprop-pps=RAEJ"}
	depay := RTPDepay(codec, func(p *rtp.Packet) { got = bytes.Clone(p.Payload) })
	depay(lossPacket(10, 100, false, []byte{98, 1, 147, 10}))
	depay(lossPacket(11, 100, true, []byte{98, 1, 83, 11}))
	want := lossNALs([]byte{64, 1, 7}, []byte{66, 1, 8}, []byte{68, 1, 9}, []byte{38, 1, 10, 11})
	if !bytes.Equal(got, want) {
		t.Fatalf("parameter sets changed: got %x, want %x", got, want)
	}
}

// Discontinuities that don't lose media must never cost a frame: a sequence
// wrap inside a fragmented unit, a timestamp wrap, a packet too short to carry
// H265 data and a camera restarting its RTP numbering between frames.
func TestRTPDepayDiscontinuityWithoutLossKeepsEveryFrame(t *testing.T) {
	var got [][]byte
	depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { got = append(got, bytes.Clone(p.Payload)) })

	idr := []byte{NALUTypeIFrame << 1, 1, 0xaa}
	p2, p3, p4 := []byte{NALUTypePFrame << 1, 1, 2}, []byte{NALUTypePFrame << 1, 1, 3}, []byte{NALUTypePFrame << 1, 1, 4}

	depay(lossPacket(65534, 0xFFFFFF00, true, idr))
	// fragmented P-frame across the sequence wrap
	depay(lossPacket(65535, 0xFFFFFFF0, false, []byte{NALUTypeFU << 1, 1, 0x80 | NALUTypePFrame, 1}))
	depay(lossPacket(0, 0xFFFFFFF0, true, []byte{NALUTypeFU << 1, 1, 0x40 | NALUTypePFrame, 2}))
	// timestamp wraps between frames
	depay(lossPacket(1, 1000, true, p2))
	// padding only packet
	depay(lossPacket(2, 1500, false, nil))
	depay(lossPacket(3, 2000, true, p3))
	// camera restarts its RTP numbering
	depay(lossPacket(42, 3000, true, p4))

	require.Equal(t, [][]byte{
		lossNALs(idr),
		lossNALs([]byte{NALUTypePFrame << 1, 1, 1, 2}),
		lossNALs(p2),
		lossNALs(p3),
		lossNALs(p4),
	}, got)
}

// Random loss, duplicates, reordering, renumbering and short packets must never
// panic or emit malformed AVCC, ex. a NAL unit whose size was never filled in.
func TestRTPDepayRandomDamageEmitsWellFormedAVCC(t *testing.T) {
	r := rand.New(rand.NewSource(1))

	var packets []*rtp.Packet
	seq, ts := uint16(65000), uint32(0)
	add := func(marker bool, payload []byte) {
		packets = append(packets, lossPacket(seq, ts, marker, payload))
		seq++
	}
	nal := func(typ byte, n int) []byte {
		b := make([]byte, n)
		r.Read(b[2:])
		b[0], b[1] = typ<<1, 1
		return b
	}
	for au := 0; au < 3000; au++ {
		if au%30 == 0 {
			add(false, buildAP(nal(NALUTypeVPS, 20), nal(NALUTypeSPS, 30), nal(NALUTypePPS, 8)))
		}
		if r.Intn(3) == 0 {
			add(r.Intn(4) == 0, nal(NALUTypePrefixSEI, 12))
		}
		typ := byte(NALUTypePFrame)
		if au%30 == 0 {
			typ = NALUTypeIFrame
		}
		switch size := 100 + r.Intn(5000); {
		case size < 600 && r.Intn(3) == 0: // OpenIPC single packet FU
			add(true, append([]byte{NALUTypeFU << 1, 1, 0xC0 | typ}, nal(typ, size)[2:]...))
		case size < 1200:
			add(true, nal(typ, size))
		default:
			body := nal(typ, size)[2:]
			for first := true; len(body) > 0; first = false {
				n := min(1000, len(body))
				fu := typ
				if first {
					fu |= 0x80
				}
				if n == len(body) {
					fu |= 0x40
				}
				add(n == len(body), append([]byte{NALUTypeFU << 1, 1, fu}, body[:n]...))
				body = body[n:]
			}
		}
		ts += 3000
	}

	var damaged []*rtp.Packet
	var offset uint16
	for i := 0; i < len(packets); i++ {
		c := *packets[i]
		c.SequenceNumber += offset
		switch r.Intn(50) {
		case 0: // lost
			continue
		case 5: // lost at the camera, the sequence stays continuous
			offset--
			continue
		case 1: // duplicated
			dup := c
			damaged = append(damaged, &dup)
		case 2: // camera restarts its numbering from here on
			offset += uint16(r.Intn(60000))
			c.SequenceNumber = packets[i].SequenceNumber + offset
		case 3: // too short to carry data
			c.Payload = c.Payload[:r.Intn(3)]
		case 4: // swapped with the next packet
			if i+1 < len(packets) {
				next := *packets[i+1]
				next.SequenceNumber += offset
				damaged = append(damaged, &next, &c)
				i++
				continue
			}
		}
		damaged = append(damaged, &c)
	}

	var frames int
	depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) {
		frames++
		for b := p.Payload; len(b) > 0; {
			require.GreaterOrEqual(t, len(b), 6, "frame %d: truncated NAL unit", frames)
			size := int(binary.BigEndian.Uint32(b))
			require.GreaterOrEqual(t, size, 2, "frame %d: NAL unit without header", frames)
			require.LessOrEqual(t, 4+size, len(b), "frame %d: NAL unit size exceeds frame", frames)
			b = b[4+size:]
		}
	})
	require.NotPanics(t, func() {
		for _, p := range damaged {
			depay(p)
		}
	})
	require.Greater(t, frames, 2000, "damage must cost single frames, not whole GOPs")
}

// Fork specific tests.

// A loss inside a fragmented NAL unit followed by single NAL units left a stale
// nuStart behind, and the end fragment of the broken unit then sliced past the
// buffer: "slice bounds out of range [404:8]".
func TestRTPDepayStaleFragmentEndDoesNotPanic(t *testing.T) {
	var got [][]byte
	depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { got = append(got, bytes.Clone(p.Payload)) })

	vps := make([]byte, 400)
	vps[0], vps[1] = NALUTypeVPS<<1, 1
	sei := []byte{NALUTypePrefixSEI << 1, 1, 5}

	require.NotPanics(t, func() {
		depay(lossPacket(1, 100, false, vps))
		depay(lossPacket(2, 100, false, []byte{NALUTypeFU << 1, 1, 0x80 | NALUTypeIFrame, 0xaa}))
		// seq 3 lost
		depay(lossPacket(4, 100, false, sei))
		depay(lossPacket(5, 100, false, sei))
		depay(lossPacket(6, 100, true, []byte{NALUTypeFU << 1, 1, 0x40 | NALUTypeIFrame, 0xbb}))
	})
	require.Empty(t, got, "the damaged access unit must be dropped")

	// the stream goes on with the next access unit, no keyframe needed
	p := []byte{NALUTypePFrame << 1, 1, 0xcc}
	depay(lossPacket(7, 200, true, p))
	require.Equal(t, [][]byte{lossNALs(p)}, got)
}

// An aggregation packet can't continue a fragmented NAL unit, so the end of that
// unit was lost. The AP branch has to drop the unfinished unit like every other
// path, it used to be emitted with a zero NAL unit size.
func TestRTPDepayAggregationPacketInsideFragment(t *testing.T) {
	var got [][]byte
	depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { got = append(got, bytes.Clone(p.Payload)) })

	vps := []byte{NALUTypeVPS << 1, 1, 7}
	p := []byte{NALUTypePFrame << 1, 1, 0xbb}
	depay(lossPacket(1, 100, false, []byte{NALUTypeFU << 1, 1, 0x80 | NALUTypeIFrame, 0xaa}))
	depay(lossPacket(2, 100, false, buildAP(vps)))
	depay(lossPacket(3, 100, true, p))
	require.Equal(t, [][]byte{lossNALs(vps, p)}, got)
}

// A marked SEI (the #244 camera quirk) is dropped before it reaches the buffer,
// but it must still count for sequence continuity. Otherwise the next packet
// looks like a gap and the keyframe with its buffered parameter sets is lost.
func TestRTPDepayMarkedSEIAfterParameterSetsKeepsKeyframe(t *testing.T) {
	var got [][]byte
	depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { got = append(got, bytes.Clone(p.Payload)) })

	vps, sps, pps := []byte{NALUTypeVPS << 1, 1, 7}, []byte{NALUTypeSPS << 1, 1, 8}, []byte{NALUTypePPS << 1, 1, 9}
	idr := []byte{NALUTypeIFrame << 1, 1, 0xaa}

	depay(lossPacket(1, 100, false, buildAP(vps, sps, pps)))
	depay(lossPacket(2, 100, true, []byte{NALUTypePrefixSEI << 1, 1, 5}))
	depay(lossPacket(3, 100, true, idr))

	require.Equal(t, [][]byte{lossNALs(vps, sps, pps, idr)}, got)
}

// A camera that ends every access unit with a marked SEI never lets the marker
// reach the flush, so the buffer would grow without limit. The h264 depay caps
// it, this one has to as well.
func TestRTPDepayCapsBufferWhenMarkerNeverFlushes(t *testing.T) {
	var lastLen int
	depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { lastLen = len(p.Payload) })

	payload := make([]byte, 1200)
	payload[0], payload[1] = NALUTypePFrame<<1, 1
	seq := uint16(0)
	for frame := range 20000 {
		ts := uint32(3000 * frame)
		depay(lossPacket(seq, ts, false, payload))
		seq++
		depay(lossPacket(seq, ts, true, []byte{NALUTypeSuffixSEI << 1, 1, 5}))
		seq++
	}

	// force a flush and measure what had accumulated
	depay(lossPacket(seq, 99999, true, payload))
	require.Less(t, lastLen, 6*1024*1024, "buffer grew past the cap")
}

// The cap must not leave a fragmented unit half-tracked: nuStart would point
// past the shrunken buffer and the fragment end would panic. The cap has to hit
// while the unit is still being assembled, so only FU continues grow the buffer
// (a single NAL unit would drop the fragment on its own before the cap).
func TestRTPDepayBufferCapDoesNotPanicOnFragmentEnd(t *testing.T) {
	var emitted [][]byte
	depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { emitted = append(emitted, bytes.Clone(p.Payload)) })

	single := make([]byte, 60000)
	single[0], single[1] = NALUTypePFrame<<1, 1
	cont := append([]byte{NALUTypeFU << 1, 1, NALUTypeIFrame}, make([]byte, 60000)...)

	seq := uint16(0)
	send := func(marker bool, b []byte) {
		depay(lossPacket(seq, 100, marker, b))
		seq++
	}

	require.NotPanics(t, func() {
		// buffered data first, so nuStart points far into the buffer
		buffered := 0
		for range 20 {
			send(false, single)
			buffered += 4 + len(single)
		}
		send(false, []byte{NALUTypeFU << 1, 1, 0x80 | NALUTypeIFrame, 0xaa})
		buffered += 4 + 2 + 1
		// grow the fragmented unit right past the cap
		for buffered <= 5*1024*1024 {
			send(false, cont)
			buffered += len(cont) - 3
		}
		// this packet triggers the cap and then ends the unit
		send(true, []byte{NALUTypeFU << 1, 1, 0x40 | NALUTypeIFrame, 0xbb})
	})

	for i, f := range emitted {
		for b := f; len(b) > 0; {
			require.GreaterOrEqual(t, len(b), 6, "frame %d: truncated NAL unit", i)
			size := int(binary.BigEndian.Uint32(b))
			require.GreaterOrEqual(t, size, 2, "frame %d: NAL unit without header", i)
			require.LessOrEqual(t, 4+size, len(b), "frame %d: NAL unit size exceeds frame", i)
			b = b[4+size:]
		}
	}
}
