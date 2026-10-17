package rtmp

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// peer writes RTMP messages the way the other side does, in chunks of its
// chunk size
type peer struct {
	buf  bytes.Buffer
	conn *Conn
}

func newPeer() *peer {
	p := &peer{}
	p.conn = &Conn{wr: &p.buf, wrPacketSize: 128}
	return p
}

func (p *peer) write(tagType byte, payload []byte) *peer {
	if err := p.conn.writeMessage(4, tagType, 0, payload); err != nil {
		panic(err)
	}
	return p
}

// setChunkSize is sent in chunks of the old size, the size is for the chunks
// after it
func (p *peer) setChunkSize(size uint32) *peer {
	p.write(TypeSetPacketSize, binary.BigEndian.AppendUint32(nil, size))
	p.conn.wrPacketSize = size
	return p
}

func (p *peer) reader() *Conn {
	return &Conn{rd: &p.buf, wr: io.Discard, chunks: map[uint8]*chunk{}, rdPacketSize: 128}
}

func payload(size int, seed byte) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

// readVideo reads the messages up to the next video message
func readVideo(t *testing.T, c *Conn) []byte {
	t.Helper()
	for {
		msgType, _, b, err := c.readMessage()
		require.NoError(t, err)
		if msgType == TypeVideo {
			return b
		}
	}
}

// The peer can change the size of its chunks at any time. Applied only while
// the commands were read, a change while streaming cut the next frames at the
// old size: the chunk headers ended up in the frames, and from there on the
// stream was garbage.
func TestReadMessageAppliesTheChunkSizeWhileStreaming(t *testing.T) {
	frame1, frame2, frame3, frame4 := payload(300, 1), payload(10000, 2), payload(1000, 3), payload(50, 4)

	p := newPeer()
	p.write(TypeVideo, frame1)
	p.setChunkSize(4096).write(TypeVideo, frame2)
	// the change itself in more than one chunk
	p.setChunkSize(2).setChunkSize(300).write(TypeVideo, frame3)
	p.setChunkSize(1).write(TypeVideo, frame4)

	c := p.reader()
	require.Equal(t, frame1, readVideo(t, c))
	require.Equal(t, frame2, readVideo(t, c))
	require.Equal(t, frame3, readVideo(t, c))
	require.Equal(t, frame4, readVideo(t, c))
	require.Equal(t, uint32(1), c.rdPacketSize)

	_, _, _, err := c.readMessage()
	require.ErrorIs(t, err, io.EOF)
}

// The same while the FLV of a play or a publish is read.
func TestReadAppliesTheChunkSize(t *testing.T) {
	frame := payload(5000, 5)

	p := newPeer()
	p.setChunkSize(4096).write(TypeVideo, frame)

	c := p.reader()
	b := make([]byte, 4+11+4+4+11+len(frame))
	var n int
	for n < len(b) {
		m, err := c.Read(b[n:])
		require.NoError(t, err)
		n += m
	}
	// the change is passed on, the FLV demuxer skips it
	require.Equal(t, byte(TypeSetPacketSize), b[4])
	require.Equal(t, uint32(4), Uint24(b[5:]))
	b = b[4+11+4:]

	require.Equal(t, byte(TypeVideo), b[4])
	require.Equal(t, uint32(len(frame)), Uint24(b[5:]))
	require.Equal(t, frame, b[4+11:])
}

// A chunk size shorter than its four bytes, or a chunk size of 0, is no size
// the next chunks can be read with. The short one panicked while the commands
// were read: a single message of any client took the whole process down. With
// 0 the next message read the rest of the connection one byte at a time.
func TestWrongChunkSize(t *testing.T) {
	for name, size := range map[string][]byte{
		"empty": nil,
		"short": {0, 0, 16},
		"zero":  {0, 0, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			stream := func() *Conn {
				p := newPeer()
				p.write(TypeSetPacketSize, size).write(TypeVideo, payload(1000, 6))
				return p.reader()
			}

			// the server, while it reads the commands
			c := stream()
			require.NotPanics(t, func() {
				require.EqualError(t, c.ReadCommands(), "rtmp: wrong chunk size "+hexOf(size))
			})

			// the client, while it waits for the response
			c = stream()
			require.NotPanics(t, func() {
				_, err := c.readResponse(func([]any) bool { return true })
				require.EqualError(t, err, "rtmp: wrong chunk size "+hexOf(size))
			})

			// both, while streaming
			c = stream()
			_, err := c.Read(make([]byte, 64*1024))
			require.EqualError(t, err, "rtmp: wrong chunk size "+hexOf(size))
			require.Equal(t, uint32(128), c.rdPacketSize)
		})
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	var s []byte
	for _, v := range b {
		s = append(s, digits[v>>4], digits[v&0xF])
	}
	return string(s)
}

// Larger messages of a chunk size are fine, the size is the first four bytes.
func TestChunkSizeWithMoreBytes(t *testing.T) {
	frame := payload(500, 7)

	p := newPeer()
	p.write(TypeSetPacketSize, []byte{0, 0, 2, 0, 0xFF})
	p.conn.wrPacketSize = 512
	p.write(TypeVideo, frame)

	c := p.reader()
	require.Equal(t, frame, readVideo(t, c))
	require.Equal(t, uint32(512), c.rdPacketSize)
}

func FuzzReadMessage(f *testing.F) {
	p := newPeer()
	p.write(TypeVideo, payload(300, 1)).setChunkSize(4096).write(TypeAudio, payload(5000, 2))
	f.Add(p.buf.Bytes())
	f.Add([]byte{0x02, 0, 0, 0, 0, 0, 2, TypeSetPacketSize, 0, 0, 0, 0, 0, 1})

	f.Fuzz(func(t *testing.T, b []byte) {
		c := &Conn{rd: bytes.NewReader(b), wr: io.Discard, chunks: map[uint8]*chunk{}, rdPacketSize: 128}
		for {
			if _, _, _, err := c.readMessage(); err != nil {
				return
			}
		}
	})
}
