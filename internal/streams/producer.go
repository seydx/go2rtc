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

	mixer *core.RTPMixer

	state              state
	mu                 sync.RWMutex
	trackMu            sync.Mutex    // Serializes conn.GetTrack calls to prevent concurrent protocol operations
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

	gopEnabled bool
}

const SourceTemplate = "{input}"

func NewProducer(source string) *Producer {
	// Parse all stream parameters
	rawSource, gopEnabled, backchannelEnabled, mixingEnabled, videoEnabled, audioEnabled, videoExplicitlySet, audioExplicitlySet, requirePrevAudio, requirePrevVideo := parseStreamParams(source)

	if strings.Contains(rawSource, SourceTemplate) {
		return &Producer{
			template:           rawSource,
			gopEnabled:         gopEnabled,
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
	rawSource, gopEnabled, backchannelEnabled, mixingEnabled, videoEnabled, audioEnabled, videoExplicitlySet, audioExplicitlySet, requirePrevAudio, requirePrevVideo := parseStreamParams(s)
	p.gopEnabled = gopEnabled
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

	// Get conn reference while holding lock
	conn := p.conn
	gopEnabled := p.gopEnabled

	// Serialize conn.GetTrack calls using trackMu to prevent concurrent
	// protocol operations (e.g. two SETUP requests or two Tapo SetupStream
	// calls on the same connection simultaneously).
	p.trackMu.Lock()
	p.mu.Unlock()

	// Double-check: another thread may have added the track while we waited for trackMu
	for _, track := range p.receivers {
		if track.Codec.Match(codec) {
			p.trackMu.Unlock()
			return track, nil
		}
	}

	// Call underlying protocol's GetTrack — serialized by trackMu
	track, err := conn.GetTrack(media, codec)
	p.trackMu.Unlock()

	if err != nil {
		return nil, err
	}

	if gopEnabled {
		track.SetupGOP()
	}

	// Reacquire lock to update state
	p.mu.Lock()

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
		if p.mixer != nil {
			// Add this consumer as parent with its actual codec (e.g., Opus from browser)
			// The mixer will handle transcoding if codecs differ from output codec
			p.mixer.AddParentWithCodec(&track.Node, track.Codec)
			p.senders = append(p.senders, track)
			p.mu.Unlock()
			return nil
		}

		// No mixer exists yet, create one with the producer's codec as output (e.g., AAC for camera)
		p.mixer = core.NewRTPMixer(ffmpegBin, media, codec)
		// Add parent with its actual codec (e.g., Opus from browser) - mixer will transcode to output
		p.mixer.AddParentWithCodec(&track.Node, track.Codec)

		// Connect mixer to underlying protocol
		// Get consumer reference and release lock BEFORE calling AddTrack
		consumer := p.conn.(core.Consumer)
		mixerReceiver := core.NewReceiver(media, codec)
		mixerReceiver.ParentNode = &p.mixer.Node
		p.mu.Unlock()

		// Call underlying protocol's AddTrack WITHOUT holding the lock
		// This prevents blocking API serialization during network operations
		if err := consumer.AddTrack(media, codec, mixerReceiver); err != nil {
			return err
		}

		// Reacquire lock to update state
		p.mu.Lock()
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
	mixer := p.mixer
	url := p.url
	p.mu.RUnlock()

	if conn != nil {
		connData, err := json.Marshal(conn)
		if err != nil {
			return nil, err
		}

		// If no mixer, return as-is
		if mixer == nil {
			return connData, nil
		}

		// Marshal mixer and append
		mixerData, err := json.Marshal(mixer)
		if err != nil {
			return nil, err
		}

		// Remove closing } and add mixers field
		result := connData[:len(connData)-1]
		result = append(result, []byte(`,"mixer":`)...)
		result = append(result, mixerData...)
		result = append(result, '}')

		return result, nil
	}

	return json.Marshal(map[string]string{"url": url})
}

// State returns the producer's connection state as a string.
func (p *Producer) State() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	switch p.state {
	case stateDialing, stateMedias, stateTracks:
		return "connecting"
	case stateStart, stateExternal, stateInternal:
		return "connected"
	default:
		return "idle"
	}
}

// HasError returns whether the producer has a dial error.
func (p *Producer) HasError() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.dialErr != nil
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
	watchdogStop := make(chan struct{})
	go p.watchdog(conn, workerID, watchdogStop)

	if err := conn.Start(); err != nil {
		close(watchdogStop)

		p.mu.Lock()
		closed := p.workerID != workerID
		p.mu.Unlock()

		if closed {
			return
		}

		log.Warn().Err(err).Str("url", p.url).Caller().Send()
	} else {
		close(watchdogStop)
	}

	// Force-close the underlying network socket *before* we enter the
	// reconnect loop. Otherwise, on a failed reconnect (camera unreachable),
	// the old half-open TCP/DTLS socket lingers for hours (default OS keepalive
	// is 2h on Linux/macOS). Many cameras (Amcrest/Dahua/Reolink) count
	// half-open sockets against their session-slot limit and refuse new
	// clients — including parallel VLC — until those slots time out.
	// Restarting go2rtc forces FIN/RST and frees the slots, which is exactly
	// the symptom we want to avoid having to do manually.
	//
	// Interrupt() closes only the network without touching receivers/senders,
	// so reconnect()'s Replace() path can still move children to the new conn
	// when the camera comes back. Calling it again here after Watchdog already
	// did is harmless (idempotent net.Conn.Close).
	if interrupter, ok := conn.(core.Interrupter); ok {
		_ = interrupter.Interrupt()
	}

	p.reconnect(workerID, 0)
}

