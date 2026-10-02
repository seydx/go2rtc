package creds

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
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

// The log chain of internal/app: a secret in a line split the event, the
// console writer failed to decode the first part and the line was lost.
func TestSecretWriterKeepsEventWhole(t *testing.T) {
	AddSecret("WHOLE-EVENT-SECRET")

	var errs []error
	zerolog.ErrorHandler = func(err error) { errs = append(errs, err) }
	t.Cleanup(func() { zerolog.ErrorHandler = nil })

	var console, memory bytes.Buffer
	logger := zerolog.New(SecretWriter(zerolog.MultiLevelWriter(&zerolog.ConsoleWriter{Out: &console, NoColor: true}, &memory)))
	logger.Info().Msg("start producer url=http://cam/snap?token=WHOLE-EVENT-SECRET&ch=1")

	require.Empty(t, errs)
	require.Contains(t, console.String(), "start producer url=http://cam/snap?token=***&ch=1")

	var event map[string]any
	require.NoError(t, json.Unmarshal(memory.Bytes(), &event))
	require.Equal(t, "start producer url=http://cam/snap?token=***&ch=1", event["message"])
}
