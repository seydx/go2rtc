package rtsp

import (
	"bufio"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// wireFrame is one interleaved frame as the client reads it
type wireFrame struct {
	rtp *rtp.Packet        // data channel
	sr  *rtcp.SenderReport // control channel
	at  time.Time
}

// readWire collects every interleaved frame from the client side
func readWire(conn net.Conn) func() []wireFrame {
	var mu sync.Mutex
	var frames []wireFrame
	done := make(chan struct{})

	go func() {
		defer close(done)
		r := bufio.NewReader(conn)
		hdr := make([]byte, 4)
		for {
			if _, err := io.ReadFull(r, hdr); err != nil {
				return
			}
			buf := make([]byte, int(hdr[2])<<8|int(hdr[3]))
			if _, err := io.ReadFull(r, buf); err != nil {
				return
			}
			f := wireFrame{at: time.Now()}
			if hdr[1]&1 == 0 {
				f.rtp = &rtp.Packet{}
				if f.rtp.Unmarshal(buf) != nil {
					return
				}
			} else {
				packets, err := rtcp.Unmarshal(buf)
				if err != nil {
					return
				}
				f.sr, _ = packets[0].(*rtcp.SenderReport)
			}
			mu.Lock()
			frames = append(frames, f)
			mu.Unlock()
		}
	}()

	return func() []wireFrame {
		_ = conn.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		return frames
	}
}

// TestConsumerReportBeforeJump: after a producer swap the consumer must get
// the report for the new timeline before the first packet of it, and see no
// extra reports otherwise. The backchannel to a camera keeps the cadence.
func TestConsumerReportBeforeJump(t *testing.T) {
	const clockRate = 90000
	const frame = 40 * time.Millisecond
	const newBase = uint32(0xDEADBEEF)

	for _, mode := range []core.Mode{core.ModePassiveConsumer, core.ModeActiveProducer} {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()

			server, client := net.Pipe()
			c := NewServer(server)
			c.mode = mode
			c.state = StatePlay
			c.playOK.Store(true)
			wire := readWire(client)

			// RTP video passes through unchanged and is buffered per frame
			codec := &core.Codec{Name: core.CodecVP8, ClockRate: clockRate, PayloadType: 96}
			handler := c.packetWriter(codec, 0, 96)

			var seq uint16
			send := func(ts uint32, marker bool) {
				seq++
				handler(&rtp.Packet{
					Header:  rtp.Header{Version: 2, Marker: marker, SequenceNumber: seq, Timestamp: ts, SSRC: 0x1234},
					Payload: []byte{1, 2, 3},
				})
			}
			sendFrame := func(ts uint32) {
				send(ts, false)
				send(ts, false)
				send(ts, true)
			}

			// past the warm-up on the old timeline, then a frame cut off by
			// the swap and the first frame of the new producer
			ts := uint32(1000)
			start := time.Now()
			for time.Since(start) < srWarmup+300*time.Millisecond {
				sendFrame(ts)
				ts += clockRate / 25
				time.Sleep(frame)
			}
			send(ts, false)
			send(ts, false)
			swapped := time.Now()
			for i := uint32(0); i < 10; i++ {
				sendFrame(newBase + i*3600)
				time.Sleep(frame)
			}

			frames := wire()
			first := -1
			for i, f := range frames {
				if f.rtp != nil && f.rtp.Timestamp == newBase {
					first = i
					break
				}
			}
			require.Positive(t, first)

			var reports []*rtcp.SenderReport
			for _, f := range frames {
				if f.sr != nil {
					reports = append(reports, f.sr)
				}
			}

			// rtpTS is the RTP time of a data frame, 0 for a report
			rtpTS := func(f wireFrame) uint32 {
				if f.rtp == nil {
					return 0
				}
				return f.rtp.Timestamp
			}

			if mode != core.ModePassiveConsumer {
				for _, f := range frames[first-2 : first] {
					require.Equal(t, ts, rtpTS(f), "cut-off frame, no report")
				}
				require.Len(t, reports, 2, "only the regular cadence")
				return
			}

			// the cut-off frame goes out first, then the report
			for _, f := range frames[first-3 : first-1] {
				require.Equal(t, ts, rtpTS(f))
			}
			sr := frames[first-1].sr
			require.NotNil(t, sr, "report right before the first packet of the new timeline")
			require.Equal(t, newBase, sr.RTPTime)
			require.WithinDuration(t, swapped.Add(-srStartBias), ntpToTime(sr.NTPTime), 50*time.Millisecond)

			// the first report and the one 2.5s later, then the re-anchor;
			// nothing else although frames kept flowing
			require.Len(t, reports, 3)
			require.Same(t, sr, reports[2])
		})
	}
}
