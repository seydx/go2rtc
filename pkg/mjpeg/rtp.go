package mjpeg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

type rtpFrame struct {
	typ           byte
	width, height uint16
	lqt, cqt      []byte
	scan          []byte
}

// RFC 2435 sends the size in 8 pixel units, at most 255 of them
func (f *rtpFrame) fitsRTP() bool {
	return f.width > 0 && f.height > 0 && f.width%8 == 0 && f.height%8 == 0 && f.width <= 2040 && f.height <= 2040
}

// RTPDepay joins RFC 2435 packets into JPEG pictures. A picture that lost a
// packet is dropped. The payload passed to handlerFunc is reused for the next
// picture.
func RTPDepay(handlerFunc core.HandlerFunc) core.HandlerFunc {
	buf := make([]byte, 0, 512*1024) // 512K

	// dynamic tables outlive their frame, a sender may send them only once (Length 0 afterwards)
	var dynLqt, dynCqt []byte

	var seq uint16
	var ts uint32

	return func(packet *rtp.Packet) {
		//log.Printf("[RTP] codec: %s, size: %6d, ts: %10d, pt: %2d, ssrc: %d, seq: %d, mark: %v", track.Codec.Name, len(packet.Payload), packet.Timestamp, packet.PayloadType, packet.SSRC, packet.SequenceNumber, packet.Marker)

		// https://www.rfc-editor.org/rfc/rfc2435#section-3.1
		b := packet.Payload
		if len(b) < 8 {
			return
		}

		// 3.1.  JPEG header
		t := b[4]
		offset := uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])

		// 3.1.7.  Restart Marker header
		var dri uint16
		if 64 <= t && t <= 127 {
			if len(b) < 12 {
				return
			}
			// the RST markers are in the scan, the picture needs the interval
			dri = binary.BigEndian.Uint16(b[8:])
			b = b[12:]
		} else {
			b = b[8:]
		}

		// a lost packet, or a lost marker that would glue two pictures together
		if len(buf) > 0 && (packet.SequenceNumber != seq+1 || packet.Timestamp != ts) {
			buf = buf[:0]
		}
		seq, ts = packet.SequenceNumber, packet.Timestamp

		if len(buf) == 0 {
			// the start of this picture is gone
			if offset != 0 {
				return
			}

			var lqt, cqt []byte

			// 3.1.8.  Quantization Table header
			q := packet.Payload[5]
			if q >= 128 {
				if len(b) < 4 {
					return
				}
				size := 4 + int(binary.BigEndian.Uint16(b[2:]))
				if len(b) < size {
					return
				}
				switch {
				case size >= 4+128:
					dynLqt = append(dynLqt[:0], b[4:68]...)
					dynCqt = append(dynCqt[:0], b[68:132]...)
				case size >= 4+64:
					// one table for luma and chroma, as ffmpeg sends it
					dynLqt = append(dynLqt[:0], b[4:68]...)
					dynCqt = append(dynCqt[:0], b[4:68]...)
				}
				if dynLqt == nil {
					return
				}
				lqt, cqt = dynLqt, dynCqt
				b = b[size:]
			} else {
				lqt, cqt = MakeTables(q)
			}

			// https://www.rfc-editor.org/rfc/rfc2435#section-3.1.5
			// The maximum width is 2040 pixels.
			w := uint16(packet.Payload[6]) << 3
			h := uint16(packet.Payload[7]) << 3

			// fix sizes more than 2040
			switch {
			// 512x1920 512x1440
			case w == cutSize(2560) && (h == 1920 || h == 1440):
				w = 2560
			// 1792x112
			case w == cutSize(3840) && h == cutSize(2160):
				w = 3840
				h = 2160
			// 256x1296
			case w == cutSize(2304) && h == 1296:
				w = 2304
			}

			//fmt.Printf("t: %d, q: %d, w: %d, h: %d\n", t, q, w, h)
			buf = MakeHeaders(buf, t, w, h, lqt, cqt, dri)
		}

		// 3.1.9.  JPEG Payload
		buf = append(buf, b...)

		if !packet.Marker {
			return
		}

		if len(buf) < 2 || buf[len(buf)-2] != 0xFF || buf[len(buf)-1] != 0xD9 {
			buf = append(buf, 0xFF, 0xD9)
		}

		clone := *packet
		clone.Payload = buf

		buf = buf[:0] // clear buffer

		handlerFunc(&clone)
	}
}

func cutSize(size uint16) uint16 {
	return ((size >> 3) & 0xFF) << 3
}

func RTPPay(handlerFunc core.HandlerFunc) core.HandlerFunc {
	const packetSize = 1436

	sequencer := rtp.NewRandomSequencer()

	return func(packet *rtp.Packet) {
		f := parseRTPFrame(packet.Payload)
		if f == nil || !f.fitsRTP() {
			// reincode image to more common form
			b, err := Transcode(packet.Payload)
			if err != nil {
				return
			}
			if f = parseRTPFrame(b); f == nil {
				return
			}
		}

		h1 := make([]byte, 8)
		h1[4] = f.typ // Type
		h1[5] = 255   // Q
		h1[6] = uint8(f.width >> 3)
		h1[7] = uint8(f.height >> 3)

		// MBZ=0, Precision=0, Length=128
		h2 := make([]byte, 4, 132)
		h2[3] = 128
		h2 = append(h2, f.lqt...)
		h2 = append(h2, f.cqt...)

		jpgData := f.scan

		offset := 0
		p := make([]byte, 0)

		for jpgData != nil {
			p = p[:0]

			if offset > 0 {
				h1[1] = byte(offset >> 16)
				h1[2] = byte(offset >> 8)
				h1[3] = byte(offset)
				p = append(p, h1...)
			} else {
				p = append(p, h1...)
				p = append(p, h2...)
			}

			dataLen := packetSize - len(p)
			if dataLen < len(jpgData) {
				p = append(p, jpgData[:dataLen]...)
				jpgData = jpgData[dataLen:]
				offset += dataLen
			} else {
				p = append(p, jpgData...)
				jpgData = nil
			}

			clone := rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					Marker:         jpgData == nil,
					SequenceNumber: sequencer.NextSequenceNumber(),
					Timestamp:      packet.Timestamp,
				},
				Payload: p,
			}
			handlerFunc(&clone)
		}
	}
}

