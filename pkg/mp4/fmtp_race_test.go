package mp4

import (
	"sync/atomic"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
)

// A snapshot consumer attaching hands the track codec to its muxer while
// another consumer's depacketizer updates the same codec's FmtpLine.
// Meaningful under -race.
func TestKeyframeAddTrackWhileFmtpUpdates(t *testing.T) {
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	media.Codecs = []*core.Codec{codec}
	track := core.NewReceiver(media, codec)

	nal := func(b ...byte) []byte { return append([]byte{0, 0, 0, byte(len(b))}, b...) }
	var keyframe []byte
	keyframe = append(keyframe, nal(0x67, 0x42, 0xC0, 0x1E, 0xD9, 0x03, 0xC5, 0x68)...)
	keyframe = append(keyframe, nal(0x68, 0xCE, 0x06, 0xE2)...)
	keyframe = append(keyframe, nal(0x65, 0x88, 0x84, 0x00)...)

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			depay := h264.RepairAVCC(codec, func(*rtp.Packet) {})
			depay(&rtp.Packet{Payload: append([]byte(nil), keyframe...)})
		}
	}()

	for range 100 {
		cons := NewKeyframe(nil)
		_ = cons.AddTrack(media, codec, track)
		_ = cons.Stop()
	}

	stop.Store(true)
	<-done
}
