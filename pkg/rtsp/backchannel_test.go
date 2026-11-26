package rtsp

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

const backchannelSDP = "v=0\r\n" +
	"o=- 1 1 IN IP4 0.0.0.0\r\n" +
	"s=test\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"t=0 0\r\n" +
	"m=video 0 RTP/AVP 96\r\n" +
	"a=rtpmap:96 H264/90000\r\n" +
	"a=control:trackID=0\r\n" +
	"m=audio 0 RTP/AVP 0\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=control:trackID=1\r\n" +
	"a=sendonly\r\n"

// fakeCamera - RTSP server that records the methods of each session
type fakeCamera struct {
	ln       net.Listener
	mu       sync.Mutex
	sessions [][]string
	played   chan int    // session index after PLAY
	rtp      chan []byte // backchannel RTP received by the camera

	rejectSetup int  // session index that fails on SETUP, -1 for none
	rejectTalk  int  // session index that fails on SETUP of the talk track, -1 for none
	strictTalk  bool // SETUP of the talk track without the Require header fails (2N)

	sessionTimeout int // in the Session header of SETUP, 60 for 0

	requires [][]string // the Require header of each request, by session
}

func newFakeCamera(t *testing.T) *fakeCamera {
	ln, err := net.Listen("tcp", "localhost:0")
	require.Nil(t, err)

	cam := &fakeCamera{ln: ln, played: make(chan int, 10), rtp: make(chan []byte, 10), rejectSetup: -1, rejectTalk: -1}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			cam.mu.Lock()
			cam.sessions = append(cam.sessions, nil)
			cam.requires = append(cam.requires, nil)
			i := len(cam.sessions) - 1
			cam.mu.Unlock()
			go cam.handle(conn, i)
		}
	}()
	return cam
}

func (cam *fakeCamera) methods(i int) []string {
	cam.mu.Lock()
	defer cam.mu.Unlock()
	return append([]string{}, cam.sessions[i]...)
}

func (cam *fakeCamera) handle(conn net.Conn, i int) {
	defer conn.Close()
	r := bufio.NewReader(conn)

	for {
		b, err := r.Peek(1)
		if err != nil {
			return
		}

		if b[0] == '$' {
			header := make([]byte, 4)
			if _, err = io.ReadFull(r, header); err != nil {
				return
			}
			data := make([]byte, int(header[2])<<8|int(header[3]))
			if _, err = io.ReadFull(r, data); err != nil {
				return
			}
			cam.rtp <- data
			continue
		}

		req, err := tcp.ReadRequest(r)
		if err != nil {
			return
		}

		cam.mu.Lock()
		cam.sessions[i] = append(cam.sessions[i], req.Method)
		cam.requires[i] = append(cam.requires[i], req.Header.Get("Require"))
		cam.mu.Unlock()

		talk := req.Method == MethodSetup && strings.HasSuffix(req.URL.Path, "trackID=1")

		res := "RTSP/1.0 200 OK\r\nCSeq: " + req.Header.Get("CSeq") + "\r\n"
		switch {
		case req.Method == MethodSetup && i == cam.rejectSetup, talk && i == cam.rejectTalk:
			res = "RTSP/1.0 453 Not Enough Bandwidth\r\nCSeq: " + req.Header.Get("CSeq") + "\r\n\r\n"
		case talk && cam.strictTalk && req.Header.Get("Require") == "":
			res = "RTSP/1.0 400 Bad Request\r\nCSeq: " + req.Header.Get("CSeq") + "\r\n\r\n"
		case req.Method == MethodDescribe:
			res += "Content-Type: application/sdp\r\nContent-Length: " +
				strconv.Itoa(len(backchannelSDP)) + "\r\n\r\n" + backchannelSDP
		case req.Method == MethodSetup:
			timeout := cam.sessionTimeout
			if timeout == 0 {
				timeout = 60
			}
			res += "Session: " + strconv.Itoa(i+1) + ";timeout=" + strconv.Itoa(timeout) + "\r\nTransport: " +
				req.Header.Get("Transport") + "\r\n\r\n"
		default:
			res += "\r\n"
		}
		if _, err = conn.Write([]byte(res)); err != nil {
			return
		}

		if req.Method == MethodPlay {
			cam.played <- i
		}
	}
}

