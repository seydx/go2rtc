package crypto

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Data shorter than its nonce took the whole process down.
func TestDecodeRefusesDataWithoutNonce(t *testing.T) {
	key := make([]byte, 32)
	for n := range 8 {
		_, err := Decode(make([]byte, n), key)
		require.Error(t, err)
	}

	for _, s := range []string{"", "frame"} {
		b, err := Encode([]byte(s), key)
		require.NoError(t, err)
		b, err = Decode(b, key)
		require.NoError(t, err)
		require.Equal(t, s, string(b))
	}
}
