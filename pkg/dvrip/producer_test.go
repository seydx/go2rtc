package dvrip

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// The low nibble of the media code is the codec, the high one differs between
// streams: a camera sends 0x52 for the H.264 of its 4K main stream (#2671), as
// H.265 comes as 0x43 and 0x53 too. Its video went missing.
func TestVideoTrackH264Codes(t *testing.T) {
	frame := annexb.EncodeToAVCC([]byte{
		0, 0, 0, 1, 0x67, 0x42, 0, 0x1f, 0xe9, // SPS
		0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80, // PPS
		0, 0, 0, 1, 0x65, 0x88, 0x84, // IDR
	})

	for _, code := range []byte{0x02, 0x12, 0x42, 0x52} {
		c := &Producer{}
		c.addVideoTrack(code, frame)
		require.NotNil(t, c.video, "media code %#x", code)
		require.Equal(t, core.CodecH264, c.video.Codec.Name)
		require.Contains(t, c.video.Codec.FmtpLine, "profile-level-id=42001f")
		require.Len(t, c.Medias, 1)
	}
}

// camera answers the Claim of Play, takes its Start, sends the media packets,
// each in a chunk of its own, and hangs up
func camera(t *testing.T, packets ...[]byte) *Client {
	t.Helper()

	client, camera := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = camera.Close()
	})

	chunk := func(payload []byte) []byte {
		hdr := make([]byte, 20)
		hdr[0] = 255
		binary.LittleEndian.PutUint32(hdr[16:], uint32(len(payload)))
		return append(hdr, payload...)
	}

	go func() {
		rd := bufio.NewReader(camera)
		command := func() error {
			hdr := make([]byte, 20)
			if _, err := io.ReadFull(rd, hdr); err != nil {
				return err
			}
			_, err := io.ReadFull(rd, make([]byte, binary.LittleEndian.Uint32(hdr[16:])))
			return err
		}

		if command() != nil {
			return
		}
		if _, err := camera.Write(chunk([]byte(`{"Ret":100}`))); err != nil {
			return
		}
		if command() != nil {
			return
		}
		for _, packet := range packets {
			if _, err := camera.Write(chunk(packet)); err != nil {
				return
			}
		}
		_ = camera.Close()
	}()

	return &Client{conn: client, rd: bufio.NewReader(client)}
}

func iframe(code, fps byte, frame []byte) []byte {
	b := []byte{0, 0, 1, 0xFC, code, fps, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[12:], uint32(len(frame)))
	return append(b, frame...)
}

func pframe(frame []byte) []byte {
	b := []byte{0, 0, 1, 0xFD, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[4:], uint32(len(frame)))
	return append(b, frame...)
}

func audio(code, sampleRate byte) []byte {
	b := []byte{0, 0, 1, 0xFA, code, sampleRate, 0, 0}
	binary.LittleEndian.PutUint16(b[6:], 160)
	return append(b, make([]byte, 160)...)
}

// Values a camera sends took the whole process down in the probe: an H.265
// keyframe without start code (its parameter sets were read blind), a frame
// rate of 0 (divided by) and a sample rate index of 0 (read as 255).
func TestProbeSurvivesOddHeaders(t *testing.T) {
	c := &Producer{client: camera(t,
		iframe(0x53, 0, []byte{0x8f, 0x3a, 0x11, 0x42, 0x07}),
		audio(14, 0),
	)}

	require.NotPanics(t, func() { require.NoError(t, c.probe()) })
	require.Equal(t, core.CodecH265, c.video.Codec.Name)
	require.Equal(t, core.CodecPCMA, c.audio.Codec.Name)
}

// The parameter sets of an H.265 keyframe go into the codec as before.
func TestVideoTrackH265ParameterSets(t *testing.T) {
	vps := []byte{0x40, 0x01, 0x0c, 0x01, 0xff}
	sps := []byte{0x42, 0x01, 0x01, 0x01, 0x60}
	pps := []byte{0x44, 0x01, 0xc1, 0x72}

	var frame []byte
	for _, nalu := range [][]byte{vps, sps, pps, {0x26, 0x01, 0xaf}} {
		frame = append(append(frame, 0, 0, 0, 1), nalu...)
	}

	c := &Producer{}
	c.addVideoTrack(0x53, annexb.EncodeToAVCC(frame))
	require.Equal(t, "profile-id=1"+
		";sprop-vps="+base64.StdEncoding.EncodeToString(vps)+
		";sprop-sps="+base64.StdEncoding.EncodeToString(sps)+
		";sprop-pps="+base64.StdEncoding.EncodeToString(pps), c.video.Codec.FmtpLine)
}

// Without a frame rate the frames can't be spaced by it: they keep the time
// they arrive at, rather than all one timestamp.
func TestFramesWithoutFrameRateKeepTheirArrival(t *testing.T) {
	c := &Producer{client: camera(t,
		iframe(0x52, 0, []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0x1f, 0xe9, 0, 0, 0, 1, 0x65, 0x88}),
		audio(14, 1),
	)}
	require.NoError(t, c.probe())

	stamps := make(chan uint32, 2)
	sender := core.NewSender(c.video.Media, c.video.Codec)
	sender.Handler = func(pkt *rtp.Packet) { stamps <- pkt.Timestamp }
	sender.HandleRTP(c.video)
	t.Cleanup(sender.Close)

	// a second session: Play is behind, Start reads the frames
	// ReadPacket waits for 16 bytes, a shorter last packet would never be read
	slice := []byte{0, 0, 0, 1, 0x41, 0x9a, 1, 2, 3, 4, 5, 6, 7, 8}
	c.client = camera(t, pframe(slice), pframe(slice))
	require.NoError(t, c.client.Play())

	start := core.Now90000()
	require.Error(t, c.Start(), "the camera hangs up")

	for range 2 {
		select {
		case ts := <-stamps:
			require.Less(t, ts-start, uint32(90000), "the time the frame came")
		case <-time.After(time.Second):
			require.FailNow(t, "the frame never came")
		}
	}
}
