package onvif

import (
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
)

// VideoSource is the input of a camera, its profiles encode it: the main and
// the sub stream of a regular camera share one
type VideoSource struct {
	Token    string
	Profiles []*MediaProfile // the highest quality first
}

// MediaProfile is a stream of a video source. go2rtc stream name = profile token.
type MediaProfile struct {
	Token  string
	Name   string
	Source *VideoSource
	Video  Video
}

type Video struct {
	Encoding string // H264 or H265
	Profile  string // of H264: Baseline, Main, Extended or High
	Width    int
	Height   int
}

// placeholders for what a stream doesn't tell
const (
	defaultWidth     = 1920
	defaultHeight    = 1080
	defaultFrameRate = 30
	defaultBitrate   = 8192
)

// NewVideo describes the video of a stream from its codec, the resolution from
// the SPS of its fmtp. What it doesn't tell keeps a placeholder.
func NewVideo(codec, fmtp string) Video {
	v := Video{Encoding: "H264", Profile: "Main", Width: defaultWidth, Height: defaultHeight}

	switch codec {
	case core.CodecH264:
		sps, _ := h264.GetParameterSet(fmtp)
		if len(sps) == 0 {
			break
		}
		if s := h264.DecodeSPS(sps); s != nil && s.Width() > 0 && s.Height() > 0 {
			v.Width, v.Height = int(s.Width()), int(s.Height())
			switch profile := s.Profile(); profile {
			case "Baseline", "Main", "Extended", "High":
				v.Profile = profile
			default:
				v.Profile = "High" // the profiles above High (ex. High 10) are of its family
			}
		}

	case core.CodecH265:
		v.Encoding = "H265"
		v.Profile = ""
		if _, sps, _ := h265.GetParameterSet(fmtp); len(sps) > 2 {
			if s := h265.DecodeSPS(sps); s != nil && s.Width() > 0 && s.Height() > 0 {
				v.Width, v.Height = int(s.Width()), int(s.Height())
			}
		}
	}

	return v
}

// size is the one of the source: of its highest quality profile
func (s *VideoSource) size() (width, height int) {
	if len(s.Profiles) == 0 {
		return defaultWidth, defaultHeight
	}
	return s.Profiles[0].Video.Width, s.Profiles[0].Video.Height
}
