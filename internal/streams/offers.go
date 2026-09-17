package streams

import (
	"bytes"
	"strconv"
	"strings"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h265"
)

const (
	OffersLive    = "live"
	OffersCached  = "cached"
	OffersUnknown = "unknown"
)

// Offers is what a consumer can get from a stream across all of its
// producers: the primary producer decides whether a kind exists at all, every
// producer that would take part adds its codecs.
type Offers struct {
	State string `json:"state"`
	// one media per offered codec: video and audio sendonly, talk recvonly
	SDP         string            `json:"sdp"`
	Video       []*OfferCodec     `json:"video"`
	Audio       []*OfferCodec     `json:"audio"`
	Backchannel *BackchannelOffer `json:"backchannel"`
}

type OfferCodec struct {
	Codec       string `json:"codec"`
	Rate        uint32 `json:"rate,omitempty"`
	Channels    uint8  `json:"channels,omitempty"`
	Fmtp        string `json:"fmtp,omitempty"`
	PayloadType uint8  `json:"payload_type"`
	// ffprobe codec name, ex. "hevc", "aac", "pcm_alaw"
	FFmpeg string `json:"ffmpeg,omitempty"`
	// H264 and H265, ex. "High" or "Main 10", level times ten (51 for 5.1)
	Profile string `json:"profile,omitempty"`
	Level   uint8  `json:"level,omitempty"`
	// sent by the camera itself, not converted by another source
	Native bool `json:"native"`
}

type BackchannelOffer struct {
	Codecs []*OfferCodec `json:"codecs"`
	// the mixer converts any talk codec into one the camera takes
	Transcode bool `json:"transcode"`
}

// OfferMedias tells which medias a source will produce, known from its
// configuration before it ever ran (ex. ffmpeg:...#audio=opus).
type OfferMedias func(url string) []*core.Media

var offerHandlers = map[string]OfferMedias{}
var offerHandlersMu sync.RWMutex

// HandleOffers registers how to read the medias of a stopped source of scheme.
func HandleOffers(scheme string, handler OfferMedias) {
	offerHandlersMu.Lock()
	offerHandlers[scheme] = handler
	offerHandlersMu.Unlock()
}

func configMedias(url string) []*core.Media {
	scheme, _, ok := strings.Cut(url, ":")
	if !ok {
		return nil
	}
	offerHandlersMu.RLock()
	handler := offerHandlers[scheme]
	offerHandlersMu.RUnlock()
	if handler == nil {
		return nil
	}
	return handler(url)
}

// OffersOf builds the offers of a stream. It takes the stream and producer
// locks itself, never call it with them held.
func OffersOf(s *Stream) *Offers {
	s.mu.Lock()
	producers := append([]*Producer(nil), s.producers...)
	s.mu.Unlock()

	offers := &Offers{State: OffersUnknown, Video: []*OfferCodec{}, Audio: []*OfferCodec{}}

	var primary *Producer
	for _, prod := range producers {
		if !prod.requirePrevAudio && !prod.requirePrevVideo {
			primary = prod
			break
		}
	}
	if primary == nil {
		return offers
	}

	primaryMedias, state := primary.offerMedias()
	if state == OffersUnknown {
		return offers
	}
	offers.State = state

	hasVideo := findMedia(primaryMedias, core.KindVideo, core.DirectionRecvonly) != nil
	sourceAudio := findMedia(primaryMedias, core.KindAudio, core.DirectionRecvonly)
	hasAudio := sourceAudio != nil
	hasTalk := findMedia(primaryMedias, core.KindAudio, core.DirectionSendonly) != nil

	talk := &BackchannelOffer{Codecs: []*OfferCodec{}}

	for _, prod := range producers {
		if prod.requirePrevAudio && !hasAudio || prod.requirePrevVideo && !hasVideo {
			continue
		}

		medias := primaryMedias
		if prod != primary {
			if medias, state = prod.offerMedias(); state == OffersUnknown {
				medias = prod.visibleMedias(configMedias(prod.urlSnapshot()))
			}
		}

		for _, media := range medias {
			switch {
			case media.Kind == core.KindVideo && media.Direction == core.DirectionRecvonly && hasVideo:
				offers.Video = addOfferCodecs(offers.Video, media.Codecs, nil, prod == primary)
			case media.Kind == core.KindAudio && media.Direction == core.DirectionRecvonly && hasAudio:
				offers.Audio = addOfferCodecs(offers.Audio, media.Codecs, sourceAudio.Codecs[0], prod == primary)
			case media.Kind == core.KindAudio && media.Direction == core.DirectionSendonly && hasTalk:
				talk.Codecs = addOfferCodecs(talk.Codecs, media.Codecs, nil, prod == primary)
				if prod.mixingEnabled {
					talk.Transcode = true
				}
			}
		}
	}

	if len(talk.Codecs) > 0 {
		offers.Backchannel = talk
	}

	assignPayloadTypes(offers)
	offers.SDP = offersSDP(offers)

	return offers
}

