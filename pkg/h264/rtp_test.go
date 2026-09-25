package h264

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// Based on AlexxIT/go2rtc#2491. Unlike upstream, the fork never waits for a
// keyframe after a loss: only the damaged access unit is dropped, so NVRs and
// recorders don't get a gap of a whole GOP.

// fragmentedIDR returns one big AVCC IDR NAL and its FU-A RTP packets.
func fragmentedIDR(t *testing.T) (avcc []byte, packets []*rtp.Packet) {
	t.Helper()

	nalu := make([]byte, 3000)
	nalu[0] = 0x65 // IDR, nal_ref_idc=3
	for i := 1; i < len(nalu); i++ {
		nalu[i] = byte(i)
	}
	avcc = make([]byte, 4+len(nalu))
	binary.BigEndian.PutUint32(avcc, uint32(len(nalu)))
	copy(avcc[4:], nalu)

	payloader := &Payloader{IsAVC: true}
	payloads := payloader.Payload(200, avcc)
	require.Greater(t, len(payloads), 3, "expected the NAL to be fragmented")

	for i, p := range payloads {
		packets = append(packets, &rtp.Packet{
			Header:  rtp.Header{Version: 2, Marker: i == len(payloads)-1, SequenceNumber: uint16(i)},
			Payload: p,
		})
	}
	return
}

func depay(t *testing.T) (core.HandlerFunc, *[][]byte) {
	t.Helper()
	var got [][]byte
	handler := RTPDepay(&core.Codec{Name: core.CodecH264}, func(packet *rtp.Packet) {
		got = append(got, bytes.Clone(packet.Payload))
	})
	return handler, &got
}

// renumber gives packets consecutive sequence numbers starting at first.
func renumber(packets []*rtp.Packet, first uint16) []*rtp.Packet {
	out := make([]*rtp.Packet, len(packets))
	for i, p := range packets {
		c := *p
		c.SequenceNumber = first + uint16(i)
		out[i] = &c
	}
	return out
}

// pFrame is a single-NAL P-frame packet that completes an access unit.
func pFrame(seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, Marker: true, SequenceNumber: seq},
		Payload: []byte{0x41, 0x9a, 0x00, 0x11, 0x22},
	}
}

// A consumer that attaches while an FU-A fragmented NAL is in flight has no
// slice header for it; the tail must be dropped, not passed on as a bogus NAL.
func TestRTPDepayDropsFUATailWithoutStart(t *testing.T) {
	avcc, packets := fragmentedIDR(t)
	handler, got := depay(t)

	// join mid-NAL: skip the fragment carrying the start bit
	for _, p := range packets[2:] {
		handler(p)
	}
	require.Empty(t, *got, "tail fragments without a start bit produced a NAL")

	// the next complete NAL must still come through intact
	for _, p := range packets {
		handler(p)
	}
	require.Len(t, *got, 1)
	require.Equal(t, avcc, (*got)[0])
}

// A start fragment arriving while a previous fragment set never finished
// (lost end bit) must discard the stale partial data.
func TestRTPDepayRestartsOnNewFUAStart(t *testing.T) {
	avcc, packets := fragmentedIDR(t)
	handler, got := depay(t)

	// first NAL loses its tail
	for _, p := range packets[:len(packets)-2] {
		handler(p)
	}
	// second NAL arrives complete
	for _, p := range packets {
		handler(p)
	}
	require.Len(t, *got, 1)
	require.Equal(t, avcc, (*got)[0])
}

// A fragment lost from the middle of an IDR must not produce a keyframe with
// a hole in it; the next complete IDR is passed on.
func TestRTPDepayDropsIDRWithLostFragment(t *testing.T) {
	avcc, packets := fragmentedIDR(t)
	handler, got := depay(t)

	damaged := renumber(packets, 100)
	for i, p := range damaged {
		if i == 2 {
			continue // lost in transit
		}
		handler(p)
	}
	require.Empty(t, *got, "an IDR missing a fragment was passed on")

	for _, p := range renumber(packets, 100+uint16(len(packets))) {
		handler(p)
	}
	require.Len(t, *got, 1)
	require.Equal(t, avcc, (*got)[0])
}

// Fork: upstream drops every P-frame after a loss until the next keyframe. A
// lost packet between access units damages nothing, every frame goes on.
func TestRTPDepayForwardsFramesAfterLoss(t *testing.T) {
	avcc, packets := fragmentedIDR(t)
	handler, got := depay(t)

	n := uint16(len(packets))
	for _, p := range renumber(packets, 0) {
		handler(p)
	}
	handler(pFrame(n))
	require.Len(t, *got, 2, "clean IDR and P-frame should pass")

	handler(pFrame(n + 2)) // n+1 was lost
	handler(pFrame(n + 3))
	require.Len(t, *got, 4, "P-frames after a loss must not wait for a keyframe")
	require.Equal(t, avcc, (*got)[0])
}

