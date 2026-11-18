package mpegts

import (
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// The camera's video already flows to the consumer while the stream still
// dials the companion that brings the audio, and adds its track only then
// (#2682). A map access that races the video ends the whole process, so this
// one runs with -race.
func TestAddTrackWhileVideoFlows(t *testing.T) {
	cons := NewConsumer()
	t.Cleanup(func() { _ = cons.Stop() })

	h265 := &core.Codec{Name: core.CodecH265, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	video := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{h265}}
	camera := core.NewReceiver(video, h265)
	require.NoError(t, cons.AddTrack(video, h265, camera))

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for ts := uint32(0); ; ts += 3000 {
			select {
			case <-done:
				return
			default:
			}
			camera.Input(&rtp.Packet{
				Header:  rtp.Header{Timestamp: ts},
				Payload: []byte{0, 0, 0, 3, 0x26, 0x01, 0xaf},
			})
			time.Sleep(time.Millisecond)
		}
	}()

	time.Sleep(20 * time.Millisecond)

	aac := &core.Codec{Name: core.CodecAAC, ClockRate: 16000, PayloadType: core.PayloadTypeRAW}
	audio := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{aac}}
	require.NoError(t, cons.AddTrack(audio, aac, core.NewReceiver(audio, aac)))

	time.Sleep(20 * time.Millisecond)
	close(done)
	<-stopped

	header := cons.muxer.GetHeader()
	require.Len(t, header, 2*PacketSize, "PAT and PMT")
	require.Equal(t, []byte{StreamTypeH265}, header[PacketSize+17:PacketSize+18], "the video comes first in the PMT")
	require.Equal(t, []byte{StreamTypeAAC}, header[PacketSize+22:PacketSize+23], "the audio after it")
}
