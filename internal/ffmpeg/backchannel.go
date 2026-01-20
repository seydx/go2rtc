package ffmpeg

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/ffmpeg"
	"github.com/AlexxIT/go2rtc/pkg/shell"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
)

type BackchannelProducer struct {
	core.Connection
	url   string
	query url.Values
	done  chan struct{}

	mu          sync.Mutex
	inputs      []*backchannelInput
	cmd         *shell.Command
	outputTrack *core.Receiver
	targetProd  core.Producer
	targetMedia *core.Media
	outputCodec *core.Codec
}

type backchannelInput struct {
	sender *core.Sender
	conn   net.Conn
	port   int
	codec  *core.Codec
}

func NewBackchannelProducer(rawURL string) *BackchannelProducer {
	i := strings.IndexByte(rawURL, '#')
	p := &BackchannelProducer{
		url:   rawURL[:i],
		query: streams.ParseQuery(rawURL[i+1:]),
		done:  make(chan struct{}),
	}

	p.ID = core.NewID()
	p.FormatName = "mixer"
	p.Protocol = "internal"

	p.Medias = []*core.Media{{
		Kind:      core.KindAudio,
		Direction: core.DirectionSendonly,
		Codecs: []*core.Codec{
			{Name: core.CodecOpus, ClockRate: 48000, Channels: 2},
			{Name: core.CodecPCMA, ClockRate: 48000},
			{Name: core.CodecPCMA, ClockRate: 16000},
			{Name: core.CodecPCMA, ClockRate: 8000},
			{Name: core.CodecPCMU, ClockRate: 48000},
			{Name: core.CodecPCMU, ClockRate: 16000},
			{Name: core.CodecPCMU, ClockRate: 8000},
			{Name: core.CodecPCML, ClockRate: 48000},
			{Name: core.CodecPCML, ClockRate: 16000},
			{Name: core.CodecPCML, ClockRate: 8000},
			{Name: core.CodecPCM, ClockRate: 48000},
			{Name: core.CodecPCM, ClockRate: 16000},
			{Name: core.CodecPCM, ClockRate: 8000},
			{Name: core.CodecG722, ClockRate: 48000},
			{Name: core.CodecG722, ClockRate: 16000},
			{Name: core.CodecG722, ClockRate: 8000},
			{Name: core.CodecAAC, ClockRate: 48000},
			{Name: core.CodecAAC, ClockRate: 16000},
			{Name: core.CodecAAC, ClockRate: 8000},
		},
	}}

	return p
}

func (p *BackchannelProducer) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	return nil, core.ErrCantGetTrack
}

func (p *BackchannelProducer) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Find target producer on first AddTrack
	if p.targetProd == nil {
		if err := p.findTarget(); err != nil {
			return err
		}
	}

	// Allocate UDP port for this input
	port, err := getFreePort()
	if err != nil {
		return err
	}

	// Create sender for this consumer
	sender := core.NewSender(media, codec)

	input := &backchannelInput{
		sender: sender,
		port:   port,
		codec:  codec,
	}
	p.inputs = append(p.inputs, input)
	p.Senders = append(p.Senders, sender)

	// Restart FFmpeg with all inputs
	if err := p.restartFFmpeg(); err != nil {
		return err
	}

	// Setup UDP connection and packet forwarding
	conn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	input.conn = conn

	var seq uint16
	var ts uint32
	sender.Handler = func(pkt *rtp.Packet) {
		outPkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    codec.PayloadType,
				SequenceNumber: seq,
				Timestamp:      ts,
				SSRC:           pkt.SSRC,
			},
			Payload: pkt.Payload,
		}
		seq++
		ts += codec.ClockRate / 50 // 20ms

		if data, err := outPkt.Marshal(); err == nil {
			conn.Write(data)
			p.Send += len(pkt.Payload)
		}
	}

	sender.HandleRTP(track)

	log.Debug().
		Int("inputs", len(p.inputs)).
		Str("codec", codec.String()).
		Int("port", port).
		Msg("[ffmpeg/backchannel] added input")

	// Cleanup when track (Consumer's Receiver) is closed
	track.Node.OnClose = func() {
		log.Debug().
			Int("inputs", len(p.inputs)-1).
			Str("codec", codec.String()).
			Int("port", port).
			Msg("[ffmpeg/backchannel] removing input")

		sender.Close()
		p.removeInput(input)
	}

	return nil
}

