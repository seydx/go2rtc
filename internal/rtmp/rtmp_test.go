package rtmp

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// A saved config moves the RTMP server to another port or turns it off, without
// a restart of go2rtc
func TestListenerOfSavedConfig(t *testing.T) {
	t.Cleanup(func() { listen("") })

	free := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		require.NoError(t, ln.Close())
		return ln.Addr().String()
	}
	dial := func(addr string) error {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
		}
		return err
	}
	first, second := free(), free()

	reloadListener([]byte("rtmp:\n  listen: " + first + "\n"))
	require.NoError(t, dial(first))

	reloadListener([]byte("rtmp:\n  listen: " + second + "\n"))
	require.NoError(t, dial(second))
	require.Error(t, dial(first))

	// a config that is no YAML for the module changes nothing
	reloadListener([]byte("rtmp:\n  listen: [1, 2]\n"))
	require.NoError(t, dial(second))

	reloadListener([]byte("streams: {}\n"))
	require.Error(t, dial(second))
}
