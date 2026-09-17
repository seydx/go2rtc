package h265

import (
	"encoding/binary"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
)

func RTPDepay(codec *core.Codec, handler core.HandlerFunc) core.HandlerFunc {
	vps, sps, pps := GetParameterSet(codec.Fmtp())
	ps := h264.JoinNALU(vps, sps, pps)

	buf := make([]byte, 0, 512*1024) // 512K
	var nuStart int
	var seqNum uint16
	var timestamp uint32
	var fragmented bool

	// drop discards the access unit being assembled, it is incomplete. The stream
	// goes on with the next access unit without waiting for a keyframe: go2rtc
	// feeds NVRs and recorders, decoders conceal missing references, but a gap
	// of a whole GOP in a recording can't be repaired.
	drop := func() {
		buf = buf[:0]
		fragmented = false
	}

	fmtpLineUpdated := false

	return func(packet *rtp.Packet) {
		if packet.Version == h264.RTPPacketVersionAVC {
			handler(packet)
			return
		}

		// A sequence gap, or a new timestamp inside a fragmented NAL unit, means
		// the access unit being assembled lost data. Between access units nothing
		// is dropped, so seq wraps and cameras restarting RTP numbering go on.
		// Tracked before any early return, so skipped packets keep continuity.
		if len(buf) > 0 && (packet.SequenceNumber-seqNum != 1 || (fragmented && packet.Timestamp != timestamp)) {
			drop()
		}
		seqNum, timestamp = packet.SequenceNumber, packet.Timestamp

		// Memory overflow protection, same as the h264 depay: a camera whose
		// marker never reaches the flush (ex. every access unit ends with a
		// marked SEI) would grow the buffer without limit. Resetting fragmented
		// matters, nuStart would otherwise point past the shrunken buffer.
		if len(buf) > 5*1024*1024 {
			buf = buf[: 0 : 512*1024]
			fragmented = false
		}

		data := packet.Payload
		if len(data) < 3 {
			return // too short to carry H265 data, ex. padding only
		}

		nuType := (data[0] >> 1) & 0x3F
		//log.Printf("[RTP] codec: %s, nalu: %2d, size: %6d, ts: %10d, pt: %2d, ssrc: %d, seq: %d, %v", track.Codec.Name, nuType, len(packet.Payload), packet.Timestamp, packet.PayloadType, packet.SSRC, packet.SequenceNumber, packet.Marker)

		// Fix for RtspServer https://github.com/AlexxIT/go2rtc/issues/244
		if packet.Marker && len(data) < h264.PSMaxSize {
			switch nuType {
			case NALUTypeVPS, NALUTypeSPS, NALUTypePPS:
				packet.Marker = false
			case NALUTypePrefixSEI, NALUTypeSuffixSEI:
				return
			}
		}

		if nuType == NALUTypeFU {
			switch data[2] >> 6 {
			case 0b10: // begin
				if fragmented {
					drop() // the previous fragmented unit never ended
				}
				fragmented = true
				nuType = data[2] & 0x3F

				// push PS data before keyframe
				if len(buf) == 0 && nuType >= 19 && nuType <= 21 {
					buf = append(buf, ps...)
				}

				nuStart = len(buf)
				buf = append(buf, 0, 0, 0, 0) // NAL unit size
				buf = append(buf, (data[0]&0x81)|(nuType<<1), data[1])
				buf = append(buf, data[3:]...)
				return
			case 0b00: // continue
				if !fragmented {
					drop() // the start of this unit was lost
					return
				}

				buf = append(buf, data[3:]...)
				return
			case 0b01: // end
				if !fragmented {
					drop() // the start of this unit was lost
					return
				}
				fragmented = false

				buf = append(buf, data[3:]...)

				binary.BigEndian.PutUint32(buf[nuStart:], uint32(len(buf)-nuStart-4))
			case 0b11: // wrong RFC 7798 realisation from OpenIPC project
				if fragmented {
					drop()
				}
				// A non-fragmented NAL unit MUST NOT be transmitted in one FU; i.e.,
				// the Start bit and End bit must not both be set to 1 in the same FU
				// header.
				nuType = data[2] & 0x3F
				buf = binary.BigEndian.AppendUint32(buf, uint32(len(data))-1) // NAL unit size
				buf = append(buf, (data[0]&0x81)|(nuType<<1), data[1])
				buf = append(buf, data[3:]...)
			}
		} else if nuType == NALUTypeAP {
			// RFC 7798 §4.4.2 Aggregation Packet: [PayloadHdr][2-byte size + NALU]*
			// (no DONL — sprop-max-don-diff=0). Emitted by libavformat's HEVC RTP
			// packetizer (e.g. exec/ffmpeg sources), which bundles VPS+SPS+PPS into
			// one packet. Split it into individual AVCC length-prefixed NAL units.
			// The AP already carries parameter sets in-band — don't prepend ps.
			if fragmented {
				drop() // an AP can't continue a fragmented unit, its end was lost
			}
			for i := 2; i < len(data); {
				if i+2 > len(data) {
					drop() // drop truncated AP (same convention as FU)
					return
				}
				size := int(binary.BigEndian.Uint16(data[i:]))
				i += 2
				if size < 2 || i+size > len(data) {
					drop() // drop corrupted AP
					return
				}
				buf = binary.BigEndian.AppendUint32(buf, uint32(size)) // NAL unit size
				buf = append(buf, data[i:i+size]...)
				i += size
			}
		} else {
			if fragmented {
				drop() // a single NAL unit can't continue a fragmented unit
			}
			buf = binary.BigEndian.AppendUint32(buf, uint32(len(data))) // NAL unit size
			buf = append(buf, data...)
		}

		// collect all NAL Units for Access Unit
		if !packet.Marker {
			return
		}

		//log.Printf("[HEVC] %v, len: %d", Types(buf), len(buf))

		// Update FmtpLine from first keyframe with parameter sets
		// This fixes MSE aspect ratio issues when RTSP cameras don't send VPS/SPS/PPS in DESCRIBE
		if !fmtpLineUpdated && ContainsParameterSets(buf) {
			newFmtpLine := GetFmtpLine(buf)
			if newFmtpLine != "" {
				codec.SetFmtp(newFmtpLine)
				// Re-extract VPS/SPS/PPS with updated FmtpLine
				vps, sps, pps = GetParameterSet(newFmtpLine)
				ps = h264.JoinNALU(vps, sps, pps)
			}
			fmtpLineUpdated = true
		}

		clone := *packet
		clone.Version = h264.RTPPacketVersionAVC
		clone.Payload = buf

		buf = buf[:0]

		handler(&clone)
	}
}

