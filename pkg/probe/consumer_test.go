package probe

import (
	"encoding/binary"
	"math/rand"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// h264Stream is an RTP stream of a camera that sends SPS and PPS only in-band:
// an IDR with parameter sets every 50 frames, FU-A fragmented
func h264Stream(gops int) []*rtp.Packet {
	r := rand.New(rand.NewSource(1))
	sps := []byte{0x67, 0x42, 0xE0, 0x1F, 0xDA, 0x02, 0x80, 0xF6, 0x80, 0x6D, 0x0A, 0x13, 0x50}
	pps := []byte{0x68, 0xCE, 0x3C, 0x80}

	var packets []*rtp.Packet
	pay := h264.RTPPay(1400+12, func(p *rtp.Packet) {
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
				avcc = h264.JoinNALU(sps, pps)
			}
			avcc = binary.BigEndian.AppendUint32(avcc, uint32(len(nal)))
			avcc = append(avcc, nal...)
			pay(&rtp.Packet{Header: rtp.Header{Version: h264.RTPPacketVersionAVC, Timestamp: ts}, Payload: avcc})
			ts += 3600
		}
	}
	return packets
}

func h264Track() (*core.Media, *core.Receiver) {
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96, FmtpLine: "packetization-mode=1"}
	media.Codecs = []*core.Codec{codec}
	return media, core.NewReceiver(media, codec)
}

func attach(t testing.TB, media *core.Media, track *core.Receiver) *Probe {
	t.Helper()
	p := Create("preload", url.Values{"video": nil})
	require.NoError(t, p.AddTrack(media, track.Codec, track))
	t.Cleanup(func() { _ = p.Stop() })
	return p
}

// MSE viewers need the SPS for the aspect ratio, a camera without
// sprop-parameter-sets in its SDP gets them from the preload probe
func TestProbeLearnsFmtpFromInbandParameterSets(t *testing.T) {
	media, track := h264Track()
	p := attach(t, media, track)

	var size int
	for _, packet := range h264Stream(2) {
		track.Input(packet)
		size += len(packet.Payload)
	}

	require.Eventually(t, func() bool {
		return strings.Contains(track.Codec.Fmtp(), "sprop-parameter-sets=")
	}, 5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return p.Bytes() == size }, 5*time.Second, 10*time.Millisecond, "the probe counts the received payload")
}

// attachAfterGop caches the first of two GOPs, attaches the probe, feeds the
// second one live and waits until the probe has handled the replay and live
// packets; it returns the second GOP
func attachAfterGop(t *testing.T) (*Probe, *core.Receiver, []*rtp.Packet) {
	t.Helper()
	media, track := h264Track()
	track.SetupGOP()
	packets := h264Stream(2)
	cached, live := packets[:len(packets)/2], packets[len(packets)/2:]

	var size int
	depay := h264.RTPDepay(&core.Codec{Name: core.CodecH264, FmtpLine: "packetization-mode=1"}, func(frame *rtp.Packet) {
		size += len(frame.Payload)
	})
	for _, packet := range cached {
		track.Input(packet)
		depay(packet)
	}

	p := attach(t, media, track)
	for _, packet := range live {
		track.Input(packet)
		size += len(packet.Payload)
	}
	require.Eventually(t, func() bool { return p.Bytes() == size }, 5*time.Second, 10*time.Millisecond)
	return p, track, live
}

// the probe attaches to a replayed AVC keyframe and never depays it, the
// fmtp comes from the GOP cache's depayer working on the same codec
func TestProbeWithGopCacheKeepsFmtp(t *testing.T) {
	_, track, _ := attachAfterGop(t)
	require.Contains(t, track.Codec.Fmtp(), "sprop-parameter-sets=")
}

func BenchmarkProbe(b *testing.B) {
	packets := h264Stream(4)
	media, track := h264Track()
	handler := attach(b, media, track).Senders[0].Handler

	var size int
	for _, packet := range packets {
		size += len(packet.Payload)
	}
	b.SetBytes(int64(size / len(packets)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		handler(packets[i%len(packets)])
	}
}
