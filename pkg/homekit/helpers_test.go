package homekit

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/hap"
	"github.com/AlexxIT/go2rtc/pkg/hap/camera"
	"github.com/stretchr/testify/require"
)

func tlv8Character(t *testing.T, typ string, v any) *hap.Character {
	t.Helper()
	char := &hap.Character{Type: typ, Format: hap.FormatTLV8}
	require.NoError(t, char.Write(v))
	return char
}

// accessory has the configurations in one stream service, or each in a
// service of its own
func accessory(t *testing.T, apart bool, video []camera.VideoCodecConfiguration, audio []camera.AudioCodecConfiguration) *hap.Accessory {
	t.Helper()
	charVideo := tlv8Character(t, camera.TypeSupportedVideoStreamConfiguration, &camera.SupportedVideoStreamConfiguration{Codecs: video})
	charAudio := tlv8Character(t, camera.TypeSupportedAudioStreamConfiguration, &camera.SupportedAudioStreamConfiguration{Codecs: audio})
	if apart {
		return &hap.Accessory{Services: []*hap.Service{{Characters: []*hap.Character{charVideo}}, {Characters: []*hap.Character{charAudio}}}}
	}
	return &hap.Accessory{Services: []*hap.Service{{Characters: []*hap.Character{charVideo, charAudio}}}}
}

func h264Config(params ...camera.VideoCodecParameters) camera.VideoCodecConfiguration {
	return camera.VideoCodecConfiguration{
		CodecType:   camera.VideoCodecTypeH264,
		CodecParams: params,
		VideoAttrs:  []camera.VideoCodecAttributes{{Width: 1920, Height: 1080, Framerate: 30}},
	}
}

func opusConfig(params ...camera.AudioCodecParameters) camera.AudioCodecConfiguration {
	return camera.AudioCodecConfiguration{CodecType: camera.AudioCodecTypeOpus, CodecParams: params}
}

var (
	mainLevel40 = camera.VideoCodecParameters{ProfileID: []byte{camera.VideoCodecProfileMain}, Level: []byte{camera.VideoCodecLevel40}}
	mono16k     = camera.AudioCodecParameters{Channels: 1, SampleRate: []byte{camera.AudioCodecSampleRate16Khz}}
)

// A camera offers what HAP knows: H265, AMR, more sample rates, or leaves a
// list empty. Anything of it took the whole process down when the stream was
// read or started. What can't be streamed is left out, the rest streams.
func TestReadStreamConfigKeepsWhatStreams(t *testing.T) {
	h265 := h264Config(mainLevel40)
	h265.CodecType = 1
	amr := opusConfig(mono16k)
	amr.CodecType = camera.AudioCodecTypeAMR

	for _, tc := range []struct {
		name  string
		video []camera.VideoCodecConfiguration
		audio []camera.AudioCodecConfiguration
	}{
		{"h265 before h264", []camera.VideoCodecConfiguration{h265, h264Config(mainLevel40)}, []camera.AudioCodecConfiguration{opusConfig(mono16k)}},
		{"no profile", []camera.VideoCodecConfiguration{h264Config(camera.VideoCodecParameters{Level: []byte{camera.VideoCodecLevel40}}, mainLevel40)}, []camera.AudioCodecConfiguration{opusConfig(mono16k)}},
		{"no level", []camera.VideoCodecConfiguration{h264Config(camera.VideoCodecParameters{ProfileID: []byte{camera.VideoCodecProfileMain}}, mainLevel40)}, []camera.AudioCodecConfiguration{opusConfig(mono16k)}},
		{"amr before opus", []camera.VideoCodecConfiguration{h264Config(mainLevel40)}, []camera.AudioCodecConfiguration{amr, opusConfig(mono16k)}},
		{"unknown sample rate", []camera.VideoCodecConfiguration{h264Config(mainLevel40)}, []camera.AudioCodecConfiguration{opusConfig(camera.AudioCodecParameters{Channels: 1, SampleRate: []byte{5, camera.AudioCodecSampleRate16Khz}})}},
		{"no sample rate", []camera.VideoCodecConfiguration{h264Config(mainLevel40)}, []camera.AudioCodecConfiguration{opusConfig(camera.AudioCodecParameters{Channels: 1}, mono16k)}},
	} {
		for _, apart := range []bool{false, true} {
			name := tc.name
			if apart {
				name += " apart"
			}
			t.Run(name, func(t *testing.T) {
				c := &Client{}
				require.True(t, c.readStreamConfig(accessory(t, apart, tc.video, tc.audio)))

				// GetMedias
				video := videoToMedia(c.videoConfig.Codecs)
				require.Len(t, video.Codecs, 1)
				require.Equal(t, core.CodecH264, video.Codecs[0].Name)
				require.Equal(t, "profile-level-id=4D0028", video.Codecs[0].FmtpLine)

				audio := audioToMedia(c.audioConfig.Codecs)
				require.Len(t, audio.Codecs, 1)
				require.Equal(t, core.CodecOpus, audio.Codecs[0].Name)
				require.Equal(t, uint32(16000), audio.Codecs[0].ClockRate)

				// Start
				videoTrack := core.NewReceiver(video, video.Codecs[0])
				videoCodec := trackToVideo(videoTrack, &c.videoConfig.Codecs[0], 0, 0)
				require.Equal(t, byte(camera.VideoCodecTypeH264), videoCodec.CodecType)
				require.Equal(t, []byte{camera.VideoCodecProfileMain}, videoCodec.CodecParams[0].ProfileID)
				require.Equal(t, []byte{camera.VideoCodecLevel40}, videoCodec.CodecParams[0].Level)

				for _, audioTrack := range []*core.Receiver{nil, core.NewReceiver(audio, audio.Codecs[0])} {
					audioCodec := trackToAudio(audioTrack, &c.audioConfig.Codecs[0])
					require.Equal(t, byte(camera.AudioCodecTypeOpus), audioCodec.CodecType)
					require.Equal(t, []byte{camera.AudioCodecSampleRate16Khz}, audioCodec.CodecParams[0].SampleRate)
				}
			})
		}
	}
}

