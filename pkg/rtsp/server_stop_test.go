package rtsp

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// Stopping a server-side consumer — ex. the stream evicted it — must close
// the client's TCP connection in every state, including right after DESCRIBE
// (the consumer is attached before SETUP), or the client hangs on a session
// nothing feeds anymore.
func TestServerConsumerStopClosesConnection(t *testing.T) {
	for _, state := range []State{StateNone, StateSetup, StatePlay} {
		t.Run(state.String(), func(t *testing.T) {
			server, client := net.Pipe()
			t.Cleanup(func() { _ = client.Close() })

			conn := NewServer(server)
			conn.mode = core.ModePassiveConsumer
			conn.state = state

			require.NoError(t, ignoreClosed(conn.Stop()))

			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, err := client.Read(make([]byte, 1))
			require.ErrorIs(t, err, io.EOF, "the client must see its connection closed")
		})
	}
}

func ignoreClosed(err error) error {
	if err == io.ErrClosedPipe {
		return nil
	}
	return err
}
