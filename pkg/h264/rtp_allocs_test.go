//go:build !race

package h264

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// allocsPerFrame feeds the stream once to warm up, then reports the average
// allocations per access unit of further passes
func allocsPerFrame(packets []*rtp.Packet, handler core.HandlerFunc) float64 {
	var frames int
	for _, p := range packets {
		if p.Marker {
			frames++
		}
	}
	feed := func() {
		for _, p := range packets {
			handler(p)
		}
	}
	feed()
	return testing.AllocsPerRun(5, feed) / float64(frames)
}

// every depay in front of a viewer and the GOP cache runs per packet, only
// the access unit handed on may allocate
func TestRTPDepayAllocs(t *testing.T) {
	var frames int
	allocs := allocsPerFrame(benchStream(4), RTPDepay(benchCodec(), func(*rtp.Packet) { frames++ }))
	require.LessOrEqual(t, allocs, 1.0)
	require.Positive(t, frames)
}
