package webrtc

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
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

func TestWebRTCAPIv1(t *testing.T) {
	raw := `{"type":"webrtc/offer","value":"v=0\n..."}`
	msg := new(ws.Message)
	err := json.Unmarshal([]byte(raw), msg)
	require.Nil(t, err)

	require.Equal(t, "v=0\n...", msg.String())
}

func TestWebRTCAPIv2(t *testing.T) {
	raw := `{"type":"webrtc","value":{"type":"offer","sdp":"v=0\n...","ice_servers":[{"urls":["stun:stun.l.google.com:19302"]}]}}`
	msg := new(ws.Message)
	err := json.Unmarshal([]byte(raw), msg)
	require.Nil(t, err)

	var offer struct {
		Type       string           `json:"type"`
		SDP        string           `json:"sdp"`
		ICEServers []pion.ICEServer `json:"ice_servers"`
	}
	err = msg.Unmarshal(&offer)
	require.Nil(t, err)

	require.Equal(t, "offer", offer.Type)
	require.Equal(t, "v=0\n...", offer.SDP)
	require.Equal(t, "stun:stun.l.google.com:19302", offer.ICEServers[0].URLs[0])
}

func TestCrealitySDP(t *testing.T) {
	sdp := `v=0
o=- 1495799811084970 1495799811084970 IN IP4 0.0.0.0
s=-
t=0 0
a=msid-semantic:WMS *
a=group:BUNDLE 0
m=video 9 UDP/TLS/RTP/SAVPF 96 98
a=rtcp-fb:98 nack
a=rtcp-fb:98 nack pli
a=fmtp:96 profile-level-id=42e01f;level-asymmetry-allowed=1
a=fmtp:98 profile-level-id=42e01f;packetization-mode=1;level-asymmetry-allowed=1
a=fmtp:98 x-google-max-bitrate=6000;x-google-min-bitrate=2000;x-google-start-bitrate=4000
a=rtpmap:96 H264/90000
a=rtpmap:98 H264/90000
a=ssrc:1 cname:pear
c=IN IP4 0.0.0.0
a=sendonly
a=mid:0
a=rtcp-mux
a=ice-ufrag:7AVa
a=ice-pwd:T+F/5y05Paw+mtG5Jrd8N3
a=ice-options:trickle
a=fingerprint:sha-256 A5:AB:C0:4E:29:5B:BD:3B:7D:88:24:6C:56:6B:2A:79:A3:76:99:35:57:75:AD:C8:5A:A6:34:20:88:1B:68:EF
a=setup:passive
a=candidate:1 1 UDP 2015363327 172.22.233.10 48929 typ host
a=candidate:2 1 TCP 1015021823 172.22.233.10 0 typ host tcptype active
a=candidate:3 1 TCP 1010827519 172.22.233.10 60677 typ host tcptype passive
`
	sdp, err := fixCrealitySDP(sdp)
	require.Nil(t, err)
	require.False(t, strings.Contains(sdp, "x-google-max-bitrate"))
}

type stubCamera struct {
	core.Connection
	stopped chan struct{}
	once    sync.Once
}

func (c *stubCamera) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	receiver := core.NewReceiver(media, codec)
	c.Receivers = append(c.Receivers, receiver)
	return receiver, nil
}

func (c *stubCamera) Start() error {
	<-c.stopped
	return nil
}

func (c *stubCamera) Stop() error {
	c.once.Do(func() { close(c.stopped) })
	return c.Connection.Stop()
}

func newStubStream(t *testing.T, name string) *streams.Stream {
	streams.HandleFunc("stubwebrtccam", func(string) (core.Producer, error) {
		return &stubCamera{
			Connection: core.Connection{Medias: []*core.Media{{
				Kind:      core.KindVideo,
				Direction: core.DirectionRecvonly,
				Codecs:    []*core.Codec{{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}},
			}}},
			stopped: make(chan struct{}),
		}, nil
	})
	stream, err := streams.New(name, "stubwebrtccam://"+name)
	require.NoError(t, err)
	t.Cleanup(func() { streams.Delete(name) })
	return stream
}

func usePionAPI(t *testing.T, deadline time.Duration) {
	api, err := webrtc.NewAPI()
	require.NoError(t, err)

	oldPC, oldDeadline := PeerConnection, connectDeadline
	PeerConnection = func(bool) (*pion.PeerConnection, error) {
		return api.NewPeerConnection(pion.Configuration{})
	}
	connectDeadline = deadline
	t.Cleanup(func() { PeerConnection, connectDeadline = oldPC, oldDeadline })
}

