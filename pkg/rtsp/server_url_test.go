package rtsp

import (
	"bufio"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/stretchr/testify/require"
)

// The stream of a connection is the one its first DESCRIBE or ANNOUNCE names.
// It was the URL of the first request: a client may start with OPTIONS * or
// the bare server URL (RFC 2326 10.1), its DESCRIBE got 404 and the ANNOUNCE
// of a publisher no stream. A DESCRIBE again stays with the stream it is
// attached to.
func TestServerTakesStreamFromDescribe(t *testing.T) {
	const cam = "rtsp://127.0.0.1:8554/cam?video"
	const sdp = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=control:trackID=0\r\n"

	describe := func(uri string) string {
		return "DESCRIBE " + uri + " RTSP/1.0\r\nCSeq: %d\r\n\r\n"
	}
	announce := func(uri string) string {
		return fmt.Sprintf("ANNOUNCE %s RTSP/1.0\r\nCSeq: %%d\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s", uri, len(sdp), sdp)
	}

	for _, tc := range []struct {
		name     string
		requests []string
	}{
		{"options, describe", []string{"OPTIONS " + cam + " RTSP/1.0\r\nCSeq: %d\r\n\r\n", describe(cam)}},
		{"options *, describe", []string{"OPTIONS * RTSP/1.0\r\nCSeq: %d\r\n\r\n", describe(cam)}},
		{"options without path, describe", []string{"OPTIONS rtsp://127.0.0.1:8554 RTSP/1.0\r\nCSeq: %d\r\n\r\n", describe(cam)}},
		{"options *, announce", []string{"OPTIONS * RTSP/1.0\r\nCSeq: %d\r\n\r\n", announce(cam)}},
		{"describe, describe another", []string{describe(cam), describe("rtsp://127.0.0.1:8554/other")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, client := net.Pipe()
			require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))

			c := NewServer(server)
			c.HandshakeTimeout = 5

			// the URL the handler of the server reads for the stream name, it
			// attaches the connection once, as the stream does
			var streams []string
			c.Listen(func(msg any) {
				if msg == MethodDescribe || msg == MethodAnnounce {
					streams = append(streams, c.URL.String())
				}
				if msg == MethodDescribe && c.Senders == nil {
					codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}
					media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionSendonly, Codecs: []*core.Codec{codec}}
					if err := c.AddTrack(media, codec, core.NewReceiver(media, codec)); err != nil {
						t.Error(err) // in the goroutine of Accept
					}
				}
			})

			accepted := make(chan struct{})
			go func() {
				_ = c.Accept()
				close(accepted)
			}()
			t.Cleanup(func() {
				_ = client.Close()
				<-accepted
				for _, sender := range c.Senders {
					sender.Close()
				}
			})

			reader := bufio.NewReader(client)
			for i, request := range tc.requests {
				_, err := fmt.Fprintf(client, request, i+1)
				require.NoError(t, err)
				res, err := tcp.ReadResponse(reader)
				require.NoError(t, err)
				require.Equal(t, 200, res.StatusCode)
			}

			require.NotEmpty(t, streams)
			for _, stream := range streams {
				require.Equal(t, cam, stream)
			}
		})
	}
}