// assignPayloadTypes gives codecs without a usable payload type (ex. an ffmpeg
// opus output known only from its options) a free dynamic one
func assignPayloadTypes(offers *Offers) {
	codecs := append(append([]*OfferCodec(nil), offers.Video...), offers.Audio...)
	if offers.Backchannel != nil {
		codecs = append(codecs, offers.Backchannel.Codecs...)
	}

	// 0 is PCMU, anything above 127 is not an RTP payload type (tapo sends 255)
	known := func(codec *OfferCodec) bool {
		return codec.PayloadType <= 127 && (codec.PayloadType != 0 || codec.Codec == core.CodecPCMU)
	}

	used := map[uint8]bool{}
	for _, codec := range codecs {
		if known(codec) {
			used[codec.PayloadType] = true
		}
	}

	next := uint8(96)
	for _, codec := range codecs {
		if known(codec) {
			continue
		}
		for used[next] && next < 127 {
			next++
		}
		codec.PayloadType = next
		used[next] = true
	}
}

func offersSDP(offers *Offers) string {
	var medias []*core.Media
	add := func(kind, direction string, codecs []*OfferCodec) {
		for _, codec := range codecs {
			medias = append(medias, &core.Media{
				Kind:      kind,
				Direction: direction,
				Codecs: []*core.Codec{{
					Name:        codec.Codec,
					ClockRate:   codec.Rate,
					Channels:    codec.Channels,
					FmtpLine:    codec.Fmtp,
					PayloadType: codec.PayloadType,
				}},
			})
		}
	}

	add(core.KindVideo, core.DirectionSendonly, offers.Video)
	add(core.KindAudio, core.DirectionSendonly, offers.Audio)
	if offers.Backchannel != nil {
		add(core.KindAudio, core.DirectionRecvonly, offers.Backchannel.Codecs)
	}

	if len(medias) == 0 {
		return ""
	}

	data, err := core.MarshalSDP("go2rtc", medias)
	if err != nil {
		return ""
	}
	return string(data)
}

// offerMedias returns the visible medias of the current session, or of the
// last one when the producer is not connected.
func (p *Producer) offerMedias() ([]*core.Media, string) {
	p.mu.RLock()
	conn := p.conn
	reconnecting := p.reconnecting
	last := p.lastMedias
	p.mu.RUnlock()

	switch {
	case conn != nil && !reconnecting:
		medias := conn.GetMedias()
		if narrowed, ok := narrowToReceivers(medias, p.receiversSnapshot()); ok {
			medias = narrowed
			p.mu.Lock()
			if p.conn == conn {
				p.lastMedias = narrowed
			}
			p.mu.Unlock()
		}
		return p.visibleMedias(p.preferTalkCodec(medias)), OffersLive
	case last != nil:
		return p.visibleMedias(p.preferTalkCodec(last)), OffersCached
	}
	return nil, OffersUnknown
}

func (p *Producer) rememberMedias(conn core.Producer) {
	medias := conn.GetMedias()
	if len(medias) == 0 {
		return
	}

	p.mu.Lock()
	if p.conn == conn {
		p.lastMedias = append([]*core.Media(nil), medias...)
	}
	p.mu.Unlock()
}

// keepMedias records the medias of a session that is closed right away.
func (p *Producer) keepMedias(medias []*core.Media) {
	if len(medias) == 0 {
		return
	}

	p.mu.Lock()
	p.lastMedias = append([]*core.Media(nil), medias...)
	p.mu.Unlock()
}

func (p *Producer) receiversSnapshot() []*core.Receiver {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]*core.Receiver(nil), p.receivers...)
}

// narrowToReceivers reduces a media that lists every codec its source might
// send (ex. tapo: H264 or H265) to the codec its running track receives.
func narrowToReceivers(medias []*core.Media, receivers []*core.Receiver) ([]*core.Media, bool) {
	var out []*core.Media
	for i, media := range medias {
		if media.Direction != core.DirectionRecvonly || len(media.Codecs) < 2 {
			continue
		}

		var actual []*core.Codec
		for _, receiver := range receivers {
			if receiver == nil || receiver.Codec == nil || receiver.Codec.Kind() != media.Kind {
				continue
			}
			// a track is negotiated with the first listed codec (tapo: H264), but
			// only the one the camera really sends ever gets a packet
			if _, packets := receiver.Stats(); packets == 0 {
				continue
			}
			for _, codec := range media.Codecs {
				if codec.Name == receiver.Codec.Name {
					actual = append(actual, receiver.Codec)
					break
				}
			}
		}
		if len(actual) == 0 {
			continue
		}

		if out == nil {
			out = append([]*core.Media(nil), medias...)
		}
		clone := *media
		clone.Codecs = actual
		out[i] = &clone
	}
	return out, out != nil
}