// viewerOffer is the offer of a browser that will never get through: its
// candidates never reach go2rtc, ICE can't even start.
func viewerOffer(t *testing.T) string {
	api, err := webrtc.NewAPI()
	require.NoError(t, err)
	pc, err := api.NewPeerConnection(pion.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })

	_, err = pc.AddTransceiverFromKind(pion.RTPCodecTypeVideo, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly})
	require.NoError(t, err)
	offer, err := pc.CreateOffer(nil)
	require.NoError(t, err)
	return offer.SDP
}

func consumerCount(t *testing.T, stream *streams.Stream) int {
	data, err := stream.MarshalJSON()
	require.NoError(t, err)
	var info struct {
		Consumers []json.RawMessage `json:"consumers"`
	}
	require.NoError(t, json.Unmarshal(data, &info))
	return len(info.Consumers)
}

func wsTransport(name string) *ws.Transport {
	tr := &ws.Transport{Request: httptest.NewRequest("GET", "/api/ws?src="+name, nil)}
	tr.OnWrite(func(any) error { return nil })
	return tr
}

// pion notices a peer that never connects only after about 30 s. Until then
// the consumer holds its tracks and a full packet queue, and its microphone
// stays a mixer input.
func TestUnconnectedViewerIsDroppedAfterDeadline(t *testing.T) {
	usePionAPI(t, 300*time.Millisecond)
	stream := newStubStream(t, "webrtc_unconnected_http")

	_, err := ExchangeSDP(stream, viewerOffer(t), "webrtc/test", "", "")
	require.NoError(t, err)
	require.Equal(t, 1, consumerCount(t, stream))

	require.Eventually(t, func() bool { return consumerCount(t, stream) == 0 }, 3*time.Second, 50*time.Millisecond)
}

func TestUnconnectedViewerGoesWithItsWebsocket(t *testing.T) {
	usePionAPI(t, time.Minute)
	const name = "webrtc_unconnected_ws"
	stream := newStubStream(t, name)

	tr := wsTransport(name)
	require.NoError(t, asyncHandler(tr, &ws.Message{Type: "webrtc/offer", Value: viewerOffer(t)}))
	require.Equal(t, 1, consumerCount(t, stream))

	tr.Close()
	require.Eventually(t, func() bool { return consumerCount(t, stream) == 0 }, 3*time.Second, 50*time.Millisecond)
}

// The client gave up on its first peer and offers again on the same websocket.
func TestNewOfferReplacesUnconnectedViewer(t *testing.T) {
	usePionAPI(t, time.Minute)
	const name = "webrtc_unconnected_reoffer"
	stream := newStubStream(t, name)

	tr := wsTransport(name)
	t.Cleanup(tr.Close)
	require.NoError(t, asyncHandler(tr, &ws.Message{Type: "webrtc/offer", Value: viewerOffer(t)}))
	require.NoError(t, asyncHandler(tr, &ws.Message{Type: "webrtc/offer", Value: viewerOffer(t)}))

	require.Eventually(t, func() bool { return consumerCount(t, stream) == 1 }, 3*time.Second, 50*time.Millisecond)
	require.Never(t, func() bool { return consumerCount(t, stream) != 1 }, 300*time.Millisecond, 50*time.Millisecond)
}

// Handlers run concurrently, so the client's older offer can get through
// after its newer one.
func TestOlderOfferFinishingLastKeepsNewerViewer(t *testing.T) {
	usePionAPI(t, time.Minute)
	const name = "webrtc_reoffer_order"
	stream := newStubStream(t, name)

	tr := wsTransport(name)
	t.Cleanup(tr.Close)
	require.NoError(t, asyncHandler(tr, &ws.Message{Type: "webrtc/offer", Value: viewerOffer(t), Seq: 2}))
	var newer *webrtc.Conn
	tr.WithContext(func(ctx map[any]any) { newer, _ = ctx[lastConnKey].(*webrtc.Conn) })
	require.NotNil(t, newer)

	require.NoError(t, asyncHandler(tr, &ws.Message{Type: "webrtc/offer", Value: viewerOffer(t), Seq: 1}))

	require.Eventually(t, func() bool { return consumerCount(t, stream) == 1 }, 3*time.Second, 50*time.Millisecond)
	require.False(t, newer.IsClosed(), "the newer offer's peer stays")
}

// useLoopbackAPI lets go2rtc reach a viewer on this machine.
func useLoopbackAPI(t *testing.T, deadline time.Duration) {
	api, err := webrtc.NewServerAPI("", "", &webrtc.Filters{Loopback: true, Networks: []string{"udp4"}})
	require.NoError(t, err)

	oldPC, oldDeadline := PeerConnection, connectDeadline
	PeerConnection = func(bool) (*pion.PeerConnection, error) {
		return api.NewPeerConnection(pion.Configuration{})
	}
	connectDeadline = deadline
	t.Cleanup(func() { PeerConnection, connectDeadline = oldPC, oldDeadline })
}

