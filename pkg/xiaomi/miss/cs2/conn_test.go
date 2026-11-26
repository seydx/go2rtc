package cs2

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The same IPv4 address comes in two forms: 4 bytes, and 16 bytes as from
// net.ParseIP or a dual stack socket. A host without IPv6 reads 4 bytes from
// the camera while its address was parsed to 16. Here it's the other way
// round, the camera's address is the 4 byte form. Either way it is the camera.
func TestUDPConnMatchesIPInEitherForm(t *testing.T) {
	answer := make([]byte, 16)
	camera, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = camera.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			_, from, err := camera.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = camera.WriteToUDP(answer, from)
		}
	}()

	conn, err := net.ListenUDP("udp", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))

	cameraAddr := camera.LocalAddr().(*net.UDPAddr)
	c := &udpConn{UDPConn: conn, addr: &net.UDPAddr{IP: cameraAddr.IP.To4(), Port: cameraAddr.Port}}

	res, err := c.WriteUntil([]byte("hello"), func([]byte) bool { return true })
	require.NoError(t, err)
	require.Equal(t, answer, res)

	// a short message counts only when it comes from the camera
	_, err = camera.WriteToUDP([]byte{1, 2, 3}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: conn.LocalAddr().(*net.UDPAddr).Port})
	require.NoError(t, err)
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 3, n)
}

// camera hands its datagrams to the worker, then closes the connection
type camera struct {
	net.Conn
	datagrams [][]byte
}

func (c *camera) Read(b []byte) (int, error) {
	if len(c.datagrams) == 0 {
		return 0, io.EOF
	}
	n := copy(b, c.datagrams[0])
	c.datagrams = c.datagrams[1:]
	return n, nil
}

func (c *camera) Write(b []byte) (int, error) {
	return len(b), nil
}

// receive runs the worker over the datagrams until the connection ends
func receive(isTCP bool, datagrams ...[]byte) *Conn {
	c := newConn(&camera{datagrams: datagrams}, isTCP)
	c.worker()
	return c
}

// drw is a datagram with the next bytes of channel ch
func drw(ch byte, seq uint16, data ...[]byte) []byte {
	b := []byte{magic, msgDrw, 0, 0, magicDrw, ch, byte(seq >> 8), byte(seq)}
	for _, d := range data {
		b = append(b, d...)
	}
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)-4))
	return b
}

// sized is data with its size in front, as the channels carry it
func sized(data []byte) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(data))), data...)
}

// media is a media packet with a zero header
func media(payload string) []byte {
	return sized(append(make([]byte, hdrSize), payload...))
}

// readPayloads returns the payloads of the media channel up to its end
func readPayloads(t *testing.T, c *Conn) (payloads []string, err error) {
	t.Helper()
	for {
		hdr, payload, err := c.ReadPacket()
		if err != nil {
			return payloads, err
		}
		require.Len(t, hdr, hdrSize)
		payloads = append(payloads, string(payload))
	}
}

// A datagram that ends before its data, or comes for a channel nobody reads,
// took the whole process down. It is skipped, the stream goes on.
func TestWorkerSkipsBrokenDatagrams(t *testing.T) {
	for _, tc := range []struct {
		name     string
		datagram []byte
	}{
		{"one byte", []byte{magic}},
		{"drw without seq", []byte{magic, msgDrw, 0, 2, magicDrw, 2}},
		{"drw on channel 1", drw(1, 0, media("lost"))},
		{"drw on channel 3", drw(3, 0, media("lost"))},
		{"drw on channel 255", drw(255, 0, media("lost"))},
	} {
		for _, isTCP := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s tcp=%t", tc.name, isTCP), func(t *testing.T) {
				c := receive(isTCP, drw(2, 0, media("one")), tc.datagram, drw(2, 1, media("two")))
				payloads, err := readPayloads(t, c)
				require.ErrorIs(t, err, io.EOF)
				require.Equal(t, []string{"one", "two"}, payloads)
			})
		}
	}
}