func (p *BackchannelProducer) Start() error {
	<-p.done
	return nil
}

func (p *BackchannelProducer) Stop() error {
	select {
	case <-p.done:
	default:
		close(p.done)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cmd != nil {
		p.cmd.Close()
	}

	for _, input := range p.inputs {
		if input.conn != nil {
			input.conn.Close()
		}
		input.sender.Close()
	}

	return nil
}

func (p *BackchannelProducer) removeInput(input *backchannelInput) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Find and remove input
	for i, in := range p.inputs {
		if in == input {
			if in.conn != nil {
				in.conn.Close()
			}

			p.inputs = append(p.inputs[:i], p.inputs[i+1:]...)

			for j, s := range p.Senders {
				if s == in.sender {
					p.Senders = append(p.Senders[:j], p.Senders[j+1:]...)
					break
				}
			}

			log.Debug().
				Int("remaining", len(p.inputs)).
				Msg("[ffmpeg/backchannel] removed input")

			break
		}
	}

	// Restart FFmpeg with remaining inputs, or stop if none left
	if len(p.inputs) > 0 {
		p.restartFFmpeg()
	} else if p.cmd != nil {
		p.cmd.Close()
		p.cmd = nil
	}
}

func (p *BackchannelProducer) findTarget() error {
	streamName := p.url[7:] // "ffmpeg:name" -> "name"
	stream := streams.Get(streamName)
	if stream == nil {
		return errors.New("ffmpeg/backchannel: stream not found: " + streamName)
	}

	for _, prod := range stream.Producers() {
		for _, m := range prod.GetMedias() {
			if m.Kind == core.KindAudio && m.Direction == core.DirectionSendonly {
				p.targetProd = prod
				p.targetMedia = m
				break
			}
		}
		if p.targetProd != nil {
			break
		}
	}

	if p.targetProd == nil {
		return errors.New("ffmpeg/backchannel: no backchannel target")
	}

	p.outputCodec = p.selectOutputCodec(p.targetMedia)
	if p.outputCodec == nil {
		return errors.New("ffmpeg/backchannel: no matching output codec")
	}

	return nil
}

func (p *BackchannelProducer) restartFFmpeg() error {
	// Stop existing FFmpeg
	if p.cmd != nil {
		p.cmd.Close()
		p.cmd = nil
	}

	// Generate SDP with all inputs
	sdpContent := p.generateMultiInputSDP()

	// Build FFmpeg args with amix
	args := p.buildArgs()

	log.Debug().
		Int("inputs", len(p.inputs)).
		Str("output", p.outputCodec.String()).
		Str("args", args).
		Msg("[ffmpeg/backchannel] starting ffmpeg")

	// Start FFmpeg
	cmd := shell.NewCommand(args)
	cmd.Stdin = bytes.NewReader(sdpContent)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err = cmd.Start(); err != nil {
		return err
	}
	p.cmd = cmd

	// Create output track on first start
	if p.outputTrack == nil {
		p.outputTrack = core.NewReceiver(p.targetMedia, p.outputCodec)
		p.Receivers = append(p.Receivers, p.outputTrack)

		// Add output to target
		if cons, ok := p.targetProd.(core.Consumer); ok {
			if err := cons.AddTrack(p.targetMedia, p.outputCodec, p.outputTrack); err != nil {
				cmd.Close()
				return err
			}
		} else {
			cmd.Close()
			return errors.New("ffmpeg/backchannel: target doesn't support AddTrack")
		}
	}

	// Read FFmpeg output and forward to target
	go func() {
		buf := make([]byte, 4096)
		var outSeq uint16
		var outTs uint32

		for {
			n, err := stdout.Read(buf)
			if err != nil {
				break
			}
			if n == 0 {
				continue
			}

			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    p.outputCodec.PayloadType,
					SequenceNumber: outSeq,
					Timestamp:      outTs,
				},
				Payload: append([]byte(nil), buf[:n]...),
			}
			p.outputTrack.Input(pkt)

			outSeq++
			outTs += uint32(n)
			p.Recv += n
		}
	}()

	return nil
}

