package webrtc

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

type capturingWriter struct {
	header rtp.Header
}

func (w *capturingWriter) WriteRTP(header *rtp.Header, payload []byte) (int, error) {
	w.header = *header
	return len(payload), nil
}

func (w *capturingWriter) Write(b []byte) (int, error) {
	return len(b), nil
}

func TestWriteRTPDropsSourceExtensions(t *testing.T) {
	source := &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 1}, Payload: []byte{1}}
	require.NoError(t, source.Header.SetExtension(4, []byte{0x01, 0x00}))

	writer := &capturingWriter{}
	track := &Track{ssrc: 7, writer: writer}
	require.NoError(t, track.WriteRTP(111, source))

	require.False(t, writer.header.Extension)
	require.Empty(t, writer.header.Extensions)
	require.Equal(t, uint32(7), writer.header.SSRC)
	require.Equal(t, uint8(111), writer.header.PayloadType)
	require.True(t, source.Header.Extension, "the source packet is shared with other consumers")
}
