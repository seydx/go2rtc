package rtsp

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTimeout(t *testing.T) {
	Timeout = time.Millisecond

	ln, err := net.Listen("tcp", "localhost:0")
	require.Nil(t, err)

	client := NewClient("rtsp://" + ln.Addr().String() + "/stream")
	client.Backchannel = true

	err = client.Dial()
	require.Nil(t, err)

	err = client.Describe()
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
}

func TestHandshakeTimeout(t *testing.T) {
	oldTimeout := Timeout
	defer func() { Timeout = oldTimeout }()

	Timeout = 5 * time.Second

	conn := &Conn{}
	require.Equal(t, 5*time.Second, conn.handshakeTimeout())

	// the media timeout must not leak into the handshake
	conn.Timeout = 60
	require.Equal(t, 5*time.Second, conn.handshakeTimeout())

	conn.HandshakeTimeout = 15
	require.Equal(t, 15*time.Second, conn.handshakeTimeout())
}

func TestMissedControl(t *testing.T) {
	Timeout = time.Millisecond

	ln, err := net.Listen("tcp", "localhost:0")
	require.Nil(t, err)

	go func() {
		conn, err := ln.Accept()
		require.Nil(t, err)

		b := make([]byte, 8192)
		for {
			n, err := conn.Read(b)
			if err != nil {
				return // client hung up (test finished)
			}

			req := string(b[:n])

			switch req[:4] {
			case "DESC":
				_, _ = conn.Write([]byte(`RTSP/1.0 200 OK
Cseq: 1
Content-Length: 495
Content-Type: application/sdp

v=0
o=- 1 1 IN IP4 0.0.0.0
s=go2rtc/1.2.0
c=IN IP4 0.0.0.0
t=0 0
m=audio 0 RTP/AVP 96
a=rtpmap:96 MPEG4-GENERIC/48000/2
a=fmtp:96 profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3; config=119056E500
m=audio 0 RTP/AVP 97
a=rtpmap:97 OPUS/48000/2
a=fmtp:97 sprop-stereo=1
m=video 0 RTP/AVP 98
a=rtpmap:98 H264/90000
a=fmtp:98 packetization-mode=1; sprop-parameter-sets=Z2QAKaw0yAeAIn5cBagICAoAAAfQAAE4gdDAAjhAACOEF3lxoYAEcIAARwgu8uFA,aO48MAA=; profile-level-id=640029
`))

			case "SETU":
				_, _ = conn.Write([]byte(`RTSP/1.0 200 OK
Transport: RTP/AVP/TCP;unicast;interleaved=4-5
Cseq: 3
Session: 1

`))

			default:
				t.Fail()
			}
		}
	}()

	client := NewClient("rtsp://" + ln.Addr().String() + "/stream")
	client.Backchannel = true

	err = client.Dial()
	require.Nil(t, err)

	err = client.Describe()
	require.Nil(t, err)
	require.Len(t, client.Medias, 3)

	ch, err := client.SetupMedia(client.Medias[2])
	require.Nil(t, err)
	require.Equal(t, ch, byte(4))
}

// A source that only sets the media timeout (ex. cui/reolink #timeout=60) must
// still get a handshake long enough for a plugin bridge that holds DESCRIBE
// while it waits for a slow-waking camera's parameter sets. The reolink bridge
// waits up to 10s; with only the package default (5s) go2rtc abandons the
// DESCRIBE before the bridge answers, and the stream never comes up.
func TestHandshakeCoversSlowDescribe(t *testing.T) {
	oldTimeout := Timeout
	defer func() { Timeout = oldTimeout }()
	Timeout = 5 * time.Second // package default, as in production

	// a bridge that answers DESCRIBE only after 7s (params not ready yet)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.Nil(t, err)
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSlowDescribe(conn, 7*time.Second)
		}
	}()

	// media timeout 60, no handshake override → 5s handshake → abandons at 5s
	c1 := NewClient("rtsp://" + ln.Addr().String() + "/stream")
	c1.Timeout = 60
	require.Nil(t, c1.Dial())
	require.ErrorIs(t, c1.Describe(), os.ErrDeadlineExceeded,
		"a 60s media timeout does not (and must not) rescue the handshake")

	// an explicit handshake_timeout that covers the bridge's wait works
	c2 := NewClient("rtsp://" + ln.Addr().String() + "/stream")
	c2.Timeout = 60
	c2.HandshakeTimeout = 15
	require.Nil(t, c2.Dial())
	require.Nil(t, c2.Describe(), "a handshake_timeout past the bridge's wait lets the stream come up")
}

func serveSlowDescribe(conn net.Conn, delay time.Duration) {
	defer conn.Close()
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		req := string(buf[:n])
		var cseq string
		for _, line := range splitLines(req) {
			if len(line) > 6 && line[:6] == "CSeq: " {
				cseq = line[6:]
			}
		}
		switch {
		case len(req) >= 8 && req[:8] == "OPTIONS ":
			writeReply(conn, cseq, "Public: OPTIONS, DESCRIBE, SETUP, PLAY\r\n", "")
		case len(req) >= 9 && req[:9] == "DESCRIBE ":
			time.Sleep(delay) // bridge holds DESCRIBE waiting for the camera
			sdp := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=x\r\nt=0 0\r\n" +
				"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=control:trackID=0\r\n"
			writeReply(conn, cseq, "Content-Type: application/sdp\r\n", sdp)
			return
		default:
			writeReply(conn, cseq, "", "")
		}
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '\r' && s[i+1] == '\n' {
			out = append(out, s[start:i])
			start = i + 2
		}
	}
	return out
}

func writeReply(conn net.Conn, cseq, headers, body string) {
	resp := "RTSP/1.0 200 OK\r\nCSeq: " + cseq + "\r\n" + headers +
		"Content-Length: " + itoa(len(body)) + "\r\n\r\n" + body
	_, _ = conn.Write([]byte(resp))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
