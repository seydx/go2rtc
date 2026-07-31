package talkback

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/api/ws"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/rs/zerolog"
)

func Init() {
	log = app.GetLogger("talkback")

	ws.HandleFunc("talkback", handlerWSTalkback)
}

var log zerolog.Logger

// handlerWSTalkback ingests raw audio frames from a client over the existing
// WS endpoint and feeds them into the stream's backchannel, where the mixer
// transcodes to whatever the camera speaker needs. Handshake value picks the
// input format, ex. "pcm16/16000/1"; after the "ok" reply the client streams
// binary frames of raw samples.
func handlerWSTalkback(tr *ws.Transport, msg *ws.Message) error {
	stream, _ := streams.GetOrPatch(tr.Request.URL.Query())
	if stream == nil {
		return errors.New(api.StreamNotFound)
	}

	codec, err := parseFormat(msg.String())
	if err != nil {
		return err
	}

	sender := newSender(codec)
	sender.WithRequest(tr.Request)

	if err = stream.AddConsumer(sender); err != nil {
		log.Debug().Err(err).Msg("[talkback] add consumer")
		return err
	}

	log.Debug().Msgf("[talkback] start codec=%s rate=%d channels=%d", codec.Name, codec.ClockRate, codec.Channels)

	tr.OnBinary(sender.write)
	tr.OnClose(func() {
		stream.RemoveConsumer(sender)
	})

	tr.Write(&ws.Message{Type: "talkback", Value: "ok"})

	return nil
}

func parseFormat(value string) (*core.Codec, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 3 || parts[0] != "pcm16" {
		return nil, fmt.Errorf("talkback: unsupported format %q, expected pcm16/<rate>/<channels>", value)
	}
	rate, err1 := strconv.Atoi(parts[1])
	channels, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || rate <= 0 || channels <= 0 || channels > 2 {
		return nil, fmt.Errorf("talkback: bad format %q", value)
	}
	return &core.Codec{
		Name:      core.CodecPCML,
		ClockRate: uint32(rate),
		Channels:  uint8(channels),
	}, nil
}

type sender struct {
	core.Connection
	frameBytes int
	ts         uint32
	closed     core.Waiter
}

func newSender(codec *core.Codec) *sender {
	media := &core.Media{
		Kind:      core.KindAudio,
		Direction: core.DirectionRecvonly,
		Codecs:    []*core.Codec{codec},
	}
	return &sender{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "talkback",
			Medias:     []*core.Media{media},
		},
		frameBytes: 2 * int(codec.Channels),
	}
}

func (s *sender) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	return errors.New("talkback: consumer is send-only")
}

func (s *sender) Start() error {
	return s.closed.Wait()
}

func (s *sender) Stop() error {
	s.closed.Done(nil)
	return s.Connection.Stop()
}

func (s *sender) write(data []byte) {
	if len(data) < s.frameBytes || len(s.Receivers) == 0 {
		return
	}
	s.Recv += len(data)

	// clients send little-endian samples, but the mixer announces the parent
	// as L16 to ffmpeg and RTP L16 is big-endian
	for i := 0; i+1 < len(data); i += 2 {
		data[i], data[i+1] = data[i+1], data[i]
	}

	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:   2,
			Marker:    true,
			Timestamp: s.ts,
		},
		Payload: data,
	}
	s.Receivers[0].WriteRTP(pkt)

	s.ts += uint32(len(data) / s.frameBytes)
}
