package h264

import (
	"encoding/binary"
	"math/rand"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

// benchStream is a 4 Mbit/s camera at 25 fps with a 2 s GOP: a 120 KB IDR
// behind in-band SPS and PPS, then 49 P-frames of 17.5 KB, for a 1400 byte
// RTP payload
func benchStream(gops int) []*rtp.Packet {
	r := rand.New(rand.NewSource(1))
	sps := []byte{0x67, 0x42, 0xE0, 0x1F, 0xDA, 0x02, 0x80, 0xF6, 0x80, 0x6D, 0x0A, 0x13, 0x50}
	pps := []byte{0x68, 0xCE, 0x3C, 0x80}

	var packets []*rtp.Packet
	pay := RTPPay(1400+12, func(p *rtp.Packet) {
		c := &rtp.Packet{Header: p.Header, Payload: append([]byte(nil), p.Payload...)}
		c.PayloadType = 96
		c.SequenceNumber = uint16(len(packets))
		packets = append(packets, c)
	})

	var ts uint32
	for range gops {
		for frame := range 50 {
			nal := make([]byte, 17500)
			if frame == 0 {
				nal = make([]byte, 120000)
			}
			r.Read(nal)
			nal[0] = 0x41
			var avcc []byte
			if frame == 0 {
				nal[0] = 0x65
				avcc = JoinNALU(sps, pps)
			}
			avcc = binary.BigEndian.AppendUint32(avcc, uint32(len(nal)))
			avcc = append(avcc, nal...)
			pay(&rtp.Packet{Header: rtp.Header{Version: RTPPacketVersionAVC, Timestamp: ts}, Payload: avcc})
			ts += 3600
		}
	}
	return packets
}

func benchCodec() *core.Codec {
	return &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96, FmtpLine: "packetization-mode=1"}
}

func benchPackets(b *testing.B, packets []*rtp.Packet, handler core.HandlerFunc) {
	var size int
	for _, p := range packets {
		size += len(p.Payload)
	}
	b.SetBytes(int64(size / len(packets)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		handler(packets[i%len(packets)])
	}
}

func BenchmarkRTPDepay(b *testing.B) {
	var frames int
	benchPackets(b, benchStream(4), RTPDepay(benchCodec(), func(*rtp.Packet) { frames++ }))
}