// Logitech Circle 2 has a stub stream service, a picture size short, before
// the one that streams: the service with more to offer is read. One with
// nothing to stream is passed over, in any order.
func TestReadStreamConfigPrefersTheRichestService(t *testing.T) {
	stub := h264Config(mainLevel40)
	stub.VideoAttrs = nil
	full := h264Config(mainLevel40)
	full.VideoAttrs = append(full.VideoAttrs, camera.VideoCodecAttributes{Width: 1280, Height: 720, Framerate: 30})
	h265 := h264Config(mainLevel40)
	h265.CodecType = 1
	h265.VideoAttrs = append(h265.VideoAttrs, full.VideoAttrs...)

	service := func(video camera.VideoCodecConfiguration) *hap.Service {
		return accessory(t, false, []camera.VideoCodecConfiguration{video}, []camera.AudioCodecConfiguration{opusConfig(mono16k)}).Services[0]
	}

	for _, services := range [][]*hap.Service{
		{service(stub), service(full)},
		{service(full), service(stub)},
		{service(h265), service(full)},
		{service(full), service(h265)},
		{service(h265), service(stub), service(full)},
	} {
		c := &Client{}
		require.True(t, c.readStreamConfig(&hap.Accessory{Services: services}))
		require.Len(t, c.videoConfig.Codecs, 1)
		require.Equal(t, full, c.videoConfig.Codecs[0])
	}
}

// A camera with nothing to stream gets an error for a stream. Its pictures
// don't need one.
func TestStartRefusesNothingToStream(t *testing.T) {
	h265 := h264Config(mainLevel40)
	h265.CodecType = 1
	amr := opusConfig(mono16k)
	amr.CodecType = camera.AudioCodecTypeAMR

	for _, tc := range []struct {
		name  string
		video []camera.VideoCodecConfiguration
		audio []camera.AudioCodecConfiguration
	}{
		{"no audio", []camera.VideoCodecConfiguration{h264Config(mainLevel40)}, nil},
		{"only h265", []camera.VideoCodecConfiguration{h265}, []camera.AudioCodecConfiguration{opusConfig(mono16k)}},
		{"only amr", []camera.VideoCodecConfiguration{h264Config(mainLevel40)}, []camera.AudioCodecConfiguration{amr}},
		{"no video parameters", []camera.VideoCodecConfiguration{h264Config()}, []camera.AudioCodecConfiguration{opusConfig(mono16k)}},
		{"no audio parameters", []camera.VideoCodecConfiguration{h264Config(mainLevel40)}, []camera.AudioCodecConfiguration{opusConfig()}},
	} {
		for _, apart := range []bool{false, true} {
			name := tc.name
			if apart {
				name += " apart"
			}
			t.Run(name, func(t *testing.T) {
				c := &Client{}
				require.True(t, c.readStreamConfig(accessory(t, apart, tc.video, tc.audio)))
				_ = videoToMedia(c.videoConfig.Codecs)
				_ = audioToMedia(c.audioConfig.Codecs)

				for _, codec := range []*core.Codec{
					{Name: core.CodecH264, ClockRate: 90000},
					{Name: core.CodecOpus, ClockRate: 16000, Channels: 1},
				} {
					media := &core.Media{Kind: codec.Kind(), Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}}
					c.Receivers = []*core.Receiver{core.NewReceiver(media, codec)}
					require.EqualError(t, c.Start(), "homekit: no stream configuration")
				}
			})
		}
	}
}