// Watchdog defaults — tuned for typical camera reconnect / handshake timing.
// Could be made configurable per stream via #stale=N#grace=N URL flags later.
const (
	watchdogStaleThreshold = 10 * time.Second
	watchdogGraceDuration  = 30 * time.Second
	watchdogCheckInterval  = 1 * time.Second
)

// watchdog detects when a producer's underlying connection looks alive
// (Start() hasn't returned) but no packets are flowing on any receiver.
// It calls conn.Interrupt() to break the read loop, which forces Start()
// to return so worker() triggers reconnect().
//
// Lifetime: spawned per worker invocation, exits when stop is closed
// (worker about to call reconnect) or when the conn no longer matches
// p.conn (a parallel reconnect already swapped it out).
//
// Grace period: only fires staleThreshold after the watchdog started, to
// allow the camera handshake (DESCRIBE/SETUP/PLAY for RTSP, multipart
// chunked POST for Tapo, DTLS+auth for Wyze) to complete.
func (p *Producer) watchdog(myConn core.Producer, workerID int, stop chan struct{}) {
	interrupter, ok := myConn.(core.Interrupter)
	if !ok {
		// Producer doesn't support Interrupt — nothing the watchdog can do.
		return
	}

	startedAt := time.Now()
	ticker := time.NewTicker(watchdogCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			p.mu.RLock()
			currentWorkerID := p.workerID
			currentConn := p.conn
			receivers := p.receivers
			p.mu.RUnlock()

			// Worker invalidated, or reconnect already swapped to a different
			// conn — let the new worker's watchdog take over.
			if currentWorkerID != workerID || currentConn != myConn {
				return
			}

			if time.Since(startedAt) < watchdogGraceDuration {
				continue
			}

			if !receiversAllStale(receivers, watchdogStaleThreshold) {
				continue
			}

			log.Debug().Str("url", p.url).Msg("[streams] watchdog: data stale, interrupting conn")
			_ = interrupter.Interrupt()
			return
		}
	}
}

// receiversAllStale returns true if there are receivers and none of them
// has received a packet within maxAge. A receiver that never received any
// packet (LastPacketTime zero) is also considered stale once we're past
// the grace period — covers "connected but no data" scenarios.
func receiversAllStale(receivers []*core.Receiver, maxAge time.Duration) bool {
	if len(receivers) == 0 {
		return false
	}
	for _, recv := range receivers {
		if recv == nil {
			continue
		}
		if recv.IsActive(maxAge) {
			return false
		}
	}
	return true
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
			// If a backchannel mixer exists for this producer, the new conn
			// needs a fresh mixerReceiver so its conn-side sender becomes a
			// child of the existing mixer.Node. The mixer's parents (the
			// microphone tracks from real consumers) stay attached and keep
			// feeding into the mixer — only the conn-side child changes.
			// Without this, the previous reconnect logic bypassed the mixer
			// and bound the new sender directly to the original mic track,
			// breaking multi-consumer mixing after the first reconnect.
			p.mu.Lock()
			mixer := p.mixer
			mixingEnabled := p.mixingEnabled
			p.mu.Unlock()

			if mixer != nil && mixingEnabled && mixer.Codec != nil {
				codec := media.MatchCodec(mixer.Codec)
				if codec == nil && len(media.Codecs) > 0 {
					// Camera came back with a different codec — fall back to
					// the first available so we at least re-establish the
					// backchannel pipe; the mixer will transcode if needed.
					codec = media.Codecs[0]
				}
				if codec != nil {
					mixerReceiver := core.NewReceiver(media, mixer.Codec)
					mixerReceiver.ParentNode = &mixer.Node
					if err := conn.(core.Consumer).AddTrack(media, codec, mixerReceiver); err != nil {
						log.Warn().Err(err).Str("url", url).Msg("[streams] reconnect mixer AddTrack failed")
					}
				}
			} else {
				for _, sender := range senders {
					codec := media.MatchCodec(sender.Codec)
					if codec == nil {
						continue
					}

					_ = conn.(core.Consumer).AddTrack(media, codec, sender)
				}
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

	if p.conn != nil {
		_ = p.conn.Stop()
		p.conn = nil
	}

	if p.mixer != nil {
		p.mixer.Close()
		p.mixer = nil
	}

	p.state = stateNone
	p.receivers = nil
	p.senders = nil
}

func parseStreamParams(source string) (
	rawURL string,
	gopEnabled bool,
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
