package webrtc

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api/ws"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/webrtc"
	pion "github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// guardedCamera sets up its tracks while the API reads them, like the real
// producers do
type guardedCamera struct {
	stubCamera
	mu sync.Mutex
}

func (c *guardedCamera) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stubCamera.GetTrack(media, codec)
}

func (c *guardedCamera) MarshalJSON() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return json.Marshal(&c.Connection)
}

func newStubAVStream(t *testing.T, name string) *streams.Stream {
	streams.HandleFunc("stubwebrtcavcam", func(string) (core.Producer, error) {
		return &guardedCamera{stubCamera: stubCamera{
			Connection: core.Connection{Medias: []*core.Media{{
				Kind:      core.KindVideo,
				Direction: core.DirectionRecvonly,
				Codecs:    []*core.Codec{{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}},
			}, {
				Kind:      core.KindAudio,
				Direction: core.DirectionRecvonly,
				Codecs:    []*core.Codec{{Name: core.CodecPCMU, ClockRate: 8000}},
			}}},
			stopped: make(chan struct{}),
		}}, nil
	})
	stream, err := streams.New(name, "stubwebrtcavcam://"+name)
	require.NoError(t, err)
	t.Cleanup(func() { streams.Delete(name) })
	return stream
}

func lastConn(tr *ws.Transport) (conn *webrtc.Conn) {
	tr.WithContext(func(ctx map[any]any) { conn, _ = ctx[lastConnKey].(*webrtc.Conn) })
	return
}

// A browser that turns the audio on later offers again from the same peer,
// on its websocket. go2rtc answers it for the peer it has: the viewer keeps
// watching and gets the audio too, no second peer comes up.
func TestReofferRenegotiatesTheConnectedViewer(t *testing.T) {
	useLoopbackAPI(t, time.Minute)
	const name = "webrtc_renegotiate"
	stream := newStubAVStream(t, name)

	tr, answers, candidates := signalingTransport("/api/ws?src=" + name)
	t.Cleanup(tr.Close)

	viewer, offer := connectingViewer(t)
	require.NoError(t, asyncHandler(tr, &ws.Message{Type: "webrtc/offer", Value: offer, Seq: 1}))
	require.NoError(t, viewer.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: <-answers}))
	require.Eventually(t, func() bool {
		select {
		case candidate := <-candidates:
			_ = viewer.AddICECandidate(pion.ICECandidateInit{Candidate: candidate})
		default:
		}
		return viewerConnected(viewer)()
	}, 5*time.Second, 10*time.Millisecond)

	conn := lastConn(tr)
	require.NotNil(t, conn)
	_, senders := conn.Tracks()
	require.Len(t, senders, 1, "video only")

	// camera.ui polls the stream while the viewer renegotiates
	stop := make(chan struct{})
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = stream.MarshalJSON()
			_ = streams.StatsOf(stream)
		}
	}()

	_, err := viewer.AddTransceiverFromKind(pion.RTPCodecTypeAudio, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly})
	require.NoError(t, err)
	reoffer, err := viewer.CreateOffer(nil)
	require.NoError(t, err)
	require.NoError(t, viewer.SetLocalDescription(reoffer))

	require.NoError(t, asyncHandler(tr, &ws.Message{Type: "webrtc/offer", Value: reoffer.SDP, Seq: 2}))
	require.NoError(t, viewer.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: <-answers}))

	close(stop)
	<-polled

	require.Same(t, conn, lastConn(tr))
	require.Equal(t, 1, consumerCount(t, stream))
	_, senders = conn.Tracks()
	require.Len(t, senders, 2, "video and audio")
	require.False(t, conn.IsClosed())
	require.Never(t, func() bool { return !viewerConnected(viewer)() }, 500*time.Millisecond, 50*time.Millisecond)
}
