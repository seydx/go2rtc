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
	stateDialing
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

	state              state
	mu                 sync.RWMutex
	dialDone           chan struct{} // Closed when dial completes (success or failure)
	dialErr            error         // Error from dial attempt (if any)
	workerID           int
	backchannelEnabled bool // Whether this producer supports backchannel (default: true)
	mixingEnabled      bool // Whether to enable audio mixing for multiple backchannel consumers (default: true)
	videoEnabled       bool // Whether this producer provides video (default: true)
	audioEnabled       bool // Whether this producer provides audio (default: true)
	videoExplicitlySet bool // Whether #video was explicitly set in URL
	audioExplicitlySet bool // Whether #audio was explicitly set in URL
	requirePrevAudio   bool // Only start if previous producer has audio (#requirePrevAudio)
	requirePrevVideo   bool // Only start if previous producer has video (#requirePrevVideo)

	// Producer-level prebuffer for replay (owns all tracks)
	prebuffer         *core.StreamPrebuffer
	prebufferDone     chan struct{}
	prebufferOffset   int // Client-requested offset for replay
	prebufferDuration int

	gopEnabled bool
}

const SourceTemplate = "{input}"

func NewProducer(source string) *Producer {
	// Parse all stream parameters
	rawSource, gopEnabled, prebufferDuration, backchannelEnabled, mixingEnabled, videoEnabled, audioEnabled, videoExplicitlySet, audioExplicitlySet, requirePrevAudio, requirePrevVideo := parseStreamParams(source)

	if strings.Contains(rawSource, SourceTemplate) {
		return &Producer{
			template:           rawSource,
			gopEnabled:         gopEnabled,
			prebufferDuration:  prebufferDuration,
			backchannelEnabled: backchannelEnabled,
			mixingEnabled:      mixingEnabled,
			videoEnabled:       videoEnabled,
			audioEnabled:       audioEnabled,
			videoExplicitlySet: videoExplicitlySet,
			audioExplicitlySet: audioExplicitlySet,
			requirePrevAudio:   requirePrevAudio,
			requirePrevVideo:   requirePrevVideo,
		}
	}

	return &Producer{
		url:                rawSource,
		gopEnabled:         gopEnabled,
		prebufferDuration:  prebufferDuration,
		backchannelEnabled: backchannelEnabled,
		mixingEnabled:      mixingEnabled,
		videoEnabled:       videoEnabled,
		audioEnabled:       audioEnabled,
		videoExplicitlySet: videoExplicitlySet,
		audioExplicitlySet: audioExplicitlySet,
		requirePrevAudio:   requirePrevAudio,
		requirePrevVideo:   requirePrevVideo,
	}
}

func (p *Producer) SetSource(s string) {
	rawSource, gopEnabled, prebufferDuration, backchannelEnabled, mixingEnabled, videoEnabled, audioEnabled, videoExplicitlySet, audioExplicitlySet, requirePrevAudio, requirePrevVideo := parseStreamParams(s)
	p.gopEnabled = gopEnabled
	p.prebufferDuration = prebufferDuration
	p.backchannelEnabled = backchannelEnabled
	p.mixingEnabled = mixingEnabled
	p.videoEnabled = videoEnabled
	p.audioEnabled = audioEnabled
	p.videoExplicitlySet = videoExplicitlySet
	p.audioExplicitlySet = audioExplicitlySet
	p.requirePrevAudio = requirePrevAudio
	p.requirePrevVideo = requirePrevVideo

	if p.template == "" {
		p.url = rawSource
	} else {
		p.url = strings.Replace(p.template, SourceTemplate, rawSource, 1)
	}
}

