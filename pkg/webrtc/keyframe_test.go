package webrtc

import (
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

func TestActiveProducerRequestsKeyframes(t *testing.T) {
	const interval = time.Second

	api, err := NewServerAPI("", "", &Filters{Loopback: true, Networks: []string{"udp4"}})
	require.NoError(t, err)
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
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

	settings := webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	media := &webrtc.MediaEngine{}
	require.NoError(t, media.RegisterDefaultCodecs())
	camera, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings), webrtc.WithMediaEngine(media)).NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = camera.Close() })

	require.NoError(t, camera.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}))
	video, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}, "video", "camera")
	require.NoError(t, err)
	sender, err := camera.AddTrack(video)
	require.NoError(t, err)

	answer, err := camera.CreateAnswer(nil)
	require.NoError(t, err)
	gathered := webrtc.GatheringCompletePromise(camera)
	require.NoError(t, camera.SetLocalDescription(answer))
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("answer ICE timeout")
	}
	require.NoError(t, conn.SetAnswer(camera.LocalDescription().SDP))
	require.Eventually(t, func() bool { return camera.ConnectionState() == webrtc.PeerConnectionStateConnected }, 5*time.Second, 10*time.Millisecond)

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

// TestKeyframeRequest - the browser asks for a keyframe with PLI and FIR, the
// request goes to the source of that track only
func TestKeyframeRequest(t *testing.T) {
	api, err := NewAPI()
	require.Nil(t, err)

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	require.Nil(t, err)

	conn := NewConn(pc)
	conn.Mode = core.ModePassiveConsumer
	defer conn.Close()

	browser, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.Nil(t, err)
	defer browser.Close()

	connected := make(chan struct{})
	browser.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			close(connected)
		}
	})

	for range 2 {
		_, err = browser.AddTransceiverFromKind(
			webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
		)
		require.Nil(t, err)
	}

	require.Nil(t, conn.SetOffer(createOffer(t, browser)))

	// one camera per track
	requests := map[string]chan struct{}{}
	for _, mid := range []string{"0", "1"} {
		media := conn.getMedia(mid, core.DirectionSendonly)
		require.NotNil(t, media)
		codec := getCodec(media, core.CodecH264)
		require.NotNil(t, codec)

		camera := core.NewReceiver(
			&core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly},
			&core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96},
		)
		ch := make(chan struct{}, 100)
		camera.OnKeyframe(func() { ch <- struct{}{} })
		require.Nil(t, conn.AddTrack(media, codec, camera))

		requests[mid] = ch
	}

	answer, err := conn.GetCompleteAnswer(nil, nil)
	require.Nil(t, err)
	require.Nil(t, browser.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}))

	// no media is needed, the browser sends RTCP for the negotiated SSRC
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("connection timeout")
	}
	ssrc := map[string]uint32{}
	for mid := range requests {
		track := conn.GetSenderTrack(mid)
		require.Eventually(t, func() bool {
			track.mu.Lock()
			defer track.mu.Unlock()
			ssrc[mid] = track.ssrc
			return track.ssrc != 0
		}, 5*time.Second, 10*time.Millisecond)
	}

	// RTCP is not retransmitted, so send until the camera is asked
	request := func(mid string, pkt rtcp.Packet) {
		for n := 0; ; n++ {
			require.Less(t, n, 100, "no keyframe request for %T", pkt)
			require.Nil(t, browser.WriteRTCP([]rtcp.Packet{pkt}))

			select {
			case <-requests[mid]:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}

	request("0", &rtcp.PictureLossIndication{MediaSSRC: ssrc["0"]})
	time.Sleep(50 * time.Millisecond)
	require.Empty(t, requests["1"], "PLI for track 0 asked the camera of track 1")

	request("1", &rtcp.FullIntraRequest{FIR: []rtcp.FIREntry{{SSRC: ssrc["1"]}}})
}

// rtcpReads returns a reader that replays errs (nil = PLI packet) and counts
// the remaining reads
func rtcpReads(t *testing.T, errs ...error) (func([]byte) (int, interceptor.Attributes, error), *[]error) {
	pli, err := rtcp.Marshal([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: 1}})
	require.Nil(t, err)

	read := func(b []byte) (int, interceptor.Attributes, error) {
		err := errs[0]
		errs = errs[1:]
		if err != nil {
			return 0, nil, err
		}
		return copy(b, pli), nil, nil
	}
	return read, &errs
}

