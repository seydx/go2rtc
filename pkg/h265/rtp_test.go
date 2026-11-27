package h265

import (
	"encoding/binary"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// buildAP wraps NAL units into an RFC 7798 §4.4.2 Aggregation Packet:
// [PayloadHdr (type=48)] [2-byte size][NALU]*  (no DONL — sprop-max-don-diff=0,
// which is what libavformat's HEVC RTP packetizer emits).
func buildAP(nalus ...[]byte) []byte {
	ap := []byte{NALUTypeAP << 1, 0x01}
	for _, nalu := range nalus {
		ap = binary.BigEndian.AppendUint16(ap, uint16(len(nalu)))
		ap = append(ap, nalu...)
	}
	return ap
}

// TestRTPDepayAggregationPacket reproduces the production failure behind
// black thumbnails / "exit status 183" for HEVC cameras behind exec:ffmpeg
// sources: libavformat bundles VPS+SPS+PPS into one type-48 Aggregation
// Packet, which the depacketizer must split into individual AVCC
// length-prefixed NAL units. Without AP support the whole packet is emitted
// as a single bogus NAL of type 48 — an unspecified non-VCL type that breaks
// every downstream decoder.
func TestRTPDepayAggregationPacket(t *testing.T) {
	vps := []byte{NALUTypeVPS << 1, 0x01, 0xAA, 0xBB, 0xCC}
	sps := []byte{NALUTypeSPS << 1, 0x01, 0x11, 0x22, 0x33, 0x44}
	pps := []byte{NALUTypePPS << 1, 0x01, 0x55}
	idr := []byte{NALUTypeIFrame << 1, 0x01, 0xDE, 0xAD, 0xBE, 0xEF}

	var got []*rtp.Packet
	depay := RTPDepay(&core.Codec{}, func(packet *rtp.Packet) {
		got = append(got, packet)
	})

	// Version: 2 marks a real incoming RTP packet — the fork's RTPDepay passes
	// Version 0 (RTPPacketVersionAVC) packets straight through as already-AVCC.
	depay(&rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: 100, Marker: false},
		Payload: buildAP(vps, sps, pps),
	})
	depay(&rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: 101, Marker: true},
		Payload: idr,
	})

	require.Len(t, got, 1)
	require.Equal(t, []byte{NALUTypeVPS, NALUTypeSPS, NALUTypePPS, NALUTypeIFrame}, Types(got[0].Payload))

	// Each NAL must round-trip byte-identical with its own AVCC length prefix.
	payload := got[0].Payload
	for _, want := range [][]byte{vps, sps, pps, idr} {
		size := int(binary.BigEndian.Uint32(payload))
		require.Equal(t, len(want), size)
		require.Equal(t, want, payload[4:4+size])
		payload = payload[4+size:]
	}
	require.Empty(t, payload)
}

// TestRTPDepayAggregationPacketMalformed verifies the corruption-drop
// convention (same as the FU branch): a truncated AP must clear the buffer
// and emit nothing, and the next intact access unit must still go through.
func TestRTPDepayAggregationPacketMalformed(t *testing.T) {
	var got []*rtp.Packet
	depay := RTPDepay(&core.Codec{}, func(packet *rtp.Packet) {
		got = append(got, packet)
	})

	// Size field claims 200 bytes but only 3 follow.
	malformed := []byte{NALUTypeAP << 1, 0x01, 0x00, 200, 0x40, 0x01, 0xFF}
	depay(&rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: 50, Marker: false},
		Payload: malformed,
	})
	require.Empty(t, got)

	idr := []byte{NALUTypeIFrame << 1, 0x01, 0xDE, 0xAD}
	depay(&rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: 51, Marker: true},
		Payload: idr,
	})

	require.Len(t, got, 1)
	require.Equal(t, []byte{NALUTypeIFrame}, Types(got[0].Payload))
}

// The tests from upstream (#2191). Version 2 marks an RTP packet: one of
// version 0 (RTPPacketVersionAVC) is AVCC already and passes through.

func TestRTPDepayIgnoresFUContinuationWithoutStart(t *testing.T) {
	var output *rtp.Packet
	depay := RTPDepay(&core.Codec{FmtpLine: "sprop-vps=QAE="}, func(packet *rtp.Packet) {
		output = packet
	})

	// End an IDR FU without ending the access unit, leaving the prepended VPS
	// in the buffer. A later orphan continuation must not be treated as active.
	depay(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1}, Payload: []byte{0x62, 1, 0x80 | NALUTypeIFrame, 0xAA}})
	depay(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 2}, Payload: []byte{0x62, 1, 0x40 | NALUTypeIFrame, 0xBB}})
	depay(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 3, Marker: true}, Payload: []byte{0x62, 1, NALUTypeIFrame, 0xCC}})

	require.Nil(t, output)
}

func TestRTPDepayReassemblesFU(t *testing.T) {
	var output *rtp.Packet
	depay := RTPDepay(&core.Codec{}, func(packet *rtp.Packet) {
		output = packet
	})
	fragments := [][]byte{
		[]byte{0x62, 1, 0x80 | NALUTypeIFrame, 0xAA},
		[]byte{0x62, 1, 0x40 | NALUTypeIFrame, 0xBB, 0xCC},
	}
	for i, payload := range fragments {
		packet := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: uint16(10 + i)},
			Payload: payload,
		}
		packet.Marker = i == len(fragments)-1
		depay(packet)
	}

	require.NotNil(t, output)
	require.Equal(t, h264.JoinNALU([]byte{byte(NALUTypeIFrame << 1), 1, 0xAA, 0xBB, 0xCC}), output.Payload)
}

func TestRTPDepayDropsShortFU(t *testing.T) {
	depay := RTPDepay(&core.Codec{}, func(*rtp.Packet) {
		t.Fatal("forwarded a short FU")
	})
	for _, payload := range [][]byte{{0x62, 1, 0x80}, {0x62, 1, 0x40}} {
		require.NotPanics(t, func() {
			depay(&rtp.Packet{Header: rtp.Header{Version: 2, Marker: true}, Payload: payload})
		})
	}
}