func (p *Producer) Dial() error {
	p.mu.Lock()

	switch p.state {
	case stateNone:
		// We're the first - start dialing
		p.state = stateDialing
		p.dialDone = make(chan struct{})
		p.dialErr = nil
		url := p.url
		p.mu.Unlock()

		// Do the actual dial without holding the lock
		conn, err := GetProducer(url)

		// Reacquire lock to update state
		p.mu.Lock()
		if err != nil {
			p.dialErr = err
			p.state = stateNone
			close(p.dialDone)
			p.mu.Unlock()
			return err
		}

		p.conn = conn
		p.state = stateMedias
		close(p.dialDone)
		p.mu.Unlock()
		return nil

	case stateDialing:
		// Someone else is dialing - wait for them to finish
		dialDone := p.dialDone
		p.mu.Unlock()

		<-dialDone // Wait for dial to complete

		p.mu.Lock()
		err := p.dialErr
		p.mu.Unlock()
		return err

	default:
		// Already connected (stateMedias, stateTracks, stateStart, etc.)
		p.mu.Unlock()
		return nil
	}
}

func (p *Producer) GetMedias() []*core.Media {
	p.mu.RLock()
	conn := p.conn
	p.mu.RUnlock()

	if conn == nil {
		return nil
	}

	return conn.GetMedias()
}

func (p *Producer) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	p.mu.Lock()

	if p.state == stateNone {
		p.mu.Unlock()
		return nil, errors.New("get track from none state")
	}

	for _, track := range p.receivers {
		if track.Codec.Match(codec) {
			p.mu.Unlock()
			return track, nil
		}
	}

	// Get conn reference and release lock BEFORE calling GetTrack
	conn := p.conn
	gopEnabled := p.gopEnabled
	prebufferDuration := p.prebufferDuration
	p.mu.Unlock()

	// Call underlying protocol's GetTrack WITHOUT holding the lock
	track, err := conn.GetTrack(media, codec)
	if err != nil {
		return nil, err
	}

	if gopEnabled {
		track.SetupGOP()
	}

	// Reacquire lock to update state
	p.mu.Lock()

	// Setup producer-level prebuffer if configured
	if prebufferDuration > 0 {
		if p.prebuffer == nil {
			// Create producer prebuffer (first track with prebuffer enabled)
			p.prebuffer = core.NewStreamPrebuffer(prebufferDuration)
		}

		// Set packet hook to capture packets for producer prebuffer
		trackID := track.ID
		track.PacketHook = func(packet *core.Packet, id byte) {
			// Clone packet to avoid race conditions
			clone := &core.Packet{
				Header:  packet.Header,
				Payload: make([]byte, len(packet.Payload)),
			}
			copy(clone.Payload, packet.Payload)
			p.prebuffer.Add(clone, trackID)
		}
	}

	p.receivers = append(p.receivers, track)

	if p.state == stateMedias {
		p.state = stateTracks
	}

	p.mu.Unlock()
	return track, nil
}

