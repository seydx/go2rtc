package h264

import (
	"encoding/binary"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/pion/rtp"
)

const RTPPacketVersionAVC = core.RTPPacketVersionAVC

const PSMaxSize = 128 // the biggest SPS I've seen is 48 (EZVIZ CS-CV210)

// IsKeyframeRTP reports whether an RTP payload starts an IDR picture: the IDR
// slice itself, a STAP-A carrying it or its first FU-A fragment.
func IsKeyframeRTP(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}

	switch payload[0] & 0x1F {
	case NALUTypeIFrame:
		return true
	case NALUTypeSTAPA:
		for b := payload[1:]; len(b) > 2; {
			size := int(binary.BigEndian.Uint16(b))
			b = b[2:]
			if size == 0 || size > len(b) {
				return false
			}
			if b[0]&0x1F == NALUTypeIFrame {
				return true
			}
			b = b[size:]
		}
	case NALUTypeFUA:
		return len(payload) > 1 && payload[1]&0x80 != 0 && payload[1]&0x1F == NALUTypeIFrame
	}
	return false
}

func RTPDepay(codec *core.Codec, handler core.HandlerFunc) core.HandlerFunc {
	sps, pps := GetParameterSet(codec.Fmtp())
	ps := JoinNALU(sps, pps)

	// the access unit, a fragmented NAL unit is assembled at its end
	buf := make([]byte, 0, 512*1024) // 512K
	var nalStart int
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
		if packet.Version == RTPPacketVersionAVC {
			handler(packet)
			return
		}

		// A sequence gap, or a new timestamp inside a fragmented NAL unit, means
		// the access unit being assembled lost data. Between access units nothing
		// is dropped, so seq wraps and cameras restarting RTP numbering go on.
		// Tracked before any early return, so skipped packets keep continuity.
		if (len(buf) > 0 || fragmented) && (packet.SequenceNumber-seqNum != 1 || (fragmented && packet.Timestamp != timestamp)) {
			drop()
		}
		seqNum, timestamp = packet.SequenceNumber, packet.Timestamp

		// FU-A fragments are joined only from a start bit on: a tail whose start
		// was lost would become a bogus NAL unit, and a NAL unit whose end was
		// lost would get glued in front of the next one.
		if b := packet.Payload; len(b) >= 2 && b[0]&0x1F == NALUTypeFUA {
			if b[1]&0x80 != 0 { // start
				if fragmented {
					drop() // the previous fragmented NAL unit never ended
				}
				fragmented = true
			} else if !fragmented {
				drop() // the start of this NAL unit was lost
				return
			}
			if b[1]&0x40 != 0 { // end
				fragmented = false
			}
		} else if fragmented {
			drop() // a single NAL unit or STAP-A can't continue a fragmented NAL unit
		}

		//log.Printf("[RTP] codec: %s, nalu: %2d, size: %6d, ts: %10d, pt: %2d, ssrc: %d, seq: %d, %v", codec.Name, packet.Payload[0]&0x1F, len(packet.Payload), packet.Timestamp, packet.PayloadType, packet.SSRC, packet.SequenceNumber, packet.Marker)

		// the NAL units of this packet go to buf[start:] as AVCC
		start := len(buf)

		switch b := packet.Payload; {
		case len(b) == 0:
			return
		case b[0]&0x1F == NALUTypeFUA:
			if len(b) < 2 {
				return
			}
			if b[1]&0x80 != 0 {
				nalStart = start
				buf = append(buf, 0, 0, 0, 0, 0) // size and NAL header, set from the end fragment
			}
			buf = append(buf, b[2:]...)
			if b[1]&0x40 == 0 {
				return
			}
			binary.BigEndian.PutUint32(buf[nalStart:], uint32(len(buf)-nalStart-4))
			buf[nalStart+4] = b[0]&0x60 | b[1]&0x1F
			start = nalStart
		case b[0]&0x1F == NALUTypeSTAPA:
			for i := 1; len(b)-i >= 2; {
				size := int(binary.BigEndian.Uint16(b[i:]))
				i += 2
				if size > len(b)-i {
					buf = buf[:start]
					return // corrupted STAP-A
				}
				if size > 0 {
					buf = binary.BigEndian.AppendUint32(buf, uint32(size))
					buf = append(buf, b[i:i+size]...)
				}
				i += size
			}
			if len(buf) == start {
				return
			}
		case b[0]&0x1F < NALUTypeSTAPA && b[0]&0x1F > 0:
			buf = binary.BigEndian.AppendUint32(buf, uint32(len(b)))
			buf = append(buf, b...)
		default:
			return // reserved types, STAP-B, MTAP and FU-B
		}

		// Memory overflow protection. Can happen if we miss a lot of packets with the marker.
		// https://github.com/AlexxIT/go2rtc/issues/675
		if start > 5*1024*1024 {
			buf = append(make([]byte, 0, 512*1024), buf[start:]...)
			start = 0
		}

		// Fix TP-Link Tapo TC70: sends SPS and PPS with packet.Marker = true
		// Reolink Duo 2: sends SPS with Marker and PPS without
		if packet.Marker && len(buf)-start < PSMaxSize {
			switch NALUType(buf[start:]) {
			case NALUTypeSPS, NALUTypePPS:
				return
			case NALUTypeSEI:
				// a STAP-A with the SEI and a slice is a picture
				if hasVCL(buf[start:]) {
					break
				}
				// RtspServer https://github.com/AlexxIT/go2rtc/issues/244
				// sends, marked SPS, marked PPS, marked SEI, marked IFrame
				buf = buf[:start]
				// An SEI can't follow the picture it belongs to, it is
				// dropped. But its marker ends the access unit: skipped, it
				// would never reach the flush and no frame would come out.
				if !hasVCL(buf) {
					return
				}
			}
		}

		if start == 0 {
			for {
				// Amcrest IP4M-1051: 9, 7, 8, 6, 28...
				// Amcrest IP4M-1051: 9, 6, 1
				switch NALUType(buf) {
				case NALUTypeIFrame:
					// fix IFrame without SPS,PPS
					n := len(buf)
					buf = append(buf, ps...)
					copy(buf[len(ps):], buf[:n])
					copy(buf, ps)
				case NALUTypeSEI, NALUTypeAUD:
					// fix ffmpeg with transcoding first frame
					i := int(4 + binary.BigEndian.Uint32(buf))

					// check if only one NAL (fix ffmpeg transcoding for Reolink RLC-510A)
					if i == len(buf) {
						buf = buf[:0]
						return
					}

					buf = buf[:copy(buf, buf[i:])]
					continue
				case NALUTypePFrame, NALUTypeSPS, NALUTypePPS: // pass
				default:
					buf = buf[:0]
					return // skip any unknown NAL unit type
				}
				break
			}
		}

		// collect all NALs for Access Unit
		if !packet.Marker {
			return
		}

		payload := buf
		buf = buf[:0]

		// should not be that huge SPS
		if NALUType(payload) == NALUTypeSPS && binary.BigEndian.Uint32(payload) >= PSMaxSize {
			// some Chinese buggy cameras have a single packet with SPS+PPS+IFrame separated by 00 00 00 01
			// https://github.com/AlexxIT/WebRTC/issues/391
			// https://github.com/AlexxIT/WebRTC/issues/392
			payload = annexb.FixAnnexBInAVCC(payload)
		}

		//log.Printf("[AVC] %v, len: %d, ts: %10d, seq: %d", NALUTypes(payload), len(payload), packet.Timestamp, packet.SequenceNumber)

		// Update FmtpLine from first keyframe with parameter sets
		// This fixes MSE aspect ratio issues when RTSP cameras don't send SPS/PPS in DESCRIBE
		if !fmtpLineUpdated && ContainsParameterSets(payload) {
			newFmtpLine := GetFmtpLine(payload)
			if newFmtpLine != "" {
				codec.SetFmtp(newFmtpLine)
				// Re-extract SPS/PPS with updated FmtpLine
				sps, pps = GetParameterSet(newFmtpLine)
				ps = JoinNALU(sps, pps)
			}
			fmtpLineUpdated = true
		}

		clone := *packet
		clone.Version = RTPPacketVersionAVC
		clone.Payload = payload
		handler(&clone)
	}
}

// hasVCL reports whether the AVCC holds picture data
func hasVCL(avcc []byte) bool {
	for len(avcc) > 4 {
		if t := NALUType(avcc); t >= NALUTypePFrame && t <= NALUTypeIFrame {
			return true
		}
		size := 4 + uint64(binary.BigEndian.Uint32(avcc))
		if size >= uint64(len(avcc)) {
			break
		}
		avcc = avcc[size:]
	}
	return false
}

func RTPPay(mtu uint16, handler core.HandlerFunc) core.HandlerFunc {
	if mtu == 0 {
		mtu = 1472
	}

	payloader := &Payloader{IsAVC: true}
	sequencer := rtp.NewRandomSequencer()
	mtu -= 12 // rtp.Header size

	return func(packet *rtp.Packet) {
		if packet.Version != RTPPacketVersionAVC {
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