func (p *BackchannelProducer) generateMultiInputSDP() []byte {
	sd := &sdp.SessionDescription{
		Origin:      sdp.Origin{Username: "-", SessionID: 1, SessionVersion: 1, NetworkType: "IN", AddressType: "IP4", UnicastAddress: "0.0.0.0"},
		SessionName: "backchannel",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN", AddressType: "IP4",
			Address: &sdp.Address{Address: "127.0.0.1"},
		},
		TimeDescriptions: []sdp.TimeDescription{{Timing: sdp.Timing{}}},
	}

	for _, input := range p.inputs {
		codec := input.codec
		codecName := codec.Name
		if codecName == core.CodecPCML {
			codecName = core.CodecPCM
		}

		channels := codec.Channels
		if channels == 0 {
			channels = 1
		}

		md := &sdp.MediaDescription{
			MediaName: sdp.MediaName{
				Media:  core.KindAudio,
				Port:   sdp.RangedPort{Value: input.port},
				Protos: []string{"RTP", "AVP"},
			},
		}
		md.WithCodec(codec.PayloadType, codecName, codec.ClockRate, uint16(channels), codec.FmtpLine)
		md.WithPropertyAttribute(core.DirectionRecvonly)

		sd.MediaDescriptions = append(sd.MediaDescriptions, md)
	}

	data, _ := sd.Marshal()
	return data
}

func (p *BackchannelProducer) selectOutputCodec(media *core.Media) *core.Codec {
	for _, audio := range p.query["audio"] {
		name := strings.ToUpper(core.Before(audio, "/"))
		for _, c := range media.Codecs {
			if c.Name == name || strings.EqualFold(c.Name, name) {
				return c
			}
		}
	}
	if len(media.Codecs) > 0 {
		return media.Codecs[0]
	}
	return nil
}

func (p *BackchannelProducer) buildArgs() string {
	args := &ffmpeg.Args{
		Bin:     defaults["bin"],
		Global:  defaults["global"],
		Input:   "-protocol_whitelist pipe,rtp,udp,file,crypto -f sdp -i pipe:0",
		Version: verAV,
	}

	// Add amix filter for multiple inputs
	if len(p.inputs) > 1 {
		args.AddAudioFilter(fmt.Sprintf("amix=inputs=%d:duration=longest", len(p.inputs)))
	}

	// Output codec
	if len(p.query["audio"]) > 0 {
		for _, audio := range p.query["audio"] {
			if preset := defaults[audio]; preset != "" {
				args.AddCodec(preset)
			} else {
				args.AddCodec("-c:a " + audio)
			}
		}
	} else {
		switch p.outputCodec.Name {
		case core.CodecAAC:
			args.AddCodec(fmt.Sprintf("-c:a aac -ar:a %d -ac:a 1", p.outputCodec.ClockRate))
		case core.CodecPCMA:
			args.AddCodec(fmt.Sprintf("-c:a pcm_alaw -ar:a %d -ac:a 1", p.outputCodec.ClockRate))
		case core.CodecPCMU:
			args.AddCodec(fmt.Sprintf("-c:a pcm_mulaw -ar:a %d -ac:a 1", p.outputCodec.ClockRate))
		case core.CodecOpus:
			args.AddCodec("-c:a libopus -ar:a 48000 -ac:a 2")
		default:
			args.AddCodec("-c:a copy")
		}
	}

	// Output format
	switch p.outputCodec.Name {
	case core.CodecAAC:
		args.Output = "-f adts pipe:1"
	case core.CodecPCMA:
		args.Output = "-f alaw pipe:1"
	case core.CodecPCMU:
		args.Output = "-f mulaw pipe:1"
	case core.CodecOpus:
		args.Output = "-f ogg pipe:1"
	default:
		args.Output = "-f adts pipe:1"
	}

	return args.String()
}

func getFreePort() (int, error) {
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port, nil
}