func RTPPay(mtu uint16, handler core.HandlerFunc) core.HandlerFunc {
	if mtu == 0 {
		mtu = 1472
	}

	payloader := &Payloader{}
	sequencer := rtp.NewRandomSequencer()
	mtu -= 12 // rtp.Header size

	return func(packet *rtp.Packet) {
		if packet.Version != h264.RTPPacketVersionAVC {
			clone := *packet
			clone.Header.SequenceNumber = sequencer.NextSequenceNumber()
			handler(&clone)
			return
		}

		payloads := payloader.Payload(mtu, packet.Payload)
		last := len(payloads) - 1
		for i, payload := range payloads {
			clone := rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					Marker:         i == last,
					SequenceNumber: sequencer.NextSequenceNumber(),
					Timestamp:      packet.Timestamp,
				},
				Payload: payload,
			}
			handler(&clone)
		}
	}
}

// SafariPay - generate Safari friendly payload for H265
// https://github.com/AlexxIT/Blog/issues/5
func SafariPay(mtu uint16, handler core.HandlerFunc) core.HandlerFunc {
	sequencer := rtp.NewRandomSequencer()
	size := int(mtu - 12) // rtp.Header size

	return func(packet *rtp.Packet) {
		if packet.Version != h264.RTPPacketVersionAVC {
			handler(packet)
			return
		}

		// protect original packets from modification
		au := make([]byte, len(packet.Payload))
		copy(au, packet.Payload)

		var start byte

		for i := 0; i < len(au); {
			size := int(binary.BigEndian.Uint32(au[i:])) + 4

			// convert AVC to Annex-B
			au[i] = 0
			au[i+1] = 0
			au[i+2] = 0
			au[i+3] = 1

			switch NALUType(au[i:]) {
			case NALUTypeIFrame, NALUTypeIFrame2, NALUTypeIFrame3:
				start = 3
			default:
				if start == 0 {
					start = 2
				}
			}

			i += size
		}

		// rtp.Packet payload
		b := make([]byte, 1, size)
		size-- // minus header byte

		for au != nil {
			b[0] = start

			if start > 1 {
				start -= 2
			}

			if len(au) > size {
				b = append(b, au[:size]...)
				au = au[size:]
			} else {
				b = append(b, au...)
				au = nil
			}

			clone := rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					Marker:         au == nil,
					SequenceNumber: sequencer.NextSequenceNumber(),
					Timestamp:      packet.Timestamp,
				},
				Payload: b,
			}
			handler(&clone)

			b = b[:1] // clear buffer
		}
	}
}
