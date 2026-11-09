package ring

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestServerPingGetsPong(t *testing.T) {
	received := make(chan map[string]any, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var msg map[string]any
		if conn.ReadJSON(&msg) == nil {
			received <- msg
		}
	}))
	defer srv.Close()

	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	defer ws.Close()

	client := &Client{
		cameraID: 42,
		wsClient: &WSClient{ws: ws, cameraID: 42, dialogID: "dialog", sessionID: "session", closed: make(chan struct{})},
	}
	client.onWSMessage(WSMessage{Method: "ping", Body: map[string]any{"doorbot_id": float64(42), "session_id": "session"}})

	select {
	case msg := <-received:
		require.Equal(t, "pong", msg["method"])
		require.Equal(t, "dialog", msg["dialog_id"])
		require.Equal(t, map[string]any{"doorbot_id": float64(42), "session_id": "session"}, msg["body"])
	case <-time.After(2 * time.Second):
		t.Fatal("no pong sent")
	}
}
