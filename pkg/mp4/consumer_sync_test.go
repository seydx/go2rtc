package mp4

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	_ "github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// timeline returns where each sample sits on its track, in seconds, keyed by
// the capture index the test wrote into the last four bytes of the sample
func timeline(t *testing.T, cons *Consumer, clockRates map[uint32]float64) map[uint32]map[uint32]float64 {
	t.Helper()
	out := map[uint32]map[uint32]float64{}
	var track uint32
	var dts uint64

	cons.mu.Lock()
	b := bytes.Clone(cons.wr.Writer.(*bytes.Buffer).Bytes())
	cons.mu.Unlock()

	for len(b) >= 8 {
		size := int(binary.BigEndian.Uint32(b))
		require.True(t, size >= 8 && size <= len(b))
		switch string(b[4:8]) {
		case "moof":
			traf := b[8+16 : size] // after mfhd
			for inner := traf[8:]; len(inner) >= 8; {
				n := int(binary.BigEndian.Uint32(inner))
				switch string(inner[4:8]) {
				case "tfhd":
					track = binary.BigEndian.Uint32(inner[12:])
				case "tfdt":
					dts = binary.BigEndian.Uint64(inner[12:])
				}
				inner = inner[n:]
			}
		case "mdat":
			if out[track] == nil {
				out[track] = map[uint32]float64{}
			}
			out[track][binary.BigEndian.Uint32(b[size-4:])] = float64(dts) / clockRates[track]
		}
		b = b[size:]
	}
	return out
}

// A viewer joins in the middle of a GOP. The cached frames go out first,
// compressed in time, and the live picture follows behind them. Sound and
// picture recorded at the same moment must still land at the same place.
func TestConsumerKeepsTheSoundWithThePictureAfterAGOPReplay(t *testing.T) {
	vcodec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	acodec := &core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2, PayloadType: core.PayloadTypeRAW}
	video := core.NewReceiver(&core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{vcodec}}, vcodec)
	video.SetupGOP()
	audio := core.NewReceiver(&core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{acodec}}, acodec)

	var index uint32
	capture := func() {
		frame := []byte{0, 0, 0, 5, 0x41, 0, 0, 0, 0}
		if index%100 == 0 {
			frame[4] = 0x65
		}
		binary.BigEndian.PutUint32(frame[5:], index)
		video.Input(&rtp.Packet{Header: rtp.Header{Timestamp: 1_000_000 + index*3600}, Payload: frame})

		sound := make([]byte, 4)
		binary.BigEndian.PutUint32(sound, index)
		audio.Input(&rtp.Packet{Header: rtp.Header{Version: 2, Timestamp: 5_000 + index*1920}, Payload: sound})
		index++
	}

	const cached = 50
	for range cached {
		capture()
	}

	cons := NewConsumer(ParseCodecs(MimeH264+","+MimeOpus, true))
	cons.UseGOP = true
	require.NoError(t, cons.AddTrack(cons.Medias[0], vcodec, video))
	require.NoError(t, cons.AddTrack(cons.Medias[1], acodec, audio))

	for range 30 {
		time.Sleep(40 * time.Millisecond)
		capture()
	}
	time.Sleep(100 * time.Millisecond)

	tracks := timeline(t, cons, map[uint32]float64{1: 90000, 2: 48000})
	for _, i := range []uint32{cached + 5, cached + 15, cached + 25} {
		picture, ok := tracks[1][i]
		require.True(t, ok, "picture %d", i)
		sound, ok := tracks[2][i]
		require.True(t, ok, "sound %d", i)
		t.Logf("captured together (%d): picture at %.3f s, sound at %.3f s", i, picture, sound)
		require.Less(t, math.Abs(picture-sound), 0.08, "picture and sound of capture %d", i)
	}
}

// Without a cached GOP nothing is replayed, both tracks start together.
func TestConsumerStartsTheSoundWithThePictureWithoutAReplay(t *testing.T) {
	vcodec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	acodec := &core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2, PayloadType: core.PayloadTypeRAW}
	cons := NewConsumer(ParseCodecs(MimeH264+","+MimeOpus, true))
	require.NoError(t, addTrack(t, cons, cons.Medias[0], vcodec))
	require.NoError(t, addTrack(t, cons, cons.Medias[1], acodec))

	cons.Senders[0].Handler(&rtp.Packet{Header: rtp.Header{Timestamp: 9000}, Payload: []byte{0, 0, 0, 5, 0x65, 0, 0, 0, 1}})
	cons.Senders[1].Handler(&rtp.Packet{Header: rtp.Header{Version: 2, Timestamp: 960}, Payload: []byte{0, 0, 0, 1}})

	tracks := timeline(t, cons, map[uint32]float64{1: 90000, 2: 48000})
	require.Less(t, tracks[2][1], 0.02)
}