func waitPlayed(t *testing.T, cam *fakeCamera) int {
	select {
	case i := <-cam.played:
		return i
	case <-time.After(5 * time.Second):
		t.Fatal("PLAY timeout")
	}
	return -1
}

// TestBackchannelSeparateSession - adding a backchannel to a playing stream
// should not reconnect the main session (no glitch in the video)
func TestBackchannelSeparateSession(t *testing.T) {
	Timeout = 5 * time.Second

	cam := newFakeCamera(t)
	defer cam.ln.Close()

	client := NewClient("rtsp://" + cam.ln.Addr().String() + "/stream")
	client.Backchannel = true

	require.Nil(t, client.Dial())
	require.Nil(t, client.Describe())

	medias := client.GetMedias()
	require.Len(t, medias, 2)
	video, backchannel := medias[0], medias[1]
	require.Equal(t, core.DirectionRecvonly, video.Direction)
	require.Equal(t, core.DirectionSendonly, backchannel.Direction)

	// 1. Start the stream with video only
	_, err := client.GetTrack(video, video.Codecs[0])
	require.Nil(t, err)

	go func() { _ = client.Start() }()
	defer client.Stop()

	require.Equal(t, 0, waitPlayed(t, cam))

	// 2. Enable backchannel while playing
	mic := core.NewReceiver(
		&core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly},
		&core.Codec{Name: core.CodecPCMU, ClockRate: 8000, PayloadType: 0},
	)
	require.Nil(t, client.AddTrack(backchannel, backchannel.Codecs[0], mic))

	require.Equal(t, 1, waitPlayed(t, cam))

	// main session is not interrupted
	require.Equal(t, []string{MethodDescribe, MethodSetup, MethodPlay}, cam.methods(0))
	// backchannel session only setups the backchannel media
	require.Equal(t, []string{MethodDescribe, MethodSetup, MethodPlay}, cam.methods(1))

	// 3. Audio from the client is sent to the camera with the backchannel session
	for seq := uint16(0); ; seq++ {
		mic.WriteRTP(&rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 160},
			Payload: make([]byte, 160),
		})

		select {
		case data := <-cam.rtp:
			packet := &rtp.Packet{}
			require.Nil(t, packet.Unmarshal(data))
			// G.711 goes to the camera repacked, as in the main session
			require.Len(t, packet.Payload, 1024)
			goto stop
		case <-time.After(20 * time.Millisecond):
		}

		if seq > 200 {
			t.Fatal("backchannel RTP timeout")
		}
	}

stop:
	// 4. Stop closes both sessions
	require.Nil(t, client.Stop())
	require.Eventually(t, func() bool {
		return len(cam.methods(1)) == 4
	}, time.Second, 10*time.Millisecond, fmt.Sprint(cam.methods(1)))
	// the camera reads each session in its own goroutine, the main one may
	// record its TEARDOWN after the backchannel one
	require.Eventually(t, func() bool {
		methods := cam.methods(0)
		return methods[len(methods)-1] == MethodTeardown
	}, time.Second, 10*time.Millisecond, fmt.Sprint(cam.methods(0)))
	require.Equal(t, MethodTeardown, cam.methods(1)[3])
}

// TestBackchannelFallback - camera doesn't accept a separate backchannel session,
// so the main session is reconnected with the backchannel (old behaviour)
func TestBackchannelFallback(t *testing.T) {
	Timeout = 5 * time.Second

	cam := newFakeCamera(t)
	cam.rejectSetup = 1
	defer cam.ln.Close()

	client := NewClient("rtsp://" + cam.ln.Addr().String() + "/stream")
	client.Backchannel = true

	require.Nil(t, client.Dial())
	require.Nil(t, client.Describe())

	video, backchannel := client.GetMedias()[0], client.GetMedias()[1]

	_, err := client.GetTrack(video, video.Codecs[0])
	require.Nil(t, err)

	go func() { _ = client.Start() }()
	defer client.Stop()

	require.Equal(t, 0, waitPlayed(t, cam))

	mic := core.NewReceiver(
		&core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly},
		&core.Codec{Name: core.CodecPCMU, ClockRate: 8000, PayloadType: 0},
	)
	require.Nil(t, client.AddTrack(backchannel, backchannel.Codecs[0], mic))

	// main session is played again after reconnect with video and backchannel
	require.Equal(t, 2, waitPlayed(t, cam))
	require.Equal(t, []string{MethodDescribe, MethodSetup, MethodSetup, MethodPlay}, cam.methods(2))
	require.Nil(t, client.backchannel.Load())

	// failed backchannel session and old main session are closed
	require.Eventually(t, func() bool {
		return fmt.Sprint(cam.methods(1)) == fmt.Sprint([]string{MethodDescribe, MethodSetup, MethodTeardown}) &&
			fmt.Sprint(cam.methods(0)) == fmt.Sprint([]string{MethodDescribe, MethodSetup, MethodPlay, MethodTeardown})
	}, time.Second, 10*time.Millisecond, "%v %v", cam.methods(0), cam.methods(1))
}

