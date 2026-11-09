package webrtc

import (
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

func TestActiveProducerRequestsKeyframes(t *testing.T) {
	const interval = time.Second

	api, err := NewServerAPI("", "", &Filters{Loopback: true, Networks: []string{"udp4"}})
	require.NoError(t, err)
	pc, err := api.NewPeerConnection(pion.Configuration{})
	require.NoError(t, err)
	conn := NewConn(pc)
	conn.Mode = core.ModeActiveProducer
	conn.KeyframeInterval = interval
	t.Cleanup(func() { _ = conn.Close() })

	offer, err := conn.CreateCompleteOffer([]*core.Media{{
		Kind: core.KindVideo, Direction: core.DirectionRecvonly,
		Codecs: []*core.Codec{{Name: core.CodecH264, ClockRate: 90000}},
	}})
	require.NoError(t, err)

	settings := pion.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]pion.NetworkType{pion.NetworkTypeUDP4})
	media := &pion.MediaEngine{}
	require.NoError(t, media.RegisterDefaultCodecs())
	camera, err := pion.NewAPI(pion.WithSettingEngine(settings), pion.WithMediaEngine(media)).NewPeerConnection(pion.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = camera.Close() })

	require.NoError(t, camera.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: offer}))
	video, err := pion.NewTrackLocalStaticRTP(pion.RTPCodecCapability{
		MimeType: pion.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}, "video", "camera")
	require.NoError(t, err)
	sender, err := camera.AddTrack(video)
	require.NoError(t, err)

	answer, err := camera.CreateAnswer(nil)
	require.NoError(t, err)
	gathered := pion.GatheringCompletePromise(camera)
	require.NoError(t, camera.SetLocalDescription(answer))
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("answer ICE timeout")
	}
	require.NoError(t, conn.SetAnswer(camera.LocalDescription().SDP))
	require.Eventually(t, func() bool { return camera.ConnectionState() == pion.PeerConnectionStateConnected }, 5*time.Second, 10*time.Millisecond)

	plis := make(chan time.Time, 16)
	go func() {
		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			for _, pkt := range pkts {
				if _, ok := pkt.(*rtcp.PictureLossIndication); ok {
					plis <- time.Now()
				}
			}
		}
	}()

	firstPacket := time.Now()
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for timestamp := uint32(0); ; timestamp += 1800 {
			packet := &rtp.Packet{Header: rtp.Header{Version: 2, Marker: true, Timestamp: timestamp}, Payload: []byte{0x65, 0x88, 0x84}}
			if video.WriteRTP(packet) != nil {
				return
			}
			<-ticker.C
		}
	}()

	nextPLI := func() time.Time {
		select {
		case at := <-plis:
			return at
		case <-time.After(3 * interval):
			t.Fatal("no keyframe request")
			return time.Time{}
		}
	}

	first := nextPLI()
	require.Less(t, first.Sub(firstPacket), interval/2, "the first request goes out with the first video packet")

	second := nextPLI()
	require.InDelta(t, interval.Seconds(), second.Sub(first).Seconds(), 0.3)
}
