package dvrip

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// Only what reached the camera counts as sent: the counter went up on the
// failed writes alone (#2388).
func TestBackchannelCountsWrittenBytes(t *testing.T) {
	client, camera := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = camera.Close()
	})

	readCmd := func() []byte {
		hdr := make([]byte, 20)
		if _, err := io.ReadFull(camera, hdr); err != nil {
			return nil
		}
		b := make([]byte, binary.LittleEndian.Uint32(hdr[16:]))
		if _, err := io.ReadFull(camera, b); err != nil {
			return nil
		}
		return append(hdr, b...)
	}

	talkData := make(chan int, 1)
	go func() {
		readCmd() // OPTalk Claim
		body := []byte(`{"Ret":100}` + "\x0a\x00")
		hdr := make([]byte, 20)
		hdr[0] = 255
		binary.LittleEndian.PutUint32(hdr[16:], uint32(len(body)))
		_, _ = camera.Write(append(hdr, body...))
		readCmd() // OPTalk Start
		talkData <- len(readCmd())
		_ = camera.Close()
	}()

	c := &Backchannel{client: &Client{conn: client, rd: bufio.NewReader(client)}}
	media := &core.Media{
		Kind:      core.KindAudio,
		Direction: core.DirectionSendonly,
		Codecs:    []*core.Codec{{Name: core.CodecPCMA, ClockRate: 8000}},
	}
	track := core.NewReceiver(media, media.Codecs[0])
	require.NoError(t, c.AddTrack(media, media.Codecs[0], track))
	handler := c.Senders[0].Handler

	// 320 bytes of audio make one talk packet
	handler(&rtp.Packet{Payload: make([]byte, 320)})
	n := <-talkData
	require.Equal(t, 20+8+320, n)
	require.Equal(t, n, c.Send)

	// the camera has hung up
	handler(&rtp.Packet{Payload: make([]byte, 320)})
	require.Equal(t, n, c.Send)
}
