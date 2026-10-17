package h264

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"math/rand"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/require"
)

// pionRTPDepay assembles access units with pion's depacketizer, RTPDepay
// builds them in place and must emit the same bytes
func pionRTPDepay(codec *core.Codec, handler core.HandlerFunc) core.HandlerFunc {
	depack := &codecs.H264Packet{IsAVC: true}

	sps, pps := GetParameterSet(codec.Fmtp())
	ps := JoinNALU(sps, pps)

	buf := make([]byte, 0, 512*1024)
	var seqNum uint16
	var timestamp uint32
	var fragmented bool

	drop := func() {
		depack = &codecs.H264Packet{IsAVC: true}
		buf = buf[:0]
		fragmented = false
	}

	fmtpLineUpdated := false

	return func(packet *rtp.Packet) {
		if packet.Version == RTPPacketVersionAVC {
			handler(packet)
			return
		}

		if (len(buf) > 0 || fragmented) && (packet.SequenceNumber-seqNum != 1 || (fragmented && packet.Timestamp != timestamp)) {
			drop()
		}
		seqNum, timestamp = packet.SequenceNumber, packet.Timestamp

		if b := packet.Payload; len(b) >= 2 && b[0]&0x1F == NALUTypeFUA {
			if b[1]&0x80 != 0 {
				if fragmented {
					drop()
				}
				fragmented = true
			} else if !fragmented {
				drop()
				return
			}
			if b[1]&0x40 != 0 {
				fragmented = false
			}
		} else if fragmented {
			drop()
		}

		payload, err := depack.Unmarshal(packet.Payload)
		if len(payload) == 0 || err != nil {
			return
		}

		if len(buf) > 5*1024*1024 {
			buf = buf[: 0 : 512*1024]
		}

		if packet.Marker && len(payload) < PSMaxSize {
			switch NALUType(payload) {
			case NALUTypeSPS, NALUTypePPS:
				buf = append(buf, payload...)
				return
			case NALUTypeSEI:
				if !containsVCL(payload) {
					if !containsVCL(buf) {
						return
					}
					payload = nil // ends the access unit in buf
				}
			}
		}

		if len(buf) == 0 {
			for {
				switch NALUType(payload) {
				case NALUTypeIFrame:
					buf = append(buf, ps...)
				case NALUTypeSEI, NALUTypeAUD:
					i := int(4 + binary.BigEndian.Uint32(payload))
					if i == len(payload) {
						return
					}
					payload = payload[i:]
					continue
				case NALUTypePFrame, NALUTypeSPS, NALUTypePPS:
				default:
					return
				}
				break
			}
		}

		if !packet.Marker {
			buf = append(buf, payload...)
			return
		}

		if len(buf) > 0 {
			payload = append(buf, payload...)
			buf = buf[:0]
		}

		if NALUType(payload) == NALUTypeSPS && binary.BigEndian.Uint32(payload) >= PSMaxSize {
			payload = annexb.FixAnnexBInAVCC(payload)
		}

		if !fmtpLineUpdated && ContainsParameterSets(payload) {
			newFmtpLine := GetFmtpLine(payload)
			if newFmtpLine != "" {
				codec.SetFmtp(newFmtpLine)
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

func containsVCL(avcc []byte) bool {
	for i := 0; i+4 < len(avcc); i += 4 + int(binary.BigEndian.Uint32(avcc[i:])) {
		if t := avcc[i+4] & 0x1F; t >= NALUTypePFrame && t <= NALUTypeIFrame {
			return true
		}
	}
	return false
}

var spropFmtp = "packetization-mode=1;sprop-parameter-sets=" +
	base64.StdEncoding.EncodeToString(testSPS) + "," + base64.StdEncoding.EncodeToString(testPPS)

// depayAll returns the access units a depayer emits for packets and the fmtp
// it leaves on its codec
func depayAll(depay func(*core.Codec, core.HandlerFunc) core.HandlerFunc, fmtp string, packets []*rtp.Packet) ([]*rtp.Packet, string) {
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96, FmtpLine: fmtp}
	var frames []*rtp.Packet
	handler := depay(codec, func(p *rtp.Packet) {
		c := *p
		c.Payload = bytes.Clone(p.Payload)
		frames = append(frames, &c)
	})
	for _, p := range packets {
		handler(p)
	}
	return frames, codec.Fmtp()
}

func requireSameAsPion(t *testing.T, fmtp string, packets []*rtp.Packet) {
	t.Helper()
	want, wantFmtp := depayAll(pionRTPDepay, fmtp, packets)
	got, gotFmtp := depayAll(RTPDepay, fmtp, packets)
	require.Equal(t, want, got)
	require.Equal(t, wantFmtp, gotFmtp)
}

// withoutParameterSets drops the SPS and PPS packets, like a camera that has
// them only in its SDP
func withoutParameterSets(packets []*rtp.Packet) []*rtp.Packet {
	var out []*rtp.Packet
	for _, p := range packets {
		switch p.Payload[0] & 0x1F {
		case NALUTypeSPS, NALUTypePPS:
			continue
		case NALUTypeSTAPA:
			if p.Payload[3]&0x1F == NALUTypeSPS {
				continue
			}
		}
		out = append(out, p)
	}
	return renumber(out, 1000)
}

func fuPackets(seq uint16, ts uint32, marker bool, nal []byte, size int) []*rtp.Packet {
	var out []*rtp.Packet
	body := nal[1:]
	for first := true; first || len(body) > 0; first = false {
		n := min(size, len(body))
		fu := nal[0] & 0x1F
		if first {
			fu |= fuStart
		}
		if n == len(body) {
			fu |= fuEnd
		}
		out = append(out, lossPacket(seq, ts, marker && n == len(body), append([]byte{nal[0]&0xE0 | NALUTypeFUA, fu}, body[:n]...)))
		seq++
		body = body[n:]
	}
	return out
}

func stapA(nals ...[]byte) []byte {
	b := []byte{NALUTypeSTAPA}
	for _, nal := range nals {
		b = binary.BigEndian.AppendUint16(b, uint16(len(nal)))
		b = append(b, nal...)
	}
	return b
}

func sequence(groups ...[]*rtp.Packet) []*rtp.Packet {
	var out []*rtp.Packet
	for _, g := range groups {
		out = append(out, g...)
	}
	return renumber(out, 500)
}

func one(ts uint32, marker bool, payload []byte) []*rtp.Packet {
	return []*rtp.Packet{lossPacket(0, ts, marker, payload)}
}

func TestRTPDepayMatchesPion(t *testing.T) {
	for seed := range 20 {
		r := rand.New(rand.NewSource(int64(seed)))
		packets, _, _ := rtpStream(r, 300)
		for _, fmtp := range []string{"", spropFmtp} {
			requireSameAsPion(t, fmtp, packets)
			requireSameAsPion(t, fmtp, withoutParameterSets(packets))
			requireSameAsPion(t, fmtp, damage(r, packets, true))
			requireSameAsPion(t, fmtp, damage(r, packets, false))
			requireSameAsPion(t, fmtp, damage(r, withoutParameterSets(packets), false))
		}
	}
	requireSameAsPion(t, "", benchStream(2))
}

func TestRTPDepayMatchesPionCameraQuirks(t *testing.T) {
	sei := []byte{0x06, 0x05, 0x01, 0x80}
	aud := []byte{0x09, 0xF0}
	idr := append([]byte{0x65}, bytes.Repeat([]byte{0xAB}, 3000)...)
	smallIDR := []byte{0x65, 0x88, 0x84}
	p := append([]byte{0x41}, bytes.Repeat([]byte{0xCD}, 900)...)
	bigSPS := append(append([]byte{0x67}, bytes.Repeat([]byte{0x11}, 140)...), 0, 0, 0, 1)
	bigSPS = append(append(append(bigSPS, testPPS...), 0, 0, 0, 1), smallIDR...)

	tests := map[string][]*rtp.Packet{
		"tapo marked parameter sets": sequence(
			one(0, true, testSPS), one(0, true, testPPS), fuPackets(0, 0, true, idr, 1000), one(3000, true, p)),
		"rtspserver marked sei": sequence(
			one(0, true, testSPS), one(0, true, testPPS), one(0, true, sei), one(0, true, smallIDR)),
		"amcrest aud sps pps sei idr": sequence(
			one(0, false, aud), one(0, false, testSPS), one(0, false, testPPS), one(0, false, sei), fuPackets(0, 0, true, idr, 1000)),
		"amcrest aud sei p": sequence(
			one(0, false, aud), one(0, false, sei), one(0, true, p)),
		"sei alone": sequence(one(0, true, append(append([]byte(nil), sei...), make([]byte, 200)...)), one(3000, true, p)),
		"aggregated aud sei idr": sequence(
			one(0, true, stapA(aud, sei, smallIDR)), one(3000, true, stapA(aud, p))),
		"aggregated marked small sei p": sequence(one(0, true, stapA(sei, p[:20])), one(3000, true, p)),
		"idr without parameter sets": sequence(
			one(0, true, smallIDR), fuPackets(3000, 3000, true, idr, 700), one(6000, true, p)),
		"annex-b in sps":             sequence(one(0, true, bigSPS), one(3000, true, p)),
		"forbidden bit in fu-a":      sequence(fuPackets(0, 0, true, append([]byte{0xE5}, idr[1:]...), 1000)),
		"fu-a end with other header": sequence(fuPackets(0, 0, false, idr, 1000)[:2], one(0, true, []byte{0x5C, fuEnd | NALUTypePFrame, 1, 2})),
		"fu-a start and end at once": sequence(one(0, true, []byte{0x7C, fuStart | fuEnd | NALUTypeIFrame, 1, 2, 3})),
		"fu-a without data": sequence(
			one(0, false, []byte{0x7C, fuStart | NALUTypeIFrame}), one(0, false, []byte{0x7C, NALUTypeIFrame}), one(0, true, []byte{0x7C, fuEnd | NALUTypeIFrame})),
		"fu-a header only":                sequence(one(0, true, []byte{0x7C}), one(3000, true, p)),
		"aggregation with cut length":     sequence(one(0, true, append(stapA(testSPS, testPPS, smallIDR), 0))),
		"aggregation with too big length": sequence(one(0, true, append(stapA(testSPS, testPPS), 0, 9, 0x65)), one(3000, true, p)),
		"empty aggregation":               sequence(one(0, true, []byte{NALUTypeSTAPA}), one(3000, true, p)),
		"marked fragmented sps":           sequence(fuPackets(0, 0, true, testSPS, 3), one(0, true, smallIDR)),
		"data partitions and reserved types": sequence(
			one(0, true, []byte{0x42, 1}), one(0, true, []byte{0x4D, 1}), one(0, false, p), one(0, true, []byte{0x57, 1}),
			one(3000, true, []byte{0x00, 1}), one(3000, true, []byte{0x59, 1}), one(3000, true, []byte{0x5D, 1}), one(3000, true, p)),
		"padding inside a fragmented nal": sequence(fuPackets(0, 0, false, idr, 1000)[:2], one(0, false, nil), fuPackets(0, 0, true, idr, 1000)),
	}

	var huge []*rtp.Packet
	for range 6000 {
		huge = append(huge, one(0, false, p)...)
	}
	tests["access unit over 5 MB without marker"] = sequence(huge, one(0, true, smallIDR), one(3000, true, p))

	for name, packets := range tests {
		t.Run(name, func(t *testing.T) {
			for _, fmtp := range []string{"", spropFmtp} {
				requireSameAsPion(t, fmtp, packets)
			}
		})
	}
}

// fuzzPackets turns data into packets: a control byte (marker, sequence gap,
// repeated sequence number, new timestamp), a length byte, then the payload
func fuzzPackets(data []byte) []*rtp.Packet {
	var packets []*rtp.Packet
	var seq uint16
	var ts uint32
	for len(data) >= 2 {
		ctl, n := data[0], min(int(data[1]), len(data)-2)
		payload := data[2 : 2+n]
		data = data[2+n:]
		switch {
		case ctl&0x02 != 0:
			seq += 2
		case ctl&0x04 == 0:
			seq++
		}
		if ctl&0x08 != 0 {
			ts += 3000
		}
		packets = append(packets, lossPacket(seq, ts, ctl&0x01 != 0, payload))
	}
	return packets
}

// the old depay passes on an empty NAL unit for a zero size aggregation entry,
// or panics when the access unit holds nothing else; RTPDepay leaves such
// entries out
func hasEmptyAggregatedNAL(packets []*rtp.Packet) bool {
	for _, p := range packets {
		b := p.Payload
		if len(b) == 0 || b[0]&0x1F != NALUTypeSTAPA {
			continue
		}
		for i := 1; len(b)-i >= 2; {
			size := int(binary.BigEndian.Uint16(b[i:]))
			i += 2
			if len(b) < i+size {
				break
			}
			if size == 0 {
				return true
			}
			i += size
		}
	}
	return false
}

func FuzzRTPDepayMatchesPion(f *testing.F) {
	f.Add(false, []byte{1, 4, 0x41, 1, 2, 3})
	f.Add(true, []byte{0, 4, 0x7C, 0x85, 1, 2, 0, 3, 0x7C, 0x05, 3, 1, 3, 0x7C, 0x45, 4, 9, 4, 0x41, 1, 2, 3})
	f.Add(false, []byte{1, 11, 24, 0, 2, 0x67, 1, 0, 2, 0x68, 2, 0, 1, 1, 3, 0x65, 1, 2})
	f.Add(true, []byte{0, 2, 0x09, 0xF0, 0, 3, 0x06, 5, 1, 1, 3, 0x65, 0x88, 0x84})
	f.Fuzz(func(t *testing.T, sprop bool, data []byte) {
		fmtp := ""
		if sprop {
			fmtp = spropFmtp
		}
		packets := fuzzPackets(data)
		got, gotFmtp := depayAll(RTPDepay, fmtp, packets)
		if hasEmptyAggregatedNAL(packets) {
			return
		}
		want, wantFmtp := depayAll(pionRTPDepay, fmtp, packets)
		require.Equal(t, want, got)
		require.Equal(t, wantFmtp, gotFmtp)
	})
}
