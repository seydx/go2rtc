package mp4

import (
	"encoding/json"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api/ws"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// switchCam is a camera whose codec is decided at dial time. hangup drops the
// session like a camera rebooting into new settings.
type switchCam struct {
	core.Connection
	hangup chan struct{}
	once   sync.Once
}

func (c *switchCam) Start() error {
	<-c.hangup
	return nil
}

func (c *switchCam) Stop() error {
	c.drop()
	return c.Connection.Stop()
}

func (c *switchCam) drop() {
	c.once.Do(func() { close(c.hangup) })
}

type camera struct {
	codec atomic.Value // string
	conn  atomic.Pointer[switchCam]
}

func newCamera(t *testing.T, scheme, codec string) *camera {
	cam := &camera{}
	cam.codec.Store(codec)
	streams.HandleFunc(scheme, func(string) (core.Producer, error) {
		conn := &switchCam{
			Connection: core.Connection{Medias: []*core.Media{{
				Kind:      core.KindVideo,
				Direction: core.DirectionRecvonly,
				Codecs: []*core.Codec{{
					Name:        cam.codec.Load().(string),
					ClockRate:   90000,
					PayloadType: core.PayloadTypeRAW,
				}},
			}}},
			hangup: make(chan struct{}),
		}
		cam.conn.Store(conn)
		return conn, nil
	})
	return cam
}

// reconfigure switches the camera's codec and drops the running session.
func (c *camera) reconfigure(codec string) {
	c.codec.Store(codec)
	c.conn.Load().drop()
}

// transport wires a ws.Transport the way api/ws does: a server-side
// disconnect closes the socket, which runs the close handlers.
func transport(src string) (*ws.Transport, *atomic.Bool) {
	var disconnected atomic.Bool
	tr := &ws.Transport{Request: httptest.NewRequest("GET", "/api/ws?src="+src, nil)}
	tr.OnWrite(func(any) error { return nil })
	tr.OnDisconnect(func() {
		disconnected.Store(true)
		tr.Close()
	})
	return tr, &disconnected
}

func consumerCount(t *testing.T, name string) int {
	t.Helper()
	data, err := json.Marshal(streams.Get(name))
	require.NoError(t, err)
	var info struct {
		Consumers []json.RawMessage `json:"consumers"`
	}
	require.NoError(t, json.Unmarshal(data, &info))
	return len(info.Consumers)
}

// An MSE viewer negotiated its init segment for h264. The camera switches to
// h265: the stream evicts the viewer and the websocket is closed, so the
// player reconnects and negotiates the new codec.
func TestMSEViewerDisconnectedOnCodecChange(t *testing.T) {
	cam := newCamera(t, "mseswitch", core.CodecH264)
	_, err := streams.New("mse_codec_switch", "mseswitch://cam")
	require.NoError(t, err)

	tr, disconnected := transport("mse_codec_switch")
	require.NoError(t, handlerWSMSE(tr, &ws.Message{Type: "mse"}))
	require.Equal(t, 1, consumerCount(t, "mse_codec_switch"))

	cam.reconfigure(core.CodecH265)

	require.Eventually(t, disconnected.Load, 15*time.Second, 50*time.Millisecond, "the websocket must be closed")
	require.Equal(t, 0, consumerCount(t, "mse_codec_switch"), "the viewer must be removed from the stream")
}

// mse/stop removes the consumer on the client's request (auto mode settled
// on WebRTC). The websocket carries the rest of the session and stays open.
func TestMSEStopKeepsWebsocketOpen(t *testing.T) {
	newCamera(t, "msestop", core.CodecH264)
	_, err := streams.New("mse_stop", "msestop://cam")
	require.NoError(t, err)

	tr, disconnected := transport("mse_stop")
	require.NoError(t, handlerWSMSE(tr, &ws.Message{Type: "mse"}))
	require.Equal(t, 1, consumerCount(t, "mse_stop"))

	require.NoError(t, handlerWSMSEStop(tr, &ws.Message{Type: "mse/stop"}))
	require.Equal(t, 0, consumerCount(t, "mse_stop"))

	time.Sleep(200 * time.Millisecond)
	require.False(t, disconnected.Load(), "mse/stop must not close the websocket")
}

// A re-sent mse replaces the previous consumer on the same websocket: also
// not an eviction.
func TestMSEResendKeepsWebsocketOpen(t *testing.T) {
	newCamera(t, "mseresend", core.CodecH264)
	_, err := streams.New("mse_resend", "mseresend://cam")
	require.NoError(t, err)

	tr, disconnected := transport("mse_resend")
	require.NoError(t, handlerWSMSE(tr, &ws.Message{Type: "mse"}))
	require.NoError(t, handlerWSMSE(tr, &ws.Message{Type: "mse"}))
	require.Equal(t, 1, consumerCount(t, "mse_resend"))

	time.Sleep(200 * time.Millisecond)
	require.False(t, disconnected.Load(), "replacing the consumer must not close the websocket")
}