// A sequence number wrapping from 65535 to 0 is continuity, not loss.
func TestRTPDepaySequenceRollover(t *testing.T) {
	avcc, packets := fragmentedIDR(t)
	handler, got := depay(t)

	for _, p := range renumber(packets, 65535-uint16(len(packets)/2)) {
		handler(p)
	}
	require.Len(t, *got, 1)
	require.Equal(t, avcc, (*got)[0])
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

// FU-A indicator with nal_ref_idc=3 and the FU header bits
const (
	fuA     = 0x60 | NALUTypeFUA
	fuStart = 0x80
	fuEnd   = 0x40
)

func TestRTPDepayLossDropsOnlyDamagedAccessUnit(t *testing.T) {
	tests := []struct {
		name    string
		packets []*rtp.Packet
		want    [][]byte
	}{
		{"missing middle fragment", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{fuA, fuStart | NALUTypePFrame, 10}),
			lossPacket(12, 100, true, []byte{fuA, fuEnd | NALUTypePFrame, 12}),
		}, nil},
		{"missing start fragment", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{fuA, NALUTypePFrame, 10}),
			lossPacket(11, 100, true, []byte{fuA, fuEnd | NALUTypePFrame, 11}),
		}, nil},
		{"missing start fragment after complete NAL", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{0x41, 10}),
			lossPacket(12, 100, false, []byte{fuA, NALUTypePFrame, 12}),
			lossPacket(13, 100, true, []byte{fuA, fuEnd | NALUTypePFrame, 13}),
		}, nil},
		{"lost at camera, start fragment", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{0x41, 10}),
			lossPacket(11, 100, true, []byte{fuA, fuEnd | NALUTypePFrame, 11}),
		}, nil},
		{"timestamp changes before end", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{fuA, fuStart | NALUTypePFrame, 10}),
			lossPacket(11, 200, true, []byte{fuA, fuEnd | NALUTypePFrame, 11}),
		}, nil},
		{"new start before end", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{fuA, fuStart | NALUTypePFrame, 10}),
			lossPacket(11, 100, false, []byte{fuA, fuStart | NALUTypePFrame, 11}),
			lossPacket(12, 100, true, []byte{fuA, fuEnd | NALUTypePFrame, 12}),
		}, [][]byte{lossNALs([]byte{0x61, 11, 12})}},
		{"single NAL before end", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{fuA, fuStart | NALUTypePFrame, 10}),
			lossPacket(11, 100, true, []byte{0x41, 11}),
		}, [][]byte{lossNALs([]byte{0x41, 11})}},
		{"single NAL before end, then fragments", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{fuA, fuStart | NALUTypePFrame, 10}),
			lossPacket(11, 100, false, []byte{0x41, 11}),
			lossPacket(12, 100, true, []byte{fuA, fuEnd | NALUTypePFrame, 12}),
		}, nil},
		{"complete NAL, then single NAL before end", []*rtp.Packet{
			lossPacket(9, 100, false, []byte{0x41, 9}),
			lossPacket(10, 100, false, []byte{fuA, fuStart | NALUTypePFrame, 10}),
			lossPacket(11, 100, true, []byte{0x41, 11}),
		}, [][]byte{lossNALs([]byte{0x41, 11})}},
		{"aggregation packet before end", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{fuA, fuStart | NALUTypePFrame, 10}),
			lossPacket(11, 100, false, []byte{24, 0, 2, 0x06, 5}),
			lossPacket(12, 100, true, []byte{0x41, 12}),
		}, [][]byte{lossNALs([]byte{0x41, 12})}},
		{"next frame starts before end", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{fuA, fuStart | NALUTypePFrame, 10}),
			lossPacket(11, 200, false, []byte{24, 0, 2, 0x67, 1, 0, 2, 0x68, 2}),
			lossPacket(12, 200, true, []byte{0x65, 12}),
		}, [][]byte{lossNALs([]byte{0x67, 1}, []byte{0x68, 2}, []byte{0x65, 12})}},
		{"whole frame lost between access units", []*rtp.Packet{
			lossPacket(10, 100, true, []byte{0x41, 10}),
			lossPacket(12, 300, true, []byte{0x41, 12}),
		}, [][]byte{lossNALs([]byte{0x41, 10}), lossNALs([]byte{0x41, 12})}},
		{"marker packet lost", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{0x41, 10}),
			lossPacket(12, 200, true, []byte{0x41, 12}),
		}, [][]byte{lossNALs([]byte{0x41, 12})}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, got := depay(t)
			for _, p := range tt.packets {
				handler(p)
			}
			require.Equal(t, tt.want, *got, "only the damaged access unit may be dropped")

			// the next access unit is forwarded right away, no waiting for a keyframe
			seq := tt.packets[len(tt.packets)-1].SequenceNumber + 1
			handler(lossPacket(seq, 400, true, []byte{0x41, 13}))
			require.Equal(t, append(tt.want, lossNALs([]byte{0x41, 13})), *got)
		})
	}
}

