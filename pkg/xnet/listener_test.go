package xnet

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// A saved config moves a server to another port or stops it, the connections
// it has keep running
func TestListenerMoves(t *testing.T) {
	var l Listener
	t.Cleanup(func() { _, _ = l.Listen("") })

	first, second := freeAddress(t), freeAddress(t)

	ln, err := l.Listen(first)
	require.NoError(t, err)
	require.NotNil(t, ln)
	go func(ln net.Listener) {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				b := make([]byte, 4)
				for {
					n, err := conn.Read(b)
					if err != nil {
						return
					}
					_, _ = conn.Write(b[:n])
				}
			}()
		}
	}(ln)

	open, err := net.Dial("tcp", first)
	require.NoError(t, err)
	t.Cleanup(func() { _ = open.Close() })
	ping := func() {
		_, err := open.Write([]byte("ping"))
		require.NoError(t, err)
		b := make([]byte, 4)
		_, err = open.Read(b)
		require.NoError(t, err)
		require.Equal(t, "ping", string(b))
	}
	ping()

	// the same address: nothing new to serve
	ln, err = l.Listen(first)
	require.NoError(t, err)
	require.Nil(t, ln)

	ln, err = l.Listen(second)
	require.NoError(t, err)
	require.NotNil(t, ln)
	_, err = net.Dial("tcp", first)
	require.Error(t, err)

	ping()

	// a port in use: an error, the old listener is closed all the same
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = busy.Close() })
	_, err = l.Listen(busy.Addr().String())
	require.Error(t, err)
	_, err = net.Dial("tcp", second)
	require.Error(t, err)

	// and after the error the same address is tried again
	require.NoError(t, busy.Close())
	ln, err = l.Listen(busy.Addr().String())
	require.NoError(t, err)
	require.NotNil(t, ln)

	ln, err = l.Listen("")
	require.NoError(t, err)
	require.Nil(t, ln)
	_, err = net.Dial("tcp", busy.Addr().String())
	require.Error(t, err)
}

// A server turned off, or one whose new port was in use, opens again on its old
// address
func TestListenerReopens(t *testing.T) {
	var l Listener
	t.Cleanup(func() { _, _ = l.Listen("") })

	addr := freeAddress(t)
	open := func() {
		t.Helper()
		ln, err := l.Listen(addr)
		require.NoError(t, err)
		require.NotNil(t, ln)
		conn, err := net.Dial("tcp", addr)
		require.NoError(t, err)
		_ = conn.Close()
	}

	open()
	_, err := l.Listen("")
	require.NoError(t, err)
	open()

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = busy.Close() })
	_, err = l.Listen(busy.Addr().String())
	require.Error(t, err)
	open()
}
