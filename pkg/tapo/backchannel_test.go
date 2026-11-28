package tapo

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mpegts"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// readClientPart reads one part the client sends to the camera
func readClientPart(rd *bufio.Reader) ([]byte, error) {
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "----client-stream-boundary--\r\n" {
			break // the CRLF after the body of a request comes first
		}
	}
	header, err := textproto.NewReader(rd).ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	size, err := strconv.Atoi(header.Get("Content-Length"))
	if err != nil {
		return nil, err
	}
	body := make([]byte, size)
	_, err = io.ReadFull(rd, body)
	return body, err
}

// The PTS of the talk audio counts in 90 kHz. The RTP timestamps of PCMA
// count in 8 kHz: taken as they are, the camera played the audio about 11
// times too slow (#2646).
func TestBackchannelPTSClock(t *testing.T) {
	conn, camera := net.Pipe()
	t.Cleanup(func() { _ = conn.Close(); _ = camera.Close() })

	bodies := make(chan []byte, 10)
	go func() {
		defer close(bodies)
		rd := bufio.NewReader(camera)
		if _, err := readClientPart(rd); err != nil { // the talk request
			return
		}
		res := `{"params":{"session_id":"talk"}}`
		_, _ = camera.Write([]byte("----device-stream-boundary--\r\nContent-Type: application/json\r\n" +
			"Content-Length: " + strconv.Itoa(len(res)) + "\r\n\r\n" + res + "\r\n"))
		for {
			body, err := readClientPart(rd)
			if err != nil {
				return
			}
			bodies <- body
		}
	}()

	// session1 is empty: the backchannel takes conn1
	client := &Client{conn1: conn}

	codec := &core.Codec{Name: core.CodecPCMA, ClockRate: 8000}
	media := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{codec}}
	track := core.NewReceiver(media, codec)
	require.NoError(t, client.AddTrack(media, codec, track))

	var ts []byte
	read := func() {
		select {
		case body, ok := <-bodies:
			require.True(t, ok)
			ts = append(ts, body...)
		case <-time.After(time.Second):
			t.Fatal("no backchannel part")
		}
	}
	read() // PAT and PMT

	// 20 ms of PCMA each, the first timestamp is 0 like for any fresh track
	for i := range uint32(3) {
		track.Input(&rtp.Packet{
			Header:  rtp.Header{Version: 2, Timestamp: i * 160},
			Payload: make([]byte, 160),
		})
		read()
	}

	d := mpegts.NewDemuxer()
	rd := bytes.NewReader(ts)
	var pts []uint32
	for {
		pkt, err := d.ReadPacket(rd)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if pkt.PayloadType == mpegts.StreamTypePCMATapo {
			pts = append(pts, pkt.Timestamp)
		}
	}
	require.Equal(t, []uint32{0, 1800, 3600}, pts)
}
