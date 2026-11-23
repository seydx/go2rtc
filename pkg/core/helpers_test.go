package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A plain number is seconds, like ?duration=10 always was. A unit gives
// any duration, like ?timeout=500ms. Anything else is the default.
func TestParseDuration(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want time.Duration
	}{
		{"10", 10 * time.Second},
		{"0", 0},
		{"500ms", 500 * time.Millisecond},
		{"1m30s", 90 * time.Second},
		{"", 7 * time.Second},
		{"abc", 7 * time.Second},
	} {
		require.Equal(t, tc.want, ParseDuration(tc.s, 7*time.Second), tc.s)
	}
}
