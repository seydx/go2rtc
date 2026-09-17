package tapo

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mpegts"
	"github.com/stretchr/testify/require"
)

// cameraStream muxes one audio part followed by video frames of videoType
// into tapo multipart parts
func cameraStream(videoType byte, frames int) string {
	mux := mpegts.NewMuxer()
	audio := mux.AddTrack(mpegts.StreamTypePCMATapo)
	video := mux.AddTrack(videoType)

	nalu := []byte{0, 0, 0, 4, 0x65, 0x88, 0x84, 0x00}
	if videoType == mpegts.StreamTypeH265 {
		nalu = []byte{0, 0, 0, 4, 0x26, 0x01, 0xAF, 0x09}
	}

	part := func(body []byte) string {
		return testBoundary + fmt.Sprintf("Content-Type: video/mp2t\r\nContent-Length: %d\r\n\r\n", len(body)) + string(body) + "\r\n"
	}

	stream := part(append(mux.GetHeader(), mux.GetPayload(audio, 0, make([]byte, 160))...))
	for i := 0; i < frames; i++ {
		stream += part(append(mux.GetHeader(), mux.GetPayload(video, uint32(i*3000), nalu)...))
	}
	return stream
}

// probedClient runs the codec probe against a camera sending stream. The
// stream request is already answered, so SetupStream sends nothing.
func probedClient(t *testing.T, stream string) (*Client, error) {
	t.Helper()
	clientSide, camSide := net.Pipe()
	c := &Client{conn1: clientSide, decrypt: func(b []byte) []byte { return b }, session1: "test"}
	t.Cleanup(func() { _ = c.Close(); _ = camSide.Close() })

	go func() {
		_, _ = camSide.Write([]byte(stream))
		// camera keeps the connection open
	}()

	return c, c.probe()
}

func anyVideoTrack(t *testing.T, c *Client) *core.Receiver {
	t.Helper()
	video := c.GetMedias()[0]
	anyVideo := &core.Media{Kind: core.KindVideo, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecAny}}}
	codec, _ := video.MatchMedia(anyVideo)
	require.NotNil(t, codec)
	track, err := c.GetTrack(video, codec)
	require.NoError(t, err)
	return track
}

func receivedPackets(receiver *core.Receiver) int {
	_, packets := receiver.Stats()
	return packets
}

// A consumer taking any video codec (the preload's video=ANY) is matched with
// the first listed one, so the list may only hold what the camera sends.
func TestProbeOffersTheCodecTheCameraSends(t *testing.T) {
	for _, tc := range []struct {
		name       string
		streamType byte
		codec      string
	}{
		{name: "h265", streamType: mpegts.StreamTypeH265, codec: core.CodecH265},
		{name: "h264", streamType: mpegts.StreamTypeH264, codec: core.CodecH264},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := probedClient(t, cameraStream(tc.streamType, 5))
			require.NoError(t, err)

			codecs := c.GetMedias()[0].Codecs
			require.Len(t, codecs, 1)
			require.Equal(t, tc.codec, codecs[0].Name)

			track := anyVideoTrack(t, c)
			require.Equal(t, tc.codec, track.Codec.Name)

			go func() { _ = c.Handle() }()

			// the first frame was read by the probe, Handle must still deliver it
			require.Eventually(t, func() bool { return receivedPackets(track) == 5 }, 2*time.Second, 20*time.Millisecond)
		})
	}
}

func TestProbeWithoutVideoKeepsBothCodecs(t *testing.T) {
	timeout := probeTimeout
	probeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { probeTimeout = timeout })

	c, err := probedClient(t, cameraStream(mpegts.StreamTypeH264, 0))
	require.NoError(t, err, "a camera that is slow to send video is not a failed dial")

	var names []string
	for _, codec := range c.GetMedias()[0].Codecs {
		names = append(names, codec.Name)
	}
	require.Equal(t, []string{core.CodecH264, core.CodecH265}, names)
}

func TestProbeFailsWhenTheCameraClosesTheStream(t *testing.T) {
	clientSide, camSide := net.Pipe()
	c := &Client{conn1: clientSide, decrypt: func(b []byte) []byte { return b }, session1: "test"}
	t.Cleanup(func() { _ = c.Close() })

	_ = camSide.Close()
	require.Error(t, c.probe())
}
