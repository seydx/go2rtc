package rtsp

import (
	"net"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// The session reads packets while a viewer's new track is set up on it, and
// the API reports the connection meanwhile. Meaningful under -race.
func TestTrackSetupWhileReadingPackets(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()

	c := NewServer(server)
	c.mode = core.ModePassiveConsumer

	buf, err := (&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1, Timestamp: 1}, Payload: []byte{1}}).Marshal()
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 2000 {
			_ = c.handleRawPacket(0, buf)
			c.addRecv(len(buf))
			_, _ = c.MarshalJSON()
		}
	}()

	for i := range 100 {
		media := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly}
		_, err := c.GetTrack(media, &core.Codec{Name: core.CodecPCMU, ClockRate: uint32(8000 + i)})
		require.NoError(t, err)
	}
	<-done

	receivers, _ := c.tracks()
	require.Len(t, receivers, 100)
}
