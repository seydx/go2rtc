package mp4

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mjpeg"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// Chrome takes the video size for MSE from the init segment. An H265 camera
// coding 1080 lines as 1088 must still get 1920x1080 in tkhd and hev1, or the
// picture is stretched (AlexxIT/go2rtc#2526).
func TestMuxerH265InitUsesDisplaySize(t *testing.T) {
	m := &Muxer{}
	m.AddTrack(&core.Codec{
		Name:      core.CodecH265,
		ClockRate: 90000,
		FmtpLine:  "sprop-sps=QgEGIWAAAAMAAAMAAAMAAAMAewAAoAPAgBEHy7ve96clEVcqn1KS5uAgICAQ",
	})
	init, err := m.GetInit()
	require.Nil(t, err)

	// VisualSampleEntry: 24 bytes after the type, then width and height
	i := bytes.Index(init, []byte("hev1"))
	require.Greater(t, i, 0)
	require.Equal(t, uint16(1920), binary.BigEndian.Uint16(init[i+28:]))
	require.Equal(t, uint16(1080), binary.BigEndian.Uint16(init[i+30:]))

	// tkhd ends with width and height as 16.16 fixed point
	i = bytes.Index(init, []byte("tkhd"))
	require.Greater(t, i, 0)
	end := i - 4 + int(binary.BigEndian.Uint32(init[i-4:]))
	require.Equal(t, uint32(1920), binary.BigEndian.Uint32(init[end-8:])>>16)
	require.Equal(t, uint32(1080), binary.BigEndian.Uint32(init[end-4:])>>16)
}

func TestMuxerJPEG(t *testing.T) {
	m := &Muxer{}
	m.AddVideoTrack(&core.Codec{Name: core.CodecJPEG, ClockRate: 90000}, 640, 480)
	init, err := m.GetInit()
	require.Nil(t, err)

	i := bytes.Index(init, []byte("jpeg"))
	require.Greater(t, i, 0)
	require.Equal(t, uint16(640), binary.BigEndian.Uint16(init[i+28:]))
	require.Equal(t, uint16(480), binary.BigEndian.Uint16(init[i+30:]))
	require.NotContains(t, string(init), "avcC")
}

// MJPEG cameras send a picture every few seconds, or lose one at 1 fps: the
// samples keep these intervals. The first picture has none, a new session
// jumps.
func TestMuxerJPEGKeepsLongIntervals(t *testing.T) {
	m := &Muxer{}
	m.AddVideoTrack(&core.Codec{Name: core.CodecJPEG, ClockRate: 90000}, 640, 480)

	var durations []uint32
	var dts uint64
	// the first picture 5 s into the session, 0.5 fps, 10 s later, 11 s later
	// (a jump), back in time (a new session), 0.5 fps
	for _, ts := range []uint32{450_000, 630_000, 810_000, 1_710_000, 2_700_000, 1_000, 181_000} {
		b := m.GetPayload(0, &rtp.Packet{Header: rtp.Header{Timestamp: ts}, Payload: []byte{0xFF, 0xD8, 0xFF, 0xD9}})
		i, j := bytes.Index(b, []byte("tfhd")), bytes.Index(b, []byte("tfdt"))
		require.Equal(t, dts, binary.BigEndian.Uint64(b[j+8:]))
		duration := binary.BigEndian.Uint32(b[i+12:])
		durations = append(durations, duration)
		dts += uint64(duration)
	}
	// a sample lasts from the previous picture on
	require.Equal(t, []uint32{91, 180_000 - 91, 180_000, 900_000, 91, 91, 180_000 - 91}, durations)
}

func TestMSECodecsJPEG(t *testing.T) {
	medias := ParseCodecs(MimeH264+","+MimeJPEG+","+MimeAAC, true)
	require.Len(t, medias, 2)
	require.Equal(t, core.CodecJPEG, medias[0].Codecs[1].Name)
	require.Equal(t, "jpeg,mp4a.40.2", MimeCodecs([]*core.Codec{{Name: core.CodecJPEG}, {Name: core.CodecAAC}}))
}

// no keyframe search: the first picture already leaves as a fragment
func TestConsumerJPEG(t *testing.T) {
	medias := ParseCodecs(MimeJPEG, false)
	cons := NewConsumer(medias)
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecJPEG, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	media.Codecs = []*core.Codec{codec}
	require.NoError(t, cons.AddTrack(medias[0], codec, core.NewReceiver(media, codec)))

	cons.Senders[0].Handler(&rtp.Packet{Header: rtp.Header{Timestamp: 9000}, Payload: []byte{0xFF, 0xD8, 0xFF, 0xD9}})
	require.Greater(t, cons.Send, 0)
	require.Equal(t, MimeJPEG, MimeCodecs(cons.Codecs()))
}

