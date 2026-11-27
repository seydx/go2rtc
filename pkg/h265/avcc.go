// Package h265 - AVCC format related functions
package h265

import (
	"bytes"
	"encoding/base64"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/pion/rtp"
)

func RepairAVCC(codec *core.Codec, handler core.HandlerFunc) core.HandlerFunc {
	vps, sps, pps := GetParameterSet(codec.Fmtp())
	ps := h264.JoinNALU(vps, sps, pps)

	fmtpLineUpdated := false

	return func(packet *rtp.Packet) {
		// Empty NAL units are left out. Without length prefixes that add up
		// there is no telling where a NAL unit ends: such a payload is dropped.
		payload := annexb.NormalizeAVCC(packet.Payload, h265NaluHeaderSize)
		if len(payload) == 0 {
			return
		}

		// Update FmtpLine from first keyframe with parameter sets
		// This fixes MSE aspect ratio issues when RTSP cameras don't send VPS/SPS/PPS in DESCRIBE
		if !fmtpLineUpdated && ContainsParameterSets(payload) {
			newFmtpLine := GetFmtpLine(payload)
			if newFmtpLine != "" {
				codec.SetFmtp(newFmtpLine)
				// Re-extract VPS/SPS/PPS with updated FmtpLine
				vps, sps, pps = GetParameterSet(newFmtpLine)
				ps = h264.JoinNALU(vps, sps, pps)
			}
			fmtpLineUpdated = true
		}

		prependPS := len(ps) > 0 && isIRAP(NALUType(payload)) && !ContainsParameterSets(payload)
		if prependPS {
			payload = h264.Join(ps, payload)
		}

		// the packet is shared with the other consumers of the track and
		// with the GOP cache
		if prependPS || len(payload) != len(packet.Payload) {
			clone := *packet
			clone.Payload = payload
			packet = &clone
		}
		handler(packet)
	}
}

func AVCCToCodec(avcc []byte) *core.Codec {
	buf := bytes.NewBufferString("profile-id=1")

	for len(avcc) > 0 {
		size := annexb.AVCCNALUSize(avcc)
		if size == 0 {
			break
		}

		if size >= 4+h265NaluHeaderSize {
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
		}

		if size < len(avcc) {
			avcc = avcc[size:]
		} else {
			break
		}
	}

	return &core.Codec{
		Name:        core.CodecH265,
		ClockRate:   90000,
		FmtpLine:    buf.String(),
		PayloadType: core.PayloadTypeRAW,
	}
}
