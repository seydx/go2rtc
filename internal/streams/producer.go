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

	state              state
	mu                 sync.Mutex
	workerID           int
	backchannelEnabled bool // Whether this producer supports backchannel (default: true)
	videoEnabled       bool // Whether this producer provides video (default: true)
	audioEnabled       bool // Whether this producer provides audio (default: true)
	videoExplicitlySet bool // Whether #video was explicitly set in URL
	audioExplicitlySet bool // Whether #audio was explicitly set in URL

	gopEnabled        bool
	prebufferDuration int

	// Producer-level prebuffer for replay (owns all tracks)
	prebuffer       *core.StreamPrebuffer
	prebufferDone   chan struct{}
	prebufferOffset int // Client-requested offset for replay
}

const SourceTemplate = "{input}"

func parseStreamParams(source string) (rawURL string, gopEnabled bool, prebufferDuration int) {
	rawURL = source

	// Parse #gop=X
	if idx := strings.Index(rawURL, "#gop="); idx >= 0 {
		part := rawURL[idx+5:]
		if nextIdx := strings.Index(part, "#"); nextIdx > 0 {
			gopEnabled = part[:nextIdx] == "1"
			rawURL = rawURL[:idx] + part[nextIdx:]
		} else {
			gopEnabled = part == "1"
			rawURL = rawURL[:idx]
		}
	}

	// Parse #prebuffer=X
	if idx := strings.Index(rawURL, "#prebuffer="); idx >= 0 {
		part := rawURL[idx+11:]
		if nextIdx := strings.Index(part, "#"); nextIdx > 0 {
			prebufferDuration = core.Atoi(part[:nextIdx])
			rawURL = rawURL[:idx] + part[nextIdx:]
		} else {
			prebufferDuration = core.Atoi(part)
			rawURL = rawURL[:idx]
		}
	}

	return
}

func NewProducer(source string) *Producer {
	// Parse #noVideo, #noAudio, #noBackchannel flags
	backchannelEnabled := true // default: enabled
	videoEnabled := true       // default: enabled
	audioEnabled := true       // default: enabled
	videoExplicitlySet := false
	audioExplicitlySet := false

	// Helper function to remove flag from source
	removeFlag := func(src, flag string) string {
		if idx := strings.Index(src, flag); idx >= 0 {
			// Check if there's a # after the flag
			end := idx + len(flag)
			if end < len(src) && src[end] == '#' {
				// Remove flag but keep the following #
				return src[:idx] + src[end:]
			}
			// Flag is at the end, just remove it
			return src[:idx]
		}
		return src
	}

	// Parse #noBackchannel
	if strings.Contains(source, "#noBackchannel") {
		backchannelEnabled = false
		source = removeFlag(source, "#noBackchannel")
	}

	// Parse #noVideo
	if strings.Contains(source, "#noVideo") {
		videoEnabled = false
		videoExplicitlySet = true
		source = removeFlag(source, "#noVideo")
	}

	// Parse #noAudio
	if strings.Contains(source, "#noAudio") {
		audioEnabled = false
		audioExplicitlySet = true
		source = removeFlag(source, "#noAudio")
	}

	rawSource, gopEnabled, prebufferDuration := parseStreamParams(source)

	if strings.Contains(rawSource, SourceTemplate) {
		return &Producer{
			
			template:                    rawSource,
			gopEnabled:        gopEnabled,
			prebufferDuration: prebufferDuration,
		,
			backchannelEnabled: backchannelEnabled,
			videoEnabled:       videoEnabled,
			audioEnabled:       audioEnabled,
			videoExplicitlySet: videoExplicitlySet,
			audioExplicitlySet: audioExplicitlySet,
		}
	}

	return &Producer{
		
		url:                              rawSource,
		gopEnabled:        gopEnabled,
		prebufferDuration: prebufferDuration,
	,
		backchannelEnabled: backchannelEnabled,
		videoEnabled:       videoEnabled,
		audioEnabled:       audioEnabled,
		videoExplicitlySet: videoExplicitlySet,
		audioExplicitlySet: audioExplicitlySet,
	}
}

