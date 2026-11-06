package onvif

import (
	"bytes"
	"encoding/xml"
	"io"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// streamingCapabilities reads RTP_TCP and RTP_RTSP_TCP of a response, as
// elements (GetCapabilities) or as attributes (GetServiceCapabilities)
func streamingCapabilities(t *testing.T, b []byte) map[string]string {
	t.Helper()
	caps := map[string]string{}
	d := xml.NewDecoder(bytes.NewReader(b))
	var element string
	for {
		token, err := d.Token()
		if err == io.EOF {
			return caps
		}
		require.NoError(t, err, "%s", b)
		switch token := token.(type) {
		case xml.StartElement:
			element = token.Name.Local
			for _, attr := range token.Attr {
				if attr.Name.Local == "RTP_TCP" || attr.Name.Local == "RTP_RTSP_TCP" {
					caps[attr.Name.Local] = attr.Value
				}
			}
		case xml.CharData:
			if element == "RTP_TCP" || element == "RTP_RTSP_TCP" {
				caps[element] = string(bytes.TrimSpace(token))
			}
		case xml.EndElement:
			element = ""
		}
	}
}

// Clients such as Hikvision ISC check RTP_TCP before they set up a stream,
// with false they skipped it. The RTSP server streams over TCP interleaved,
// what cameras declare as RTP_TCP too.
func TestStreamingOverTCP(t *testing.T) {
	want := map[string]string{"RTP_TCP": "true", "RTP_RTSP_TCP": "true"}
	require.Equal(t, want, streamingCapabilities(t, GetCapabilitiesResponse("http", "192.168.1.2:8899")))
	require.Equal(t, want, streamingCapabilities(t, StaticResponse(ServiceGetServiceCapabilities)))
}

const (
	fmtpH264High    = "packetization-mode=1;sprop-parameter-sets=Z2QAM6wVFKAoAPGQ,aO48sA=="                    // High, 2560x1920
	fmtpH264Base    = "packetization-mode=1;sprop-parameter-sets=Z0IAHvQCgC3I,aM48gA=="                        // Baseline, 1280x720
	fmtpH265Wide    = "sprop-sps=QgEBAWAAAAMAAAMAAAMAAAMAmaAAoAgBaH+KrTuiS7/8AAQABbAgApMuADN/mAE="             // 5120x1440
	fmtpH265Sub     = "sprop-sps=QgEBIUAAAAMAkAAAAwAAAwCWoAUCAWlnpbkShc1AQIC4QAAAAwBAAAAFFEn/eEAOpgAV+V8IBBA=" // 640x360
	fmtpH264Missing = "packetization-mode=1"
)

// A profile told H264 1920x1080 for every stream: UniFi Protect showed an
// H.265 stream black and couldn't tell the main stream from the sub stream.
// What the stream doesn't tell keeps that placeholder.
func TestNewVideo(t *testing.T) {
	for _, tc := range []struct {
		name, codec, fmtp string
		video             Video
	}{
		{"h264 high", core.CodecH264, fmtpH264High, Video{"H264", "High", 2560, 1920}},
		{"h264 baseline", core.CodecH264, fmtpH264Base, Video{"H264", "Baseline", 1280, 720}},
		{"h264 without sps", core.CodecH264, fmtpH264Missing, Video{"H264", "Main", 1920, 1080}},
		{"h265", core.CodecH265, fmtpH265Wide, Video{"H265", "", 5120, 1440}},
		{"h265 sub stream", core.CodecH265, fmtpH265Sub, Video{"H265", "", 640, 360}},
		{"h265 without sps", core.CodecH265, "", Video{"H265", "", 1920, 1080}},
		{"no video", "", "", Video{"H264", "Main", 1920, 1080}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.video, NewVideo(tc.codec, tc.fmtp))
		})
	}
}

type profilesXML struct {
	Profiles []struct {
		Token  string `xml:"token,attr"`
		Name   string `xml:"Name"`
		Source struct {
			Token       string `xml:"token,attr"`
			UseCount    int    `xml:"UseCount"`
			SourceToken string `xml:"SourceToken"`
			Bounds      struct {
				Width  int `xml:"width,attr"`
				Height int `xml:"height,attr"`
			} `xml:"Bounds"`
		} `xml:"VideoSourceConfiguration"`
		Encoder encoderXML `xml:"VideoEncoderConfiguration"`
	} `xml:"Body>GetProfilesResponse>Profiles"`
}

type encoderXML struct {
	Token    string `xml:"token,attr"`
	Encoding string `xml:"Encoding"`
	Width    int    `xml:"Resolution>Width"`
	Height   int    `xml:"Resolution>Height"`
	H264     *struct {
		Profile string `xml:"H264Profile"`
	} `xml:"H264"`
}

func (e *encoderXML) profile() string {
	if e.H264 == nil {
		return ""
	}
	return e.H264.Profile
}

func unmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	require.NoError(t, xml.Unmarshal(b, v), "%s", b)
}

// newSource is a camera with an H.265 main stream and an H.264 sub stream
func newSource() *VideoSource {
	source := &VideoSource{Token: "front"}
	source.Profiles = []*MediaProfile{
		{Token: "front_main", Name: "Front 1", Source: source, Video: NewVideo(core.CodecH265, fmtpH265Wide)},
		{Token: "front_sub", Name: "Front 2", Source: source, Video: NewVideo(core.CodecH264, fmtpH264Base)},
	}
	return source
}

