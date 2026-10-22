package miss

import (
	"encoding/binary"
	"io"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/xiaomi/crypto"
	"github.com/stretchr/testify/require"
)

// camera hands out its media packets, then ends
type camera struct {
	Conn
	packets [][2][]byte
}

func (c *camera) ReadPacket() (hdr, payload []byte, err error) {
	if len(c.packets) == 0 {
		return nil, nil, io.EOF
	}
	p := c.packets[0]
	c.packets = c.packets[1:]
	return p[0], p[1], nil
}

// A payload shorter than its nonce took the whole process down. The packet is
// skipped, the next one counts.
func TestClientSkipsPacketWithoutNonce(t *testing.T) {
	key := make([]byte, 32)
	payload, err := crypto.Encode([]byte("frame"), key)
	require.NoError(t, err)

	hdr := make([]byte, hdrSize)
	binary.LittleEndian.PutUint32(hdr[4:], codecH264)
	c := &Client{Conn: &camera{packets: [][2][]byte{{hdr, nil}, {hdr, payload[:7]}, {hdr, payload}}}, key: key}

	pkt, err := c.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, uint32(codecH264), pkt.CodecID)
	require.Equal(t, "frame", string(pkt.Payload))

	_, err = c.ReadPacket()
	require.ErrorIs(t, err, io.EOF)
}