func (p *Producer) SetSource(s string) {
	rawSource, gopEnabled, prebufferDuration := parseStreamParams(s)
	p.gopEnabled = gopEnabled
	p.prebufferDuration = prebufferDuration

	if p.template == "" {
		p.url = rawSource
	} else {
		p.url = strings.Replace(p.template, SourceTemplate, rawSource, 1)
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

	if p.gopEnabled {
		track.SetupGOP()
	}

	// Setup producer-level prebuffer if configured
	if p.prebufferDuration > 0 {
		if p.prebuffer == nil {
			// Create producer prebuffer (first track with prebuffer enabled)
			p.prebuffer = core.NewStreamPrebuffer(p.prebufferDuration)
			// log.Debug().Msgf("[streams] Created producer prebuffer with %ds duration", p.prebufferDuration)
		}

		// Set packet hook to capture packets for producer prebuffer
		trackID := track.ID
		// log.Debug().Msgf("[streams] Setup packet hook for trackID=%d, codec=%s", trackID, codec.Name)
		track.PacketHook = func(packet *core.Packet, id byte) {
			// Clone packet to avoid race conditions
			clone := &core.Packet{
				Header:  packet.Header,
				Payload: make([]byte, len(packet.Payload)),
			}
			copy(clone.Payload, packet.Payload)
			// log.Trace().Msgf("[streams] Captured packet trackID=%d, size=%d bytes", trackID, len(clone.Payload))
			p.prebuffer.Add(clone, trackID)
		}
	}

	p.receivers = append(p.receivers, track)

	if p.state == stateMedias {
		p.state = stateTracks
	}

	return track, nil
}

func (p *Producer) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == stateNone {
		return errors.New("add track from none state")
	}

	if err := p.conn.(core.Consumer).AddTrack(media, codec, track); err != nil {
		return err
	}

	p.senders = append(p.senders, track)

	if p.state == stateMedias {
		p.state = stateTracks
	}

	return nil
}

