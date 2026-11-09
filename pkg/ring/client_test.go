package ring

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func newTestSocket(t *testing.T) (*WSClient, <-chan map[string]any) {
	t.Helper()

	received := make(chan map[string]any, 16)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(received)

		for {
			var msg map[string]any
			if conn.ReadJSON(&msg) != nil {
				return
			}
			received <- msg
		}
	}))
	t.Cleanup(srv.Close)

	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.Close() })

	return &WSClient{ws: ws, cameraID: 42, dialogID: "dialog", sessionID: "session", closed: make(chan struct{})}, received
}

func nextMessage(t *testing.T, received <-chan map[string]any) (map[string]any, bool) {
	t.Helper()

	select {
	case msg, ok := <-received:
		return msg, ok
	case <-time.After(2 * time.Second):
		t.Fatal("socket neither sent nor closed")
		return nil, false
	}
}

func TestServerPingGetsPong(t *testing.T) {
	ws, received := newTestSocket(t)
	client := &Client{cameraID: 42, wsClient: ws}

	client.onWSMessage(WSMessage{Method: "ping", Body: map[string]any{"doorbot_id": float64(42), "session_id": "session"}})

	msg, ok := nextMessage(t, received)
	require.True(t, ok)
	require.Equal(t, "pong", msg["method"])
	require.Equal(t, "dialog", msg["dialog_id"])
	require.Equal(t, map[string]any{"doorbot_id": float64(42), "session_id": "session"}, msg["body"])
}

func TestSessionIDArrivesWhileSending(t *testing.T) {
	ws, received := newTestSocket(t)
	ws.sessionID = ""
	client := &Client{cameraID: 42, wsClient: ws}

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		_ = ws.sendSessionMessage("ping", nil)
	}()
	client.onWSMessage(WSMessage{Method: "session_created", Body: map[string]any{"doorbot_id": float64(42), "session_id": "new"}})
	<-sent

	_, ok := nextMessage(t, received)
	require.True(t, ok)
	require.Equal(t, "new", ws.sessionID)
}

func TestConcurrentStopSendsOneClose(t *testing.T) {
	ws, received := newTestSocket(t)
	client := &Client{cameraID: 42, wsClient: ws}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_ = client.Stop()
		})
	}
	wg.Wait()

	require.True(t, client.IsClosed())

	var methods []string
	for {
		msg, ok := nextMessage(t, received)
		if !ok {
			break
		}
		methods = append(methods, msg["method"].(string))
	}
	require.Equal(t, []string{"close"}, methods)
}

func TestDialWithAccessToken(t *testing.T) {
	client, err := Dial("ring:?camera_id=42&device_id=abc&access_token=ab%2Bc%2Fd%3D%3D&hardware_id=hid&snapshot=")
	require.NoError(t, err)
	require.Equal(t, "ab+c/d==", client.api.authToken.AccessToken)
	require.Equal(t, "hid", client.api.hardwareID)
	require.True(t, client.api.externalAuth)

	_, err = Dial("ring:?camera_id=42&device_id=abc&access_token=token&snapshot")
	require.Error(t, err, "without the hardware id ring can't match the token to its session")
}