// Losing any single packet of a fragmented stream costs exactly the frame it
// belonged to, every other frame is forwarded intact.
func TestRTPDepayEveryFragmentLossDropsOneFrame(t *testing.T) {
	var packets []*rtp.Packet
	var expected [][]byte
	for frame := 0; frame < 8; frame++ {
		typ := byte(NALUTypePFrame)
		if frame%4 == 0 {
			typ = NALUTypeIFrame
		}
		expected = append(expected, lossNALs([]byte{0x60 | typ, byte(frame), 10, 11}))
		packets = append(packets,
			lossPacket(uint16(len(packets)), uint32(frame*3000), false, []byte{fuA, fuStart | typ, byte(frame)}),
			lossPacket(uint16(len(packets)+1), uint32(frame*3000), false, []byte{fuA, typ, 10}),
			lossPacket(uint16(len(packets)+2), uint32(frame*3000), true, []byte{fuA, fuEnd | typ, 11}),
		)
	}
	for missing := range packets {
		handler, got := depay(t)
		for i, p := range packets {
			if i != missing {
				handler(p)
			}
		}
		var want [][]byte
		for frame, b := range expected {
			if frame != missing/3 {
				want = append(want, b)
			}
		}
		require.Equal(t, want, *got, "missing packet %d", missing)
	}
}

// Discontinuities that don't lose media must never cost a frame: a sequence
// wrap inside a fragmented NAL unit, a timestamp wrap, a padding only packet,
// the marked parameter sets and SEI of some cameras and a camera restarting its
// RTP numbering between frames.
func TestRTPDepayDiscontinuityWithoutLossKeepsEveryFrame(t *testing.T) {
	handler, got := depay(t)

	sps, pps := []byte{0x67, 0x42, 0x00, 0x1f}, []byte{0x68, 0xce}
	idr := []byte{0x65, 0xaa}
	p2, p3, p4 := []byte{0x41, 2}, []byte{0x41, 3}, []byte{0x41, 4}

	// TP-Link Tapo TC70 and RtspServer (#244): marked SPS, marked PPS, marked SEI
	handler(lossPacket(65532, 0xFFFFFF00, true, sps))
	handler(lossPacket(65533, 0xFFFFFF00, true, pps))
	handler(lossPacket(65534, 0xFFFFFF00, true, []byte{0x06, 5, 1, 0x80}))
	handler(lossPacket(65535, 0xFFFFFF00, true, idr))
	// fragmented P-frame across the sequence wrap
	handler(lossPacket(0, 0xFFFFFFF0, false, []byte{fuA, fuStart | NALUTypePFrame, 1}))
	handler(lossPacket(1, 0xFFFFFFF0, true, []byte{fuA, fuEnd | NALUTypePFrame, 2}))
	// timestamp wraps between frames
	handler(lossPacket(2, 1000, true, p2))
	// padding only packet
	handler(lossPacket(3, 1500, false, nil))
	handler(lossPacket(4, 2000, true, p3))
	// camera restarts its RTP numbering
	handler(lossPacket(42, 3000, true, p4))

	require.Equal(t, [][]byte{
		lossNALs(sps, pps, idr),
		lossNALs([]byte{0x61, 1, 2}),
		lossNALs(p2),
		lossNALs(p3),
		lossNALs(p4),
	}, *got)
}

