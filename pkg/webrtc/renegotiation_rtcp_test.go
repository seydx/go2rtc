package webrtc

import (
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// The RTCP of each sender is read once the connection is up: pion's NACK
// responder only runs then. A sender a renegotiation adds comes after that
// and needs its reader too.
func TestRenegotiatedSenderGetsItsRTCP(t *testing.T) {
	api, err := NewAPI()
	require.NoError(t, err)

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)

	conn := NewConn(pc)
	conn.Mode = core.ModePassiveConsumer
	t.Cleanup(func() { _ = conn.Close() })

	pli := make(chan uint32, 16)
	conn.OnSenderRTCP = func(packets []rtcp.Packet) {
		for _, packet := range packets {
			if p, ok := packet.(*rtcp.PictureLossIndication); ok {
				select {
				case pli <- p.MediaSSRC:
				default:
				}
			}
		}
	}

	browser, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = browser.Close() })

	connected := make(chan struct{})
	browser.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			close(connected)
		}
	})

	// 1. The browser watches the video
	_, err = browser.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	require.NoError(t, err)
	require.NoError(t, conn.SetOffer(createOffer(t, browser)))

	video := conn.getMedia("0", core.DirectionSendonly)
	require.NotNil(t, video)
	camera := core.NewReceiver(
		&core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly},
		&core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96},
	)
	require.NoError(t, conn.AddTrack(video, getCodec(video, core.CodecH264), camera))

	answer, err := conn.GetCompleteAnswer(nil, nil)
	require.NoError(t, err)
	require.NoError(t, browser.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}))

	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("connection timeout")
	}

	// 2. It turns the audio on
	_, err = browser.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	require.NoError(t, err)

	medias, err := conn.SetReOffer(createOffer(t, browser))
	require.NoError(t, err)
	require.Len(t, medias, 1)

	mic := core.NewReceiver(
		&core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly},
		&core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2, PayloadType: 111},
	)
	require.NoError(t, conn.AddTrack(medias[0], getCodec(medias[0], core.CodecOpus), mic))

	answer, err = conn.GetAnswer()
	require.NoError(t, err)
	require.NoError(t, browser.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}))

	// 3. RTCP for the new sender reaches it
	ssrc := uint32(conn.getTranseiver(medias[0].ID).Sender().GetParameters().Encodings[0].SSRC)
	timeout := time.After(10 * time.Second)
	for {
		_ = browser.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
		select {
		case got := <-pli:
			if got == ssrc {
				return
			}
		case <-time.After(50 * time.Millisecond):
		case <-timeout:
			t.Fatal("the new sender's RTCP is not read")
		}
	}
}
