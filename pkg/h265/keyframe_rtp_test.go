package h265

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsKeyframeRTP(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"idr slice", []byte{0x26, 0x01, 0xAF}, true},
		{"cra slice", []byte{0x2A, 0x01, 0xAF}, true},
		{"trail slice", []byte{0x02, 0x01, 0xD0}, false},
		{"vps alone", []byte{0x40, 0x01, 0x0C}, false},
		{"ap with idr", []byte{0x60, 0x01, 0x00, 0x03, 0x40, 0x01, 0x0C, 0x00, 0x03, 0x26, 0x01, 0xAF}, true},
		{"ap with parameter sets only", []byte{0x60, 0x01, 0x00, 0x03, 0x40, 0x01, 0x0C, 0x00, 0x03, 0x42, 0x01, 0x01}, false},
		{"ap cut short", []byte{0x60, 0x01, 0x00, 0x09, 0x26}, false},
		{"fu idr start", []byte{0x62, 0x01, 0x93, 0xAF}, true},
		{"fu idr middle", []byte{0x62, 0x01, 0x13, 0xAF}, false},
		{"fu trail start", []byte{0x62, 0x01, 0x81, 0xD0}, false},
		{"too short", []byte{0x26, 0x01}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, IsKeyframeRTP(tc.payload))
		})
	}
}

func TestIsKeyframeShortFrame(t *testing.T) {
	require.False(t, IsKeyframe(nil))
	require.False(t, IsKeyframe([]byte{0, 0, 0, 1}))
}
