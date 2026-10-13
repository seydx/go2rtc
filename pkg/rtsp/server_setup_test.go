package rtsp

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/stretchr/testify/require"
)

// RFC 2326: the SETUP of a further track joins the session of the first one.
// A new session ID per track breaks clients that check it.
func TestServerKeepsSessionAcrossSetups(t *testing.T) {
	server, client := net.Pipe()
	require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))

	c := NewServer(server)
	c.HandshakeTimeout = 5
	c.mode = core.ModePassiveConsumer
	for _, codec := range []*core.Codec{
		{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96},
		{Name: core.CodecPCMA, ClockRate: 8000, PayloadType: 8},
	} {
		media := &core.Media{Kind: core.GetKind(codec.Name), Direction: core.DirectionSendonly, Codecs: []*core.Codec{codec}}
		require.NoError(t, c.AddTrack(media, codec, core.NewReceiver(media, codec)))
	}

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
	var sessions []string
	for i := range 2 {
		var header string
		if i > 0 {
			header = "Session: " + sessions[0] + "\r\n"
		}
		_, err := fmt.Fprintf(client, "SETUP rtsp://127.0.0.1/cam/trackID=%d RTSP/1.0\r\nCSeq: %d\r\nTransport: RTP/AVP/TCP;unicast;interleaved=%d-%d\r\n%s\r\n", i, i+1, 2*i, 2*i+1, header)
		require.NoError(t, err)

		res, err := tcp.ReadResponse(reader)
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode)
		require.Equal(t, fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", 2*i, 2*i+1), res.Header.Get("Transport"))

		session, _, _ := strings.Cut(res.Header.Get("Session"), ";")
		require.NotEmpty(t, session)
		sessions = append(sessions, session)
	}

	require.Equal(t, sessions[0], sessions[1], "both tracks are in one session")
}