// addTrack attaches a producer track of codec to the consumer's media
func addTrack(t *testing.T, cons core.Consumer, media *core.Media, codec *core.Codec) error {
	t.Helper()
	kind := core.GetKind(codec.Name)
	track := core.NewReceiver(&core.Media{Kind: kind, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}}, codec)
	return cons.AddTrack(media, codec, track)
}

// samples returns the samples the consumer wrote so far
func samples(t *testing.T, cons *Consumer) [][]byte {
	t.Helper()
	var out [][]byte
	for b := cons.wr.Writer.(*bytes.Buffer).Bytes(); len(b) > 0; {
		require.GreaterOrEqual(t, len(b), 8)
		size := int(binary.BigEndian.Uint32(b))
		require.True(t, size >= 8 && size <= len(b))
		if string(b[4:8]) == "mdat" {
			out = append(out, b[8:size])
		}
		b = b[size:]
	}
	return out
}

// The audio waits for the first picture, as it waits for the first keyframe
// of H.264.
func TestConsumerJPEGStartsTheAudio(t *testing.T) {
	cons := NewConsumer(ParseCodecs(MimeJPEG+","+MimeAAC, true))
	require.NoError(t, addTrack(t, cons, cons.Medias[0], &core.Codec{Name: core.CodecJPEG, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}))
	require.NoError(t, addTrack(t, cons, cons.Medias[1], &core.Codec{Name: core.CodecAAC, ClockRate: 16000, PayloadType: core.PayloadTypeRAW}))

	audio := func(ts uint32) {
		cons.Senders[1].Handler(&rtp.Packet{Header: rtp.Header{Timestamp: ts}, Payload: []byte{0x21, 0x10, 0x05, 0x00}})
	}
	audio(1024)
	require.Empty(t, samples(t, cons), "audio before the first picture")

	cons.Senders[0].Handler(&rtp.Packet{Header: rtp.Header{Timestamp: 9000}, Payload: []byte{0xFF, 0xD8, 0xFF, 0xD9}})
	audio(2048)
	require.Len(t, samples(t, cons), 2)
}

// A camera sends a picture in RFC 2435 packets: they leave as one sample.
func TestConsumerJPEGOverRTP(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	var pic bytes.Buffer
	require.NoError(t, jpeg.Encode(&pic, img, nil))

	var packets []*rtp.Packet
	mjpeg.RTPPay(func(packet *rtp.Packet) {
		clone := *packet
		clone.Payload = bytes.Clone(packet.Payload)
		packets = append(packets, &clone)
	})(&rtp.Packet{Header: rtp.Header{Timestamp: 9000}, Payload: pic.Bytes()})
	require.Greater(t, len(packets), 1)

	cons := NewConsumer(ParseCodecs(MimeJPEG, false))
	require.NoError(t, addTrack(t, cons, cons.Medias[0], &core.Codec{Name: core.CodecJPEG, ClockRate: 90000, PayloadType: 26}))
	for _, packet := range packets {
		cons.Senders[0].Handler(packet)
	}

	out := samples(t, cons)
	require.Len(t, out, 1)
	conf, err := jpeg.DecodeConfig(bytes.NewReader(out[0]))
	require.NoError(t, err)
	require.Equal(t, [2]int{320, 240}, [2]int{conf.Width, conf.Height})
}

// The "mp4" message of the websocket API takes the same codecs as "mse". Its
// keyframe consumer has no handler for JPEG: the sender called nil on the
// first picture and took the whole process down.
func TestKeyframeRefusesJPEG(t *testing.T) {
	cons := NewKeyframe(ParseCodecs(MimeJPEG, false))
	require.Error(t, addTrack(t, cons, cons.Medias[0], &core.Codec{Name: core.CodecJPEG, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}))
	require.Empty(t, cons.Senders)

	cons = NewKeyframe(nil)
	require.NoError(t, addTrack(t, cons, cons.Medias[0], &core.Codec{
		Name:      core.CodecH265,
		ClockRate: 90000,
		FmtpLine:  "sprop-sps=QgEGIWAAAAMAAAMAAAMAAAMAewAAoAPAgBEHy7ve96clEVcqn1KS5uAgICAQ",
	}))
	require.Len(t, cons.Senders, 1)
}
