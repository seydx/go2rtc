package mqtt

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

var _ io.ByteWriter = (*Message)(nil)

// Remaining length encoding at the boundaries of the MQTT variable byte
// integer: 7 bits per byte, the high bit continues.
func TestWriteLen(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want []byte
	}{
		{1, []byte{0x01}},
		{127, []byte{0x7F}},
		{128, []byte{0x80, 0x01}},
		{16383, []byte{0xFF, 0x7F}},
		{16384, []byte{0x80, 0x80, 0x01}},
		{2097151, []byte{0xFF, 0xFF, 0x7F}},
		{2097152, []byte{0x80, 0x80, 0x80, 0x01}},
	} {
		m := &Message{}
		m.WriteLen(tc.n)
		require.Equal(t, tc.want, m.Bytes(), "%d", tc.n)

		n, err := ReadLen(bytes.NewReader(m.Bytes()))
		require.NoError(t, err)
		require.Equal(t, uint32(tc.n), n)
	}
}

// Every message announces exactly the remaining length it carries.
func TestMessageRemainingLength(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, 200) // two byte remaining length
	for name, m := range map[string]*Message{
		"connect":   NewConnect("go2rtc", "user", "secret"),
		"subscribe": NewSubscribe(1, "topic/#", 1),
		"publish":   NewPublish("topic", payload),
		"publish1":  NewPublishQOS1(2, "topic", payload),
	} {
		r := bytes.NewReader(m.Bytes()[1:])
		n, err := ReadLen(r)
		require.NoError(t, err, name)
		require.Equal(t, uint32(r.Len()), n, name)
	}
}
