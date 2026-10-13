package mpegts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// H265 reaches the muxer without any repair in front of it: a unit with a
// damaged length prefix must not take the process down (HLS, mpegts).
func TestMuxerSurvivesMalformedAVCC(t *testing.T) {
	for _, streamType := range []byte{StreamTypeH264, StreamTypeH265} {
		m := NewMuxer()
		pid := m.AddTrack(streamType)

		for _, payload := range [][]byte{
			{0, 0, 0, 2, 0x26, 0x01, 0},                      // a byte after the last unit
			{0, 0, 0, 2, 0x26, 0x01, 0, 0, 0},                // three bytes after it
			{0x80, 0, 0, 0, 0x26, 0x01},                      // negative as a 32 bit int
			{0, 0, 0, 2, 0x26, 0x01, 0, 0, 0, 9, 0x02, 0x01}, // the last unit too long
		} {
			require.NotPanics(t, func() { m.GetPayload(pid, 0, payload) })
		}
	}
}
