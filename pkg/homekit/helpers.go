package homekit

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/hap/camera"
)

var videoCodecs = [...]string{core.CodecH264}
var videoProfiles = [...]string{"4200", "4D00", "6400"}
var videoLevels = [...]string{"1F", "20", "28"}

func videoToMedia(codecs []camera.VideoCodecConfiguration) *core.Media {
	media := &core.Media{
		Kind: core.KindVideo, Direction: core.DirectionRecvonly,
	}

	for _, codec := range codecs {
		for _, param := range codec.CodecParams {
			// get best profile and level; clamp to table bounds
			profileID := core.Max(param.ProfileID)
			if int(profileID) >= len(videoProfiles) {
				profileID = byte(len(videoProfiles) - 1)
			}
			level := core.Max(param.Level)
			if int(level) >= len(videoLevels) {
				level = byte(len(videoLevels) - 1)
			}
			profile := videoProfiles[profileID] + videoLevels[level]
			mediaCodec := &core.Codec{
				Name:      videoCodecs[codec.CodecType],
				ClockRate: 90000,
				FmtpLine:  "profile-level-id=" + profile,
			}
			media.Codecs = append(media.Codecs, mediaCodec)
		}
	}

	return media
}

var audioCodecs = [...]string{core.CodecPCMU, core.CodecPCMA, core.CodecELD, core.CodecOpus}
var audioSampleRates = [...]uint32{8000, 16000, 24000}

func audioToMedia(codecs []camera.AudioCodecConfiguration) *core.Media {
	media := &core.Media{
		Kind: core.KindAudio, Direction: core.DirectionRecvonly,
	}

	for _, codec := range codecs {
		for _, param := range codec.CodecParams {
			for _, sampleRate := range param.SampleRate {
				mediaCodec := &core.Codec{
					Name:      audioCodecs[codec.CodecType],
					ClockRate: audioSampleRates[sampleRate],
					Channels:  param.Channels,
				}

				if mediaCodec.Name == core.CodecELD {
					// only this version works with FFmpeg
					conf := aac.EncodeConfig(aac.TypeAACELD, 24000, 1, true)
					mediaCodec.FmtpLine = aac.FMTP + hex.EncodeToString(conf)
				}

				media.Codecs = append(media.Codecs, mediaCodec)
			}
		}
	}

	return media
}

func trackToVideo(track *core.Receiver, video0 *camera.VideoCodecConfiguration, maxWidth, maxHeight int) *camera.VideoCodecConfiguration {
	profileID := video0.CodecParams[0].ProfileID[0]
	level := video0.CodecParams[0].Level[0]
	var attrs camera.VideoCodecAttributes

	if track != nil {
		profile := h264.GetProfileLevelID(track.Codec.FmtpLine)

		for i, s := range videoProfiles {
			if s == profile[:4] {
				profileID = byte(i)
				break
			}
		}

		for i, s := range videoLevels {
			if s == profile[4:] {
				level = byte(i)
				break
			}
		}

		for _, s := range video0.VideoAttrs {
			if (maxWidth > 0 && int(s.Width) > maxWidth) || (maxHeight > 0 && int(s.Height) > maxHeight) {
				continue
			}
			// compare by area: with `Width > || Height >` a 1024x768 entry
			// would beat 1280x720, and some cameras (Logi Circle 2) reject
			// a 4:3 mode in SelectedStreamConfiguration with -70410
			if int(s.Width)*int(s.Height) > int(attrs.Width)*int(attrs.Height) {
				attrs = s
			}
		}
	}

	return &camera.VideoCodecConfiguration{
		CodecType: video0.CodecType,
		CodecParams: []camera.VideoCodecParameters{
			{
				ProfileID: []byte{profileID},
				Level:     []byte{level},
			},
		},
		VideoAttrs: []camera.VideoCodecAttributes{attrs},
	}
}

func trackToAudio(track *core.Receiver, audio0 *camera.AudioCodecConfiguration) *camera.AudioCodecConfiguration {
	codecType := audio0.CodecType
	channels := audio0.CodecParams[0].Channels
	sampleRate := audio0.CodecParams[0].SampleRate[0]

	if track != nil {
		channels = uint8(track.Codec.Channels)

		for i, s := range audioCodecs {
			if s == track.Codec.Name {
				codecType = byte(i)
				break
			}
		}

		for i, s := range audioSampleRates {
			if s == track.Codec.ClockRate {
				sampleRate = byte(i)
				break
			}
		}
	}

	return &camera.AudioCodecConfiguration{
		CodecType: codecType,
		CodecParams: []camera.AudioCodecParameters{
			{
				Channels:   channels,
				SampleRate: []byte{sampleRate},
				RTPTime:    []uint8{20},
			},
		},
	}
}

// NALUTypeSTAPA - single-time aggregation packet (RFC 6184), the form
// accessories use to deliver SPS and PPS ahead of a keyframe
const NALUTypeSTAPA = 24

// collectParameterSets extracts H264 SPS/PPS from an RTP payload. Parameter
// sets are small and arrive either as a single NALU or inside a STAP-A
// aggregate, so fragmented (FU) payloads are not considered.
func collectParameterSets(payload []byte, sps, pps *[]byte) {
	if len(payload) == 0 {
		return
	}

	if payload[0]&0x1F != NALUTypeSTAPA {
		storeParameterSet(payload, sps, pps)
		return
	}

	b := payload[1:]
	for len(b) >= 2 {
		size := int(binary.BigEndian.Uint16(b))
		b = b[2:]
		if size == 0 || size > len(b) {
			return
		}
		storeParameterSet(b[:size], sps, pps)
		b = b[size:]
	}
}

func storeParameterSet(nalu []byte, sps, pps *[]byte) {
	if len(nalu) == 0 {
		return
	}

	switch nalu[0] & 0x1F {
	case h264.NALUTypeSPS:
		if *sps == nil {
			*sps = append([]byte(nil), nalu...)
		}
	case h264.NALUTypePPS:
		if *pps == nil {
			*pps = append([]byte(nil), nalu...)
		}
	}
}

// withParameterSets appends sprop-parameter-sets to an fmtp line. It has to go
// last because h264.GetParameterSet reads up to the next ";" or end of string.
func withParameterSets(fmtp string, sps, pps []byte) string {
	if s, p := h264.GetParameterSet(fmtp); s != nil && p != nil {
		return fmtp
	}

	ps := "sprop-parameter-sets=" +
		base64.StdEncoding.EncodeToString(sps) + "," +
		base64.StdEncoding.EncodeToString(pps)

	if fmtp == "" {
		return ps
	}
	return fmtp + ";" + ps
}
