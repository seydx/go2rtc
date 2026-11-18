package miss

import (
	"encoding/binary"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/xiaomi/crypto"
	"github.com/stretchr/testify/require"
)

// A frame without an Annex B start code converts to no unit at all, and the
// probe read the type of the first one anyway: the process went down. Such a
// frame is passed over, the codec comes from the next one.
func TestProbeSkipsFrameWithoutUnits(t *testing.T) {
	key := make([]byte, 32)

	for _, tc := range []struct {
		codec uint32
		frame []byte
		name  string
	}{
		{codecH264, []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0x1f, 0xe9}, core.CodecH264},
		{codecH265, []byte{0, 0, 0, 1, 0x40, 0x01, 0x0c, 0x01}, core.CodecH265},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var packets [][2][]byte
			for _, frame := range [][]byte{
				{},                 // nothing
				{0x8f, 0x3a, 0x11}, // garbage
				{0, 0, 0, 1},       // a start code and nothing after it
				tc.frame,
			} {
				payload, err := crypto.Encode(frame, key)
				require.NoError(t, err)
				hdr := make([]byte, hdrSize)
				binary.LittleEndian.PutUint32(hdr[4:], tc.codec)
				packets = append(packets, [2][]byte{hdr, payload})
			}

			c := &Client{Conn: &camera{packets: packets}, key: key}

			var medias []*core.Media
			require.NotPanics(t, func() {
				var err error
				medias, err = probe(c, false)
				require.NoError(t, err)
			})
			require.Len(t, medias, 1)
			require.Equal(t, tc.name, medias[0].Codecs[0].Name)
		})
	}
}