// rtpStream generates a camera like H264 RTP stream: parameter sets as STAP-A
// or single (sometimes marked) packets, SEI, and one or two slices per frame,
// big ones FU-A fragmented. It returns the access units the fork is expected
// to emit and the set of all NAL units in the stream.
func rtpStream(r *rand.Rand, count int) (packets []*rtp.Packet, aus [][]byte, nals map[string]bool) {
	seq, ts := uint16(65000), uint32(0)
	add := func(marker bool, payload []byte) {
		packets = append(packets, lossPacket(seq, ts, marker, payload))
		seq++
	}
	nals = map[string]bool{}
	var au []byte
	nal := func(header byte, n int) []byte {
		b := make([]byte, n)
		r.Read(b[1:])
		b[0] = header
		nals[string(b)] = true
		au = append(au, lossNALs(b)...)
		return b
	}
	for i := 0; i < count; i++ {
		au = nil
		if i%30 == 0 {
			sps, pps := nal(0x67, 20), nal(0x68, 6)
			if r.Intn(2) == 0 {
				add(false, append(append(append([]byte{24, 0, 20}, sps...), 0, 6), pps...))
			} else {
				add(r.Intn(2) == 0, sps) // Tapo sends them marked
				add(false, pps)
			}
		}
		if r.Intn(3) == 0 {
			sei := nal(0x06, 12)
			if i%30 != 0 {
				au = nil // a leading SEI is skipped, it breaks ffmpeg transcoding
			}
			add(false, sei)
		}
		header := byte(0x41)
		if i%30 == 0 {
			header = 0x65
		}
		for slices := 1 + r.Intn(2); slices > 0; slices-- {
			last := slices == 1
			switch size := 100 + r.Intn(5000); {
			case size < 1200:
				add(last, nal(header, size))
			default:
				body := nal(header, size)[1:]
				for first := true; len(body) > 0; first = false {
					n := min(1000, len(body))
					fu := header & 0x1F
					if first {
						fu |= fuStart
					}
					if n == len(body) {
						fu |= fuEnd
					}
					add(last && n == len(body), append([]byte{header&0x60 | NALUTypeFUA, fu}, body[:n]...))
					body = body[n:]
				}
			}
		}
		aus = append(aus, au)
		ts += 3000
	}
	return
}

// Every access unit of an intact stream is forwarded unchanged, the loss
// tracking must not touch a healthy stream.
func TestRTPDepayIntactStream(t *testing.T) {
	packets, want, _ := rtpStream(rand.New(rand.NewSource(1)), 1000)
	handler, got := depay(t)
	for _, p := range packets {
		handler(p)
	}
	require.Equal(t, want, *got)
}

// Random damage must never panic or emit malformed AVCC. Damage that is visible
// in the sequence numbers (loss, duplicates, reordering, renumbering) must
// also never emit a NAL unit that isn't one of the originals, ex. an IDR with
// a hole or the stale head of a NAL unit whose end was lost.
func TestRTPDepayRandomDamageEmitsWellFormedAVCC(t *testing.T) {
	for _, visible := range []bool{true, false} {
		r := rand.New(rand.NewSource(1))
		packets, _, nals := rtpStream(r, 3000)

		var damaged []*rtp.Packet
		var offset uint16
		for i := 0; i < len(packets); i++ {
			c := *packets[i]
			c.SequenceNumber += offset
			switch r.Intn(50) {
			case 0: // lost
				continue
			case 1: // duplicated
				dup := c
				damaged = append(damaged, &dup)
			case 2: // camera restarts its numbering from here on
				offset += uint16(r.Intn(60000))
				c.SequenceNumber = packets[i].SequenceNumber + offset
			case 3: // swapped with the next packet
				if i+1 < len(packets) {
					next := *packets[i+1]
					next.SequenceNumber += offset
					damaged = append(damaged, &next, &c)
					i++
					continue
				}
			case 4: // lost at the camera, the sequence stays continuous
				if !visible {
					offset--
					continue
				}
			case 5: // too short to carry data
				if !visible {
					c.Payload = c.Payload[:r.Intn(3)]
				}
			}
			damaged = append(damaged, &c)
		}

		var frames int
		handler := RTPDepay(&core.Codec{Name: core.CodecH264}, func(p *rtp.Packet) {
			frames++
			for b := p.Payload; len(b) > 0; {
				require.GreaterOrEqual(t, len(b), 5, "frame %d: truncated NAL unit", frames)
				size := int(binary.BigEndian.Uint32(b))
				require.GreaterOrEqual(t, size, 1, "frame %d: NAL unit without header", frames)
				require.LessOrEqual(t, 4+size, len(b), "frame %d: NAL unit size exceeds frame", frames)
				if visible {
					require.True(t, nals[string(b[4:4+size])], "frame %d: damaged NAL unit passed on", frames)
				}
				b = b[4+size:]
			}
		})
		require.NotPanics(t, func() {
			for _, p := range damaged {
				handler(p)
			}
		})
		require.Greater(t, frames, 2000, "damage must cost single frames, not whole GOPs")
	}
}
