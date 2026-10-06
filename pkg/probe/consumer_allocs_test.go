//go:build !race

package probe

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// a preloaded stream only needs its bytes counted once the fmtp is known
func TestProbeAllocsAfterFirstKeyframe(t *testing.T) {
	packets := h264Stream(4)
	media, track := h264Track()
	handler := attach(t, media, track).Senders[0].Handler

	feed := func() {
		for _, packet := range packets {
			handler(packet)
		}
	}
	feed()
	allocs := testing.AllocsPerRun(5, feed) / float64(len(packets))
	require.Less(t, allocs, 0.01)
}

func TestProbeWithGopCacheStopsDepaying(t *testing.T) {
	p, _, packets := attachAfterGop(t)
	handler := p.Senders[0].Handler

	allocs := testing.AllocsPerRun(5, func() {
		for _, packet := range packets {
			handler(packet)
		}
	}) / float64(len(packets))
	require.Less(t, allocs, 0.01)
}
