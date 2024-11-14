package mp4

import (
	"encoding/binary"
	"encoding/hex"
	"math"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/iso"
	"github.com/pion/rtp"
)

type Muxer struct {
	index  uint32
    tracks []Track
	codecs []*core.Codec
}

type Track struct {
    dts uint64
    pts uint32
    lastDTS uint64
}

func (m *Muxer) AddTrack(codec *core.Codec) {
    m.codecs = append(m.codecs, codec)
    m.tracks = append(m.tracks, Track{})
}

func (m *Muxer) GetInit() ([]byte, error) {
	mv := iso.NewMovie(1024)
	mv.WriteFileType()

	mv.StartAtom(iso.Moov)
	mv.WriteMovieHeader()

	for i, codec := range m.codecs {
		switch codec.Name {
		case core.CodecH264:
			sps, pps := h264.GetParameterSet(codec.FmtpLine)
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
			vps, sps, pps := h265.GetParameterSet(codec.FmtpLine)
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

		case core.CodecAAC:
			s := core.Between(codec.FmtpLine, "config=", ";")
			b, err := hex.DecodeString(s)
			if err != nil {
				return nil, err
			}

			mv.WriteAudioTrack(
				uint32(i+1), codec.Name, codec.ClockRate, codec.Channels, b,
			)

		case core.CodecOpus, core.CodecMP3, core.CodecPCMA, core.CodecPCMU, core.CodecFLAC:
			mv.WriteAudioTrack(
				uint32(i+1), codec.Name, codec.ClockRate, codec.Channels, nil,
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
    for i := range m.tracks {
        m.tracks[i].dts = 0
        m.tracks[i].pts = 0
    }
}

func (m *Muxer) GetPayload(trackID byte, packet *rtp.Packet) []byte {
    track := &m.tracks[trackID]
	codec := m.codecs[trackID]

	m.index++

	duration := packet.Timestamp - track.pts
    track.pts = packet.Timestamp

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
	case core.CodecAAC:
		duration = 1024         // important for Apple Finder and QuickTime
		flags = iso.SampleAudio // not important?
	default:
		flags = iso.SampleAudio // important for FLAC on Android Telegram
	}

	// minumum duration important for MSE in Apple Safari
	if duration == 0 || duration > codec.ClockRate {
		duration = codec.ClockRate/1000 + 1
        track.pts += duration
	}

	size := len(packet.Payload)

	mv := iso.NewMovie(1024 + size)
	mv.WriteMovieFragment(
		// ExtensionProfile - wrong place for CTS (supported by mpegts.Demuxer)
		m.index, uint32(trackID+1), duration, uint32(size), flags, track.dts, uint32(packet.ExtensionProfile),
	)
	mv.WriteData(packet.Payload)

	//log.Printf("[MP4] idx:%3d trk:%d dts:%6d cts:%4d dur:%5d time:%10d len:%5d", m.index, trackID+1, m.dts[trackID], packet.SSRC, duration, packet.Timestamp, len(packet.Payload))

    track.dts += uint64(duration)

	return mv.Bytes()
}

func (m *Muxer) GetFragmentPayload(audioTrackID byte, videoTrackID byte, fragment *struct {
    video []*rtp.Packet
    audio []*rtp.Packet
}) []byte {
    if len(fragment.video) == 0 {
        return nil
    }

    videoTrack := &m.tracks[videoTrackID]
    audioTrack := &m.tracks[audioTrackID]

    videoCodec := m.codecs[videoTrackID]
    audioCodec := m.codecs[audioTrackID]

    m.index++

    mv := iso.NewMovie(1024 * (len(fragment.video) + len(fragment.audio)))

    moofStart := len(mv.Bytes())

    // Begin 'moof' atom
    mv.StartAtom(iso.Moof)

    // Write 'mfhd' atom
    mv.StartAtom(iso.MoofMfhd)
    mv.Skip(1)                  // version
    mv.Skip(3)                  // flags
    mv.WriteUint32(m.index)     // sequence number
    mv.EndAtom()                // 'mfhd'

    // Save data_offset positions for later update
    type trackInfo struct {
        dataOffsetPosition int
        totalDataSize      int
    }
    var trackInfos []trackInfo

    // Video
    videoSampleDurations, videoSampleSizes, videoSampleFlags, videoBaseMediaDecodeTime, videoDataSize := m.PrepareTrackFragment(videoTrack, videoCodec, fragment.video)

    // log.Printf("[MP4] video dts:%d pts:%d dur:%d size:%d", videoBaseMediaDecodeTime, videoTrack.pts, videoSampleDurations, videoSampleSizes)

    videoDataOffsetPos := mv.WriteTrackFragment(
        videoBaseMediaDecodeTime,
        videoTrackID,
        videoSampleDurations,
        videoSampleSizes,
        videoSampleFlags,
    )
    trackInfos = append(trackInfos, trackInfo{videoDataOffsetPos, videoDataSize})

    // Audio
    if len(fragment.audio) > 0 {
        m.syncAudioVideo(audioTrack, videoTrack, audioCodec, videoCodec)
        audioSampleDurations, audioSampleSizes, audioSampleFlags, audioBaseMediaDecodeTime, audioDataSize := m.PrepareTrackFragment(audioTrack, audioCodec, fragment.audio)

        // log.Printf("[MP4] audio dts:%d pts:%d dur:%d size:%d", audioBaseMediaDecodeTime, audioTrack.pts, audioSampleDurations, audioSampleSizes)

        audioDataOffsetPos := mv.WriteTrackFragment(
            audioBaseMediaDecodeTime,
            audioTrackID,
            audioSampleDurations,
            audioSampleSizes,
            audioSampleFlags,
        )
        trackInfos = append(trackInfos, trackInfo{audioDataOffsetPos, audioDataSize})
    }

    mv.EndAtom() // 'moof'

    // Calculate size of 'moof' atom
    moofSize := len(mv.Bytes()) - moofStart

    // Update data_offset values in trun boxes
    cumulativeDataOffset := 0
    for _, info := range trackInfos {
        calculatedDataOffset := moofSize + 8 + cumulativeDataOffset
        binary.BigEndian.PutUint32(mv.Bytes()[info.dataOffsetPosition:], uint32(calculatedDataOffset))
        cumulativeDataOffset += info.totalDataSize
    }

    // Start 'mdat' atom
    mv.StartAtom(iso.Mdat)

    // Write Video Sample Data
    for _, packet := range fragment.video {
        mv.Write(packet.Payload)
    }

    // Write Audio Sample Data
    for _, packet := range fragment.audio {
        mv.Write(packet.Payload)
    }

    mv.EndAtom() // 'mdat'

    return mv.Bytes()
}

func (m *Muxer) PrepareTrackFragment(track *Track, codec *core.Codec, packets []*rtp.Packet) (sampleDurations, sampleSizes, sampleFlags []uint32, baseMediaDecodeTime uint64, totalDataSize int) {
    sampleCount := len(packets)
    sampleDurations = make([]uint32, sampleCount)
    sampleSizes = make([]uint32, sampleCount)
    sampleFlags = make([]uint32, sampleCount)
    totalDataSize = 0

    isAudio := codec.Name != core.CodecH264 && codec.Name != core.CodecH265

    if m.index == 1 {
        track.dts = 0
        track.pts = 0
        track.lastDTS = 0
    } else if track.dts == 0 && sampleCount > 0 {
        if isAudio {
            // Audio: Set DTS/PTS based on first packet
            videoCodec := m.codecs[1]
            if videoCodec != nil {
                track.dts = uint64(float64(packets[0].Timestamp) * float64(codec.ClockRate) / float64(videoCodec.ClockRate))
            }
            track.pts = uint32(track.dts)
            track.lastDTS = track.dts
        } else {
            track.dts = uint64(packets[0].Timestamp)
            track.pts = packets[0].Timestamp
            track.lastDTS = track.dts
        }
    }

    // Audio
    if isAudio {
        var defaultDuration uint32
        switch codec.Name {
        case core.CodecAAC:
            defaultDuration = 1024
        case core.CodecOpus:
            defaultDuration = 480
        case core.CodecFLAC:
            defaultDuration = 80
        default:
            defaultDuration = uint32(codec.ClockRate / 100)
        }

        // Make sure DTS is greater than last DTS
        if track.dts <= track.lastDTS {
            track.dts = track.lastDTS + 1
        }

        baseMediaDecodeTime = track.dts

        for i := 0; i < sampleCount; i++ {
            sampleDurations[i] = defaultDuration
            sampleSizes[i] = uint32(len(packets[i].Payload))
            sampleFlags[i] = iso.SampleAudio
            totalDataSize += len(packets[i].Payload)
        }

        // Calculate DTS/PTS
        track.dts += uint64(defaultDuration * uint32(sampleCount))
        track.pts = uint32(track.dts - uint64(defaultDuration))
        track.lastDTS = track.dts  // Update lastDTS

        return
    }

    // Video
    var totalDuration uint64

    for i, packet := range packets {
        var duration uint32
        if i < sampleCount-1 {
            nextTS := packets[i+1].Timestamp
            if nextTS < packet.Timestamp {
                duration = (0xFFFFFFFF - packet.Timestamp) + nextTS + 1
            } else {
                duration = nextTS - packet.Timestamp
            }
        } else {
            duration = codec.ClockRate/30
        }

        if duration == 0 || duration > codec.ClockRate {
            duration = codec.ClockRate/30
        }

        sampleDurations[i] = duration
        sampleSizes[i] = uint32(len(packet.Payload))
        totalDuration += uint64(duration)

        if i == 0 && h264.IsKeyframe(packet.Payload) {
            sampleFlags[i] = iso.SampleVideoIFrame
        } else {
            sampleFlags[i] = iso.SampleVideoNonIFrame
        }

        totalDataSize += len(packet.Payload)
    }

    // Make sure DTS is greater than last DTS
    if track.dts <= track.lastDTS {
        track.dts = track.lastDTS + 1
    }

    baseMediaDecodeTime = track.dts
    track.dts += totalDuration
    track.pts = packets[sampleCount-1].Timestamp
    track.lastDTS = track.dts  // Update lastDTS

    return
}

func (m *Muxer) syncAudioVideo(audioTrack, videoTrack *Track, audioCodec, videoCodec *core.Codec) {
    if audioTrack.dts == 0 || videoTrack.dts == 0 {
        return
    }

    // Calculate audio/video relation time
    audioTime := float64(audioTrack.dts) / float64(audioCodec.ClockRate)
    videoTime := float64(videoTrack.dts) / float64(videoCodec.ClockRate)

    // Check if audio is ahead of video
    if diff := math.Abs(audioTime - videoTime); diff > 0.1 { // 100ms difference
        if audioTime > videoTime {
            audioTrack.dts = uint64(videoTime * float64(audioCodec.ClockRate))
        } else {
            audioTrack.dts += uint64((diff + 0.01) * float64(audioCodec.ClockRate)) // +10ms extra
        }
    }
}