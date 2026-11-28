package wyze

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The best profile of a model, before buildK10056 adds its wire offset of 1.
// The Bulb Cam (HL_BC) needs wire byte 5 like the floodlight for 2304x1296,
// the usual 2K byte 4 leaves it at 360p (#2497).
func TestHDFrameSize(t *testing.T) {
	for model, frameSize := range map[string]uint8{
		"HL_BC":          FrameSizeFloodlight,
		"HL_CFL2":        FrameSizeFloodlight,
		"HL_CAM3P":       FrameSize2K,
		"HL_CAM4":        FrameSize2K,
		"WYZE_CAKP2JFUS": FrameSize1080P,
	} {
		c := &Client{model: model}
		require.Equal(t, frameSize, c.hdFrameSize(), model)
	}

	// quality 3 asks for 2K directly: the same profile for the Bulb Cam
	c := &Client{model: "HL_BC"}
	require.Equal(t, uint8(FrameSizeFloodlight), c.wireFrameSize2K())
}
