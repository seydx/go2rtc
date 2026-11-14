package rtmp

import (
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// serve accepts one connection and reads its commands, the result of that
// arrives on the channel together with the connection
func serve(t *testing.T) (string, <-chan serveResult) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	done := make(chan serveResult, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- serveResult{err: err}
			return
		}
		t.Cleanup(func() { _ = conn.Close() })

		c, err := NewServer(conn)
		if err == nil {
			err = c.ReadCommands()
		}
		done <- serveResult{conn: c, err: err}
	}()

	return "rtmp://" + ln.Addr().String() + "/camera", done
}

type serveResult struct {
	conn *Conn
	err  error
}

func dial(t *testing.T, rawURL string) *Conn {
	t.Helper()

	u, err := url.Parse(rawURL)
	require.NoError(t, err)

	conn, err := net.Dial("tcp", u.Host)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	c, err := NewClient(conn, u)
	require.NoError(t, err)
	return c
}

func wait(t *testing.T, done <-chan serveResult, limit time.Duration) serveResult {
	t.Helper()

	select {
	case res := <-done:
		return res
	case <-time.After(limit):
		t.Fatalf("the server still waits for a command after %s", limit)
		return serveResult{}
	}
}

func TestServerDropsAClientThatSendsNoCommand(t *testing.T) {
	t.Parallel()

	rawURL, done := serve(t)
	dial(t, rawURL)

	res := wait(t, done, core.ConnDeadline+3*time.Second)

	var netErr net.Error
	require.ErrorAs(t, res.err, &netErr)
	require.True(t, netErr.Timeout())
}

func TestServerRefusesALargeMessageBeforeTheStreamIsChosen(t *testing.T) {
	t.Parallel()

	rawURL, done := serve(t)
	c := dial(t, rawURL)

	// the header of a 16 MiB command, and nothing after it
	_, err := c.conn.Write([]byte{0x03, 0, 0, 0, 0xFF, 0xFF, 0xFF, TypeCommand, 0, 0, 0, 0})
	require.NoError(t, err)

	res := wait(t, done, 2*time.Second)
	require.ErrorContains(t, res.err, "message too large")
}

func TestServerReadsLargeFramesOnceTheStreamIsChosen(t *testing.T) {
	t.Parallel()

	rawURL, done := serve(t)
	c := dial(t, rawURL)

	frame := payload(maxCommandSize+1000, 3)

	go func() {
		_ = c.publish()
		_ = c.writeMessage(4, TypeVideo, 0, frame)
	}()

	res := wait(t, done, 2*time.Second)
	require.NoError(t, res.err)
	require.Equal(t, CommandPublish, res.conn.Intent)
	require.NoError(t, res.conn.WriteStart())

	require.Equal(t, frame, readVideo(t, res.conn))
}
