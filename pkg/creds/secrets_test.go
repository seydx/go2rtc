package creds

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestString(t *testing.T) {
	AddSecret("admin")
	AddSecret("pa$$word")

	// userinfo is masked as a whole, registered or not
	s := SecretString("rtsp://admin:pa$$word@192.168.1.123/stream1")
	require.Equal(t, "rtsp://***@192.168.1.123/stream1", s)

	// outside userinfo only the registered secrets are masked
	s = SecretString("rtsp://192.168.1.123/stream1?user=admin&password=pa$$word")
	require.Equal(t, "rtsp://192.168.1.123/stream1?user=***&password=***", s)
}
