package ffmpeg

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
)

// codecInstance holds an ffmpeg instance for a specific codec
type codecInstance struct {
	ffmpeg   core.Producer
	receiver *core.Receiver
	refCount int
}

// Producer - Smart FFmpeg producer with on-demand codec transcoding
// Supports multiple audio codecs (comma-separated) and starts ffmpeg instances only when needed.
// Multiple consumers requesting the same codec share the same ffmpeg instance.
type Producer struct {
	core.Connection
	url   string
	query url.Values

	mu        sync.Mutex
	instances map[string]*codecInstance // codec name -> instance
	codecs    []string                  // requested codecs from URL
	done      chan struct{}             // signals when producer should stop
}

// allSupportedCodecs - all codecs the smart producer can handle
var allSupportedCodecs = []*core.Codec{
	{Name: core.CodecOpus, ClockRate: 48000, Channels: 2},
	{Name: core.CodecPCML, ClockRate: 16000},
	{Name: core.CodecPCM, ClockRate: 16000},
	{Name: core.CodecPCMA, ClockRate: 16000},
	{Name: core.CodecPCMU, ClockRate: 16000},
	{Name: core.CodecPCML, ClockRate: 8000},
	{Name: core.CodecPCM, ClockRate: 8000},
	{Name: core.CodecPCMA, ClockRate: 8000},
	{Name: core.CodecPCMU, ClockRate: 8000},
	{Name: core.CodecAAC, ClockRate: 16000, FmtpLine: aac.FMTP + "1408"},
}

// NewProducer - FFmpeg producer with auto selection video/audio codec based on client capabilities
// Supports formats:
//   - ffmpeg:source#audio=auto                    (any codec, original behavior)
//   - ffmpeg:source#audio=opus,pcma,aac           (specific codecs, on-demand)
func NewProducer(rawURL string) (core.Producer, error) {
	i := strings.IndexByte(rawURL, '#')
	if i < 0 {
		return nil, errors.New("ffmpeg: missing parameters")
	}

	baseURL := rawURL[:i]
	query := streams.ParseQuery(rawURL[i+1:])

	// Don't handle video transcoding - this shouldn't happen as redirect function
	// should route video requests to normal exec: handler
	if len(query["video"]) != 0 {
		return nil, errors.New("ffmpeg: smart producer is audio-only, use single audio param for video+audio")
	}

	audioParams := query["audio"]
	if len(audioParams) == 0 {
		return nil, errors.New("ffmpeg: missing audio parameter")
	}

	p := &Producer{
		url:       baseURL,
		query:     query,
		instances: make(map[string]*codecInstance),
		done:      make(chan struct{}),
	}

	p.ID = core.NewID()
	p.FormatName = "ffmpeg/smart"

	// Parse audio codecs - support both comma-separated and multiple params
	// e.g., audio=opus,pcma,aac OR audio=opus#audio=pcma#audio=aac
	for _, param := range audioParams {
		for _, codec := range strings.Split(param, ",") {
			codec = strings.TrimSpace(codec)
			if codec != "" {
				p.codecs = append(p.codecs, strings.ToLower(codec))
			}
		}
	}

	if len(p.codecs) == 0 {
		return nil, errors.New("ffmpeg: no audio codecs specified")
	}

	// Build media with requested codecs only
	var codecs []*core.Codec

	// Special case: "auto" means all codecs
	if len(p.codecs) == 1 && p.codecs[0] == "auto" {
		codecs = allSupportedCodecs
	} else {
		// Filter to only requested codecs
		for _, reqCodec := range p.codecs {
			for _, supported := range allSupportedCodecs {
				if strings.EqualFold(supported.Name, reqCodec) {
					// Check if already added (avoid duplicates)
					found := false
					for _, c := range codecs {
						if c.Name == supported.Name {
							found = true
							break
						}
					}
					if !found {
						codecs = append(codecs, supported)
					}
					break
				}
			}
		}
	}

	if len(codecs) == 0 {
		return nil, errors.New("ffmpeg: no supported codecs found in: " + strings.Join(p.codecs, ","))
	}

	p.Medias = []*core.Media{
		{
			Kind:      core.KindAudio,
			Direction: core.DirectionRecvonly,
			Codecs:    codecs,
		},
	}

	return p, nil
}

