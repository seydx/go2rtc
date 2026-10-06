//go:build !race

package h264

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

func TestGopCacheInputAllocs(t *testing.T) {
	cache := core.NewGopCache(benchCodec())
	allocs := allocsPerFrame(benchStream(4), cache.Input)
	require.LessOrEqual(t, allocs, 1.0)

	frames, _ := cache.Snapshot()
	require.Len(t, frames, 50)
}

func BenchmarkGopCacheInput(b *testing.B) {
	benchPackets(b, benchStream(4), core.NewGopCache(benchCodec()).Input)
}
