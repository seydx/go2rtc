package ffmpeg

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
)

var (
	sampleRateArg = regexp.MustCompile(`-ar:a (\d+)`)
	channelsArg   = regexp.MustCompile(`-ac:a (\d+)`)
)

// offerMedias lists the medias an ffmpeg source will produce, read from its
// #video= and #audio= options. Copied tracks are left out, their codec is
// only known once the input is open.
func offerMedias(url string) []*core.Media {
	_, options, ok := strings.Cut(url, "#")
	if !ok {
		return nil
	}
	query := streams.ParseQuery(options)

	var medias []*core.Media
	for _, value := range query["video"] {
		if codec := offerVideoCodec(value); codec != nil {
			medias = append(medias, &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}})
		}
	}
	for _, value := range query["audio"] {
		if codec := offerAudioCodec(value); codec != nil {
			medias = append(medias, &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}})
		}
	}
	return medias
}

func offerVideoCodec(value string) *core.Codec {
	name, _, _ := strings.Cut(strings.ToLower(value), "/")
	switch name {
	case "h264":
		return &core.Codec{Name: core.CodecH264, ClockRate: 90000}
	case "h265", "hevc":
		return &core.Codec{Name: core.CodecH265, ClockRate: 90000}
	case "mjpeg":
		return &core.Codec{Name: core.CodecJPEG, ClockRate: 90000}
	}
	return nil
}

func offerAudioCodec(value string) *core.Codec {
	key := strings.ToLower(value)
	name, suffix, _ := strings.Cut(key, "/")

	codec := &core.Codec{}
	switch name {
	case "opus":
		// RTP always describes opus as 48000/2, whatever the encoder runs at
		return &core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2}
	case "pcmu":
		codec.Name = core.CodecPCMU
	case "pcma":
		codec.Name = core.CodecPCMA
	case "aac":
		codec.Name = core.CodecAAC
	case "mp3":
		codec.Name = core.CodecMP3
	case "pcm":
		codec.Name = core.CodecPCM
	case "pcml":
		codec.Name = core.CodecPCML
	default:
		return nil
	}

	if rate, err := strconv.Atoi(suffix); err == nil {
		codec.ClockRate = uint32(rate)
	}

	// aac without a rate keeps the rate and channels of its input
	template := defaults[key]
	if m := sampleRateArg.FindStringSubmatch(template); m != nil && codec.ClockRate == 0 {
		rate, _ := strconv.Atoi(m[1])
		codec.ClockRate = uint32(rate)
	}
	if m := channelsArg.FindStringSubmatch(template); m != nil {
		channels, _ := strconv.Atoi(m[1])
		codec.Channels = uint8(channels)
	}

	return codec
}