func (cam *fakeCamera) requireHeaders(i int) []string {
	cam.mu.Lock()
	defer cam.mu.Unlock()
	return append([]string{}, cam.requires[i]...)
}

// A 2N intercom refuses the SETUP of the talk track without the Require
// header (#2725), the ONVIF example sends it with each request of the session.
// Talk while playing took the separate session, failed and reconnected the
// main one, and the intercom got no audio.
func TestBackchannelRequireOnEveryRequest(t *testing.T) {
	cam := newFakeCamera(t)
	cam.strictTalk = true
	defer cam.ln.Close()

	client := NewClient("rtsp://" + cam.ln.Addr().String() + "/stream")
	client.Backchannel = true

	require.Nil(t, client.Dial())
	require.Nil(t, client.Describe())

	video, backchannel := client.GetMedias()[0], client.GetMedias()[1]
	_, err := client.GetTrack(video, video.Codecs[0])
	require.Nil(t, err)

	// the session ends before the test, nothing of it reads on afterwards
	ended := make(chan struct{})
	go func() {
		_ = client.Start()
		close(ended)
	}()
	defer func() {
		_ = client.Stop()
		<-ended
	}()

	require.Equal(t, 0, waitPlayed(t, cam))

	mic := core.NewReceiver(
		&core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly},
		&core.Codec{Name: core.CodecPCMU, ClockRate: 8000, PayloadType: 0},
	)
	require.Nil(t, client.AddTrack(backchannel, backchannel.Codecs[0], mic))

	// the talk session plays, the main one is not reconnected
	require.Equal(t, 1, waitPlayed(t, cam))
	require.NotNil(t, client.backchannel.Load())
	require.Equal(t, []string{MethodDescribe, MethodSetup, MethodPlay}, cam.methods(1))

	const tag = "www.onvif.org/ver20/backchannel"
	for i := range 2 {
		for _, header := range cam.requireHeaders(i) {
			require.Equal(t, tag, header, "session %d: %v", i, cam.methods(i))
		}
	}
}

// A camera with a single talk slot refuses the talk track while another
// client holds it. The session goes on without talk: it is neither dialed
// again nor stripped of its other tracks.
func TestBackchannelBusyKeepsTheSession(t *testing.T) {
	cam := newFakeCamera(t)
	cam.rejectTalk = 0
	defer cam.ln.Close()

	client := NewClient("rtsp://" + cam.ln.Addr().String() + "/stream")
	client.Backchannel = true

	require.Nil(t, client.Dial())
	require.Nil(t, client.Describe())

	video, backchannel := client.GetMedias()[0], client.GetMedias()[1]
	_, err := client.GetTrack(video, video.Codecs[0])
	require.Nil(t, err)

	mic := core.NewReceiver(
		&core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly},
		&core.Codec{Name: core.CodecPCMU, ClockRate: 8000, PayloadType: 0},
	)
	err = client.AddTrack(backchannel, backchannel.Codecs[0], mic)
	require.ErrorIs(t, err, ErrBackchannelBusy)

	// the session ends before the test, nothing of it reads on afterwards
	ended := make(chan struct{})
	go func() {
		_ = client.Start()
		close(ended)
	}()
	defer func() {
		_ = client.Stop()
		<-ended
	}()

	require.Equal(t, 0, waitPlayed(t, cam), "the session plays as it is")
	require.Equal(t, []string{MethodDescribe, MethodSetup, MethodSetup, MethodPlay}, cam.methods(0))
	cam.mu.Lock()
	defer cam.mu.Unlock()
	require.Len(t, cam.sessions, 1, "no second dial")
}
