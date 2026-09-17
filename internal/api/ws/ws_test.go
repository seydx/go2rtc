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
