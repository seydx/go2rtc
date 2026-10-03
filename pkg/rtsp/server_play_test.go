package rtsp

import (
	"bufio"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// PLAY starts the senders before the server marks the session as playing, so
// the sender goroutines already run while the server sets playOK. Meaningful
// under -race.
func TestServerPlayWhileSendersRun(t *testing.T) {
	server, client := net.Pipe()

	c := NewServer(server)
	c.mode = core.ModePassiveConsumer
	c.HandshakeTimeout = 5 // independent of the package Timeout other tests shrink

	media := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly}
	codec := &core.Codec{Name: core.CodecPCMA, ClockRate: 8000, PayloadType: 8}
	media.Codecs = []*core.Codec{codec}
	track := core.NewReceiver(media, codec)
	require.Nil(t, c.AddTrack(media, codec, track))

	// what SETUP leaves behind
	c.state = StateSetup
	sender := c.Senders[0]
	sender.Media.ID = MethodSetup

	// the sender goroutine outlives Close, and writing reads the package
	// Timeout: let it finish its handler before the test returns
	var handlerMu sync.Mutex
	var handlerDone bool
	handler := sender.Handler
	sender.Handler = func(packet *rtp.Packet) {
		handlerMu.Lock()
		defer handlerMu.Unlock()
		if !handlerDone {
			handler(packet)
		}
	}

	accepted := make(chan error, 1)
	go func() { accepted <- c.Accept() }()

	// the camera keeps sending, also while PLAY is handled
	stop := make(chan struct{})
	fed := make(chan struct{})
	go func() {
		defer close(fed)
		for seq := uint16(0); ; seq++ {
			select {
			case <-stop:
				return
			default:
			}
			track.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 160},
				Payload: make([]byte, 160),
			})
			time.Sleep(time.Millisecond)
		}
	}()

	var gotData atomic.Bool
	go func() {
		r := bufio.NewReader(client)
		for {
			b, err := r.ReadByte()
			if err != nil {
				return
			}
			if b == '$' {
				gotData.Store(true)
				_, _ = io.Copy(io.Discard, r)
				return
			}
		}
	}()

	defer func() {
		close(stop)
		<-fed
		sender.Close()
		handlerMu.Lock()
		handlerDone = true
		handlerMu.Unlock()
		_ = client.Close()
		<-accepted
	}()

	// give the sender goroutine a head start before PLAY arrives
	time.Sleep(20 * time.Millisecond)

	_ = client.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := client.Write([]byte("PLAY rtsp://127.0.0.1/cam RTSP/1.0\r\nCSeq: 1\r\nSession: 1\r\n\r\n"))
	require.Nil(t, err, "the server stopped reading before PLAY")

	require.Eventually(t, gotData.Load, 5*time.Second, time.Millisecond, "no media after PLAY")
}
