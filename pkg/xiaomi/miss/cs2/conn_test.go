package cs2

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The same IPv4 address comes in two forms: 4 bytes, and 16 bytes as from
// net.ParseIP or a dual stack socket. A host without IPv6 reads 4 bytes from
// the camera while its address was parsed to 16. Here it's the other way
// round, the camera's address is the 4 byte form. Either way it is the camera.
func TestUDPConnMatchesIPInEitherForm(t *testing.T) {
	answer := make([]byte, 16)
	camera, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = camera.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			_, from, err := camera.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = camera.WriteToUDP(answer, from)
		}
	}()

	conn, err := net.ListenUDP("udp", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))

	cameraAddr := camera.LocalAddr().(*net.UDPAddr)
	c := &udpConn{UDPConn: conn, addr: &net.UDPAddr{IP: cameraAddr.IP.To4(), Port: cameraAddr.Port}}

	res, err := c.WriteUntil([]byte("hello"), func([]byte) bool { return true })
	require.NoError(t, err)
	require.Equal(t, answer, res)

	// a short message counts only when it comes from the camera
	_, err = camera.WriteToUDP([]byte{1, 2, 3}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: conn.LocalAddr().(*net.UDPAddr).Port})
	require.NoError(t, err)
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 3, n)
}