// GetTrack - returns a receiver for the requested codec
// Starts an ffmpeg instance on-demand if not already running for this codec
func (p *Producer) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	codecKey := strings.ToLower(codec.Name)

	// Check if we already have an instance for this codec
	if inst, ok := p.instances[codecKey]; ok {
		inst.refCount++
		log.Debug().Msgf("[ffmpeg/smart] reusing instance for %s (refCount=%d)", codecKey, inst.refCount)
		return inst.receiver, nil
	}

	// Create new ffmpeg instance for this codec
	log.Debug().Msgf("[ffmpeg/smart] starting new instance for %s", codecKey)

	ffmpegURL := p.buildURL(codec)
	ffmpeg, err := streams.GetProducer(ffmpegURL)
	if err != nil {
		return nil, err
	}

	// Get the media and track from ffmpeg
	ffmpegMedias := ffmpeg.GetMedias()
	if len(ffmpegMedias) == 0 {
		ffmpeg.Stop()
		return nil, errors.New("ffmpeg: no medias from ffmpeg")
	}

	ffmpegMedia := ffmpegMedias[0]
	if len(ffmpegMedia.Codecs) == 0 {
		ffmpeg.Stop()
		return nil, errors.New("ffmpeg: no codecs from ffmpeg")
	}

	track, err := ffmpeg.GetTrack(ffmpegMedia, ffmpegMedia.Codecs[0])
	if err != nil {
		ffmpeg.Stop()
		return nil, err
	}

	// Store the instance BEFORE starting (Start() is blocking)
	p.instances[codecKey] = &codecInstance{
		ffmpeg:   ffmpeg,
		receiver: track,
		refCount: 1,
	}

	// Start the ffmpeg process in a goroutine (Start() is a blocking loop)
	go func() {
		if err := ffmpeg.Start(); err != nil {
			log.Trace().Err(err).Msgf("[ffmpeg/smart] instance ended for %s", codecKey)
		}
	}()

	return track, nil
}

// Start - blocks until Stop() is called (instances are started on-demand in GetTrack)
func (p *Producer) Start() error {
	// Block until done channel is closed (by Stop())
	<-p.done
	return nil
}

// Stop - stops all running ffmpeg instances
func (p *Producer) Stop() error {
	p.mu.Lock()

	// Close done channel to unblock Start()
	select {
	case <-p.done:
		// Already closed
	default:
		close(p.done)
	}

	var lastErr error
	for codecKey, inst := range p.instances {
		log.Debug().Msgf("[ffmpeg/smart] stopping instance for %s", codecKey)
		if err := inst.ffmpeg.Stop(); err != nil {
			lastErr = err
		}
	}
	p.instances = make(map[string]*codecInstance)

	p.mu.Unlock()
	return lastErr
}

// MarshalJSON - serialize producer state in standard format
func (p *Producer) MarshalJSON() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Build medias list from available codecs
	var medias []string
	for _, media := range p.Medias {
		medias = append(medias, media.String())
	}

	// Build receivers list from active instances
	type receiverInfo struct {
		ID      byte        `json:"id"`
		Codec   interface{} `json:"codec"`
		Bytes   int         `json:"bytes"`
		Packets int         `json:"packets"`
	}

	var receivers []receiverInfo
	for _, inst := range p.instances {
		if inst.receiver != nil {
			receivers = append(receivers, receiverInfo{
				ID:      inst.receiver.ID,
				Codec:   inst.receiver.Codec,
				Bytes:   inst.receiver.Bytes,
				Packets: inst.receiver.Packets,
			})
		}
	}

	type jsonProducer struct {
		ID         uint32         `json:"id"`
		FormatName string         `json:"format_name"`
		Source     string         `json:"source"`
		Medias     []string       `json:"medias,omitempty"`
		Receivers  []receiverInfo `json:"receivers,omitempty"`
	}

	jp := jsonProducer{
		ID:         p.ID,
		FormatName: p.FormatName,
		Source:     p.url,
		Medias:     medias,
		Receivers:  receivers,
	}

	return json.Marshal(jp)
}

// buildURL - creates the ffmpeg URL for a specific codec
func (p *Producer) buildURL(codec *core.Codec) string {
	s := p.url

	// Add audio codec
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

	// Add other params (excluding audio which we already added)
	for key, values := range p.query {
		if key == "audio" {
			continue
		}
		for _, value := range values {
			if value == "" {
				s += "#" + key // Flag without value
			} else {
				s += "#" + key + "=" + value // Key=Value
			}
		}
	}

	return s
}
