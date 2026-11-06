package srtp

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// A saved config moves the socket to another port for the next sessions, the
// running ones keep the socket they have
func TestServerMovesBetweenSessions(t *testing.T) {
	free := func() (string, int) {
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		require.NoError(t, conn.Close())
		addr := conn.LocalAddr().(*net.UDPAddr)
		return addr.String(), addr.Port
	}
	session := func(ssrc uint32) *Session {
		endpoint := func() *Endpoint {
			return &Endpoint{Addr: "127.0.0.1", Port: 9, MasterKey: make([]byte, 16), MasterSalt: make([]byte, 14), SSRC: ssrc}
		}
		return &Session{Local: endpoint(), Remote: endpoint()}
	}

	first, firstPort := free()
	second, secondPort := free()
	s := NewServer(first)

	running := session(1)
	s.AddSession(running)
	require.Equal(t, firstPort, s.Port())

	s.SetAddress(second)
	require.Equal(t, firstPort, s.Port())
	joining := session(2)
	s.AddSession(joining)
	require.Equal(t, firstPort, s.Port())

	s.DelSession(running)
	s.DelSession(joining)
	require.Equal(t, secondPort, s.Port())

	next := session(3)
	s.AddSession(next)
	t.Cleanup(func() { s.DelSession(next) })
	require.Equal(t, secondPort, s.Port())
}
