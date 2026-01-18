package streams

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

type state byte

const (
	stateNone state = iota
	stateMedias
	stateTracks
	stateStart
	stateExternal
	stateInternal
)

type Producer struct {
	core.Listener

	url      string
	template string

	conn      core.Producer
	receivers []*core.Receiver
	senders   []*core.Receiver

	// Mixers for backchannel - one per codec
	mixers []*core.RTPMixer

	state    state
	mu       sync.RWMutex
	workerID int
}

const SourceTemplate = "{input}"

func NewProducer(source string) *Producer {
	if strings.Contains(source, SourceTemplate) {
		return &Producer{template: source}
	}

	return &Producer{url: source}
}

func (p *Producer) SetSource(s string) {
	if p.template == "" {
		p.url = s
	} else {
		p.url = strings.Replace(p.template, SourceTemplate, s, 1)
	}
}

func (p *Producer) Dial() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == stateNone {
		conn, err := GetProducer(p.url)
		if err != nil {
			return err
		}

		p.conn = conn
		p.state = stateMedias
	}

	return nil
}

func (p *Producer) GetMedias() []*core.Media {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn == nil {
		return nil
	}

	return p.conn.GetMedias()
}

func (p *Producer) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == stateNone {
		return nil, errors.New("get track from none state")
	}

	for _, track := range p.receivers {
		if track.Codec == codec {
			return track, nil
		}
	}

	track, err := p.conn.GetTrack(media, codec)
	if err != nil {
		return nil, err
	}

	p.receivers = append(p.receivers, track)

	if p.state == stateMedias {
		p.state = stateTracks
	}

	return track, nil
}

func (p *Producer) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	p.mu.Lock()

	if p.state == stateNone {
		p.mu.Unlock()
		return errors.New("add track from none state")
	}

	// Check if we already have ANY mixer for this media kind (audio backchannel)
	// Reuse existing mixer even if codec is different - FFmpeg will transcode
	for _, mixer := range p.mixers {
		if mixer.Media.Kind == media.Kind {
			// Add this consumer as parent with its actual codec (e.g., Opus from browser)
			// The mixer will handle transcoding if codecs differ from output codec
			mixer.AddParentWithCodec(&track.Node, track.Codec)
			p.senders = append(p.senders, track)
			p.mu.Unlock()
			return nil
		}
	}

	// No mixer exists yet, create one with the producer's codec as output (e.g., AAC for camera)
	mixer := core.NewRTPMixer(ffmpegBin, media, codec)
	// Add parent with its actual codec (e.g., Opus from browser) - mixer will transcode to output
	mixer.AddParentWithCodec(&track.Node, track.Codec)

	// Connect mixer to underlying protocol
	// Get consumer reference and release lock BEFORE calling AddTrack
	consumer := p.conn.(core.Consumer)
	mixerReceiver := core.NewReceiver(media, codec)
	mixerReceiver.ParentNode = &mixer.Node
	p.mu.Unlock()

	// Call underlying protocol's AddTrack WITHOUT holding the lock
	// This prevents blocking API serialization during network operations
	if err := consumer.AddTrack(media, codec, mixerReceiver); err != nil {
		return err
	}

	// Reacquire lock to update state
	p.mu.Lock()
	p.mixers = append(p.mixers, mixer)
	p.senders = append(p.senders, track)

	if p.state == stateMedias {
		p.state = stateTracks
	}

	p.mu.Unlock()
	return nil
}

func (p *Producer) MarshalJSON() ([]byte, error) {
	// Use RLock for read-only access - doesn't block other readers
	p.mu.RLock()
	conn := p.conn
	mixers := p.mixers
	url := p.url
	p.mu.RUnlock()

	if conn != nil {
		connData, err := json.Marshal(conn)
		if err != nil {
			return nil, err
		}

		// If no mixers, return as-is
		if len(mixers) == 0 {
			return connData, nil
		}

		// Marshal mixers and append
		mixersData, err := json.Marshal(mixers)
		if err != nil {
			return nil, err
		}

		// Remove closing } and add mixers field
		result := connData[:len(connData)-1]
		result = append(result, []byte(`,"mixers":`)...)
		result = append(result, mixersData...)
		result = append(result, '}')

		return result, nil
	}

	return json.Marshal(map[string]string{"url": url})
}

// internals

func (p *Producer) start() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state != stateTracks {
		return
	}

	log.Debug().Msgf("[streams] start producer url=%s", p.url)

	p.state = stateStart
	p.workerID++

	go p.worker(p.conn, p.workerID)
}

func (p *Producer) worker(conn core.Producer, workerID int) {
	if err := conn.Start(); err != nil {
		p.mu.Lock()
		closed := p.workerID != workerID
		p.mu.Unlock()

		if closed {
			return
		}

		log.Warn().Err(err).Str("url", p.url).Caller().Send()
	}

	p.reconnect(workerID, 0)
}

func (p *Producer) reconnect(workerID, retry int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.workerID != workerID {
		log.Trace().Msgf("[streams] stop reconnect url=%s", p.url)
		return
	}

	log.Debug().Msgf("[streams] retry=%d to url=%s", retry, p.url)

	conn, err := GetProducer(p.url)
	if err != nil {
		log.Debug().Msgf("[streams] producer=%s", err)

		timeout := time.Minute
		if retry < 5 {
			timeout = time.Second
		} else if retry < 10 {
			timeout = time.Second * 5
		} else if retry < 20 {
			timeout = time.Second * 10
		}

		time.AfterFunc(timeout, func() {
			p.reconnect(workerID, retry+1)
		})
		return
	}

	for _, media := range conn.GetMedias() {
		switch media.Direction {
		case core.DirectionRecvonly:
			for i, receiver := range p.receivers {
				codec := media.MatchCodec(receiver.Codec)
				if codec == nil {
					continue
				}

				track, err := conn.GetTrack(media, codec)
				if err != nil {
					continue
				}

				receiver.Replace(track)
				p.receivers[i] = track
				break
			}

		case core.DirectionSendonly:
			for _, sender := range p.senders {
				codec := media.MatchCodec(sender.Codec)
				if codec == nil {
					continue
				}

				_ = conn.(core.Consumer).AddTrack(media, codec, sender)
			}
		}
	}

	// stop previous connection after moving tracks (fix ghost exec/ffmpeg)
	_ = p.conn.Stop()
	// swap connections
	p.conn = conn

	go p.worker(conn, workerID)
}

func (p *Producer) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch p.state {
	case stateExternal:
		log.Trace().Msgf("[streams] skip stop external producer")
		return
	case stateNone:
		log.Trace().Msgf("[streams] skip stop none producer")
		return
	case stateStart:
		p.workerID++
	}

	log.Debug().Msgf("[streams] stop producer url=%s", p.url)

	if p.conn != nil {
		_ = p.conn.Stop()
		p.conn = nil
	}

	// Close all mixers
	for _, mixer := range p.mixers {
		mixer.Close()
	}

	p.state = stateNone
	p.receivers = nil
	p.senders = nil
	p.mixers = nil
}
