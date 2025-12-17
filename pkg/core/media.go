package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pion/sdp/v3"
)

// Media take best from:
// - deepch/vdk/format/rtsp/sdp.Media
// - pion/sdp.MediaDescription
type Media struct {
	Kind      string   `json:"kind,omitempty"`      // video or audio
	Direction string   `json:"direction,omitempty"` // sendonly, recvonly
	Codecs    []*Codec `json:"codecs,omitempty"`

	ID      string `json:"id,omitempty"` // MID for WebRTC, Control for RTSP
	Channel byte   // Channel for RTSP
}

func (m *Media) String() string {
	s := fmt.Sprintf("%s, %s", m.Kind, m.Direction)
	for _, codec := range m.Codecs {
		name := codec.String()

		if strings.Contains(s, name) {
			continue
		}

		s += ", " + name
	}
	return s
}

func (m *Media) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

func (m *Media) Clone() *Media {
	clone := *m
	clone.Codecs = make([]*Codec, len(m.Codecs))
	for i, codec := range m.Codecs {
		clone.Codecs[i] = codec.Clone()
	}
	return &clone
}

func (m *Media) MatchMedia(remote *Media) (codec, remoteCodec *Codec) {
	// check same kind and opposite dirrection
	if m.Kind != remote.Kind ||
		m.Direction == DirectionSendonly && remote.Direction != DirectionRecvonly ||
		m.Direction == DirectionRecvonly && remote.Direction != DirectionSendonly {
		return nil, nil
	}

	for _, codec = range m.Codecs {
		for _, remoteCodec = range remote.Codecs {
			if codec.Match(remoteCodec) {
				return
			}
		}
	}

	return nil, nil
}

func (m *Media) MatchCodec(remote *Codec) *Codec {
	for _, codec := range m.Codecs {
		if codec.Match(remote) {
			return codec
		}
	}
	return nil
}

func (m *Media) MatchAll() bool {
	for _, codec := range m.Codecs {
		if codec.Name == CodecAll {
			return true
		}
	}
	return false
}

func (m *Media) Equal(media *Media) bool {
	if media.ID != "" {
		return m.ID == media.ID
	}
	return m.String() == media.String()
}

func GetKind(name string) string {
	switch name {
	case CodecH264, CodecH265, CodecVP8, CodecVP9, CodecAV1, CodecJPEG, CodecRAW:
		return KindVideo
	case CodecPCMU, CodecPCMA, CodecAAC, CodecOpus, CodecG722, CodecMP3, CodecPCM, CodecPCML, CodecELD, CodecFLAC:
		return KindAudio
	}

	// G726 variants: G726-16, G726-24, G726-32, G726-40
	if strings.HasPrefix(name, CodecG726) {
		return KindAudio
	}

	return ""
}

func MarshalSDP(name string, medias []*Media) ([]byte, error) {
	sd := &sdp.SessionDescription{
		Origin: sdp.Origin{
			Username: "-", SessionID: 1, SessionVersion: 1,
			NetworkType: "IN", AddressType: "IP4", UnicastAddress: "0.0.0.0",
		},
		SessionName: sdp.SessionName(name),
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN", AddressType: "IP4", Address: &sdp.Address{
				Address: "0.0.0.0",
			},
		},
		TimeDescriptions: []sdp.TimeDescription{
			{Timing: sdp.Timing{}},
		},
	}

	for _, media := range medias {
		if media.Codecs == nil {
			continue
		}

		codec := media.Codecs[0]

		switch codec.Name {
		case CodecELD:
			name = CodecAAC
		case CodecPCML:
			name = CodecPCM // beacuse we using pcm.LittleToBig for RTSP server
		default:
			name = codec.Name
		}

		md := &sdp.MediaDescription{
			MediaName: sdp.MediaName{
				Media:  media.Kind,
				Protos: []string{"RTP", "AVP"},
			},
		}
		md.WithCodec(codec.PayloadType, name, codec.ClockRate, uint16(codec.Channels), codec.FmtpLine)

		if media.Direction != "" {
			md.WithPropertyAttribute(media.Direction)
		}

		if media.ID != "" {
			md.WithValueAttribute("control", media.ID)
		}

		sd.MediaDescriptions = append(sd.MediaDescriptions, md)
	}

	return sd.Marshal()
}

