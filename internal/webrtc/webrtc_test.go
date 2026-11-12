package webrtc

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api/ws"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/webrtc"
	"github.com/gorilla/websocket"
	"github.com/pion/stun/v3"
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

func TestWebRTCAPIv1ICEServers(t *testing.T) {
	raw := `{"type":"webrtc/offer","value":"v=0\n...","ice_servers":[{"urls":["turn:turn.example.com:3478"],"username":"user","credential":"pass"}]}`
	msg := new(ws.Message)
	require.NoError(t, json.Unmarshal([]byte(raw), msg))

	require.Equal(t, "v=0\n...", msg.String())
	require.Equal(t, []pion.ICEServer{{URLs: []string{"turn:turn.example.com:3478"}, Username: "user", Credential: "pass"}}, msg.ICEServers)

	answer, err := json.Marshal(&ws.Message{Type: "webrtc/answer", Value: "v=0"})
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"webrtc/answer","value":"v=0"}`, string(answer))
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

	_, err := ExchangeSDP(stream, viewerOffer(t), "webrtc/test", "", "", nil)
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
	answer, err := ExchangeSDP(stream, offer, "webrtc/test", "", "", nil)
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

// connectOverWebsocket runs one offer through asyncHandler and completes ICE
// until the viewer is connected. It returns go2rtc's side of the peer.
func connectOverWebsocket(t *testing.T, tr *ws.Transport, answers, candidates chan string, seq uint64) *webrtc.Conn {
	viewer, offer := connectingViewer(t)
	require.NoError(t, asyncHandler(tr, &ws.Message{Type: "webrtc/offer", Value: offer, Seq: seq}))
	var conn *webrtc.Conn
	tr.WithContext(func(ctx map[any]any) { conn, _ = ctx[lastConnKey].(*webrtc.Conn) })
	require.NotNil(t, conn)
	require.NoError(t, viewer.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: <-answers}))
	require.Eventually(t, func() bool {
		select {
		case candidate := <-candidates:
			_ = viewer.AddICECandidate(pion.ICECandidateInit{Candidate: candidate})
		default:
		}
		return viewerConnected(viewer)()
	}, 5*time.Second, 10*time.Millisecond)
	return conn
}

func signalingTransport(target string) (*ws.Transport, chan string, chan string) {
	answers := make(chan string, 4)
	candidates := make(chan string, 64)
	tr := &ws.Transport{Request: httptest.NewRequest("GET", target, nil)}
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
	return tr, answers, candidates
}

// A share viewer must not keep watching after the relay closed its websocket,
// or reopen sockets to collect more peers than the share allows.
func TestTaggedViewerGoesWithItsWebsocket(t *testing.T) {
	useLoopbackAPI(t, time.Minute)
	const name = "webrtc_tagged_ws_close"
	stream := newStubStream(t, name)

	tr, answers, candidates := signalingTransport("/api/ws?src=" + name + "&tag=share_x")
	connectOverWebsocket(t, tr, answers, candidates, 1)
	require.Equal(t, 1, consumerCount(t, stream))

	tr.Close()
	require.Eventually(t, func() bool { return consumerCount(t, stream) == 0 }, 3*time.Second, 50*time.Millisecond)
}

var wsRoutes sync.Once

// wsServer serves api/ws the way go2rtc does, with the webrtc handlers.
func wsServer(t *testing.T) *httptest.Server {
	wsRoutes.Do(func() {
		ws.Init()
		ws.HandleFunc("webrtc/offer", asyncHandler)
	})
	srv := httptest.NewServer(http.DefaultServeMux)
	t.Cleanup(srv.Close)
	return srv
}

// A revoked share deletes its viewer through the API. The websocket must go
// with it, or the client offers again on it and is back with the same tag.
func TestDeletedTaggedViewerLosesItsWebsocket(t *testing.T) {
	usePionAPI(t, time.Minute)
	const name = "webrtc_tagged_deleted"
	const tag = "share_deleted"
	stream := newStubStream(t, name)

	srv := wsServer(t)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/ws?src="+name+"&tag="+tag, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	require.NoError(t, client.WriteJSON(&ws.Message{Type: "webrtc/offer", Value: viewerOffer(t)}))
	require.Eventually(t, func() bool { return consumerCount(t, stream) == 1 }, 3*time.Second, 50*time.Millisecond)

	require.Equal(t, 1, streams.KillConsumersByTag(tag))

	// read the close before offering again: data sent to a closed socket
	// gets a reset, which may discard the close frame
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	for err == nil {
		_, _, err = client.ReadMessage()
	}

	_ = client.WriteJSON(&ws.Message{Type: "webrtc/offer", Value: viewerOffer(t)})
	require.Never(t, func() bool { return consumerCount(t, stream) != 0 }, time.Second, 50*time.Millisecond,
		"an offer on the same websocket must not attach again (read: %v)", err)
	require.True(t, websocket.IsCloseError(err, websocket.CloseServiceRestart), "the client sees a 1012 close, got %v", err)
}

func TestNewOfferReplacesConnectedTaggedViewer(t *testing.T) {
	useLoopbackAPI(t, time.Minute)
	const name = "webrtc_tagged_reoffer"
	newStubStream(t, name)

	tr, answers, candidates := signalingTransport("/api/ws?src=" + name + "&tag=share_x")
	t.Cleanup(tr.Close)
	first := connectOverWebsocket(t, tr, answers, candidates, 1)
	second := connectOverWebsocket(t, tr, answers, candidates, 2)

	require.Eventually(t, first.IsClosed, 3*time.Second, 50*time.Millisecond)
	require.False(t, second.IsClosed(), "the newer peer stays")
}

// Untagged clients keep a connected peer when they offer again, the camera.ui
// player negotiates its talk peer on the same websocket.
func TestNewOfferKeepsConnectedUntaggedViewer(t *testing.T) {
	useLoopbackAPI(t, time.Minute)
	const name = "webrtc_untagged_reoffer"
	newStubStream(t, name)

	tr, answers, candidates := signalingTransport("/api/ws?src=" + name)
	t.Cleanup(tr.Close)
	first := connectOverWebsocket(t, tr, answers, candidates, 1)
	second := connectOverWebsocket(t, tr, answers, candidates, 2)

	require.Never(t, func() bool { return first.IsClosed() || second.IsClosed() }, 300*time.Millisecond, 50*time.Millisecond)
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

// The selected candidate pair rewrites the viewer's protocol and address while
// the API reports the stream: the stream JSON and the state camera.ui watches.
// Meaningful under -race.
func TestViewerAddressReadWhileConnecting(t *testing.T) {
	useLoopbackAPI(t, time.Minute)
	const name = "webrtc_address_read"
	stream := newStubStream(t, name)

	viewer, offer := connectingViewer(t)
	answer, err := ExchangeSDP(stream, offer, "webrtc/test", "", "", nil)
	require.NoError(t, err)

	// the pair is selected once the viewer has the answer
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = stream.MarshalJSON()
			_ = streams.StateOf(name, stream)
		}
	}()

	require.NoError(t, viewer.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer}))
	require.Eventually(t, viewerConnected(viewer), 5*time.Second, 10*time.Millisecond)

	close(stop)
	<-done

	state := streams.StateOf(name, stream)
	require.Len(t, state.Consumers, 1)
	require.Contains(t, state.Consumers[0].Protocol, "+udp", "the protocol of the selected pair")
	require.NotEmpty(t, state.Consumers[0].RemoteAddr)
}

// A saved config changes the candidates, the ICE servers and the filter of the
// host candidates for the next connections. A candidate added at runtime
// (ngrok) stays, after the ones of the config.
func TestPeerConfigOfSavedConfig(t *testing.T) {
	candidatesMu.Lock()
	before := struct {
		addresses  []*Address
		configured int
		stuns      []string
		filters    webrtc.Filters
	}{addresses, configured, stuns, filters}
	candidatesMu.Unlock()
	conf := pionConf.Load()
	t.Cleanup(func() {
		candidatesMu.Lock()
		addresses, configured, stuns, filters = before.addresses, before.configured, before.stuns, before.filters
		candidatesMu.Unlock()
		pionConf.Store(conf)
	})

	candidates := func() (list []string) {
		candidatesMu.Lock()
		defer candidatesMu.Unlock()
		for _, address := range addresses {
			list = append(list, address.Network+" "+net.JoinHostPort(address.host, address.Port))
		}
		return
	}
	host := func(address string) *pion.ICECandidate {
		return &pion.ICECandidate{Typ: pion.ICECandidateTypeHost, Address: address, Protocol: pion.ICEProtocolUDP}
	}

	reloadPeerConfig([]byte("webrtc:\n  candidates: [192.168.1.10:8555]\n"))
	AddCandidate("tcp", "1.2.3.4:443")
	require.Equal(t, []string{"tcp 192.168.1.10:8555", "udp 192.168.1.10:8555", "tcp 1.2.3.4:443"}, candidates())

	reloadPeerConfig([]byte("webrtc:\n  listen: :8555/udp\n  candidates: [10.0.0.2:8555]\n" +
		"  ice_servers:\n    - urls: [turn:turn.example.com]\n  filters:\n    candidates: [10.0.0.2]\n"))
	require.Equal(t, []string{"udp 10.0.0.2:8555", "tcp 1.2.3.4:443"}, candidates())
	require.Equal(t, []pion.ICEServer{{URLs: []string{"turn:turn.example.com"}}}, pionConf.Load().ICEServers)
	require.False(t, FilterCandidate(host("192.168.1.10")))
	require.True(t, FilterCandidate(host("10.0.0.2")))

	// without the block: the defaults of go2rtc
	reloadPeerConfig([]byte("streams: {}\n"))
	require.Equal(t, []string{"tcp 1.2.3.4:443"}, candidates())
	require.Equal(t, defaultConfig().Mod.IceServers, pionConf.Load().ICEServers)
	require.True(t, FilterCandidate(host("192.168.1.10")))

	// a config that is no YAML for the module changes nothing
	reloadPeerConfig([]byte("webrtc:\n  candidates: 1\n"))
	require.Equal(t, []string{"tcp 1.2.3.4:443"}, candidates())
}

var (
	globalICEServers  = []pion.ICEServer{{URLs: []string{"stun:stun.example.com:3478"}}}
	sessionICEServers = []pion.ICEServer{{URLs: []string{"turn:127.0.0.1:9?transport=tcp"}, Username: "user", Credential: "pass"}}
)

func useConfiguredAPI(t *testing.T) {
	api, err := webrtc.NewAPI()
	require.NoError(t, err)

	oldAPI, oldPC, oldConf := serverAPI, PeerConnection, pionConf.Load()
	serverAPI = api
	pionConf.Store(&pion.Configuration{ICEServers: globalICEServers, SDPSemantics: pion.SDPSemanticsUnifiedPlanWithFallback})
	PeerConnection = func(bool) (*pion.PeerConnection, error) {
		return serverAPI.NewPeerConnection(*pionConf.Load())
	}
	t.Cleanup(func() {
		serverAPI, PeerConnection = oldAPI, oldPC
		pionConf.Store(oldConf)
	})
}

func offerWithICEServers(t *testing.T, name string, iceServers []pion.ICEServer) (*pion.Configuration, error) {
	newStubStream(t, name)
	tr := wsTransport(name)
	t.Cleanup(tr.Close)

	err := asyncHandler(tr, &ws.Message{Type: "webrtc/offer", Value: viewerOffer(t), ICEServers: iceServers})
	var conn *webrtc.Conn
	tr.WithContext(func(ctx map[any]any) { conn, _ = ctx[lastConnKey].(*webrtc.Conn) })
	if conn == nil {
		return nil, err
	}
	conf := conn.Transport.(*pion.PeerConnection).GetConfiguration()
	return &conf, err
}

// The relay comes with the offer of a remote viewer and replaces the
// configured ICE servers for that peer only.
func TestOfferICEServersReplaceConfiguredOnes(t *testing.T) {
	useConfiguredAPI(t)

	conf, err := offerWithICEServers(t, "webrtc_session_ice", sessionICEServers)
	require.NoError(t, err)
	require.Equal(t, sessionICEServers, conf.ICEServers)
	require.Equal(t, pion.SDPSemanticsUnifiedPlanWithFallback, conf.SDPSemantics)
}

func TestOfferWithoutICEServersUsesConfiguredOnes(t *testing.T) {
	useConfiguredAPI(t)

	conf, err := offerWithICEServers(t, "webrtc_configured_ice", nil)
	require.NoError(t, err)
	require.Equal(t, globalICEServers, conf.ICEServers)
}

func TestOfferTURNWithoutCredentialsIsRefused(t *testing.T) {
	useConfiguredAPI(t)

	conf, err := offerWithICEServers(t, "webrtc_session_ice_no_credentials", []pion.ICEServer{{URLs: []string{"turn:127.0.0.1:9"}}})
	require.Error(t, err)
	require.Nil(t, conf)
}

// A STUN server that never answers must not hold the complete answer back
// until pion gives up on it.
func TestCompleteAnswerDoesNotWaitForSilentSTUN(t *testing.T) {
	useConfiguredAPI(t)
	stream := newStubStream(t, "webrtc_silent_stun")

	silent, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = silent.Close() })

	start := time.Now()
	iceServers := []pion.ICEServer{{URLs: []string{"stun:" + silent.LocalAddr().String()}}}
	answer, err := ExchangeSDP(stream, viewerOffer(t), "webrtc/test", "", "", iceServers)
	require.NoError(t, err)
	require.Less(t, time.Since(start), 3*time.Second)
	require.Contains(t, answer, "typ host")
}

// A viewer that offers passive TCP candidates makes pion create an active one
// per local address. They do not belong into the answer.
func TestCompleteAnswerLeavesOutActiveTCP(t *testing.T) {
	useConfiguredAPI(t)
	stream := newStubStream(t, "webrtc_active_tcp")

	offer := viewerOffer(t) + "a=candidate:1 1 tcp 1518280447 192.0.2.10 50000 typ host tcptype passive\r\n"
	answer, err := ExchangeSDP(stream, offer, "webrtc/test", "", "", []pion.ICEServer{})
	require.NoError(t, err)
	require.Contains(t, answer, "typ host")
	require.NotContains(t, answer, "tcptype active")
}

// One answering STUN server is enough: the answer leaves once its candidates
// are in, not when the silent server next to it times out.
func TestCompleteAnswerLeavesAfterReachableSTUN(t *testing.T) {
	useConfiguredAPI(t)
	stream := newStubStream(t, "webrtc_quiet_stun")

	silent, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = silent.Close() })

	server, _ := stunServer(t, 0)

	start := time.Now()
	iceServers := []pion.ICEServer{{URLs: []string{"stun:" + server, "stun:" + silent.LocalAddr().String()}}}
	answer, err := ExchangeSDP(stream, viewerOffer(t), "webrtc/test", "", "", iceServers)
	require.NoError(t, err)
	require.Less(t, time.Since(start), time.Second)
	require.Contains(t, answer, "203.0.113.7")
}

// A STUN server farther away than the pause between the host candidates still
// gets its candidate into the answer
func TestCompleteAnswerWaitsForSlowSTUN(t *testing.T) {
	useConfiguredAPI(t)
	stream := newStubStream(t, "webrtc_slow_stun")

	server, _ := stunServer(t, 500*time.Millisecond)

	iceServers := []pion.ICEServer{{URLs: []string{"stun:" + server}}}
	answer, err := ExchangeSDP(stream, viewerOffer(t), "webrtc/test", "", "", iceServers)
	require.NoError(t, err)
	require.Contains(t, answer, "203.0.113.7")
}

// A candidate that comes in after the answer left doesn't touch the answer
// while it is written (go test -race).
func TestCompleteAnswerIgnoresLateCandidate(t *testing.T) {
	useConfiguredAPI(t)
	stream := newStubStream(t, "webrtc_late_stun")

	server, answered := stunServer(t, 2*time.Second)

	iceServers := []pion.ICEServer{{URLs: []string{"stun:" + server}}}
	answer, err := ExchangeSDP(stream, viewerOffer(t), "webrtc/test", "", "", iceServers)
	require.NoError(t, err)
	require.NotContains(t, answer, "203.0.113.7")

	// the late candidate reaches the gatherer
	<-answered
	time.Sleep(200 * time.Millisecond)
}

// A JSON offer brings the viewer's ICE servers along, like one over the
// websocket
func TestPostedOfferWithICEServers(t *testing.T) {
	useConfiguredAPI(t)
	newStubStream(t, "webrtc_post_ice")
	server, _ := stunServer(t, 0)

	body, err := json.Marshal(map[string]any{
		"type": "offer", "sdp": viewerOffer(t),
		"ice_servers": []pion.ICEServer{{URLs: []string{"stun:" + server}}},
	})
	require.NoError(t, err)
	r := httptest.NewRequest("POST", "/api/webrtc?src=webrtc_post_ice", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	outputWebRTC(w, r)
	require.Equal(t, http.StatusOK, w.Code)

	var answer pion.SessionDescription
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &answer))
	require.Contains(t, answer.SDP, "203.0.113.7")
}

// stunServer answers binding requests after delay, with 203.0.113.7 as the
// address the client is seen from. Answered is closed with the first answer.
func stunServer(t *testing.T, delay time.Duration) (addr string, answered <-chan struct{}) {
	server, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })

	done := make(chan struct{})
	var once sync.Once
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := server.ReadFrom(buf)
			if err != nil {
				return
			}
			request := &stun.Message{Raw: append([]byte{}, buf[:n]...)}
			if request.Decode() != nil {
				continue
			}
			from := addr.(*net.UDPAddr)
			response, err := stun.Build(request, stun.BindingSuccess, &stun.XORMappedAddress{IP: net.IPv4(203, 0, 113, 7), Port: from.Port})
			if err != nil {
				continue
			}
			time.AfterFunc(delay, func() {
				_, _ = server.WriteTo(response.Raw, addr)
				once.Do(func() { close(done) })
			})
		}
	}()
	return server.LocalAddr().String(), done
}
