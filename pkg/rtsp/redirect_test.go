package rtsp

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/stretchr/testify/require"
)

const (
	basicUserPass   = "Basic dXNlcjpwYXNz" // user:pass
	resUnauthorized = "RTSP/1.0 401 Unauthorized\r\nWWW-Authenticate: Basic realm=\"test\""
	resOK           = "RTSP/1.0 200 OK"
)

// fakeServer answers every request with reply. conn numbers the connections
// from 1, a redirect dials a new one.
type fakeServer struct {
	ln    net.Listener
	conns atomic.Int32
}

func newFakeServer(t *testing.T, reply func(conn int32, req *tcp.Request) string) *fakeServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := &fakeServer{ln: ln}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var open []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, conn := range open {
			_ = conn.Close() // the client may still hold it
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
			n := s.conns.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					req, err := tcp.ReadRequest(r)
					if err != nil {
						return
					}
					res := reply(n, req) + "\r\nCSeq: " + req.Header.Get("CSeq") + "\r\n\r\n"
					if _, err = conn.Write([]byte(res)); err != nil {
						return
					}
				}
			}()
		}
	}()

	return s
}

func (s *fakeServer) port() string {
	return s.ln.Addr().(*net.TCPAddr).AddrPort().String()[len("127.0.0.1:"):]
}

func (s *fakeServer) url(path string) string {
	return "rtsp://127.0.0.1:" + s.port() + path
}

// A camera that redirects to itself is no reason to dial forever.
func TestRedirectLoopEnds(t *testing.T) {
	var server *fakeServer
	server = newFakeServer(t, func(conn int32, _ *tcp.Request) string {
		if conn > 10*maxRedirects {
			// the client didn't stop: end it before it runs out of ports
			return "RTSP/1.0 500 Internal Server Error"
		}
		return "RTSP/1.0 302 Found\r\nLocation: " + server.url("/stream")
	})

	client := NewClient(server.url("/stream"))
	require.NoError(t, client.Dial())
	require.EqualError(t, client.Options(), "rtsp: too many redirects")
	require.Equal(t, int32(1+maxRedirects), server.conns.Load(), "every redirect dials once")
}

// Every redirect dials anew, and the new connection authenticates again. The
// retry after the 401 is the same request: it keeps counting its redirects.
func TestRedirectLoopWithAuthEnds(t *testing.T) {
	for _, test := range []struct {
		name, user, challenge string
		authorized            func(req *tcp.Request) bool
	}{
		{
			"basic", "user:pass@", resUnauthorized,
			func(req *tcp.Request) bool { return req.Header.Get("Authorization") == basicUserPass },
		},
		{
			"tp-link", "", "RTSP/1.0 401 Unauthorized\r\nWWW-Authenticate: Digest realm=\"TP-LINK Streaming Media\"",
			func(req *tcp.Request) bool { return req.URL.Host == "127.0.0.1" }, // without the port
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var server *fakeServer
			server = newFakeServer(t, func(conn int32, req *tcp.Request) string {
				if conn > 10*maxRedirects {
					return "RTSP/1.0 500 Internal Server Error"
				}
				if !test.authorized(req) {
					return test.challenge
				}
				return "RTSP/1.0 302 Found\r\nLocation: " + server.url("/stream")
			})

			client := NewClient("rtsp://" + test.user + "127.0.0.1:" + server.port() + "/stream")
			require.NoError(t, client.Dial())
			require.EqualError(t, client.Options(), "rtsp: too many redirects")
			require.Equal(t, int32(1+maxRedirects), server.conns.Load())
		})
	}
}

