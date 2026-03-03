package h265

import (
	"encoding/base64"
	"encoding/binary"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

const (
	NALUTypePFrame    = 1
	NALUTypeIFrame    = 19
	NALUTypeIFrame2   = 20
	NALUTypeIFrame3   = 21
	NALUTypeVPS       = 32
	NALUTypeSPS       = 33
	NALUTypePPS       = 34
	NALUTypePrefixSEI = 39
	NALUTypeSuffixSEI = 40
	NALUTypeAP        = 48
	NALUTypeFU        = 49
)

func NALUType(b []byte) byte {
	return (b[4] >> 1) & 0x3F
}

func IsKeyframe(b []byte) bool {
	for {
		switch NALUType(b) {
		case NALUTypePFrame:
			return false
		case NALUTypeIFrame, NALUTypeIFrame2, NALUTypeIFrame3:
			return true
		}

		size := int(binary.BigEndian.Uint32(b)) + 4
		if size < len(b) {
			b = b[size:]
			continue
		} else {
			return false
		}
	}
}

func Types(data []byte) []byte {
	var types []byte
	for len(data) >= 5 { // minimum: 4 bytes length + 1 byte NAL header
		types = append(types, NALUType(data))

		size := 4 + int(binary.BigEndian.Uint32(data))
		if size < len(data) {
			data = data[size:]
		} else {
			break
		}
	}
	return types
}

func GetParameterSet(fmtp string) (vps, sps, pps []byte) {
	if fmtp == "" {
		return
	}

	s := core.Between(fmtp, "sprop-vps=", ";")
	vps, _ = base64.StdEncoding.DecodeString(s)

	s = core.Between(fmtp, "sprop-sps=", ";")
	sps, _ = base64.StdEncoding.DecodeString(s)

	s = core.Between(fmtp, "sprop-pps=", ";")
	pps, _ = base64.StdEncoding.DecodeString(s)

	return
}

func ContainsParameterSets(payload []byte) bool {
	types := Types(payload)
	hasVPS, hasSPS, hasPPS := false, false, false

	for _, nalType := range types {
		switch nalType {
		case NALUTypeVPS:
			hasVPS = true
		case NALUTypeSPS:
			hasSPS = true
		case NALUTypePPS:
			hasPPS = true
		}
	}

	return hasVPS && hasSPS && hasPPS
}

func GetFmtpLine(avcc []byte) string {
	var vps, sps, pps []byte

	for len(avcc) >= 5 { // minimum: 4 bytes length + 1 byte NAL header
		size := 4 + int(binary.BigEndian.Uint32(avcc))

		if size > len(avcc) {
			break
		}

		switch NALUType(avcc) {
		case NALUTypeVPS:
			vps = avcc[4:size]
		case NALUTypeSPS:
			sps = avcc[4:size]
		case NALUTypePPS:
			pps = avcc[4:size]
		}

		if size < len(avcc) {
			avcc = avcc[size:]
		} else {
			break
		}
	}

	if len(vps) == 0 || len(sps) == 0 || len(pps) == 0 {
		return ""
	}

	fmtp := "sprop-vps=" + base64.StdEncoding.EncodeToString(vps)
	fmtp += ";sprop-sps=" + base64.StdEncoding.EncodeToString(sps)
	fmtp += ";sprop-pps=" + base64.StdEncoding.EncodeToString(pps)

	return fmtp
}

// ExpandAPs deaggregates Aggregation Packets (type 48) in AVCC data.
// AP format: [4B AVCC len][2B NAL hdr][2B subLen][subNALU][2B subLen][subNALU]...
// Each sub-NALU is wrapped with a 4-byte AVCC length prefix.
func ExpandAPs(avcc []byte) []byte {
	// Quick scan: check if any AP NALUs exist
	hasAP := false
	offset := 0
	for offset+4 <= len(avcc) {
		naluLen := int(binary.BigEndian.Uint32(avcc[offset:]))
		if naluLen <= 0 || offset+4+naluLen > len(avcc) {
			break
		}
		if NALUType(avcc[offset:]) == NALUTypeAP {
			hasAP = true
			break
		}
		offset += 4 + naluLen
	}
	if !hasAP {
		return avcc
	}

	// Expand APs into individual AVCC NALUs
	var out []byte
	offset = 0
	for offset+4 <= len(avcc) {
		naluLen := int(binary.BigEndian.Uint32(avcc[offset:]))
		if naluLen <= 0 || offset+4+naluLen > len(avcc) {
			break
		}

		if NALUType(avcc[offset:]) == NALUTypeAP && naluLen > 2 {
			// Skip AVCC length (4) + AP NAL header (2)
			apOff := offset + 4 + 2
			apEnd := offset + 4 + naluLen
			for apOff+2 <= apEnd {
				subLen := int(binary.BigEndian.Uint16(avcc[apOff:]))
				apOff += 2
				if subLen <= 0 || apOff+subLen > apEnd {
					break
				}
				// Wrap sub-NALU with 4-byte AVCC length prefix
				out = binary.BigEndian.AppendUint32(out, uint32(subLen))
				out = append(out, avcc[apOff:apOff+subLen]...)
				apOff += subLen
			}
		} else {
			// Regular NALU — pass through
			out = append(out, avcc[offset:offset+4+naluLen]...)
		}

		offset += 4 + naluLen
	}
	return out
}
