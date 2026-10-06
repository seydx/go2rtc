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

// The camera comes first for every consumer, so a converted copy of the codec
// it already sends reaches nobody and isn't worth encoding.
func TestTrimOutputsTheCameraAlreadySends(t *testing.T) {
	const companion = "ffmpeg:cui_front_door#cameraui#audio=pcma#audio=opus#audio=aac"

	for _, tc := range []struct {
		camera *core.Codec
		want   string
	}{
		{&core.Codec{Name: core.CodecAAC, ClockRate: 16000}, "ffmpeg:cui_front_door#cameraui#audio=pcma#audio=opus"},
		{&core.Codec{Name: core.CodecAAC, ClockRate: 48000, Channels: 2}, "ffmpeg:cui_front_door#cameraui#audio=pcma#audio=opus"},
		{&core.Codec{Name: core.CodecPCMA, ClockRate: 8000}, "ffmpeg:cui_front_door#cameraui#audio=opus#audio=aac"},
		{&core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2}, "ffmpeg:cui_front_door#cameraui#audio=pcma#audio=aac"},
		{&core.Codec{Name: core.CodecPCMA, ClockRate: 16000}, companion},
		{&core.Codec{Name: core.CodecPCMU, ClockRate: 8000}, companion},
	} {
		t.Run(tc.camera.String(), func(t *testing.T) {
			got := trimOutputs(companion, []*core.Codec{tc.camera})
			require.Equal(t, tc.want, got)

			args := parseArgs(got[len("ffmpeg:"):])
			require.Equal(t, defaults["output"], args.Output, "the outputs still go out over RTSP")
		})
	}

	aac := []*core.Codec{{Name: core.CodecAAC, ClockRate: 16000}}
	require.Equal(t, "ffmpeg:cam#audio=opus#audio=aac", trimOutputs("ffmpeg:cam#audio=opus#audio=aac", aac),
		"a single output would switch ffmpeg from RTSP to a pipe format")
	require.Equal(t, "ffmpeg:cam#audio=pcma#audio=opus#audio=aac/16000",
		trimOutputs("ffmpeg:cam#audio=pcma#audio=opus#audio=aac/16000", []*core.Codec{{Name: core.CodecAAC, ClockRate: 48000}}))
	require.Equal(t, "ffmpeg:cam", trimOutputs("ffmpeg:cam", aac))
}
