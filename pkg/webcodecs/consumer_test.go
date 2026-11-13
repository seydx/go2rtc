package webcodecs

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClockRunsOnWhenTheRTPTimestampWraps(t *testing.T) {
	const frame = 3600 // 25 fps at 90 kHz
	clock := rtpClock{rate: 90000}

	timestamp := uint32(1<<32 - 3*frame)
	last := clock.microseconds(timestamp)
	require.Equal(t, uint64(timestamp)*1_000_000/90000, last)

	for range 6 {
		timestamp += frame
		now := clock.microseconds(timestamp)
		require.Equal(t, uint64(40_000), now-last)
		last = now
	}
}

func TestClockFollowsAFrameShownBeforeItsPredecessor(t *testing.T) {
	clock := rtpClock{rate: 90000}

	require.Equal(t, uint64(1_000_000), clock.microseconds(90000))
	require.Equal(t, uint64(1_080_000), clock.microseconds(97200))
	require.Equal(t, uint64(1_040_000), clock.microseconds(93600))
}