// A datagram too short for its type isn't taken for the one before it.
func TestWorkerSkipsEmptyDatagrams(t *testing.T) {
	var acks int
	c := newConn(&camera{datagrams: [][]byte{{magic, msgDrwAck, 0, 6, magicDrw, 0, 0, 1, 0, 0}, {}, {magic}}}, false)
	c.cmdAck = func() { acks++ }
	c.worker()
	require.Equal(t, 1, acks)
}

// Data too short for its header can't be read: it is skipped, the next one
// counts. Size 0 carries no data at all and takes no place in the queue.
func TestReadSkipsShortData(t *testing.T) {
	t.Run("size 0", func(t *testing.T) {
		c := newDataChannel(0, 1)
		require.NoError(t, c.Push(append(sized(nil), sized([]byte("data"))...)))
		require.Equal(t, "data", string(<-c.popBuf))
	})

	t.Run("media", func(t *testing.T) {
		c := receive(false, drw(2, 0, sized(nil), sized(make([]byte, hdrSize-1)), media("one")))
		payloads, err := readPayloads(t, c)
		require.ErrorIs(t, err, io.EOF)
		require.Equal(t, []string{"one"}, payloads)
	})

	t.Run("command", func(t *testing.T) {
		c := receive(false, drw(0, 0, sized(nil), sized([]byte{1, 2, 3}), sized([]byte{0x01, 0x01, 0, 0, '{', '}'})))
		cmd, data, err := c.ReadCommand()
		require.NoError(t, err)
		require.Equal(t, uint32(0x101), cmd)
		require.Equal(t, "{}", string(data))
		_, _, err = c.ReadCommand()
		require.ErrorIs(t, err, io.EOF)
	})
}

// A corrupt size kept everything after it waiting, up to 4 GiB, and turned
// negative on 32 bit builds and took the process down there. The stream
// can't be split into data anymore and ends.
func TestPushRefusesHugeSize(t *testing.T) {
	for _, size := range []uint32{maxDataSize + 1, 0x80000000, 0xFFFFFFFF} {
		c := newDataChannel(0, 1)
		err := c.Push(binary.BigEndian.AppendUint32(nil, size))
		require.ErrorContains(t, err, fmt.Sprint(size))
	}

	c := newDataChannel(0, 1)
	require.NoError(t, c.Push(binary.BigEndian.AppendUint32(nil, maxDataSize)))

	conn := receive(false, drw(2, 0, media("one")), drw(2, 1, []byte{0xFF, 0xFF, 0xFF, 0xFF}), drw(2, 2, media("two")))
	payloads, err := readPayloads(t, conn)
	require.ErrorContains(t, err, "4294967295")
	require.Equal(t, []string{"one"}, payloads)
}

// Data comes split over datagrams at any byte, several in one datagram too.
// Each one leaves as soon as its last byte is in, a short one too.
func TestPushJoinsSplitData(t *testing.T) {
	stream := append(sized([]byte("abc")), sized([]byte("0123456789"))...)
	units := []struct {
		end  int
		data string
	}{{7, "abc"}, {len(stream), "0123456789"}}

	for i := range len(stream) + 1 {
		for j := i; j <= len(stream); j++ {
			c := newDataChannel(0, len(units))
			var from int
			var got []string
			for _, to := range []int{i, j, len(stream)} {
				require.NoError(t, c.Push(stream[from:to]))
				from = to

				for len(c.popBuf) > 0 {
					got = append(got, string(<-c.popBuf))
				}
				var want []string
				for _, u := range units {
					if u.end <= to {
						want = append(want, u.data)
					}
				}
				require.Equal(t, want, got, "split at %d and %d, %d bytes in", i, j, to)
			}
		}
	}
}

// The camera keeps sending notifications nobody reads: the command channel
// keeps the newest ones, a full queue doesn't end the stream (#2057). The
// media channel drops no frame silently, a full queue ends the stream.
func TestPushFullQueue(t *testing.T) {
	c := newConn(&camera{}, false)

	commands := c.channels[0]
	for i := range cap(commands.popBuf) + 2 {
		require.NoError(t, commands.Push(sized([]byte{byte(i)})))
	}
	var kept []byte
	for len(commands.popBuf) > 0 {
		kept = append(kept, (<-commands.popBuf)[0])
	}
	require.Len(t, kept, cap(commands.popBuf))
	require.Equal(t, byte(2), kept[0])
	require.Equal(t, byte(cap(commands.popBuf)+1), kept[len(kept)-1])

	video := c.channels[2]
	for i := range cap(video.popBuf) {
		require.NoError(t, video.Push(media(fmt.Sprint(i))))
	}
	require.ErrorContains(t, video.Push(media("one too many")), "pop buffer is full")
}