func (p *Producer) urlSnapshot() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.url
}

func findMedia(medias []*core.Media, kind, direction string) *core.Media {
	for _, media := range medias {
		if media.Kind == kind && media.Direction == direction && len(media.Codecs) > 0 {
			return media
		}
	}
	return nil
}

// addOfferCodecs appends codecs not listed yet. A codec without a rate keeps
// the rate and channels of the source audio, like an ffmpeg "aac" output.
func addOfferCodecs(list []*OfferCodec, codecs []*core.Codec, source *core.Codec, native bool) []*OfferCodec {
	for _, codec := range codecs {
		if codec.Name == core.CodecAll || codec.Name == core.CodecAny {
			continue
		}

		offer := &OfferCodec{
			Codec:       codec.Name,
			Rate:        codec.ClockRate,
			Channels:    codec.Channels,
			Fmtp:        codec.FmtpLine,
			PayloadType: codec.PayloadType,
			FFmpeg:      core.FFmpegCodecName(codec.Name),
			Native:      native,
		}
		if offer.Rate == 0 && source != nil {
			offer.Rate = source.ClockRate
			offer.Channels = source.Channels
		}
		if codec.IsAudio() {
			completeAudio(offer)
		}
		switch codec.Name {
		case core.CodecH264:
			offer.Profile, offer.Level = h264ProfileLevel(codec.FmtpLine)
		case core.CodecH265:
			offer.Profile, offer.Level = h265ProfileLevel(codec.FmtpLine)
		}

		key := offerKey(offer)
		duplicate := false
		for _, known := range list {
			if offerKey(known) == key {
				// the camera sends it too, a converted copy does not make it foreign
				known.Native = known.Native || native
				duplicate = true
				break
			}
		}
		if !duplicate {
			list = append(list, offer)
		}
	}
	return list
}

// completeAudio fills in what RTP fixes for a codec when the SDP or the source
// options leave it out, so a client never has to guess
func completeAudio(offer *OfferCodec) {
	switch {
	case offer.Codec == core.CodecOpus:
		offer.Rate, offer.Channels = 48000, 2
	case offer.Codec == core.CodecPCMU, offer.Codec == core.CodecPCMA, offer.Codec == core.CodecG722, strings.HasPrefix(offer.Codec, core.CodecG726):
		if offer.Rate == 0 {
			offer.Rate = 8000
		}
	}

	// an SDP without encoding parameters means one channel
	if offer.Channels == 0 {
		offer.Channels = 1
	}

	// static payload types, PCMU is 0 already
	if offer.PayloadType == 0 && offer.Rate == 8000 {
		switch offer.Codec {
		case core.CodecPCMA:
			offer.PayloadType = 8
		case core.CodecG722:
			offer.PayloadType = 9
		}
	}
}

// h264ProfileLevel reads the SPS the stream actually carries, and only falls
// back to profile-level-id, which some cameras fill in wrong, without one.
func h264ProfileLevel(fmtp string) (string, uint8) {
	if profile, level := core.DecodeH264(fmtp); profile != "" {
		return profile, level
	}

	id := core.Between(fmtp, "profile-level-id=", ";")
	if len(id) < 6 {
		return "", 0
	}
	raw, err := strconv.ParseUint(id[:6], 16, 32)
	if err != nil {
		return "", 0
	}
	switch raw >> 16 {
	case 0x42:
		return "Baseline", uint8(raw)
	case 0x4D:
		return "Main", uint8(raw)
	case 0x58:
		return "Extended", uint8(raw)
	case 0x64:
		return "High", uint8(raw)
	}
	return "", 0
}

// h265ProfileLevel reads profile_tier_level from the SPS. Its fields sit at a
// fixed offset, so profiles the full SPS decoder skips (Main 10) are read too.
func h265ProfileLevel(fmtp string) (string, uint8) {
	_, sps, _ := h265.GetParameterSet(fmtp)
	if len(sps) < 3 {
		return "", 0
	}

	rbsp := bytes.ReplaceAll(sps[2:], []byte{0, 0, 3}, []byte{0, 0})
	if len(rbsp) < 13 {
		return "", 0
	}

	var profile string
	switch rbsp[1] & 0x1F {
	case 1:
		profile = "Main"
	case 2:
		profile = "Main 10"
	case 3:
		profile = "Main Still Picture"
	case 4:
		profile = "Range Extensions"
	default:
		return "", 0
	}

	// general_level_idc is the level times 30
	return profile, rbsp[12] / 3
}

func offerKey(codec *OfferCodec) string {
	return codec.Codec + "/" + strconv.Itoa(int(codec.Rate)) + "/" + strconv.Itoa(int(codec.Channels))
}
