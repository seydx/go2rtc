package tapo

import (
	"net"
	"testing"
	"time"
)

const testBoundary = "----device-stream-boundary--\r\n"

func handleClient(t *testing.T, stream string) (*Client, net.Conn, chan error) {
	t.Helper()
	clientSide, camSide := net.Pipe()
	c := &Client{conn1: clientSide, decrypt: func(b []byte) []byte { return b }}

	go func() {
		_, _ = camSide.Write([]byte(stream))
		// camera keeps the connection open
	}()

	done := make(chan error, 1)
	go func() { done <- c.Handle() }()
	return c, camSide, done
}

// Close must always unblock Handle - the producer watchdog depends on it.
func TestHandleReturnsAfterClose(t *testing.T) {
	stream := testBoundary +
		"Content-Type: application/json\r\nContent-Length: 2\r\n\r\n{}\r\n" +
		testBoundary +
		"Content-Type: application/json\r\nContent-Length: 2\r\n\r\n{}\r\n"

	c, _, done := handleClient(t, stream)

	time.Sleep(200 * time.Millisecond) // Handle is now blocked reading the next part
	_ = c.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Handle did not return after Close")
	}
}

// A part carrying more payload than its Content-Length announces must not
// wedge Handle forever - Close has to get the producer out of it.
func TestHandleReturnsAfterCloseOnOversizedPart(t *testing.T) {
	payload := make([]byte, 100)
	stream := testBoundary +
		"Content-Type: video/mp2t\r\nContent-Length: 10\r\n\r\n" + string(payload) + "\r\n" +
		testBoundary +
		"Content-Type: application/json\r\nContent-Length: 2\r\n\r\n{}\r\n"

	c, _, done := handleClient(t, stream)

	time.Sleep(200 * time.Millisecond)
	_ = c.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Handle wedged on a part whose body exceeds its Content-Length - unbreakable by Close/Interrupt")
	}
}
