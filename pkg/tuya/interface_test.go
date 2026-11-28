package tuya

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// The skill's sampleRate is 0 on some cameras and 90000 on others. A video
// codec without a clock breaks every timestamp downstream (MP4, HLS), and
// the RTP clock of H264 and H265 is 90000 regardless.
func TestVideoCodecsUseTheRTPClock(t *testing.T) {
	c := &TuyaClient{skill: &Skill{Videos: []VideoSkill{
		{StreamType: 2, CodecType: 2, SampleRate: 0},
		{StreamType: 4, CodecType: 4, SampleRate: 90000},
		{StreamType: 8, CodecType: 2, SampleRate: 16000},
	}}}

	codecs := c.GetVideoCodecs()
	require.Len(t, codecs, 3)
	for i, name := range []string{core.CodecH264, core.CodecH265, core.CodecH264} {
		require.Equal(t, name, codecs[i].Name)
		require.Equal(t, uint32(90000), codecs[i].ClockRate)
	}
}

// The web app asks for the main stream in HD and the sub stream in SD, also
// when the camera's skill lists no videos.
func TestStreamTypeWithoutVideos(t *testing.T) {
	c := &TuyaClient{skill: &Skill{WebRTC: 3}}
	require.Equal(t, 2, c.GetStreamType("hd"))
	require.Equal(t, 4, c.GetStreamType("sd"))

	c.skill.Videos = []VideoSkill{
		{StreamType: 4, Width: 640, Height: 360},
		{StreamType: 2, Width: 1920, Height: 1080},
	}
	require.Equal(t, 2, c.GetStreamType("hd"))
	require.Equal(t, 4, c.GetStreamType("sd"))
}

// The skill sampleRate field is unreliable (0 on some cameras, 90000 on
// others), and the video RTP clock rate is always 90000 regardless
// (RFC 7798 for H265, RFC 6184 for H264).
func TestGetVideoCodecs_ClockRate(t *testing.T) {
	c := &TuyaClient{skill: &Skill{Videos: []VideoSkill{
		{StreamType: 2, CodecType: 4, SampleRate: 0},     // HEVC, sampleRate unset
		{StreamType: 4, CodecType: 2, SampleRate: 90000}, // H264, sampleRate set
	}}}

	codecs := c.GetVideoCodecs()
	if len(codecs) != 2 {
		t.Fatalf("expected 2 codecs, got %d", len(codecs))
	}

	if codecs[0].Name != core.CodecH265 {
		t.Errorf("codec 0: expected H265, got %s", codecs[0].Name)
	}
	if codecs[1].Name != core.CodecH264 {
		t.Errorf("codec 1: expected H264, got %s", codecs[1].Name)
	}

	for i, codec := range codecs {
		if codec.ClockRate != 90000 {
			t.Errorf("codec %d (%s): expected ClockRate 90000, got %d", i, codec.Name, codec.ClockRate)
		}
	}
}