// connectingViewer is a browser on this machine. Its offer carries all of its
// candidates, it connects once it has go2rtc's answer and candidates.
func connectingViewer(t *testing.T) (*pion.PeerConnection, string) {
	settings := pion.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]pion.NetworkType{pion.NetworkTypeUDP4})
	pc, err := pion.NewAPI(pion.WithSettingEngine(settings)).NewPeerConnection(pion.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })

	_, err = pc.AddTransceiverFromKind(pion.RTPCodecTypeVideo, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly})
	require.NoError(t, err)
	offer, err := pc.CreateOffer(nil)
	require.NoError(t, err)
	gathered := pion.GatheringCompletePromise(pc)
	require.NoError(t, pc.SetLocalDescription(offer))
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("viewer ICE gathering timeout")
	}
	return pc, pc.LocalDescription().SDP
}

func viewerConnected(pc *pion.PeerConnection) func() bool {
	return func() bool { return pc.ConnectionState() == pion.PeerConnectionStateConnected }
}

// The deadline is for peers that never get through, a connected viewer keeps
// watching past it.
func TestConnectedViewerOutlivesDeadline(t *testing.T) {
	const deadline = 2 * time.Second
	useLoopbackAPI(t, deadline)
	stream := newStubStream(t, "webrtc_connected_http")

	viewer, offer := connectingViewer(t)
	start := time.Now()
	answer, err := ExchangeSDP(stream, offer, "webrtc/test", "", "")
	require.NoError(t, err)
	require.NoError(t, viewer.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer}))
	require.Eventually(t, viewerConnected(viewer), deadline, 10*time.Millisecond)

	time.Sleep(time.Until(start.Add(deadline + 500*time.Millisecond)))
	require.Equal(t, 1, consumerCount(t, stream), "a connected viewer stays past the deadline")
	require.Equal(t, pion.PeerConnectionStateConnected, viewer.ConnectionState())
}

// A connected peer outlives its signaling: the websocket may close once the
// media flows, and the deadline doesn't apply either.
func TestConnectedViewerOutlivesItsWebsocket(t *testing.T) {
	const deadline = 2 * time.Second
	useLoopbackAPI(t, deadline)
	const name = "webrtc_connected_ws"
	stream := newStubStream(t, name)

	viewer, offer := connectingViewer(t)

	answers := make(chan string, 1)
	candidates := make(chan string, 16)
	tr := &ws.Transport{Request: httptest.NewRequest("GET", "/api/ws?src="+name, nil)}
	tr.OnWrite(func(msg any) error {
		if m, ok := msg.(*ws.Message); ok {
			switch m.Type {
			case "webrtc/answer":
				answers <- m.Value.(string)
			case "webrtc/candidate":
				candidates <- m.Value.(string)
			}
		}
		return nil
	})

	start := time.Now()
	require.NoError(t, asyncHandler(tr, &ws.Message{Type: "webrtc/offer", Value: offer}))
	require.NoError(t, viewer.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: <-answers}))
	require.Eventually(t, func() bool {
		select {
		case candidate := <-candidates:
			_ = viewer.AddICECandidate(pion.ICECandidateInit{Candidate: candidate})
		default:
		}
		return viewerConnected(viewer)()
	}, deadline, 10*time.Millisecond)

	tr.Close()
	time.Sleep(time.Until(start.Add(deadline + 500*time.Millisecond)))
	require.Equal(t, 1, consumerCount(t, stream), "a connected viewer stays without its websocket")
	require.Equal(t, pion.PeerConnectionStateConnected, viewer.ConnectionState())
}

// The websocket stays open, the peer never connects: the deadline applies on
// the websocket path just as on HTTP.
func TestUnconnectedViewerOnOpenWebsocketIsDroppedAfterDeadline(t *testing.T) {
	usePionAPI(t, 300*time.Millisecond)
	const name = "webrtc_unconnected_ws_deadline"
	stream := newStubStream(t, name)

	tr := wsTransport(name)
	t.Cleanup(tr.Close)
	require.NoError(t, asyncHandler(tr, &ws.Message{Type: "webrtc/offer", Value: viewerOffer(t)}))
	require.Equal(t, 1, consumerCount(t, stream))

	require.Eventually(t, func() bool { return consumerCount(t, stream) == 0 }, 3*time.Second, 50*time.Millisecond)
}
