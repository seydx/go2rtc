package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// Chrome takes the video size for MSE from the init segment. An H265 camera
// coding 1080 lines as 1088 must still get 1920x1080 in tkhd and hev1, or the
// picture is stretched (AlexxIT/go2rtc#2526).
func TestMuxerH265InitUsesDisplaySize(t *testing.T) {
	m := &Muxer{}
	m.AddTrack(&core.Codec{
		Name:      core.CodecH265,
		ClockRate: 90000,
		FmtpLine:  "sprop-sps=QgEGIWAAAAMAAAMAAAMAAAMAewAAoAPAgBEHy7ve96clEVcqn1KS5uAgICAQ",
	})
	init, err := m.GetInit()
	require.Nil(t, err)

	// VisualSampleEntry: 24 bytes after the type, then width and height
	i := bytes.Index(init, []byte("hev1"))
	require.Greater(t, i, 0)
	require.Equal(t, uint16(1920), binary.BigEndian.Uint16(init[i+28:]))
	require.Equal(t, uint16(1080), binary.BigEndian.Uint16(init[i+30:]))

	// tkhd ends with width and height as 16.16 fixed point
	i = bytes.Index(init, []byte("tkhd"))
	require.Greater(t, i, 0)
	end := i - 4 + int(binary.BigEndian.Uint32(init[i-4:]))
	require.Equal(t, uint32(1920), binary.BigEndian.Uint32(init[end-8:])>>16)
	require.Equal(t, uint32(1080), binary.BigEndian.Uint32(init[end-4:])>>16)
}
