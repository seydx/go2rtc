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
