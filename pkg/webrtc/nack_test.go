package webrtc

import (
	"runtime"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// Based on AlexxIT/go2rtc#2486.

type peerOpts struct {
	kinds []string // sender tracks to offer
	// noReplayProtection lets the receiver accept a duplicate, so a repair can
	// be verified without relying on random packet loss. Production is unchanged.
	noReplayProtection bool
	onTrack            func(*pion.TrackRemote, *pion.RTPReceiver)
}

// newLocalPeers connects a go2rtc Conn to a plain pion peer over loopback ICE.
// The caller closes both.
func newLocalPeers(t *testing.T, opts peerOpts) (*Conn, []*Track, *pion.PeerConnection) {
	t.Helper()

	api, err := NewServerAPI("", "", &Filters{Loopback: true, Networks: []string{"udp4"}})
	require.NoError(t, err)
	pc, err := api.NewPeerConnection(pion.Configuration{})
	require.NoError(t, err)
	conn := NewConn(pc)

	settings := pion.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]pion.NetworkType{pion.NetworkTypeUDP4})
	if opts.noReplayProtection {
		settings.DisableSRTPReplayProtection(true)
	}
	remoteAPI := pion.NewAPI(pion.WithSettingEngine(settings))
	remote, err := remoteAPI.NewPeerConnection(pion.Configuration{})
	require.NoError(t, err)

	if opts.onTrack != nil {
		remote.OnTrack(opts.onTrack)
	}

	var tracks []*Track
	for _, kind := range opts.kinds {
		track := NewTrack(kind)
		_, err = pc.AddTrack(track)
		require.NoError(t, err)
		tracks = append(tracks, track)
	}
	_, err = pc.CreateDataChannel("test", nil)
	require.NoError(t, err)

	offer, err := pc.CreateOffer(nil)
	require.NoError(t, err)
	gathered := pion.GatheringCompletePromise(pc)
	require.NoError(t, pc.SetLocalDescription(offer))
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("offer ICE timeout")
	}
	require.NoError(t, remote.SetRemoteDescription(*pc.LocalDescription()))
	answer, err := remote.CreateAnswer(nil)
	require.NoError(t, err)
	gathered = pion.GatheringCompletePromise(remote)
	require.NoError(t, remote.SetLocalDescription(answer))
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("answer ICE timeout")
	}
	require.NoError(t, pc.SetRemoteDescription(*remote.LocalDescription()))
	require.Eventually(t, func() bool { return pc.ConnectionState() == pion.PeerConnectionStateConnected }, 5*time.Second, 10*time.Millisecond)

	return conn, tracks, remote
}

// Pion's NACK responder only retransmits while RTCP from the senders is read.
// Exercise the real connection lifecycle over local ICE: send a packet, ask for
// it by NACK and check that the very same packet comes back.
func TestConnectionRetransmitsNack(t *testing.T) {
	packets := make(chan *rtp.Packet, 8)
	conn, tracks, remote := newLocalPeers(t, peerOpts{
		kinds:              []string{"video"},
		noReplayProtection: true,
		onTrack: func(track *pion.TrackRemote, _ *pion.RTPReceiver) {
			for {
				packet, _, readErr := track.ReadRTP()
				if readErr != nil {
					return
				}
				select {
				case packets <- packet:
				default:
				}
			}
		},
	})
	t.Cleanup(func() { _ = conn.Close() })
	t.Cleanup(func() { _ = remote.Close() })

	require.NoError(t, tracks[0].WriteRTP(96, &rtp.Packet{
		Header: rtp.Header{Version: 2, Marker: true, Timestamp: 90000}, Payload: []byte{0x65, 0x88, 0x84},
	}))
	var original *rtp.Packet
	select {
	case original = <-packets:
	case <-time.After(2 * time.Second):
		t.Fatal("initial RTP missing")
	}

	require.NoError(t, remote.WriteRTCP([]rtcp.Packet{&rtcp.TransportLayerNack{
		SenderSSRC: 1, MediaSSRC: original.SSRC,
		Nacks: []rtcp.NackPair{{PacketID: original.SequenceNumber}},
	}}))
	select {
	case repaired := <-packets:
		require.Equal(t, original.SequenceNumber, repaired.SequenceNumber)
		require.Equal(t, original.Timestamp, repaired.Timestamp)
		require.Equal(t, original.Payload, repaired.Payload)
	case <-time.After(2 * time.Second):
		t.Fatal("go2rtc did not retransmit the packet requested by NACK")
	}
}

// The RTCP readers run one goroutine per sender. go2rtc runs for months, so
// they must end when the connection closes.
func TestConnectionRTCPReadersExitOnClose(t *testing.T) {
	warmup, _, warmupRemote := newLocalPeers(t, peerOpts{kinds: []string{"video"}})
	_ = warmup.Close()
	_ = warmupRemote.Close()
	time.Sleep(500 * time.Millisecond)
	runtime.GC()
	base := runtime.NumGoroutine()

	for range 5 {
		conn, _, remote := newLocalPeers(t, peerOpts{kinds: []string{"video", "audio"}})
		_ = conn.Close()
		_ = remote.Close()
	}

	require.Eventually(t, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= base+2
	}, 4*time.Second, 100*time.Millisecond, "sender RTCP readers must exit when the connection closes")
}
