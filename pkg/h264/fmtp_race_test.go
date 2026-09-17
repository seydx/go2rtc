package h264

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

var (
	testSPS = []byte{0x67, 0x42, 0xC0, 0x1E, 0xD9, 0x03, 0xC5, 0x68}
	testPPS = []byte{0x68, 0xCE, 0x06, 0xE2}
	testIDR = []byte{0x65, 0x88, 0x84, 0x00}
)

func avccNAL(nal []byte) []byte {
	return append([]byte{0, 0, 0, byte(len(nal))}, nal...)
}

// readCodecWhile marshals and clones codec the way the API and attaching
// consumers do, until write returns.
func readCodecWhile(codec *core.Codec, write func()) {
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			_, _ = json.Marshal(codec)
			_ = codec.Clone()
		}
	}()
	write()
	stop.Store(true)
	<-done
}

// A receiver's codec is shared by every consumer of its track, and each
// consumer's depacketizer updates FmtpLine from its first keyframe while the
// API marshals and new consumers clone the same codec. Meaningful under -race.
func TestRepairAVCCFmtpUpdateIsConcurrencySafe(t *testing.T) {
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}

	var keyframe []byte
	keyframe = append(keyframe, avccNAL(testSPS)...)
	keyframe = append(keyframe, avccNAL(testPPS)...)
	keyframe = append(keyframe, avccNAL(testIDR)...)

	readCodecWhile(codec, func() {
		for range 100 {
			depay := RepairAVCC(codec, func(*rtp.Packet) {}) // a new consumer
			depay(&rtp.Packet{Payload: append([]byte(nil), keyframe...)})
		}
	})
}

func TestRTPDepayFmtpUpdateIsConcurrencySafe(t *testing.T) {
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}

	readCodecWhile(codec, func() {
		for range 100 {
			depay := RTPDepay(codec, func(*rtp.Packet) {}) // a new consumer
			depay(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1}, Payload: testSPS})
			depay(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 2}, Payload: testPPS})
			depay(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 3, Marker: true}, Payload: testIDR})
		}
	})
}
