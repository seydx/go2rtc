package wav

import (
	"os/exec"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/shell"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// Only what reached the program counts as sent: the counter went up on the
// failed writes alone (#2388).
func TestBackchannelCountsWrittenBytes(t *testing.T) {
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("no true command")
	}

	cmd := shell.NewCommand("true")
	prod, err := NewBackchannel(cmd)
	require.NoError(t, err)
	c := prod.(*Backchannel)

	media := c.Medias[0]
	track := core.NewReceiver(media, media.Codecs[0])
	require.NoError(t, c.AddTrack(media, media.Codecs[0], track))
	handler := c.Senders[0].Handler

	// the pipe holds it until the program starts
	handler(&rtp.Packet{Payload: make([]byte, 160)})
	require.Equal(t, 160, c.Send)

	// the program has exited, its stdin is closed
	require.NoError(t, c.Start())
	handler(&rtp.Packet{Payload: make([]byte, 160)})
	require.Equal(t, 160, c.Send)
}
