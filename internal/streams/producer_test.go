package streams

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/probe"
	"github.com/AlexxIT/go2rtc/pkg/rtsp"
	"github.com/stretchr/testify/require"
)

type fakeCamera struct {
	ln net.Listener

	stallSetup atomic.Bool // SETUP requests get no answer
	silentRTP  atomic.Bool // PLAY succeeds but no RTP is sent
	noAudio    atomic.Bool // DESCRIBE omits the audio media

	dialCount  atomic.Int32
	setupCount atomic.Int32

	mu     sync.Mutex
	conns  []net.Conn
	closed atomic.Bool
}

func newFakeCamera(t *testing.T) *fakeCamera {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	cam := &fakeCamera{ln: ln}
	go cam.acceptLoop()

	t.Cleanup(cam.Close)
	return cam
}

func (c *fakeCamera) URL() string {
	return "rtsp://" + c.ln.Addr().String() + "/stream"
}

func (c *fakeCamera) Close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	_ = c.ln.Close()
	c.mu.Lock()
	for _, conn := range c.conns {
		_ = conn.Close()
	}
	c.mu.Unlock()
}

// dropConns closes all current connections (camera reboot) while the
// listener keeps accepting new ones.
func (c *fakeCamera) dropConns() {
	c.mu.Lock()
	for _, conn := range c.conns {
		_ = conn.Close()
	}
	c.conns = c.conns[:0]
	c.mu.Unlock()
}

func (c *fakeCamera) acceptLoop() {
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return
		}

		c.dialCount.Add(1)
		c.mu.Lock()
		c.conns = append(c.conns, conn)
		c.mu.Unlock()

		go c.serve(conn)
	}
}

func (c *fakeCamera) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	reader := bufio.NewReader(conn)
	var rtpStop chan struct{}

	for {
		method, cseq, transport, err := readRequest(reader)
		if err != nil {
			if rtpStop != nil {
				close(rtpStop)
			}
			return
		}

		switch method {
		case "OPTIONS":
			writeResponse(conn, cseq, "Public: OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN, GET_PARAMETER\r\n", "")

		case "DESCRIBE":
			sdp := "v=0\r\n" +
				"o=- 1 1 IN IP4 127.0.0.1\r\n" +
				"s=Fake\r\n" +
				"t=0 0\r\n" +
				"m=video 0 RTP/AVP 96\r\n" +
				"a=rtpmap:96 H264/90000\r\n" +
				"a=fmtp:96 packetization-mode=1;profile-level-id=42E01F\r\n" +
				"a=control:trackID=0\r\n"
			if !c.noAudio.Load() {
				sdp += "m=audio 0 RTP/AVP 0\r\n" +
					"a=rtpmap:0 PCMU/8000\r\n" +
					"a=control:trackID=1\r\n"
			}
			writeResponse(conn, cseq, "Content-Type: application/sdp\r\n", sdp)

		case "SETUP":
			c.setupCount.Add(1)
			if c.stallSetup.Load() {
				// swallow the request, the client hangs until its own deadline
				continue
			}
			writeResponse(conn, cseq, "Transport: "+transport+"\r\nSession: 12345678;timeout=60\r\n", "")

		case "PLAY":
			writeResponse(conn, cseq, "Session: 12345678\r\nRange: npt=0.000-\r\n", "")
			rtpStop = make(chan struct{})
			go c.sendRTP(conn, rtpStop)

		case "TEARDOWN":
			writeResponse(conn, cseq, "Session: 12345678\r\n", "")

		default: // GET_PARAMETER and other keepalives
			writeResponse(conn, cseq, "Session: 12345678\r\n", "")
		}
	}
}

func (c *fakeCamera) sendRTP(conn net.Conn, stop chan struct{}) {
	var seq uint16
	var ts uint32

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if c.silentRTP.Load() {
				continue
			}

			// single-NAL H264 payload, marker set
			payload := []byte{0x65, 0x88, 0x84, 0x00, 0x01, 0x02, 0x03}
			pkt := make([]byte, 12+len(payload))
			pkt[0] = 0x80
			pkt[1] = 0x60 | 0x80
			binary.BigEndian.PutUint16(pkt[2:], seq)
			binary.BigEndian.PutUint32(pkt[4:], ts)
			binary.BigEndian.PutUint32(pkt[8:], 0x11223344)
			copy(pkt[12:], payload)

			frame := make([]byte, 4+len(pkt))
			frame[0] = '$'
			frame[1] = 0 // RTP channel from interleaved=0-1
			binary.BigEndian.PutUint16(frame[2:], uint16(len(pkt)))
			copy(frame[4:], pkt)

			if _, err := conn.Write(frame); err != nil {
				return
			}

			seq++
			ts += 3000
		}
	}
}