const testTimeout = 2 * time.Second

func newTestConn(t *testing.T, isTCP bool) (*Conn, net.Conn) {
	t.Helper()

	workerEnd, testEnd := net.Pipe()

	c := &Conn{
		Conn:  workerEnd,
		isTCP: isTCP,
		channels: [4]*dataChannel{
			newDataChannel(0, 10), nil, newDataChannel(250, 100), nil,
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.worker()
	}()

	t.Cleanup(func() {
		_ = testEnd.Close()
		_ = workerEnd.Close()

		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Error("worker didn't stop")
		}
	})

	return c, testEnd
}

func writeConn(t *testing.T, conn net.Conn, b []byte) {
	t.Helper()

	require.NoError(t, conn.SetDeadline(time.Now().Add(testTimeout)))
	_, err := conn.Write(b)
	require.NoError(t, err)
}

func readConn(t *testing.T, conn net.Conn, n int) []byte {
	t.Helper()

	require.NoError(t, conn.SetDeadline(time.Now().Add(testTimeout)))
	buf := make([]byte, n)
	_, err := io.ReadFull(conn, buf)
	require.NoError(t, err)
	return buf
}

func drwFrame(ch byte, seq uint16, payload []byte) []byte {
	buf := make([]byte, 8+len(payload))
	buf[0] = magic
	buf[1] = msgDrw
	buf[4] = magicDrw
	buf[5] = ch
	binary.BigEndian.PutUint16(buf[6:], seq)
	copy(buf[8:], payload)
	return buf
}

func TestWorkerUnknownChannelUDP(t *testing.T) {
	_, conn := newTestConn(t, false)

	// Channel 3 is nil in channels, channel 9 is out of its bounds.
	for i, ch := range []byte{3, 9} {
		writeConn(t, conn, drwFrame(ch, 1, []byte("junk")))

		if i == 0 {
			// First packet triggers the 1s keepalive PING.
			require.Equal(t, []byte{magic, msgPing, 0, 0}, readConn(t, conn, 4))
		}

		ack := readConn(t, conn, 10)
		require.Equal(t, byte(magic), ack[0])
		require.Equal(t, byte(msgDrwAck), ack[1])
		require.Equal(t, ch, ack[5])
	}

	// Worker must still be alive after the unknown channels.
	writeConn(t, conn, []byte{magic, msgPing, 0, 0})
	require.Equal(t, []byte{magic, msgPong, 0, 0}, readConn(t, conn, 4))
}

func TestWorkerUnknownChannelTCP(t *testing.T) {
	_, conn := newTestConn(t, true)

	writeConn(t, conn, drwFrame(3, 1, []byte("junk")))

	// TCP worker sends a keepalive PING before dropping the packet.
	require.Equal(t, []byte{magic, msgPing, 0, 0}, readConn(t, conn, 4))

	// No ACK must follow: the next message is the reply to our PING.
	writeConn(t, conn, []byte{magic, msgPing, 0, 0})
	require.Equal(t, []byte{magic, msgPong, 0, 0}, readConn(t, conn, 4))
}

func TestWorkerKnownChannelUDP(t *testing.T) {
	c, conn := newTestConn(t, false)

	payload := []byte("command-payload")
	body := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(body, uint32(len(payload)))
	copy(body[4:], payload)

	writeConn(t, conn, drwFrame(0, 0, body))

	require.Equal(t, []byte{magic, msgPing, 0, 0}, readConn(t, conn, 4))

	ack := readConn(t, conn, 10)
	require.Equal(t, byte(msgDrwAck), ack[1])
	require.Equal(t, byte(0), ack[5])

	got, ok := c.channels[0].Pop()
	require.True(t, ok)
	require.Equal(t, payload, got)
}