func (p *Producer) MarshalJSON() ([]byte, error) {
	if conn := p.conn; conn != nil {
		return json.Marshal(conn)
	}
	info := map[string]string{"url": p.url}
	return json.Marshal(info)
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

				if p.gopEnabled {
					track.SetupGOP()
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

// StartPrebufferReplay starts a single replay loop that reads packets sequentially
// from the producer's prebuffer and routes them to the appropriate senders
func (p *Producer) StartPrebufferReplay() {
	p.mu.Lock()

	if p.prebuffer == nil {
		// log.Debug().Msgf("[streams] Producer %s: No prebuffer available", p.url)
		p.mu.Unlock()
		return
	}

	if p.prebufferDone != nil {
		// log.Debug().Msgf("[streams] Producer %s: Prebuffer replay already running", p.url)
		p.mu.Unlock()
		return
	}

	// Use producer's configured prebuffer duration
	offsetSec := p.prebufferDuration

	// Check if prebuffer has enough data (at least 1 second)
	availableDuration := p.prebuffer.GetAvailableDuration()
	if availableDuration < 1 {
		// log.Debug().Msgf("[streams] Producer %s: Prebuffer has only %ds (< 1s), disabling prebuffer for all consumers", p.url, availableDuration)

		// Disable prebuffer on all senders - they should get live packets instead
		for _, receiver := range p.receivers {
			for _, child := range receiver.GetChildren() {
				if sender, ok := child.GetOwner().(*core.Sender); ok {
					if sender.UsePrebuffer {
						// log.Debug().Msgf("[streams] Setting sender UsePrebuffer to false (not enough buffer data)")
						sender.UsePrebuffer = false
					}
				}
			}
		}

		p.mu.Unlock()
		return
	}

	p.prebufferOffset = offsetSec
	p.prebufferDone = make(chan struct{})

	// log.Debug().Msgf("[streams] Producer %s: Starting prebuffer replay with offset=%ds (configured duration)", p.url, offsetSec)
	p.mu.Unlock()

	go p.prebufferReplayLoop()
}

// prebufferReplayLoop is the single sequential replay loop for this producer
func (p *Producer) prebufferReplayLoop() {
	prebuffer := p.prebuffer
	offsetSec := p.prebufferOffset
	done := p.prebufferDone

	// fmt.Printf("[PRODUCER] Starting prebuffer replay loop, offset=%ds\n", offsetSec)

	// Wait for prebuffer to have content
	for !prebuffer.HasContent() {
		select {
		case <-done:
			return
		case <-time.After(100 * time.Millisecond):
			// fmt.Printf("[PRODUCER] Waiting for prebuffer content...\n")
		}
	}

	// Adjust offset to actual available duration if buffer is still filling
	availableDuration := prebuffer.GetAvailableDuration()
	if availableDuration < offsetSec {
		// fmt.Printf("[PRODUCER] Requested offset=%ds but only %ds available, using available duration\n",
		// 	offsetSec, availableDuration)
		offsetSec = availableDuration
	}

	// Get initial position (offsetSec behind latest)
	currentPosition, _ := prebuffer.GetPacketsFrom(offsetSec)
	// fmt.Printf("[PRODUCER] Initial replay position=%v with offset=%ds\n", currentPosition, offsetSec)

	var lastTime time.Time
	packetCount := 0
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			liveTime := prebuffer.GetLatestTimestamp()
			if liveTime.IsZero() {
				continue
			}

			// Initialize position on first iteration
			if currentPosition.IsZero() {
				_, packets := prebuffer.GetPacketsFrom(offsetSec)
				if len(packets) > 0 {
					currentPosition = packets[0].ArrivalTime
					// fmt.Printf("[PRODUCER] Set initial position to %v\n", currentPosition)
				}
				continue
			}

			// Calculate where we should be (offsetSec behind live)
			maxAllowedPosition := liveTime.Add(-time.Duration(offsetSec) * time.Second)

			// Get next packet (any track)
			packet := prebuffer.GetNextPacketAt(currentPosition, maxAllowedPosition)
			if packet == nil {
				// No packet available yet
				continue
			}

			// Sleep to maintain original timing
			if !lastTime.IsZero() {
				sleepDuration := packet.ArrivalTime.Sub(lastTime)
				if sleepDuration > 0 {
					// fmt.Printf("[PRODUCER] Sleeping %.1fms for timing\n", sleepDuration.Seconds()*1000)
					select {
					case <-time.After(sleepDuration):
					case <-done:
						return
					}
				}
			}

			// Route packet to senders that want prebuffer
			p.mu.Lock()
			sentCount := 0
			for _, receiver := range p.receivers {
				if receiver.ID == packet.TrackID {
					// Found the receiver for this trackID
					// Send to all senders that have UsePrebuffer enabled
					for _, child := range receiver.GetChildren() {
						if sender, ok := child.GetOwner().(*core.Sender); ok {
							if sender.UsePrebuffer {
								// Use InputCache to bypass all filtering and send directly
								sender.InputCache(packet.Packet)
								sentCount++
							}
						}
					}
					break
				}
			}
			p.mu.Unlock()

			// if sentCount > 0 {
			// 	fmt.Printf("[PRODUCER] Routed replay packet trackID=%d to %d senders\n",
			// 		packet.TrackID, sentCount)
			// }

			lastTime = packet.ArrivalTime
			currentPosition = packet.ArrivalTime.Add(1)
			packetCount++

			// if packetCount%100 == 0 {
			// 	fmt.Printf("[PRODUCER] Sent %d packets, offset=%.1fs\n",
			// 		packetCount, liveTime.Sub(lastTime).Seconds())
			// }

		case <-done:
			// fmt.Printf("[PRODUCER] Replay loop stopped\n")
			return
		}
	}
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

	// Stop prebuffer replay if running
	if p.prebufferDone != nil {
		close(p.prebufferDone)
		p.prebufferDone = nil
	}

	if p.conn != nil {
		_ = p.conn.Stop()
		p.conn = nil
	}

	p.state = stateNone
	p.receivers = nil
	p.senders = nil
}
