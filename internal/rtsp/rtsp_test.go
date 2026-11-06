package rtsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/rtsp"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/stretchr/testify/require"
)

const (
	sdpVideo = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n" +
		"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=control:track1\r\n"
	sdpTalk = sdpVideo + "m=audio 0 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\na=sendonly\r\na=control:track2\r\n"
)

// testCamera answers each DESCRIBE with the SDP for the n-th connection, from
// 1, or with 404 for none. It reports the connections the client closed.
type testCamera struct {
	ln     net.Listener
	dialed atomic.Int32
	closed chan int32
}

func newTestCamera(t *testing.T, sdps ...string) *testCamera {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	cam := &testCamera{ln: ln, closed: make(chan int32, 10)}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var open []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, conn := range open {
			_ = conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			open = append(open, conn)
			mu.Unlock()
			n := cam.dialed.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				cam.serve(conn, n, sdps)
			}()
		}
	}()
	return cam
}

func (cam *testCamera) serve(conn net.Conn, n int32, sdps []string) {
	r := bufio.NewReader(conn)
	for {
		req, err := tcp.ReadRequest(r)
		if err != nil {
			// the client closed it, not the test
			if errors.Is(err, io.EOF) {
				cam.closed <- n
			}
			return
		}

		res := "RTSP/1.0 200 OK\r\nCSeq: " + req.Header.Get("CSeq") + "\r\n"
		if req.Method == "DESCRIBE" {
			if int(n) <= len(sdps) && sdps[n-1] != "" {
				sdp := sdps[n-1]
				res += fmt.Sprintf("Content-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s", len(sdp), sdp)
			} else {
				res = "RTSP/1.0 404 Not Found\r\nCSeq: " + req.Header.Get("CSeq") + "\r\n\r\n"
			}
		} else {
			res += "\r\n"
		}
		if _, err = conn.Write([]byte(res)); err != nil {
			return
		}
	}
}

func (cam *testCamera) url(query string) string {
	return "rtsp://" + cam.ln.Addr().String() + "/stream" + query
}

// closedConns returns the connections the client closed, once there are
// want of them, or after a while
func (cam *testCamera) closedConns(want int) (closed []int32) {
	timeout := time.After(time.Second)
	for len(closed) < want {
		select {
		case n := <-cam.closed:
			closed = append(closed, n)
		case <-timeout:
			return
		}
	}
	// none more
	select {
	case n := <-cam.closed:
		closed = append(closed, n)
	case <-time.After(50 * time.Millisecond):
	}
	slices.Sort(closed)
	return
}

// Every DESCRIBE that fails leaves no connection behind: a camera counts an
// open one against its sessions until the process collects it, and refuses
// new clients when there are too many. Only the one that is returned stays.
func TestHandlerClosesFailedDescribes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		query  string
		sdps   []string
		ok     bool
		dialed int32
		closed []int32
	}{
		{"without backchannel", "#backchannel=0", nil, false, 1, []int32{1}},
		{"backchannel refused, then without", "", nil, false, 2, []int32{1, 2}},
		{"backchannel refused, then without works", "", []string{"", sdpVideo}, true, 2, []int32{1}},
		{"no talk track, then without", "", []string{sdpVideo}, false, 2, []int32{1, 2}},
		{"talk track", "", []string{sdpTalk}, true, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cam := newTestCamera(t, tc.sdps...)
			prod, err := rtspHandler(cam.url(tc.query))
			if tc.ok {
				require.NoError(t, err)
				t.Cleanup(func() { _ = prod.(io.Closer).Close() })
			} else {
				require.Error(t, err)
			}
			require.Equal(t, tc.dialed, cam.dialed.Load())
			require.Equal(t, tc.closed, cam.closedConns(len(tc.closed)))
		})
	}
}

// stubCamera is a source with an H264 track that sends nothing
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

func newStubStream(t *testing.T, name string) {
	streams.HandleFunc("rtspstubcam", func(string) (core.Producer, error) {
		return &stubCamera{
			Connection: core.Connection{Medias: []*core.Media{{
				Kind:      core.KindVideo,
				Direction: core.DirectionRecvonly,
				Codecs:    []*core.Codec{{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}},
			}}},
			stopped: make(chan struct{}),
		}, nil
	})
	_, err := streams.New(name, "rtspstubcam://"+name)
	require.NoError(t, err)
	t.Cleanup(func() { streams.Delete(name) })
}

// serveRTSP runs the handler of the RTSP server on a listener of its own
func serveRTSP(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go tcpHandler(rtsp.NewServer(conn))
		}
	}()
	return ln.Addr().String()
}

func consumersOf(t *testing.T, name string) int {
	b, err := streams.Get(name).MarshalJSON()
	require.NoError(t, err)
	var info struct {
		Consumers []json.RawMessage `json:"consumers"`
	}
	require.NoError(t, json.Unmarshal(b, &info))
	return len(info.Consumers)
}

// A client may send DESCRIBE again on its connection (ex. Hikvision ISC). Each
// one attached the connection to the stream once more: the second answer
// listed every track twice, and the disconnect left one attachment behind,
// which kept the camera connected for nobody.
func TestDescribeAgainAttachesOnce(t *testing.T) {
	newStubStream(t, "describe")
	addr := serveRTSP(t)

	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	reader := bufio.NewReader(conn)

	describe := func(cseq int) string {
		_, err := fmt.Fprintf(conn, "DESCRIBE rtsp://%s/describe?video RTSP/1.0\r\nCSeq: %d\r\nAccept: application/sdp\r\n\r\n", addr, cseq)
		require.NoError(t, err)
		res, err := tcp.ReadResponse(reader)
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode)
		return string(res.Body)
	}

	first := describe(1)
	require.Equal(t, first, describe(2))
	require.Equal(t, 1, consumersOf(t, "describe"))

	require.NoError(t, conn.Close())
	require.Eventually(t, func() bool {
		return consumersOf(t, "describe") == 0
	}, 2*time.Second, 10*time.Millisecond, "an attachment outlived the client")
}

// A saved config changes the login, the packet size and the default query for
// the next client of the server, without a restart of go2rtc
func TestServerConfigOfSavedConfig(t *testing.T) {
	before := serverConfig.Load()
	t.Cleanup(func() { serverConfig.Store(before) })

	reloadServerConfig([]byte("rtsp:\n  username: cam\n  password: secret\n  pkt_size: 1200\n  default_query: video\n"))
	conf := serverConfig.Load()
	require.Equal(t, "cam", conf.username)
	require.Equal(t, "secret", conf.password)
	require.Equal(t, uint16(1200), conf.packetSize)
	require.Len(t, conf.medias, 1)
	require.Equal(t, core.KindVideo, conf.medias[0].Kind)

	// without the block: no login, the default query of go2rtc
	reloadServerConfig([]byte("streams: {}\n"))
	conf = serverConfig.Load()
	require.Empty(t, conf.username)
	require.Zero(t, conf.packetSize)
	require.Len(t, conf.medias, 2)

	// a config that is no YAML for the module changes nothing
	reloadServerConfig([]byte("rtsp:\n  username: [cam]\n"))
	require.Same(t, conf, serverConfig.Load())
}
