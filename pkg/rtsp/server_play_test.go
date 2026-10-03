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

// playingServer runs a server-side consumer over a pipe the way internal/rtsp
// does (Accept up to PLAY, then Handle): one audio track fed like a camera
// every millisecond, SETUP and PLAY from the client, and it returns once media
// arrives. The sender goroutine already runs while PLAY is handled.
func playingServer(t *testing.T) *playing {
	server, client := net.Pipe()

	c := NewServer(server)
	c.HandshakeTimeout = 5 // independent of the package Timeout other tests shrink

	media := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly}
	codec := &core.Codec{Name: core.CodecPCMA, ClockRate: 8000, PayloadType: 8}
	media.Codecs = []*core.Codec{codec}
	track := core.NewReceiver(media, codec)

	// the stream adds its tracks on DESCRIBE, before the client sets them up
	c.mode = core.ModePassiveConsumer
	require.Nil(t, c.AddTrack(media, codec, track))
	sender := c.Senders[0]

	// hold lets the sender goroutine pause before its next packet, release
	// lets it go on
	var gateMu sync.Mutex
	var gate chan struct{}
	hold := func() {
		gateMu.Lock()
		gate = make(chan struct{})
		gateMu.Unlock()
	}
	release := func() {
		gateMu.Lock()
		if gate != nil {
			close(gate)
			gate = nil
		}
		gateMu.Unlock()
	}

	// the sender goroutine outlives Close, and writing reads the package
	// Timeout: let it finish its handler before the test returns
	var handlerMu sync.Mutex
	var handlerDone bool
	handler := sender.Handler
	sender.Handler = func(packet *rtp.Packet) {
		gateMu.Lock()
		g := gate
		gateMu.Unlock()
		if g != nil {
			<-g
		}

		handlerMu.Lock()
		defer handlerMu.Unlock()
		if !handlerDone {
			handler(packet)
		}
	}

	done := make(chan struct{})
	go func() {
		if c.Accept() == nil {
			_ = c.Handle()
		}
		close(done)
	}()

	// the camera keeps sending, also while SETUP and PLAY are handled
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

	t.Cleanup(func() {
		release()
		close(stop)
		<-fed
		sender.Close()
		handlerMu.Lock()
		handlerDone = true
		handlerMu.Unlock()
		_ = client.Close()
		<-done
	})

	request(t, client, done, "SETUP rtsp://127.0.0.1/cam/trackID=0 RTSP/1.0\r\nCSeq: 1\r\nTransport: RTP/AVP/TCP;unicast;interleaved=0-1\r\n\r\n")

	// give the sender goroutine a head start before PLAY arrives
	time.Sleep(20 * time.Millisecond)

	request(t, client, done, "PLAY rtsp://127.0.0.1/cam RTSP/1.0\r\nCSeq: 2\r\nSession: 1\r\n\r\n")
	require.Eventually(t, gotData.Load, 5*time.Second, time.Millisecond, "no media after PLAY")

	return &playing{c: c, client: client, served: done, track: track, hold: hold, release: release}
}

type playing struct {
	c       *Conn
	client  net.Conn
	served  <-chan struct{} // closed when the server stops reading
	track   *core.Receiver
	hold    func() // pause the sender goroutine before its next packet
	release func()
}

func request(t *testing.T, client net.Conn, served <-chan struct{}, req string) {
	t.Helper()
	_ = client.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := client.Write([]byte(req))
	select {
	case <-served:
		t.Fatal("the server stopped reading")
	default:
	}
	require.Nil(t, err)
}

// PLAY starts the senders before the server marks the session as playing, so
// the sender goroutines already run while the server sets playOK. Meaningful
// under -race, like the tests below.
func TestServerPlayWhileSendersRun(t *testing.T) {
	playingServer(t)
}

// A client that disconnects, or a stream that evicts the consumer, stops it
// while its sender goroutine is still busy with packets.
func TestServerStopWhileSendersRun(t *testing.T) {
	p := playingServer(t)

	// a sender with a backlog (ex. a slow client) is still busy after Stop
	// closed it
	p.hold()
	for i := 0; i < 100; i++ {
		p.track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2}, Payload: make([]byte, 160)})
	}
	p.release()

	require.NoError(t, ignoreClosed(p.c.Stop()))
	select {
	case <-p.served:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stayed open after Stop")
	}

	time.Sleep(20 * time.Millisecond)
}
