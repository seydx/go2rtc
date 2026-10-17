package flv

import (
	"bytes"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/stretchr/testify/require"
)

func stream(tags ...[]byte) []byte {
	b := []byte{'F', 'L', 'V', 1, FlagsVideo | FlagsAudio, 0, 0, 0, 9, 0, 0, 0, 0}
	for _, tag := range tags {
		b = append(b, tag...)
	}
	return b
}

var (
	videoConfig = EncodeTag(TagVideo, 0, append([]byte{0x17, PacketTypeAVCHeader, 0, 0, 0}, h264.EncodeConfig(
		[]byte{0x67, 0x42, 0xC0, 0x1E, 0xD9, 0x03, 0xC5, 0x68}, []byte{0x68, 0xCE, 0x06, 0xE2},
	)...))
	audioConfig = EncodeTag(TagAudio, 0, []byte{0xAF, 0, 0x12, 0x10}) // AAC-LC, 44100, stereo

	keyframe   = []byte{0, 0, 0, 4, 0x65, 0x88, 0x84, 0x00}
	videoFrame = EncodeTag(TagVideo, 40, append([]byte{0x17, PacketTypeAVCNALU, 0, 0, 0}, keyframe...))
	aacFrame   = []byte{0x21, 0x10, 0x05, 0x00}
	audioFrame = EncodeTag(TagAudio, 23, append([]byte{0xAF, 1}, aacFrame...))
)

// open probes the stream and reads it to the end
func open(t *testing.T, b []byte) (video, audio [][]byte) {
	t.Helper()

	prod, err := Open(bytes.NewReader(b))
	require.NoError(t, err)

	for _, media := range prod.Medias {
		receiver, _ := prod.GetTrack(media, media.Codecs[0])
		receiver.Forward = func(packet *core.Packet) {
			if media.Kind == core.KindVideo {
				video = append(video, packet.Payload)
			} else {
				audio = append(audio, packet.Payload)
			}
		}
	}

	require.Error(t, prod.Start()) // the end of the stream
	return
}

// An empty audio or video tag, or one too short for its header, has no frame:
// it panicked in the probe and while streaming, and took the whole process
// down. Any RTMP publisher could send one.
func TestProducerSkipsShortTags(t *testing.T) {
	for name, tag := range map[string][]byte{
		"empty video":                   EncodeTag(TagVideo, 0, nil),
		"video of one byte":             EncodeTag(TagVideo, 0, []byte{0x17}),
		"video of four bytes":           EncodeTag(TagVideo, 0, []byte{0x17, PacketTypeAVCNALU, 0, 0}),
		"video header of four bytes":    EncodeTag(TagVideo, 0, []byte{0x17, PacketTypeAVCHeader, 0, 0}),
		"enhanced video of seven bytes": EncodeTag(TagVideo, 0, []byte{0x80 | PacketTypeCodedFrames, 'h', 'v', 'c', '1', 0, 0}),
		"empty audio":                   EncodeTag(TagAudio, 0, nil),
		"audio of one byte":             EncodeTag(TagAudio, 0, []byte{0xAF}),
	} {
		t.Run(name, func(t *testing.T) {
			var video, audio [][]byte
			require.NotPanics(t, func() {
				video, audio = open(t, stream(tag, videoConfig, tag, audioConfig, tag, videoFrame, tag, audioFrame, tag))
			})
			require.Equal(t, [][]byte{keyframe}, video)
			require.Equal(t, [][]byte{aacFrame}, audio)
		})
	}
}

func FuzzProducer(f *testing.F) {
	f.Add(stream(videoConfig, audioConfig, videoFrame, audioFrame))
	f.Add(stream(EncodeTag(TagVideo, 0, nil), EncodeTag(TagAudio, 0, nil), videoConfig, audioConfig))

	f.Fuzz(func(t *testing.T, b []byte) {
		prod, err := Open(bytes.NewReader(b))
		if err != nil {
			return
		}
		for _, media := range prod.Medias {
			receiver, _ := prod.GetTrack(media, media.Codecs[0])
			receiver.Forward = func(*core.Packet) {}
		}
		_ = prod.Start()
	})
}
