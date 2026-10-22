package mjpeg

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestRFC2435(t *testing.T) {
	lqt, cqt := MakeTables(71)
	require.Equal(t, byte(9), lqt[0])
	require.Equal(t, byte(10), cqt[0])
}

func TestRTPPay(t *testing.T) {
	t.Run("baseline 4:2:0 is sent as is", func(t *testing.T) {
		src := testJPEG(t, 64, 48)
		packets := payPackets(t, src)
		require.Equal(t, byte(1), packets[0].Payload[4])

		frames := depay(packets)
		require.Len(t, frames, 1)
		decodes(t, frames[0], 64, 48)
		require.Equal(t, scanData(t, src), scanData(t, frames[0]))
		require.Equal(t, quantTables(t, src), quantTables(t, frames[0]))
	})

	t.Run("4:2:2 is sent as is with type 0", func(t *testing.T) {
		src := readTestdata(t, "yuv422.jpg")
		packets := payPackets(t, src)
		require.Equal(t, byte(0), packets[0].Payload[4])

		frames := depay(packets)
		require.Len(t, frames, 1)
		decodes(t, frames[0], 64, 48)
		require.Equal(t, scanData(t, src), scanData(t, frames[0]))
	})

	// own Huffman tables, 4:2:2 as luma 2x2 with chroma 1x2
	for _, name := range []string{"yuv422_optimal.jpg", "yuv422_ffmpeg.jpg"} {
		t.Run(name+" is reencoded", func(t *testing.T) {
			packets := payPackets(t, readTestdata(t, name))
			require.Equal(t, byte(1), packets[0].Payload[4])

			frames := depay(packets)
			require.Len(t, frames, 1)
			decodes(t, frames[0], 64, 48)
		})
	}

	t.Run("more than 2040 pixels are cropped", func(t *testing.T) {
		frames := depay(payPackets(t, testJPEG(t, 2048, 16)))
		require.Len(t, frames, 1)
		decodes(t, frames[0], 2040, 16)
	})

	// the header has the size in 8 pixel units: a receiver took 1366 as 1360
	// and shifted every line of the picture
	t.Run("a size out of 8 pixel units is cropped", func(t *testing.T) {
		src := testJPEG(t, 1366, 770)
		frames := depay(payPackets(t, src))
		require.Len(t, frames, 1)
		decodes(t, frames[0], 1360, 768)
		showsTheMiddle(t, src, frames[0])
	})

	// the header has no grayscale: such a picture was lost, one to crop took
	// the whole process down
	for _, size := range []image.Point{{64, 48}, {70, 48}, {2048, 16}} {
		t.Run("grayscale "+size.String()+" is sent in color", func(t *testing.T) {
			src := testGrayJPEG(t, size.X, size.Y)
			frames := depay(payPackets(t, src))
			require.Len(t, frames, 1)
			decodes(t, frames[0], min(size.X, 2040)&^7, size.Y)
			showsTheMiddle(t, src, frames[0])
		})
	}

	t.Run("under 8 pixels nothing is sent", func(t *testing.T) {
		RTPPay(func(*rtp.Packet) {
			t.Fatal("a picture of 0 pixels")
		})(&rtp.Packet{Payload: testJPEG(t, 4, 48)})
	})
}

func TestRTPDepayQuantTables(t *testing.T) {
	src := testJPEG(t, 64, 48)

	t.Run("two tables", func(t *testing.T) {
		frames := depay(payPackets(t, src))
		require.Len(t, frames, 1)
		decodes(t, frames[0], 64, 48)
		require.Equal(t, scanData(t, src), scanData(t, frames[0]))
	})

	t.Run("one table for luma and chroma", func(t *testing.T) {
		packets := payPackets(t, src)
		packets[0].Payload = withQuantLength(packets[0].Payload, 64)

		frames := depay(packets)
		require.Len(t, frames, 1)
		decodes(t, frames[0], 64, 48)
		require.Equal(t, scanData(t, src), scanData(t, frames[0]))

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
		decodes(t, frames[1], 64, 48)
		require.Equal(t, frames[0], frames[1])
	})

	t.Run("truncated table header is dropped", func(t *testing.T) {
		packets := payPackets(t, src)
		packets[0].Payload = packets[0].Payload[:8+4+10]
		packets[0].Marker = true

		require.NotPanics(t, func() { require.Empty(t, depay(packets[:1])) })
	})
}

