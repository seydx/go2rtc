package mp4

import (
	"encoding/hex"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/iso"
	"github.com/pion/rtp"
)

type Muxer struct {
	index  uint32
	dts    []uint64
	pts    []uint32
	codecs []*core.Codec
	sizes  [][2]uint16
}

func (m *Muxer) AddTrack(codec *core.Codec) {
	m.AddVideoTrack(codec, 0, 0)
}

// AddVideoTrack adds a track with its picture size, for codecs whose
// parameters carry none (JPEG)
func (m *Muxer) AddVideoTrack(codec *core.Codec, width, height uint16) {
	m.dts = append(m.dts, 0)
	m.pts = append(m.pts, 0)
	m.codecs = append(m.codecs, codec)
	m.sizes = append(m.sizes, [2]uint16{width, height})
}

func (m *Muxer) GetInit() ([]byte, error) {
	mv := iso.NewMovie(1024)
	mv.WriteFileType()

	mv.StartAtom(iso.Moov)
	mv.WriteMovieHeader()

	for i, codec := range m.codecs {
		switch codec.Name {
		case core.CodecH264:
			sps, pps := h264.GetParameterSet(codec.Fmtp())
			// some dummy SPS and PPS not a problem for MP4, but problem for HLS :(
			if len(sps) == 0 {
				sps = []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}
			}
			if len(pps) == 0 {
				pps = []byte{0x68, 0xce, 0x38, 0x80}
			}

			var width, height uint16
			if s := h264.DecodeSPS(sps); s != nil {
				width = s.Width()
				height = s.Height()
			} else {
				width = 1920
				height = 1080
			}

			mv.WriteVideoTrack(
				uint32(i+1), codec.Name, codec.ClockRate, width, height, h264.EncodeConfig(sps, pps),
			)

		case core.CodecH265:
			vps, sps, pps := h265.GetParameterSet(codec.Fmtp())
			// some dummy SPS and PPS not a problem
			if len(vps) == 0 {
				vps = []byte{0x40, 0x01, 0x0c, 0x01, 0xff, 0xff, 0x01, 0x40, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x99, 0xac, 0x09}
			}
			if len(sps) == 0 {
				sps = []byte{0x42, 0x01, 0x01, 0x01, 0x40, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x99, 0xa0, 0x01, 0x40, 0x20, 0x05, 0xa1, 0xfe, 0x5a, 0xee, 0x46, 0xc1, 0xae, 0x55, 0x04}
			}
			if len(pps) == 0 {
				pps = []byte{0x44, 0x01, 0xc0, 0x73, 0xc0, 0x4c, 0x90}
			}

			var width, height uint16
			if s := h265.DecodeSPS(sps); s != nil {
				width = s.Width()
				height = s.Height()
			} else {
				width = 1920
				height = 1080
			}

			mv.WriteVideoTrack(
				uint32(i+1), codec.Name, codec.ClockRate, width, height, h265.EncodeConfig(vps, sps, pps),
			)

		case core.CodecJPEG:
			mv.WriteVideoTrack(
				uint32(i+1), codec.Name, codec.ClockRate, m.sizes[i][0], m.sizes[i][1], nil,
			)

		case core.CodecAAC:
			s := core.Between(codec.Fmtp(), "config=", ";")
			b, err := hex.DecodeString(s)
			if err != nil {
				return nil, err
			}

			mv.WriteAudioTrack(
				uint32(i+1), codec.Name, codec.ClockRate, uint16(codec.Channels), b,
			)

		case core.CodecOpus, core.CodecMP3, core.CodecPCMA, core.CodecPCMU, core.CodecFLAC:
			mv.WriteAudioTrack(
				uint32(i+1), codec.Name, codec.ClockRate, uint16(codec.Channels), nil,
			)
		}
	}

	mv.StartAtom(iso.MoovMvex)
	for i := range m.codecs {
		mv.WriteTrackExtend(uint32(i + 1))
	}
	mv.EndAtom() // MVEX

	mv.EndAtom() // MOOV

	return mv.Bytes(), nil
}

func (m *Muxer) Reset() {
	m.index = 0
	for i := range m.dts {
		m.dts[i] = 0
		m.pts[i] = 0
	}
}

func (m *Muxer) GetPayload(trackID byte, packet *rtp.Packet) []byte {
	codec := m.codecs[trackID]

	duration := packet.Timestamp - m.pts[trackID]
	m.pts[trackID] = packet.Timestamp

	// longer is a jump of the timestamps, not the interval of two samples
	maxDuration := codec.ClockRate

	switch codec.Name {
	case core.CodecJPEG:
		// cameras may send a picture every few seconds, the first one has no interval
		maxDuration = 10 * codec.ClockRate
		if m.dts[trackID] == 0 {
			duration = 0
		}
	case core.CodecAAC:
		duration = 1024 // important for Apple Finder and QuickTime
	}

	// minumum duration important for MSE in Apple Safari
	if duration == 0 || duration > maxDuration {
		duration = codec.ClockRate/1000 + 1
		m.pts[trackID] += duration
	}

	return m.GetSample(trackID, packet, duration)
}

// GetSample writes a sample of a known duration, as a file knows when the
// next one starts. GetPayload takes the interval from the previous sample.
func (m *Muxer) GetSample(trackID byte, packet *rtp.Packet, duration uint32) []byte {
	codec := m.codecs[trackID]

	m.index++

	// flags important for Apple Finder video preview
	var flags uint32

	switch codec.Name {
	case core.CodecH264:
		if h264.IsKeyframe(packet.Payload) {
			flags = iso.SampleVideoIFrame
		} else {
			flags = iso.SampleVideoNonIFrame
		}
	case core.CodecH265:
		if h265.IsKeyframe(packet.Payload) {
			flags = iso.SampleVideoIFrame
		} else {
			flags = iso.SampleVideoNonIFrame
		}
	case core.CodecJPEG:
		flags = iso.SampleVideoIFrame
	case core.CodecAAC:
		flags = iso.SampleAudio // not important?
	default:
		flags = iso.SampleAudio // important for FLAC on Android Telegram
	}

	size := len(packet.Payload)

	mv := iso.NewMovie(1024 + size)
	mv.WriteMovieFragment(
		// ExtensionProfile - wrong place for CTS (supported by mpegts.Demuxer)
		m.index, uint32(trackID+1), duration, uint32(size), flags, m.dts[trackID], uint32(packet.ExtensionProfile),
	)
	mv.WriteData(packet.Payload)

	//log.Printf("[MP4] idx:%3d trk:%d dts:%6d cts:%4d dur:%5d time:%10d len:%5d", m.index, trackID+1, m.dts[trackID], packet.SSRC, duration, packet.Timestamp, len(packet.Payload))

	m.dts[trackID] += uint64(duration)

	return mv.Bytes()
}
