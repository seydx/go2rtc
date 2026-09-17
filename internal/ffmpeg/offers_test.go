package ffmpeg

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

func TestOfferMediasFromOptions(t *testing.T) {
	medias := offerMedias("ffmpeg:cui_front_door#cameraui#video=h264#audio=pcma#audio=opus#audio=aac#audio=pcmu/16000#audio=copy#noVideo")

	var got []string
	for _, media := range medias {
		require.Equal(t, core.DirectionRecvonly, media.Direction)
		codec := media.Codecs[0]
		got = append(got, media.Kind+" "+codec.String())
	}

	require.Equal(t, []string{
		"video " + (&core.Codec{Name: core.CodecH264, ClockRate: 90000}).String(),
		"audio " + (&core.Codec{Name: core.CodecPCMA, ClockRate: 8000, Channels: 1}).String(),
		"audio " + (&core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2}).String(),
		"audio " + (&core.Codec{Name: core.CodecAAC}).String(),
		"audio " + (&core.Codec{Name: core.CodecPCMU, ClockRate: 16000, Channels: 1}).String(),
	}, got, "copied tracks are unknown until the input opens")
}

func TestOfferMediasWithoutOptions(t *testing.T) {
	require.Nil(t, offerMedias("ffmpeg:rtsp://camera/stream"))
}
