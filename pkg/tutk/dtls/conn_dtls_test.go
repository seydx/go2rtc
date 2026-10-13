package dtls

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/stretchr/testify/require"
)

// Based on AlexxIT/go2rtc#2500: Close clears clientConn under c.mu, so every
// reader outside connect/Close has to take the lock too, or a stream teardown
// can dereference nil and take down the whole process.

// dtlsPair connects a DTLS client and server through in-memory channels.
func dtlsPair(t *testing.T, ctx context.Context) (client, server *dtls.Conn) {
	psk := DerivePSK("test")
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	toClient := make(chan []byte, 1024)
	toServer := make(chan []byte, 1024)
	forward := func(ch chan []byte) func([]byte, uint8) error {
		return func(b []byte, _ uint8) error {
			ch <- append([]byte(nil), b...)
			return nil
		}
	}

	// Stand in for the camera: a server offering the client's custom suite.
	errs := make(chan error, 1)
	go func() {
		adapter := &channelAdapter{ctx: ctx, addr: addr, writeFn: forward(toClient), readChan: toServer}
		var err error
		if server, err = dtls.Server(adapter, addr, buildDTLSConfig(psk, false)); err == nil {
			err = server.HandshakeContext(ctx)
		}
		errs <- err
	}()

	client, err := NewDTLSClient(ctx, iotcChannelMain, addr, forward(toServer), toClient, psk)
	require.NoError(t, err)
	require.NoError(t, <-errs)
	return client, server
}

// Close clears clientConn while the ACK ticker started by AVClientStart is
// still running. Run with -race: the ticker must not read the field unlocked.
func TestCloseWhileAckTickerRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, server := dtlsPair(t, ctx)
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, err := server.Read(buf); err != nil {
				return
			}
		}
	}()

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)

	connCtx, connCancel := context.WithCancel(ctx)
	c := &DTLSConn{
		conn:       udp,
		ctx:        connCtx,
		cancel:     connCancel,
		clientConn: client,
		rawCmd:     make(chan []byte, 1),
	}

	resp := make([]byte, 32)
	binary.LittleEndian.PutUint16(resp, magicAVLoginResp)
	c.rawCmd <- resp

	require.NoError(t, c.AVClientStart(time.Second))

	// Let the ACK ticker (100ms interval) fire at least once before closing.
	time.Sleep(250 * time.Millisecond)
	require.NoError(t, c.Close())

	c.mu.RLock()
	defer c.mu.RUnlock()
	require.Nil(t, c.clientConn)
}

// A stream torn down before the AV login must fail the login, not
// dereference the cleared connection.
func TestAVClientStartAfterCloseReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := &DTLSConn{ctx: ctx, cancel: cancel, rawCmd: make(chan []byte, 1)}

	var err error
	require.NotPanics(t, func() { err = c.AVClientStart(100 * time.Millisecond) })
	require.Error(t, err)
}

// The worker reads from the client connection; once Close has cleared it the
// worker has to end, not dereference nil.
func TestWorkerExitsWhenConnCleared(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := &DTLSConn{ctx: ctx, cancel: cancel}
	c.wg.Add(1)

	require.NotPanics(t, c.worker)
}

// The same IPv4 address comes in two forms: 4 bytes, and 16 bytes as from
// net.ParseIP or a dual stack socket. A host without IPv6 reads 4 bytes from
// the camera while its address was parsed to 16. Here it's the other way
// round, the camera's address is the 4 byte form. Either way it is the camera.
func TestWriteAndWaitMatchesIPInEitherForm(t *testing.T) {
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

	cameraAddr := camera.LocalAddr().(*net.UDPAddr)
	c := &DTLSConn{conn: conn, addr: &net.UDPAddr{IP: cameraAddr.IP.To4(), Port: cameraAddr.Port}, isCC51: true}

	res, err := c.WriteAndWait([]byte("hello"), func([]byte) bool { return true })
	require.NoError(t, err)
	require.Equal(t, answer, res)
}