func TestRTPDepayLoss(t *testing.T) {
	src := testJPEG(t, 640, 480)
	stream := func() []*rtp.Packet {
		first, second := payPackets(t, src), payPackets(t, src)
		require.Greater(t, len(first), 2)
		packets := append(first, second...)
		for i, p := range packets {
			p.SequenceNumber = uint16(1000 + i)
			if i >= len(first) {
				p.Timestamp = 3000
			}
		}
		return packets
	}
	without := func(packets []*rtp.Packet, i int) []*rtp.Packet {
		return append(packets[:i:i], packets[i+1:]...)
	}
	n := len(payPackets(t, src))

	for name, lost := range map[string]int{"first": 0, "middle": 1, "marker": n - 1} {
		t.Run("lost "+name+" packet drops only that picture", func(t *testing.T) {
			frames := depay(without(stream(), lost))
			require.Len(t, frames, 1)
			decodes(t, frames[0], 640, 480)
			require.Equal(t, scanData(t, src), scanData(t, frames[0]))
		})
	}

	t.Run("short packets are dropped", func(t *testing.T) {
		packets := []*rtp.Packet{
			{Payload: nil},
			{Payload: []byte{0, 0, 0, 0, 1}},
			{Payload: []byte{0, 0, 0, 0, 64, 255, 80, 60, 0}, Header: rtp.Header{Marker: true}},
		}
		require.NotPanics(t, func() { require.Empty(t, depay(packets)) })
	})

	t.Run("end marker is added when the scan ends in another D9", func(t *testing.T) {
		packets := payPackets(t, src)
		last := packets[len(packets)-1]
		last.Payload = append(last.Payload, 0xD9)

		frames := depay(packets)
		require.Len(t, frames, 1)
		require.Equal(t, []byte{0xD9, 0xFF, 0xD9}, frames[0][len(frames[0])-3:])
	})
}

// A camera with a restart interval sends type 64 (4:2:2) or 65 (4:2:0) and a
// Restart Marker header. The picture needs the interval back as DRI.
func TestRTPDepayRestartMarkers(t *testing.T) {
	for name, typ := range map[string]byte{"yuv422_restart.jpg": 64, "yuv420_restart.jpg": 65} {
		t.Run(name, func(t *testing.T) {
			src := readTestdata(t, name)
			packets := restartPackets(t, src)
			require.Greater(t, len(packets), 1)
			require.Equal(t, typ, packets[0].Payload[4])

			frames := depay(packets)
			require.Len(t, frames, 1)
			require.Equal(t, decode(t, src), decode(t, frames[0]))
		})
	}
}

func testJPEG(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 5), B: 128, A: 255})
		}
	}
	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, img, &jpeg.Options{Quality: 80}))
	return b.Bytes()
}

func testGrayJPEG(t *testing.T, w, h int) []byte {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetGray(x, y, color.Gray{Y: uint8(x*4 + y*5)})
		}
	}
	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, img, &jpeg.Options{Quality: 80}))
	return b.Bytes()
}

// showsTheMiddle compares the brightness of a cropped picture with the
// middle of its source: a shifted line is far off
func showsTheMiddle(t *testing.T, src, cropped []byte) {
	t.Helper()
	a, b := decode(t, src), decode(t, cropped)
	dx := (a.Bounds().Dx() - b.Bounds().Dx()) / 2
	dy := (a.Bounds().Dy() - b.Bounds().Dy()) / 2
	luma := func(c color.Color) int {
		return int(color.GrayModel.Convert(c).(color.Gray).Y)
	}
	var sum, n int
	for y := range b.Bounds().Dy() {
		for x := range b.Bounds().Dx() {
			sum += abs(luma(b.At(x, y)) - luma(a.At(x+dx, y+dy)))
			n++
		}
	}
	require.Less(t, sum/n, 4, "mean brightness difference")
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

func readTestdata(t *testing.T, name string) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
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

// restartPackets packs a picture with a restart interval as a camera does
func restartPackets(t *testing.T, jpg []byte) []*rtp.Packet {
	i := bytes.Index(jpg, []byte{0xFF, markerDRI, 0, 4})
	require.Greater(t, i, 0)
	dri := jpg[i+4 : i+6]

	// RTPPay refuses the DRI segment, the RST markers stay in the scan
	packets := payPackets(t, append(jpg[:i:i], jpg[i+6:]...))
	for _, p := range packets {
		// F=1, L=1, Restart Count=0x3FFF: packets don't follow the intervals
		header := []byte{dri[0], dri[1], 0xFF, 0xFF}
		p.Payload = append(p.Payload[:8:8], append(header, p.Payload[8:]...)...)
		p.Payload[4] |= 64
	}
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

func decodes(t *testing.T, jpg []byte, w, h int) {
	img, err := jpeg.Decode(bytes.NewReader(jpg))
	require.NoError(t, err)
	require.Equal(t, image.Pt(w, h), img.Bounds().Size())
}

func decode(t *testing.T, jpg []byte) image.Image {
	img, err := jpeg.Decode(bytes.NewReader(jpg))
	require.NoError(t, err)
	return img
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