// The profiles of a camera share its video source, each has the encoder
// configuration of its own stream.
func TestProfilesOfVideoSource(t *testing.T) {
	source := newSource()

	var res profilesXML
	unmarshal(t, GetProfilesResponse(source.Profiles), &res)
	require.Len(t, res.Profiles, 2)

	for i, want := range []struct {
		token, name, encoding, profile string
		width, height                  int
	}{
		{"front_main", "Front 1", "H265", "", 5120, 1440},
		{"front_sub", "Front 2", "H264", "Baseline", 1280, 720},
	} {
		p := res.Profiles[i]
		require.Equal(t, want.token, p.Token)
		require.Equal(t, want.name, p.Name)

		// the source: the size of its main stream
		require.Equal(t, "front", p.Source.Token)
		require.Equal(t, "front", p.Source.SourceToken)
		require.Equal(t, 2, p.Source.UseCount)
		require.Equal(t, 5120, p.Source.Bounds.Width)
		require.Equal(t, 1440, p.Source.Bounds.Height)

		require.Equal(t, want.token, p.Encoder.Token)
		require.Equal(t, want.encoding, p.Encoder.Encoding)
		require.Equal(t, want.width, p.Encoder.Width)
		require.Equal(t, want.height, p.Encoder.Height)
		require.Equal(t, want.encoding == "H264", p.Encoder.H264 != nil, "H264 settings of an H.264 stream only")
		require.Equal(t, want.profile, p.Encoder.profile())
	}

	var sources struct {
		Sources []struct {
			Token  string `xml:"token,attr"`
			Width  int    `xml:"Resolution>Width"`
			Height int    `xml:"Resolution>Height"`
		} `xml:"Body>GetVideoSourcesResponse>VideoSources"`
	}
	unmarshal(t, GetVideoSourcesResponse([]*VideoSource{source}), &sources)
	require.Len(t, sources.Sources, 1)
	require.Equal(t, "front", sources.Sources[0].Token)
	require.Equal(t, 5120, sources.Sources[0].Width)
	require.Equal(t, 1440, sources.Sources[0].Height)

	var configs struct {
		Configs []struct {
			Token string `xml:"token,attr"`
		} `xml:"Body>GetVideoSourceConfigurationsResponse>Configurations"`
	}
	unmarshal(t, GetVideoSourceConfigurationsResponse([]*VideoSource{source}), &configs)
	require.Len(t, configs.Configs, 1)
	require.Equal(t, "front", configs.Configs[0].Token)

	var encoders struct {
		Configs []encoderXML `xml:"Body>GetVideoEncoderConfigurationsResponse>Configurations"`
	}
	unmarshal(t, GetVideoEncoderConfigurationsResponse(source.Profiles), &encoders)
	require.Len(t, encoders.Configs, 2)
	require.Equal(t, "front_main", encoders.Configs[0].Token)
	require.Equal(t, "H265", encoders.Configs[0].Encoding)
	require.Equal(t, "front_sub", encoders.Configs[1].Token)
	require.Equal(t, 1280, encoders.Configs[1].Width)

	var encoder struct {
		Config encoderXML `xml:"Body>GetVideoEncoderConfigurationResponse>Configuration"`
	}
	unmarshal(t, GetVideoEncoderConfigurationResponse(source.Profiles[1]), &encoder)
	require.Equal(t, "front_sub", encoder.Config.Token)
	require.Equal(t, "Baseline", encoder.Config.profile())
}

// The options of a profile are its own settings. Media1 has no H.265 options,
// clients (UniFi Protect) read them from the extension.
func TestEncoderOptions(t *testing.T) {
	source := newSource()

	var options struct {
		H264 *struct {
			Width    int    `xml:"ResolutionsAvailable>Width"`
			Height   int    `xml:"ResolutionsAvailable>Height"`
			Profiles string `xml:"H264ProfilesSupported"`
		} `xml:"Body>GetVideoEncoderConfigurationOptionsResponse>Options>H264"`
		H265 *struct {
			Width  int `xml:"ResolutionsAvailable>Width"`
			Height int `xml:"ResolutionsAvailable>Height"`
		} `xml:"Body>GetVideoEncoderConfigurationOptionsResponse>Options>Extension>H265"`
	}
	unmarshal(t, GetVideoEncoderConfigurationOptionsResponse(source.Profiles[0]), &options)
	require.Nil(t, options.H264)
	require.NotNil(t, options.H265)
	require.Equal(t, 5120, options.H265.Width)
	require.Equal(t, 1440, options.H265.Height)

	options.H264, options.H265 = nil, nil
	unmarshal(t, GetVideoEncoderConfigurationOptionsResponse(source.Profiles[1]), &options)
	require.Nil(t, options.H265)
	require.NotNil(t, options.H264)
	require.Equal(t, 1280, options.H264.Width)
	require.Equal(t, "Baseline", options.H264.Profiles)
}

// A request for a profile the server doesn't have gets a fault: the client
// knows it is gone, instead of getting another one or a broken one
func TestFault(t *testing.T) {
	var fault struct {
		Values []string `xml:"Body>Fault>Code>Subcode>Subcode>Value"`
		Code   string   `xml:"Body>Fault>Code>Value"`
		Reason string   `xml:"Body>Fault>Reason>Text"`
	}
	unmarshal(t, FaultResponse("NoProfile", "no such profile <x>"), &fault)
	require.Equal(t, "s:Sender", fault.Code)
	require.Equal(t, []string{"ter:NoProfile"}, fault.Values)
	require.Equal(t, "no such profile <x>", fault.Reason)
}