func readRequest(reader *bufio.Reader) (method, cseq, transport string, err error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", "", "", err
	}
	if i := strings.IndexByte(line, ' '); i > 0 {
		method = line[:i]
	}

	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			return "", "", "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return method, cseq, transport, nil
		}
		if v, ok := strings.CutPrefix(line, "CSeq: "); ok {
			cseq = v
		}
		if v, ok := strings.CutPrefix(line, "Transport: "); ok {
			transport = v
		}
	}
}

func writeResponse(w io.Writer, cseq, headers, body string) {
	_, _ = fmt.Fprintf(w, "RTSP/1.0 200 OK\r\nCSeq: %s\r\n%sContent-Length: %d\r\n\r\n%s", cseq, headers, len(body), body)
}

func registerTestRTSPHandler() {
	HandleFunc("rtsp", func(rawURL string) (core.Producer, error) {
		conn := rtsp.NewClient(rawURL)
		conn.Backchannel = false
		if err := conn.Dial(); err != nil {
			return nil, err
		}
		if err := conn.Describe(); err != nil {
			return nil, err
		}
		return conn, nil
	})
}

func speedUpWatchdog(t *testing.T) {
	stale, grace, check := watchdogStaleThreshold, watchdogGraceDuration, watchdogCheckInterval
	watchdogStaleThreshold = 2 * time.Second
	watchdogGraceDuration = 2 * time.Second
	watchdogCheckInterval = 200 * time.Millisecond
	t.Cleanup(func() {
		watchdogStaleThreshold, watchdogGraceDuration, watchdogCheckInterval = stale, grace, check
	})
}

func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func newProbeConsumer() *probe.Probe {
	query, _ := url.ParseQuery("video")
	return probe.Create("test", query)
}

func receiverActive(s *Stream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, prod := range s.producers {
		for _, recv := range prod.receivers {
			if recv != nil && recv.IsActive(time.Second) {
				return true
			}
		}
	}
	return false
}

func TestWedgedSetupFailsBoundedAndRecovers(t *testing.T) {
	registerTestRTSPHandler()

	cam := newFakeCamera(t)
	cam.stallSetup.Store(true)

	stream, err := New("wedge_setup", cam.URL())
	require.NoError(t, err)

	start := time.Now()
	err = stream.AddConsumer(newProbeConsumer())
	require.Error(t, err)
	require.Less(t, time.Since(start), 30*time.Second, "AddConsumer must fail bounded, not hang")

	// camera recovers
	cam.stallSetup.Store(false)

	cons := newProbeConsumer()
	require.NoError(t, stream.AddConsumer(cons))
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }), "data must flow after the camera recovered")

	stream.RemoveConsumer(cons)
}

func TestPartialRecoverySwapsAvailableTracks(t *testing.T) {
	registerTestRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)

	stream, err := New("wedge_partial", cam.URL())
	require.NoError(t, err)

	query, _ := url.ParseQuery("video&audio")
	cons := probe.Create("test", query)
	require.NoError(t, stream.AddConsumer(cons))
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }), "healthy camera must deliver data")

	// camera reboots into a video-only configuration: connection drops,
	// forcing a reconnect against the reduced media set
	dialsBefore := cam.dialCount.Load()
	cam.noAudio.Store(true)
	cam.dropConns()

	require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > dialsBefore }), "reconnect must reach the camera")
	require.True(t, waitUntil(60*time.Second, func() bool { return receiverActive(stream) }), "video must recover although audio is gone")

	stream.RemoveConsumer(cons)
}

func TestSilentCameraWedgesSetupThenRecovers(t *testing.T) {
	registerTestRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)

	stream, err := New("wedge_silent", cam.URL())
	require.NoError(t, err)

	cons := newProbeConsumer()
	require.NoError(t, stream.AddConsumer(cons))
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }), "healthy camera must deliver data")

	// camera half-dies: stops sending RTP, new sessions stall on SETUP
	cam.silentRTP.Store(true)
	cam.stallSetup.Store(true)

	// watchdog interrupts, reconnect attempts run into the SETUP stall
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.setupCount.Load() >= 2 }), "reconnect attempts must reach the camera")
	require.False(t, receiverActive(stream), "no data while the camera is wedged")

	// failed reconnects must escalate into backoff instead of hammering the
	// camera: over 15s of wedge the dial rate has to stay low
	dialsBefore := cam.dialCount.Load()
	time.Sleep(15 * time.Second)
	dialsDuringWedge := cam.dialCount.Load() - dialsBefore
	require.LessOrEqual(t, dialsDuringWedge, int32(6), "reconnect must back off while the camera keeps failing")

	// camera fully recovers
	cam.silentRTP.Store(false)
	cam.stallSetup.Store(false)

	require.True(t, waitUntil(60*time.Second, func() bool { return receiverActive(stream) }), "stream must recover without a restart")

	// the consumer attached before the outage must receive data again
	sendBefore := cons.Send
	require.True(t, waitUntil(10*time.Second, func() bool { return cons.Send > sendBefore }), "existing consumer must receive data after recovery")

	stream.RemoveConsumer(cons)
}
