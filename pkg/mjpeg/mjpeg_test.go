package mjpeg

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestRFC2435(t *testing.T) {
	lqt, cqt := MakeTables(71)
	require.Equal(t, byte(9), lqt[0])
	require.Equal(t, byte(10), cqt[0])
}

func TestRTPDepayQuantTables(t *testing.T) {
	src := testJPEG(t)
	sent, err := Transcode(src)
	require.NoError(t, err)

	t.Run("two tables", func(t *testing.T) {
		frames := depay(payPackets(t, src))
		require.Len(t, frames, 1)
		decodes(t, frames[0])
		require.Equal(t, scanData(t, sent), scanData(t, frames[0]))
	})

	t.Run("one table for luma and chroma", func(t *testing.T) {
		packets := payPackets(t, src)
		packets[0].Payload = withQuantLength(packets[0].Payload, 64)

		frames := depay(packets)
		require.Len(t, frames, 1)
		decodes(t, frames[0])
		require.Equal(t, scanData(t, sent), scanData(t, frames[0]))

		tables := quantTables(t, frames[0])
		require.Len(t, tables, 2)
		require.Equal(t, tables[0], tables[1])
	})

	t.Run("length 0 keeps the previous tables", func(t *testing.T) {
		first := payPackets(t, src)
		second := payPackets(t, src)
		second[0].Payload = withQuantLength(second[0].Payload, 0)

		frames := depay(append(first, second...))
		require.Len(t, frames, 2)
		decodes(t, frames[1])
		require.Equal(t, frames[0], frames[1])
	})

	t.Run("truncated table header is dropped", func(t *testing.T) {
		packets := payPackets(t, src)
		packets[0].Payload = packets[0].Payload[:8+4+10]
		packets[0].Marker = true

		require.NotPanics(t, func() { require.Empty(t, depay(packets[:1])) })
	})
}

func testJPEG(t *testing.T) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := range 48 {
		for x := range 64 {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 5), B: 128, A: 255})
		}
	}
	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, img, &jpeg.Options{Quality: 80}))
	return b.Bytes()
}

func payPackets(t *testing.T, jpg []byte) []*rtp.Packet {
	var packets []*rtp.Packet
	pay := RTPPay(func(p *rtp.Packet) {
		clone := *p
		clone.Payload = append([]byte(nil), p.Payload...)
		packets = append(packets, &clone)
	})
	pay(&rtp.Packet{Payload: jpg})
	require.NotEmpty(t, packets)
	return packets
}

func depay(packets []*rtp.Packet) [][]byte {
	var frames [][]byte
	handle := RTPDepay(func(p *rtp.Packet) {
		frames = append(frames, append([]byte(nil), p.Payload...))
	})
	for _, p := range packets {
		handle(p)
	}
	return frames
}

// rewrites the quantization table header of a frame's first packet, keeping the first n table bytes
func withQuantLength(payload []byte, n int) []byte {
	const main = 8
	size := 4 + int(binary.BigEndian.Uint16(payload[main+2:]))
	out := append([]byte(nil), payload[:main+4]...)
	binary.BigEndian.PutUint16(out[main+2:], uint16(n))
	out = append(out, payload[main+4:main+4+n]...)
	return append(out, payload[main+size:]...)
}

func decodes(t *testing.T, jpg []byte) {
	img, err := jpeg.Decode(bytes.NewReader(jpg))
	require.NoError(t, err)
	require.Equal(t, image.Pt(64, 48), img.Bounds().Size())
}

func scanData(t *testing.T, jpg []byte) []byte {
	for i := 2; i+4 <= len(jpg) && jpg[i] == 0xFF; {
		size := int(binary.BigEndian.Uint16(jpg[i+2:]))
		if jpg[i+1] == markerSOS {
			end := len(jpg)
			if jpg[end-2] == 0xFF && jpg[end-1] == 0xD9 {
				end -= 2
			}
			return jpg[i+2+size : end]
		}
		i += 2 + size
	}
	t.Fatal("no SOS marker")
	return nil
}

func quantTables(t *testing.T, jpg []byte) [][]byte {
	var tables [][]byte
	for i := 2; i+4 <= len(jpg) && jpg[i] == 0xFF; {
		size := int(binary.BigEndian.Uint16(jpg[i+2:]))
		switch jpg[i+1] {
		case markerDQT:
			for j := i + 4; j+65 <= i+2+size; j += 65 {
				tables = append(tables, jpg[j+1:j+65])
			}
		case markerSOS:
			return tables
		}
		i += 2 + size
	}
	t.Fatal("no SOS marker")
	return nil
}