func (p *Producer) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	p.mu.Lock()

	if p.state == stateNone {
		p.mu.Unlock()
		return errors.New("add track from none state")
	}

	// If mixing is enabled, use mixer for multiple backchannel consumers
	if p.mixingEnabled {
		// Check if we already have ANY mixer for audio backchannel
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
	} else {
		// Without mixing, directly pass track to underlying protocol
		// Get consumer reference and release lock BEFORE calling AddTrack
		consumer := p.conn.(core.Consumer)
		p.mu.Unlock()

		// Call underlying protocol's AddTrack WITHOUT holding the lock
		if err := consumer.AddTrack(media, codec, track); err != nil {
			return err
		}

		// Reacquire lock to update state
		p.mu.Lock()
		p.senders = append(p.senders, track)
	}

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
	// Check workerID with lock, then release before slow operations
	p.mu.Lock()
	if p.workerID != workerID {
		p.mu.Unlock()
		log.Trace().Msgf("[streams] stop reconnect url=%s", p.url)
		return
	}
	url := p.url
	receivers := p.receivers
	senders := p.senders
	gopEnabled := p.gopEnabled
	oldConn := p.conn
	p.mu.Unlock()

	log.Debug().Msgf("[streams] retry=%d to url=%s", retry, url)

	// Slow operation WITHOUT holding lock
	conn, err := GetProducer(url)
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

	// Check if still valid after slow operation
	p.mu.Lock()
	if p.workerID != workerID {
		p.mu.Unlock()
		conn.Stop()
		return
	}
	p.mu.Unlock()

	// Process tracks without holding lock (these may involve network ops)
	for _, media := range conn.GetMedias() {
		switch media.Direction {
		case core.DirectionRecvonly:
			for i, receiver := range receivers {
				codec := media.MatchCodec(receiver.Codec)
				if codec == nil {
					continue
				}

				track, err := conn.GetTrack(media, codec)
				if err != nil {
					continue
				}

				if gopEnabled {
					track.SetupGOP()
				}

				receiver.Replace(track)

				// Update receiver in slice (need lock for this)
				p.mu.Lock()
				if i < len(p.receivers) {
					p.receivers[i] = track
				}
				p.mu.Unlock()
				break
			}

		case core.DirectionSendonly:
			for _, sender := range senders {
				codec := media.MatchCodec(sender.Codec)
				if codec == nil {
					continue
				}

				_ = conn.(core.Consumer).AddTrack(media, codec, sender)
			}
		}
	}

	// Final update with lock
	p.mu.Lock()
	if p.workerID != workerID {
		p.mu.Unlock()
		conn.Stop()
		return
	}
	// stop previous connection after moving tracks (fix ghost exec/ffmpeg)
	if oldConn != nil {
		_ = oldConn.Stop()
	}
	// swap connections
	p.conn = conn
	p.mu.Unlock()

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

	// Stop prebuffer replay if running
	if p.prebufferDone != nil {
		close(p.prebufferDone)
		p.prebufferDone = nil
	}

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

func parseStreamParams(source string) (
	rawURL string,
	gopEnabled bool,
	prebufferDuration int,
	backchannelEnabled bool,
	mixingEnabled bool,
	videoEnabled bool,
	audioEnabled bool,
	videoExplicitlySet bool,
	audioExplicitlySet bool,
	requirePrevAudio bool,
	requirePrevVideo bool,
) {
	rawURL = source

	// Defaults
	backchannelEnabled = true
	mixingEnabled = true
	videoEnabled = true
	audioEnabled = true
	videoExplicitlySet = false
	audioExplicitlySet = false
	requirePrevAudio = false
	requirePrevVideo = false

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
	if strings.Contains(rawURL, "#noBackchannel") {
		backchannelEnabled = false
		rawURL = removeFlag(rawURL, "#noBackchannel")
	}

	// Parse #noMix
	if strings.Contains(rawURL, "#noMix") {
		mixingEnabled = false
		rawURL = removeFlag(rawURL, "#noMix")
	}

	// Parse #noVideo
	if strings.Contains(rawURL, "#noVideo") {
		videoEnabled = false
		videoExplicitlySet = true
		rawURL = removeFlag(rawURL, "#noVideo")
	}

	// Parse #video= (used by ffmpeg, e.g. #video=copy)
	// This sets the flag but doesn't remove the param (ffmpeg needs it)
	if strings.Contains(rawURL, "#video=") {
		videoExplicitlySet = true
	}

	// Parse #noAudio
	if strings.Contains(rawURL, "#noAudio") {
		audioEnabled = false
		audioExplicitlySet = true
		rawURL = removeFlag(rawURL, "#noAudio")
	}

	// Parse #audio= (used by ffmpeg, e.g. #audio=opus)
	// This sets the flag but doesn't remove the param (ffmpeg needs it)
	if strings.Contains(rawURL, "#audio=") {
		audioExplicitlySet = true
	}

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

	// Parse #requirePrevAudio
	if strings.Contains(rawURL, "#requirePrevAudio") {
		requirePrevAudio = true
		rawURL = removeFlag(rawURL, "#requirePrevAudio")
	}

	// Parse #requirePrevVideo
	if strings.Contains(rawURL, "#requirePrevVideo") {
		requirePrevVideo = true
		rawURL = removeFlag(rawURL, "#requirePrevVideo")
	}

	return
}
