package dvrip

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/stretchr/testify/require"
)

// The low nibble of the media code is the codec, the high one differs between
// streams: a camera sends 0x52 for the H.264 of its 4K main stream (#2671), as
// H.265 comes as 0x43 and 0x53 too. Its video went missing.
func TestVideoTrackH264Codes(t *testing.T) {
	frame := annexb.EncodeToAVCC([]byte{
		0, 0, 0, 1, 0x67, 0x42, 0, 0x1f, 0xe9, // SPS
		0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80, // PPS
		0, 0, 0, 1, 0x65, 0x88, 0x84, // IDR
	})

	for _, code := range []byte{0x02, 0x12, 0x42, 0x52} {
		c := &Producer{}
		c.addVideoTrack(code, frame)
		require.NotNil(t, c.video, "media code %#x", code)
		require.Equal(t, core.CodecH264, c.video.Codec.Name)
		require.Contains(t, c.video.Codec.FmtpLine, "profile-level-id=42001f")
		require.Len(t, c.Medias, 1)
	}
}