func Transcode(b []byte) ([]byte, error) {
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}

	// the header has the size in 8 pixel units, at most 255 of them: a
	// receiver takes anything else for another size and shifts the lines
	wh := img.Bounds().Size()
	w := min(wh.X, 2040) &^ 7
	h := min(wh.Y, 2040) &^ 7
	if w == 0 || h == 0 {
		return nil, errors.New("mjpeg: picture too small")
	}

	if w != wh.X || h != wh.Y {
		x0 := (wh.X - w) / 2
		y0 := (wh.Y - h) / 2
		rect := image.Rect(x0, y0, x0+w, y0+h)
		img = img.(interface {
			SubImage(image.Rectangle) image.Image
		}).SubImage(rect)
	}

	// the header has no grayscale, the encoder writes it as one channel
	if _, ok := img.(*image.Gray); ok {
		img = struct{ image.Image }{img}
	}

	buf := bytes.NewBuffer(nil)
	if err = jpeg.Encode(buf, img, nil); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// parseRTPFrame returns nil for anything a receiver can't rebuild from the RTP/JPEG header:
// no baseline 8-bit YUV 4:2:x, other than the standard Huffman tables, or restart markers
func parseRTPFrame(b []byte) *rtpFrame {
	if len(b) < 4 || b[0] != 0xFF || b[1] != markerSOI {
		return nil
	}

	var f rtpFrame
	var tables [4][]byte
	var ids [3]byte
	var lq, cq byte
	sof := false

	for i := 2; ; {
		if i+4 > len(b) || b[i] != 0xFF {
			return nil
		}
		marker := b[i+1]
		if marker == 0xFF {
			i++ // fill byte
			continue
		}
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		if size < 2 || i+2+size > len(b) {
			return nil
		}
		seg := b[i+4 : i+2+size]

		switch marker {
		case markerDQT:
			for len(seg) >= 65 {
				// high nibble is the precision, 16-bit tables don't fit the header
				if seg[0] > 3 {
					return nil
				}
				tables[seg[0]] = seg[1:65]
				seg = seg[65:]
			}
			if len(seg) != 0 {
				return nil
			}
		case markerSOF:
			if len(seg) != 15 || seg[0] != 8 || seg[5] != 3 {
				return nil
			}
			switch seg[7] {
			case 0x21:
				f.typ = 0
			case 0x22:
				f.typ = 1
			default:
				return nil
			}
			// the header has one table for both chroma components
			if seg[10] != 0x11 || seg[13] != 0x11 || seg[11] != seg[14] {
				return nil
			}
			f.height = binary.BigEndian.Uint16(seg[1:])
			f.width = binary.BigEndian.Uint16(seg[3:])
			ids = [3]byte{seg[6], seg[9], seg[12]}
			lq, cq = seg[8], seg[11]
			sof = true
		case markerDHT:
			for len(seg) >= 17 {
				n := 17
				for _, c := range seg[1:17] {
					n += int(c)
				}
				if len(seg) < n || !standardHuffman(seg[0], seg[1:17], seg[17:n]) {
					return nil
				}
				seg = seg[n:]
			}
			if len(seg) != 0 {
				return nil
			}
		case markerDRI:
			if len(seg) != 2 || binary.BigEndian.Uint16(seg) != 0 {
				return nil
			}
		case markerSOS:
			if !sof || len(seg) != 10 || seg[0] != 3 || lq > 3 || cq > 3 || tables[lq] == nil || tables[cq] == nil {
				return nil
			}
			if seg[1] != ids[0] || seg[2] != 0x00 || seg[3] != ids[1] || seg[4] != 0x11 || seg[5] != ids[2] || seg[6] != 0x11 {
				return nil
			}
			if seg[7] != 0 || seg[8] != 63 || seg[9] != 0 {
				return nil
			}
			f.lqt, f.cqt = tables[lq], tables[cq]
			f.scan = b[i+2+size:]
			if j := bytes.LastIndex(f.scan, []byte{0xFF, markerEOI}); j >= 0 {
				f.scan = f.scan[:j+2]
			}
			return &f
		default:
			// other frame types and arithmetic coding
			if marker > markerSOF && marker <= 0xCF {
				return nil
			}
		}

		i += 2 + size
	}
}

func standardHuffman(class byte, counts, symbols []byte) bool {
	var c, s []byte
	switch class {
	case 0x00:
		c, s = lum_dc_codelens, lum_dc_symbols
	case 0x10:
		c, s = lum_ac_codelens, lum_ac_symbols
	case 0x01:
		c, s = chm_dc_codelens, chm_dc_symbols
	case 0x11:
		c, s = chm_ac_codelens, chm_ac_symbols
	default:
		return false
	}
	return bytes.Equal(counts, c) && bytes.Equal(symbols, s)
}
