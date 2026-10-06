package ws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func dialTestServer(t *testing.T) *websocket.Conn {
	t.Helper()
	initWS("*")
	srv := httptest.NewServer(http.HandlerFunc(apiWS))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// A handler that ends the session from the server side closes the websocket
// with 1012, and the transport's close handlers run like on a client close.
func TestDisconnectClosesWebsocket(t *testing.T) {
	var closed atomic.Bool
	HandleFunc("test/disconnect", func(tr *Transport, _ *Message) error {
		tr.OnClose(func() { closed.Store(true) })
		tr.Disconnect()
		return nil
	})

	conn := dialTestServer(t)
	require.NoError(t, conn.WriteJSON(&Message{Type: "test/disconnect"}))

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := conn.ReadMessage()
	require.True(t, websocket.IsCloseError(err, websocket.CloseServiceRestart), "client sees a 1012 close, got %v", err)
	require.Eventually(t, closed.Load, 5*time.Second, 10*time.Millisecond, "close handlers run")
}

// Close only runs the handlers: a handler removing its own session (ex.
// mse/stop) must not take the websocket down.
func TestCloseHandlersKeepWebsocketOpen(t *testing.T) {
	HandleFunc("test/stop", func(tr *Transport, _ *Message) error {
		tr.Write(&Message{Type: "stopped"})
		return nil
	})

	conn := dialTestServer(t)
	require.NoError(t, conn.WriteJSON(&Message{Type: "test/stop"}))

	var msg Message
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	require.NoError(t, conn.ReadJSON(&msg))
	require.Equal(t, "stopped", msg.Type)

	// still open: the next message goes through
	require.NoError(t, conn.WriteJSON(&Message{Type: "test/stop"}))
	require.NoError(t, conn.ReadJSON(&msg))
}

// A panicking handler must not take go2rtc down, the websocket keeps working.
func TestPanickingHandlerKeepsServing(t *testing.T) {
	HandleFunc("test/panic", func(*Transport, *Message) error {
		var producers []int
		_ = producers[1]
		return nil
	})
	HandleFunc("test/echo", func(tr *Transport, _ *Message) error {
		tr.Write(&Message{Type: "echo"})
		return nil
	})

	conn := dialTestServer(t)
	require.NoError(t, conn.WriteJSON(&Message{Type: "test/panic"}))
	require.NoError(t, conn.WriteJSON(&Message{Type: "test/echo"}))

	var msg Message
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	require.NoError(t, conn.ReadJSON(&msg))
	require.Equal(t, "echo", msg.Type)
}

// Handlers run concurrently, Seq keeps the order the client sent its messages
// in. The client can't set it.
func TestMessagesNumberedInSendOrder(t *testing.T) {
	seqs := make(chan uint64, 3)
	HandleFunc("test/seq", func(_ *Transport, msg *Message) error {
		seqs <- msg.Seq
		return nil
	})

	conn := dialTestServer(t)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"test/seq","Seq":99}`)))
	require.NoError(t, conn.WriteJSON(&Message{Type: "test/seq"}))
	require.NoError(t, conn.WriteJSON(&Message{Type: "test/seq"}))

	got := map[uint64]bool{}
	for range 3 {
		select {
		case seq := <-seqs:
			got[seq] = true
		case <-time.After(5 * time.Second):
			t.Fatal("handler not called")
		}
	}
	require.Equal(t, map[uint64]bool{1: true, 2: true, 3: true}, got)
}