// The redirects count per request. A reconnect dials the same Conn again, and
// a camera that redirects each dial once has to keep working however often
// the stream reconnects.
func TestRedirectEveryDialKeepsWorking(t *testing.T) {
	var server *fakeServer
	server = newFakeServer(t, func(conn int32, _ *tcp.Request) string {
		if conn%2 == 1 {
			return "RTSP/1.0 302 Found\r\nLocation: " + server.url("/stream")
		}
		return resOK
	})

	client := NewClient(server.url("/stream"))
	for range 2 * maxRedirects {
		require.NoError(t, client.Dial())
		require.NoError(t, client.Options())
	}
	require.Equal(t, int32(4*maxRedirects), server.conns.Load())
}

// A relative Location stays on the host of the request, and so do the
// credentials.
func TestRedirectRelativeLocationKeepsCredentials(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	server := newFakeServer(t, func(_ int32, req *tcp.Request) string {
		if req.Header.Get("Authorization") != basicUserPass {
			return resUnauthorized
		}
		mu.Lock()
		paths = append(paths, req.URL.Path)
		mu.Unlock()
		if req.URL.Path == "/stream" {
			return "RTSP/1.0 302 Found\r\nLocation: /moved"
		}
		return resOK
	})

	client := NewClient("rtsp://user:pass@127.0.0.1:" + server.port() + "/stream")
	require.NoError(t, client.Dial())
	require.NoError(t, client.Options())

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"/stream", "/moved"}, paths)
}

// Another port of the same camera gets the credentials.
func TestRedirectToOtherPortKeepsCredentials(t *testing.T) {
	stream := newFakeServer(t, func(_ int32, req *tcp.Request) string {
		if req.Header.Get("Authorization") != basicUserPass {
			return resUnauthorized
		}
		return resOK
	})
	camera := newFakeServer(t, func(_ int32, req *tcp.Request) string {
		if req.Header.Get("Authorization") != basicUserPass {
			return resUnauthorized
		}
		return "RTSP/1.0 302 Found\r\nLocation: " + stream.url("/stream")
	})

	client := NewClient("rtsp://user:pass@127.0.0.1:" + camera.port() + "/stream")
	require.NoError(t, client.Dial())
	require.NoError(t, client.Options())
}

// Another host never sees the credentials: not in the URL, and not in the
// Authorization header the camera had asked for before it redirected.
func TestRedirectToOtherHostDropsCredentials(t *testing.T) {
	var leaked atomic.Bool
	other := newFakeServer(t, func(_ int32, req *tcp.Request) string {
		if req.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		return resUnauthorized
	})
	camera := newFakeServer(t, func(_ int32, req *tcp.Request) string {
		if req.Header.Get("Authorization") != basicUserPass {
			return resUnauthorized
		}
		// same machine, another host name
		return "RTSP/1.0 302 Found\r\nLocation: rtsp://localhost:" + other.port() + "/stream"
	})

	client := NewClient("rtsp://user:pass@127.0.0.1:" + camera.port() + "/stream")
	require.NoError(t, client.Dial())
	require.EqualError(t, client.Options(), "user/pass not provided")
	require.Positive(t, other.conns.Load(), "the redirect reached the other host")
	require.False(t, leaked.Load(), "the other host got credentials")
	require.False(t, strings.Contains(client.uri, "pass"), "the redirected URL keeps no credentials")
}

// The new connection has no session yet: the one of the old connection
// stays behind with it.
func TestRedirectLeavesSessionBehind(t *testing.T) {
	var sessions []string
	var mu sync.Mutex
	other := newFakeServer(t, func(_ int32, req *tcp.Request) string {
		mu.Lock()
		sessions = append(sessions, req.Header.Get("Session"))
		mu.Unlock()
		return resOK
	})
	camera := newFakeServer(t, func(_ int32, req *tcp.Request) string {
		return "RTSP/1.0 302 Found\r\nLocation: " + other.url("/stream")
	})

	client := NewClient(camera.url("/stream"))
	require.NoError(t, client.Dial())
	client.session = "12345678" // a session the camera had set up
	_, err := client.Do(&tcp.Request{Method: MethodPlay, URL: client.URL})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{""}, sessions)
}
