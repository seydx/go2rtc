package ffmpeg

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
)

type Producer struct {
	core.Connection
	url   string
	query url.Values

	mu      sync.Mutex
	ffmpeg  core.Producer
	tracks  []*core.Receiver
	stopped bool
}

// NewProducer - FFmpeg producer with auto selection video/audio codec based on client capabilities
func NewProducer(url string) (core.Producer, error) {
	p := &Producer{}

	i := strings.IndexByte(url, '#')
	p.url, p.query = url[:i], streams.ParseQuery(url[i+1:])

	// ffmpeg.NewProducer support only one audio
	if len(p.query["video"]) != 0 || len(p.query["audio"]) != 1 {
		return nil, errors.New("ffmpeg: unsupported params: " + url[i:])
	}

	p.ID = core.NewID()
	p.FormatName = "ffmpeg"
	p.Medias = []*core.Media{
		{
			// we can support only audio, because don't know FmtpLine for H264 and PayloadType for MJPEG
			Kind:      core.KindAudio,
			Direction: core.DirectionRecvonly,
			Codecs:    allSupportedAudioCodecs,
		},
	}
	return p, nil
}

func (p *Producer) Start() error {
	ff, err := streams.GetProducer(p.newURL())
	if err != nil {
		return err
	}

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return ff.Stop()
	}
	p.ffmpeg = ff
	p.mu.Unlock()

	// the placeholder receivers stay the attach point for consumers and are
	// fed by the inner producer's tracks: consumers never sit on a track the
	// inner teardown would close, and the streams layer sees them (readers,
	// staleness, reconnect moves)
	for i, media := range ff.GetMedias() {
		if i >= len(p.Receivers) {
			break
		}
		track, err := ff.GetTrack(media, media.Codecs[0])
		if err != nil {
			return err
		}

		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			return errors.New("ffmpeg: stopped during setup")
		}
		track.AttachRelay(&p.Receivers[i].Node)
		p.tracks = append(p.tracks, track)
		p.mu.Unlock()
	}

	return ff.Start()
}

// Stop detaches the relay receivers before the inner teardown, so closing
// the inner tracks cannot close the consumers riding on them.
func (p *Producer) Stop() error {
	p.mu.Lock()
	p.stopped = true
	ff := p.ffmpeg
	for i, track := range p.tracks {
		if i < len(p.Receivers) {
			track.RemoveChild(&p.Receivers[i].Node)
		}
	}
	p.tracks = nil
	p.mu.Unlock()

	if ff == nil {
		return nil
	}
	return ff.Stop()
}

// Interrupt breaks a hung inner producer so Start returns and the worker
// can reconnect.
func (p *Producer) Interrupt() error {
	return p.Stop()
}

func (p *Producer) MarshalJSON() ([]byte, error) {
	p.mu.Lock()
	ff := p.ffmpeg
	p.mu.Unlock()

	if ff == nil {
		return json.Marshal(p.Connection)
	}
	return json.Marshal(ff)
}

func (p *Producer) newURL() string {
	s := p.url
	// rewrite codecs in url from auto to known presets from defaults
	for _, receiver := range p.Receivers {
		codec := receiver.Codec
		switch codec.Name {
		case core.CodecOpus:
			s += "#audio=opus/16000"
		case core.CodecAAC:
			s += "#audio=aac/16000"
		case core.CodecPCML:
			s += "#audio=pcml/" + strconv.Itoa(int(codec.ClockRate))
		case core.CodecPCM:
			s += "#audio=pcm/" + strconv.Itoa(int(codec.ClockRate))
		case core.CodecPCMA:
			s += "#audio=pcma/" + strconv.Itoa(int(codec.ClockRate))
		case core.CodecPCMU:
			s += "#audio=pcmu/" + strconv.Itoa(int(codec.ClockRate))
		}
	}
	// add other params
	for key, values := range p.query {
		if key != "audio" {
			for _, value := range values {
				s += "#" + key + "=" + value
			}
		}
	}

	return s
}