// TestReadRTCPSkipsBrokenPackets - a failed read must not end the loop,
// a later PLI still reaches the handler
func TestReadRTCPSkipsBrokenPackets(t *testing.T) {
	read, left := rtcpReads(t, errors.New("broken packet"), io.ErrShortBuffer, nil, io.EOF)

	var calls int
	readRTCP(read, newKeyframeTrack(func() { calls++ }))
	require.Equal(t, 1, calls)
	require.Empty(t, *left)
}

// TestReadRTCPWithoutHandler - the NACK responder needs the reads, so the loop
// consumes packets until the reader is closed, also without a handler
func TestReadRTCPWithoutHandler(t *testing.T) {
	read, left := rtcpReads(t, nil, nil, errors.New("broken packet"), nil, io.ErrClosedPipe)

	readRTCP(read, nil)
	require.Empty(t, *left)
}

// newKeyframeTrack returns a track whose source calls handler on a keyframe request
func newKeyframeTrack(handler func()) *Track {
	camera := core.NewReceiver(nil, &core.Codec{})
	camera.OnKeyframe(handler)
	sender := core.NewSender(nil, &core.Codec{})
	sender.Bind(camera)

	track := NewTrack("video")
	track.setSender(sender)
	return track
}

// TestReadRTCPLateSender - a sender set after the loop started gets later PLI
// (ex. AddTrack of a backchannel after the connection)
func TestReadRTCPLateSender(t *testing.T) {
	pli, err := rtcp.Marshal([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: 1}})
	require.Nil(t, err)

	packets := make(chan []byte)
	read := func(b []byte) (int, interceptor.Attributes, error) {
		p, ok := <-packets
		if !ok {
			return 0, nil, io.EOF
		}
		return copy(b, p), nil, nil
	}

	track := NewTrack("video")
	done := make(chan struct{})
	go func() {
		readRTCP(read, track)
		close(done)
	}()

	packets <- pli // the loop is running, no sender yet

	// the first PLI may still meet the new sender, so count instead of expecting one
	var calls atomic.Int32
	track.setSender(newKeyframeTrack(func() { calls.Add(1) }).sender)
	packets <- pli
	close(packets)
	<-done

	require.Positive(t, calls.Load(), "late sender got no keyframe request")
}

// TestReadRTCPStopsAfterTooManyErrors - a reader that only fails ends the loop
func TestReadRTCPStopsAfterTooManyErrors(t *testing.T) {
	var reads int
	read := func([]byte) (int, interceptor.Attributes, error) {
		reads++
		return 0, nil, errors.New("broken packet")
	}
	readRTCP(read, nil)
	require.Equal(t, maxRTCPReadErrors, reads)
}

// TestReadRTCPAfterNackLimiter - the limiter empties a NACK (foreign SSRC), a
// PLI in the same compound still reaches the keyframe reader
func TestReadRTCPAfterNackLimiter(t *testing.T) {
	compound, err := rtcp.Marshal([]rtcp.Packet{
		&rtcp.TransportLayerNack{SenderSSRC: 1, MediaSSRC: 0xdeadbeef, Nacks: []rtcp.NackPair{{PacketID: 7}}},
		&rtcp.PictureLossIndication{SenderSSRC: 1, MediaSSRC: 0x1234},
	})
	require.Nil(t, err)

	i, err := newNackLimiter(nackMaxResendBytesPerSec, nackMaxResendsPerSeq).NewInterceptor("")
	require.Nil(t, err)

	var reads int
	reader := i.BindRTCPReader(interceptor.RTCPReaderFunc(
		func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
			if reads++; reads > 1 {
				return 0, nil, io.EOF
			}
			return copy(b, compound), a, nil
		},
	))

	var calls int
	readRTCP(func(b []byte) (int, interceptor.Attributes, error) {
		return reader.Read(b, nil)
	}, newKeyframeTrack(func() { calls++ }))
	require.Equal(t, 1, calls)
}
