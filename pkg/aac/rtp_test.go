package aac

import (
	"encoding/binary"
	"encoding/hex"
	"math/rand"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestBuggy_RTSP_AAC(t *testing.T) {
	// https: //github.com/AlexxIT/go2rtc/issues/1328
	payload, _ := hex.DecodeString("fff16080431ffc211ad4458aa309a1c0a8761a230502b7c74b2b5499252a010555e32e460128303c8ace4fd3260d654a424f7e7c65eddc96735fc6f1ac0edf94fdefa0e0bd6370da1c07b9c0e77a9d6e86b196a1ac7439dcafadcffcf6d89f60ac67f8884868e931383ad3e40cf5495470d1f606ef6f7624d285b951ebfa0e42641ab98f1371182b237d14f1bd16ad714fa2f1c6a7d23ebde7a0e34a2eca156a608a4caec49d9dca4b6fe2a09e9cdbf762c5b4148a3914abb7959c991228b0837b5988334b9fc18b8fac689b5ca1e4661573bbb8b253a86cae7ec14ace49969a9a76fd571ab6e650764cb59114d61dcedf07ac61b39e4ac66adebfd0d0ab45d518dd3c161049823f150864d977cf0855172ac8482e4b25fe911325d19617558c5405af74aff5492e4599bee53f2dbdf0503730af37078550f84c956b7ee89aae83c154fa2fa6e6792c5ddd5cd5cf6bb96bf055fee7f93bed59ffb039daee5ea7e5593cb194e9091e417c67d8f73026a6a6ae056e808f7c65c03d1b9197d3709ceb63bc7b979f7ba71df5e7c6395d99d6ea229000a6bc16fb4346d6b27d32f5d8d1200736d9366d59c0c9547210813b602473da9c46f9015bbb37594c1dd90cd6a36e96bd5d6a1445ab93c9e65505ec2c722bb4cc27a10600139a48c83594dde145253c386f6627d8c6e5102fe3828a590c709bc87f55b37e97d1ae72b017b09c6bb2c13299817bb45cc67318e10b6822075b97c6a03ec1c0")
	packet := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         true,
			SequenceNumber: 36944,
			Timestamp:      4217191328,
			SSRC:           12892774,
		},
		Payload: payload,
	}

	var size int

	RTPDepay(func(packet *core.Packet) {
		size = len(packet.Payload)
	})(packet)

	require.Equal(t, len(payload), size+ADTSHeaderSize)
}

// Based on AlexxIT/go2rtc#2513: the AU sizes come from the camera, a malformed
// packet must not slice past the payload and take down the whole process.
func TestRTPToADTSIgnoresMalformedAUHeaders(t *testing.T) {
	codec := &core.Codec{FmtpLine: "config=1408"}

	tests := []struct {
		name    string
		payload []byte
	}{
		{
			name:    "short payload",
			payload: []byte{0},
		},
		{
			name:    "truncated AU header",
			payload: []byte{0, 8, 0},
		},
		{
			name:    "AU headers exceed payload",
			payload: []byte{0, 24, 0, 0},
		},
		{
			name: "AU size exceeds payload",
			payload: func() []byte {
				payload := make([]byte, 220)
				payload[1] = 16 // one 16-bit AU header
				payload[2] = 0xFF
				payload[3] = 0xF8 // AU size: 8191 bytes
				return payload
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			handler := RTPToADTS(codec, func(*core.Packet) { calls++ })

			require.NotPanics(t, func() {
				handler(&rtp.Packet{Payload: tt.payload})
			})
			require.Zero(t, calls)
		})
	}
}

// rtpAAC builds an RFC 3640 AAC-hbr payload: AU-headers-length in bits, one
// 16-bit header per AU (13-bit size), then the AUs. sizes may differ from the
// real units to build malformed packets.
func rtpAAC(sizes []int, units ...[]byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(16*len(sizes)))
	for _, size := range sizes {
		b = binary.BigEndian.AppendUint16(b, uint16(size<<3))
	}
	for _, unit := range units {
		b = append(b, unit...)
	}
	return b
}

func adtsFrames(codec *core.Codec, units ...[]byte) []byte {
	var b []byte
	for _, unit := range units {
		hdr := CodecToADTS(codec)
		WriteADTSSize(hdr, ADTSHeaderSize+uint16(len(unit)))
		b = append(append(b, hdr...), unit...)
	}
	return b
}

// Intact packets keep producing exactly the same ADTS stream.
func TestRTPToADTSIntactPacket(t *testing.T) {
	codec := &core.Codec{FmtpLine: "config=1408"}
	au1, au2 := []byte{1, 2, 3, 4, 5}, []byte{6, 7, 8}

	var got []byte
	RTPToADTS(codec, func(p *core.Packet) { got = p.Payload })(&rtp.Packet{Payload: rtpAAC([]int{5, 3}, au1, au2)})
	require.Equal(t, adtsFrames(codec, au1, au2), got)
}

// Only the truncated AU is lost, the complete ones before it still play. An
// empty AU carries no audio and is skipped instead of becoming an empty frame.
func TestRTPToADTSKeepsCompleteUnits(t *testing.T) {
	codec := &core.Codec{FmtpLine: "config=1408"}
	au1, au2 := []byte{1, 2, 3, 4, 5}, []byte{6, 7, 8}

	var got []byte
	handler := RTPToADTS(codec, func(p *core.Packet) { got = p.Payload })

	// the second AU announces 100 bytes but only 3 arrived
	handler(&rtp.Packet{Payload: rtpAAC([]int{5, 100}, au1, au2)})
	require.Equal(t, adtsFrames(codec, au1), got)

	got = nil
	handler(&rtp.Packet{Payload: rtpAAC([]int{0, 5}, au1)})
	require.Equal(t, adtsFrames(codec, au1), got)
}

// Random packets must never panic, and whatever is forwarded is a sequence of
// complete ADTS frames.
func TestRTPToADTSRandomPayloads(t *testing.T) {
	codec := &core.Codec{FmtpLine: "config=1408"}
	r := rand.New(rand.NewSource(1))

	var got [][]byte
	handler := RTPToADTS(codec, func(p *core.Packet) { got = append(got, p.Payload) })

	require.NotPanics(t, func() {
		for range 20000 {
			payload := make([]byte, r.Intn(64))
			r.Read(payload)
			if len(payload) >= 2 && r.Intn(2) == 0 {
				// plausible AU-headers-length, so the parser gets past the first check
				binary.BigEndian.PutUint16(payload, uint16(16*r.Intn(4)))
			}
			handler(&rtp.Packet{Payload: payload})
		}
	})

	for _, b := range got {
		for len(b) > 0 {
			require.True(t, IsADTS(b), "forwarded data must be ADTS frames")
			size := int(ReadADTSSize(b))
			require.GreaterOrEqual(t, size, ADTSHeaderSize)
			require.LessOrEqual(t, size, len(b))
			b = b[size:]
		}
	}
}
