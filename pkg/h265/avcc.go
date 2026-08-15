package h265

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
)

func RepairAVCC(codec *core.Codec, handler core.HandlerFunc) core.HandlerFunc {
	vps, sps, pps := GetParameterSet(codec.FmtpLine)
	ps := h264.JoinNALU(vps, sps, pps)

	fmtpLineUpdated := false

	return func(packet *rtp.Packet) {
		// AVCC needs a four byte length prefix followed by a NALU header.
		// Some cameras intermittently emit empty or truncated video packets,
		// and NALUType would read past the end of them.
		if packet == nil || len(packet.Payload) < 5 {
			return
		}

		// Update FmtpLine from first keyframe with parameter sets
		// This fixes MSE aspect ratio issues when RTSP cameras don't send VPS/SPS/PPS in DESCRIBE
		if !fmtpLineUpdated && ContainsParameterSets(packet.Payload) {
			newFmtpLine := GetFmtpLine(packet.Payload)
			if newFmtpLine != "" {
				codec.FmtpLine = newFmtpLine
				// Re-extract VPS/SPS/PPS with updated FmtpLine
				vps, sps, pps = GetParameterSet(codec.FmtpLine)
				ps = h264.JoinNALU(vps, sps, pps)
			}
			fmtpLineUpdated = true
		}

		switch NALUType(packet.Payload) {
		case NALUTypeIFrame, NALUTypeIFrame2, NALUTypeIFrame3:
			hasPS := ContainsParameterSets(packet.Payload)

			if !hasPS {
				clone := *packet
				clone.Payload = h264.Join(ps, packet.Payload)
				handler(&clone)
			} else {
				handler(packet)
			}
		default:
			handler(packet)
		}
	}
}

func AVCCToCodec(avcc []byte) *core.Codec {
	buf := bytes.NewBufferString("profile-id=1")

	for {
		n := len(avcc)
		if n < 5 { // minimum: 4 bytes length + 1 byte NAL header
			break
		}

		naluSize := binary.BigEndian.Uint32(avcc)
		// An H.265 NAL unit has a two byte header. Reject zero length,
		// one byte and over declared units before reading their type.
		if naluSize < 2 || naluSize > uint32(n-4) {
			break
		}
		size := 4 + int(naluSize)

		switch NALUType(avcc) {
		case NALUTypeVPS:
			buf.WriteString(";sprop-vps=")
			buf.WriteString(base64.StdEncoding.EncodeToString(avcc[4:size]))
		case NALUTypeSPS:
			buf.WriteString(";sprop-sps=")
			buf.WriteString(base64.StdEncoding.EncodeToString(avcc[4:size]))
		case NALUTypePPS:
			buf.WriteString(";sprop-pps=")
			buf.WriteString(base64.StdEncoding.EncodeToString(avcc[4:size]))
		}

		avcc = avcc[size:]
	}

	return &core.Codec{
		Name:        core.CodecH265,
		ClockRate:   90000,
		FmtpLine:    buf.String(),
		PayloadType: core.PayloadTypeRAW,
	}
}
