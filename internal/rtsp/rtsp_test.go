package rtsp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
