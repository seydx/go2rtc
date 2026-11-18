package legacy

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/tutk"
	"github.com/stretchr/testify/require"
)

// A frame without an Annex B start code converts to no unit at all, and the
// probe read the type of the first one anyway: the process went down. Such a
// frame gives no codec, the probe goes on with the next one.
func TestVideoCodecOfFrameWithoutUnits(t *testing.T) {
	for _, codec := range []byte{tutk.CodecH264, tutk.CodecH265} {
		for _, frame := range [][]byte{
			nil,
			{0x8f, 0x3a, 0x11}, // garbage
			{0, 0, 0, 1},       // a start code and nothing after it
		} {
			require.NotPanics(t, func() {
				require.Nil(t, videoCodec(codec, frame))
			})
		}
	}

	h264 := videoCodec(tutk.CodecH264, []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0x1f, 0xe9})
	require.Equal(t, core.CodecH264, h264.Name)
	require.Contains(t, h264.FmtpLine, "profile-level-id=42001f")

	h265 := videoCodec(tutk.CodecH265, []byte{0, 0, 0, 1, 0x40, 0x01, 0x0c, 0x01})
	require.Equal(t, core.CodecH265, h265.Name)
	require.Contains(t, h265.FmtpLine, "sprop-vps=QAEMAQ==")

	require.Nil(t, videoCodec(tutk.CodecH264, []byte{0, 0, 0, 1, 0x41, 0x9a}), "a frame without SPS")
}
