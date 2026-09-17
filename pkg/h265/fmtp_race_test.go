package h265

import (
	"encoding/base64"
	"sync/atomic"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

// parameter sets of an Amcrest IP camera (sprop-vps/sps/pps of its SDP)
func testParameterSets() (vps, sps, pps []byte) {
	vps, _ = base64.StdEncoding.DecodeString("QAEMAf//AUAAAAMAAAMAAAMAAAMAmawJ")
	sps, _ = base64.StdEncoding.DecodeString("QgEBAUAAAAMAAAMAAAMAAAMAmaABQCAFof5a7kbBrlUE")
	pps, _ = base64.StdEncoding.DecodeString("RAHAc8BMkA==")
	return
}

func avccNAL(nal []byte) []byte {
	n := len(nal)
	return append([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, nal...)
}

// A receiver's codec is shared by every consumer of its track: each consumer's
// depacketizer updates FmtpLine from its first keyframe while new consumers
// clone the same codec. Meaningful under -race.
func TestRepairAVCCFmtpUpdateIsConcurrencySafe(t *testing.T) {
	codec := &core.Codec{Name: core.CodecH265, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	vps, sps, pps := testParameterSets()
	idr := []byte{0x26, 0x01, 0xAF, 0x08, 0x40}

	var keyframe []byte
	for _, nal := range [][]byte{vps, sps, pps, idr} {
		keyframe = append(keyframe, avccNAL(nal)...)
	}

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			_ = codec.Clone()
		}
	}()

	for range 100 {
		depay := RepairAVCC(codec, func(*rtp.Packet) {}) // a new consumer
		depay(&rtp.Packet{Payload: append([]byte(nil), keyframe...)})
	}

	stop.Store(true)
	<-done
}