func UnmarshalMedia(md *sdp.MediaDescription) *Media {
	m := &Media{
		Kind: md.MediaName.Media,
	}

	for _, attr := range md.Attributes {
		switch attr.Key {
		case DirectionSendonly, DirectionRecvonly, DirectionSendRecv:
			m.Direction = attr.Key
		case "control", "mid":
			m.ID = attr.Value
		}
	}

	for _, format := range md.MediaName.Formats {
		codec := UnmarshalCodec(md, format)
		// Filter out G726 variants with non-8kHz sample rate for sendonly (backchannel)
		if m.Direction == DirectionSendonly &&
			strings.HasPrefix(codec.Name, CodecG726) && codec.ClockRate != 8000 {
			continue
		}
		m.Codecs = append(m.Codecs, codec)
	}

	return m
}

func ParseQuery(query map[string][]string) (medias []*Media) {
	// set media candidates from query list
	for key, values := range query {
		switch key {
		case KindVideo, KindAudio:
			for _, value := range values {
				media := &Media{Kind: key, Direction: DirectionSendonly}

				for _, name := range strings.Split(value, ",") {
					media.Codecs = append(media.Codecs, ParseQueryCodec(name))
				}

				medias = append(medias, media)
			}
		}
	}

	return
}

// ParseQueryCodec parses a codec string from query parameters like "PCMU" or "PCMU/8000" or "OPUS/48000/2"
// and returns a Codec with Name, ClockRate, and optionally Channels set.
// Unlike ParseCodecString, this handles aliases and unknown codecs (for ANY matching).
func ParseQueryCodec(s string) *Codec {
	s = strings.ToUpper(s)

	// Split first to check aliases on the base name
	parts := strings.Split(s, "/")
	baseName := parts[0]

	// check aliases first
	switch baseName {
	case "", "COPY":
		return &Codec{Name: CodecAny}
	case "MJPEG":
		baseName = CodecJPEG
	case "AAC":
		baseName = CodecAAC
	case "MP3":
		baseName = CodecMP3
	}

	codec := &Codec{Name: baseName}

	// Set defaults for known codecs if not specified in query
	if len(parts) < 2 {
		switch baseName {
		// Audio codecs with static payload types
		case CodecPCMU:
			codec.ClockRate = 8000
			codec.PayloadType = 0
		case CodecPCMA:
			codec.ClockRate = 8000
			codec.PayloadType = 8
		case CodecG722:
			codec.ClockRate = 8000 // Note: actual sample rate is 16kHz but RTP uses 8000
			codec.PayloadType = 9
		case CodecMP3:
			codec.ClockRate = 90000
			codec.PayloadType = 14
		// Audio codecs with dynamic payload types
		case CodecOpus:
			codec.ClockRate = 48000
			codec.Channels = 2
			codec.PayloadType = 111
		case CodecAAC:
			codec.ClockRate = 44100
			codec.Channels = 2
			codec.PayloadType = 96
		case CodecELD:
			codec.ClockRate = 44100
			codec.Channels = 2
			codec.PayloadType = 96
		case CodecPCM: // L16 big endian
			codec.ClockRate = 44100
			codec.Channels = 2
			codec.PayloadType = 97
		case CodecPCML: // L16 little endian
			codec.ClockRate = 44100
			codec.Channels = 2
			codec.PayloadType = 97
		case CodecFLAC:
			codec.ClockRate = 44100
			codec.Channels = 2
			codec.PayloadType = 98
		// Video codecs with static payload types
		case CodecJPEG:
			codec.ClockRate = 90000
			codec.PayloadType = 26
		// Video codecs with dynamic payload types
		case CodecH264:
			codec.ClockRate = 90000
			codec.PayloadType = 96
		case CodecH265:
			codec.ClockRate = 90000
			codec.PayloadType = 96
		case CodecVP8:
			codec.ClockRate = 90000
			codec.PayloadType = 96
		case CodecVP9:
			codec.ClockRate = 90000
			codec.PayloadType = 96
		case CodecAV1:
			codec.ClockRate = 90000
			codec.PayloadType = 96
		default:
			// G726 variants: G726, G726-16, G726-24, G726-32, G726-40
			if strings.HasPrefix(baseName, CodecG726) {
				codec.ClockRate = 8000
				codec.PayloadType = 96
			}
		}
	}

	if len(parts) >= 2 {
		codec.ClockRate = uint32(Atoi(parts[1]))
	}
	if len(parts) >= 3 {
		codec.Channels = uint8(Atoi(parts[2]))
	}

	return codec
}
