package srtp

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/srtp"
	"github.com/stretchr/testify/require"
)

// A saved config moves the SRTP socket for the next sessions, without a restart
// of go2rtc. Turning it off takes a restart.
func TestAddressOfSavedConfig(t *testing.T) {
	before := Server
	t.Cleanup(func() { Server = before })
	Server = srtp.NewServer(":8443")

	reloadAddress([]byte("srtp:\n  listen: :18443\n"))
	require.Equal(t, 18443, Server.Port())

	// a config that is no YAML for the module changes nothing
	reloadAddress([]byte("srtp:\n  listen: [1]\n"))
	require.Equal(t, 18443, Server.Port())

	// without the block: the default port of go2rtc
	reloadAddress([]byte("streams: {}\n"))
	require.Equal(t, 8443, Server.Port())

	reloadAddress([]byte("srtp:\n  listen: \"\"\n"))
	require.Equal(t, 8443, Server.Port())
}
