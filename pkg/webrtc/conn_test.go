package webrtc

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

type lossyConn struct {
	net.PacketConn
	drop *atomic.Bool
}

func (c *lossyConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil || !c.drop.Load() {
			return n, addr, err
		}
	}
}

func (c *lossyConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if c.drop.Load() {
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}

func newLossyPeers(t *testing.T, drop *atomic.Bool) *Conn {
	oldGrace := disconnectGrace
	disconnectGrace = 2 * time.Second
	t.Cleanup(func() { disconnectGrace = oldGrace })

	// ICE notices the loss after 500 ms and gives up only after the test
	timeouts := func(s *pion.SettingEngine) {
		s.SetICETimeouts(500*time.Millisecond, time.Minute, 100*time.Millisecond)
	}

	local := pion.SettingEngine{}
	local.SetIncludeLoopbackCandidate(true)
	local.SetNetworkTypes([]pion.NetworkType{pion.NetworkTypeUDP4})
	timeouts(&local)

	ln, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: &lossyConn{PacketConn: ln, drop: drop}})
	t.Cleanup(func() { _ = mux.Close() })

	conn, _, remote := newLocalPeers(t, peerOpts{
		api: pion.NewAPI(pion.WithSettingEngine(local)),
		remote: func(s *pion.SettingEngine) {
			timeouts(s)
			s.SetICEUDPMux(mux)
		},
	})
	t.Cleanup(func() { _ = conn.Close() })
	t.Cleanup(func() { _ = remote.Close() })
	return conn
}

func stateIs(conn *Conn, state pion.PeerConnectionState) func() bool {
	return func() bool { return conn.pc.ConnectionState() == state }
}

// A short Wi-Fi drop or a cellular handover recovers on the same peer.
func TestDisconnectedPeerThatComesBackStays(t *testing.T) {
	var drop atomic.Bool
	conn := newLossyPeers(t, &drop)

	drop.Store(true)
	require.Eventually(t, stateIs(conn, pion.PeerConnectionStateDisconnected), 3*time.Second, 10*time.Millisecond)
	drop.Store(false)
	require.Eventually(t, stateIs(conn, pion.PeerConnectionStateConnected), 2*time.Second, 10*time.Millisecond)

	require.Never(t, func() bool { return !stateIs(conn, pion.PeerConnectionStateConnected)() },
		disconnectGrace+500*time.Millisecond, 50*time.Millisecond)
}

// A viewer that left closes after the grace, long before pion gives up.
func TestDisconnectedPeerThatStaysAwayCloses(t *testing.T) {
	var drop atomic.Bool
	conn := newLossyPeers(t, &drop)

	drop.Store(true)
	require.Eventually(t, stateIs(conn, pion.PeerConnectionStateDisconnected), 3*time.Second, 10*time.Millisecond)
	require.Eventually(t, stateIs(conn, pion.PeerConnectionStateClosed), disconnectGrace+time.Second, 50*time.Millisecond)
}
