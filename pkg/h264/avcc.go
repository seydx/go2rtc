package h264

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

func RepairAVCC(codec *core.Codec, handler core.HandlerFunc) core.HandlerFunc {
	sps, pps := GetParameterSet(codec.Fmtp())
	ps := JoinNALU(sps, pps)

	fmtpLineUpdated := false

	return func(packet *rtp.Packet) {
		// without length prefixes that add up there is no telling where a NAL
		// unit ends: a damaged unit, everything below would read past it
		if !validAVCC(packet.Payload) {
			return
		}

		// Update FmtpLine from first keyframe with parameter sets
		// This fixes MSE aspect ratio issues when RTSP cameras don't send SPS/PPS in DESCRIBE
		if !fmtpLineUpdated && ContainsParameterSets(packet.Payload) {
			newFmtpLine := GetFmtpLine(packet.Payload)
			if newFmtpLine != "" {
				codec.SetFmtp(newFmtpLine)
				// Re-extract SPS/PPS with updated FmtpLine
				sps, pps = GetParameterSet(newFmtpLine)
				ps = JoinNALU(sps, pps)
			}
			fmtpLineUpdated = true
		}

		payload := packet.Payload
		repaired := false

		// this can happen for FLV from FFmpeg
		if NALUType(payload) == NALUTypeSEI {
			if payload = payload[4+binary.BigEndian.Uint32(payload):]; len(payload) == 0 {
				return // SEI only, no picture
			}
			repaired = true
		}
		if NALUType(payload) == NALUTypeIFrame && !ContainsParameterSets(payload) {
			payload = Join(ps, payload)
			repaired = true
		}

		if repaired {
			// the packet is shared with the other consumers of the track and
			// with the GOP cache
			clone := *packet
			clone.Payload = payload
			packet = &clone
		}
		handler(packet)
	}
}

// validAVCC reports whether avcc consists of NAL units whose length prefixes
// add up to its size, each one with at least the NAL header
func validAVCC(avcc []byte) bool {
	if len(avcc) == 0 {
		return false
	}
	for len(avcc) > 0 {
		if len(avcc) < 5 {
			return false
		}
		size := binary.BigEndian.Uint32(avcc)
		if size == 0 || uint64(size) > uint64(len(avcc)-4) {
			return false
		}
		avcc = avcc[4+size:]
	}
	return true
}

func JoinNALU(nalus ...[]byte) (avcc []byte) {
	var i, n int

	for _, nalu := range nalus {
		if i = len(nalu); i > 0 {
			n += 4 + i
		}
	}

	avcc = make([]byte, n)

	n = 0
	for _, nal := range nalus {
		if i = len(nal); i > 0 {
			binary.BigEndian.PutUint32(avcc[n:], uint32(i))
			n += 4 + copy(avcc[n+4:], nal)
		}
	}

	return
}

func SplitNALU(avcc []byte) [][]byte {
	var nals [][]byte
	for {
		// get AVC length
		size := int(binary.BigEndian.Uint32(avcc)) + 4

		// check if multiple items in one packet
		if size < len(avcc) {
			nals = append(nals, avcc[:size])
			avcc = avcc[size:]
		} else {
			nals = append(nals, avcc)
			break
		}
	}
	return nals
}

func NALUTypes(avcc []byte) []byte {
	var types []byte
	for len(avcc) >= 5 { // minimum: 4 bytes length + 1 byte NAL header
		types = append(types, NALUType(avcc))

		size := 4 + int(binary.BigEndian.Uint32(avcc))
		if size < len(avcc) {
			avcc = avcc[size:]
		} else {
			break
		}
	}
	return types
}

func AVCCToCodec(avcc []byte) *core.Codec {
	buf := bytes.NewBufferString("packetization-mode=1")

	for {
		n := len(avcc)
		if n < 5 { // minimum: 4 bytes length + 1 byte NAL header
			break
		}

		size := 4 + int(binary.BigEndian.Uint32(avcc))
		if n < size {
			break
		}

		switch NALUType(avcc) {
		case NALUTypeSPS:
			// profile-level-id is read from the three bytes after the NAL
			// header, so a truncated SPS carries no usable parameter set
			if size < 8 {
				break
			}
			buf.WriteString(";profile-level-id=")
			buf.WriteString(hex.EncodeToString(avcc[5:8]))
			buf.WriteString(";sprop-parameter-sets=")
			buf.WriteString(base64.StdEncoding.EncodeToString(avcc[4:size]))
		case NALUTypePPS:
			buf.WriteString(",")
			buf.WriteString(base64.StdEncoding.EncodeToString(avcc[4:size]))
		}

		avcc = avcc[size:]
	}

	return &core.Codec{
		Name:        core.CodecH264,
		ClockRate:   90000,
		FmtpLine:    buf.String(),
		PayloadType: core.PayloadTypeRAW,
	}
}
