package rtsp

import (
	"net"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// The producer interrupts the session it reconnects. The talk session of a
// playing stream goes with it: a camera with a single backchannel (Dahua)
// would refuse it to the new session while the old one holds it.
func TestInterruptEndsTheTalkSession(t *testing.T) {
	Timeout = 5 * time.Second

	cam := newFakeCamera(t)
	defer cam.ln.Close()

	client := NewClient("rtsp://" + cam.ln.Addr().String() + "/stream")
	client.Backchannel = true
	client.HandshakeTimeout = 7

	require.Nil(t, client.Dial())
	require.Nil(t, client.Describe())

	video, backchannel := client.GetMedias()[0], client.GetMedias()[1]
	_, err := client.GetTrack(video, video.Codecs[0])
	require.Nil(t, err)

	go func() { _ = client.Start() }()
	defer client.Stop()
	require.Equal(t, 0, waitPlayed(t, cam))

	mic := core.NewReceiver(
		&core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly},
		&core.Codec{Name: core.CodecPCMU, ClockRate: 8000},
	)
	require.Nil(t, client.AddTrack(backchannel, backchannel.Codecs[0], mic))
	require.Equal(t, 1, waitPlayed(t, cam))

	bc := client.backchannel.Load()
	require.NotNil(t, bc)
	require.Equal(t, 7, bc.HandshakeTimeout, "the talk session waits for the camera as long as the main one")

	require.NoError(t, client.Interrupt())

	conn, _ := bc.netIO()
	_, err = conn.Write([]byte{0})
	require.ErrorIs(t, err, net.ErrClosed)
}
