package h264

import (
	"bytes"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// sprop-parameter-sets of a tiny SPS (0x67) and PPS (0x68)
const testFmtp = "sprop-parameter-sets=Zw==,aA=="

// A length prefix that doesn't add up leaves no telling where a NAL unit
// ends. Such a unit is dropped instead of taking the process down.
func TestRepairAVCCDropsMalformedPayload(t *testing.T) {
	for _, payload := range [][]byte{
		nil, {0}, {0, 0}, {0, 0, 0}, {0, 0, 0, 1}, {0, 0, 0, 0, 1},
		{0, 0, 0, 2, 1}, {0xff, 0xff, 0xff, 0xff, 6},
		append(JoinNALU([]byte{6}), 0, 0, 0, 1),
		append(JoinNALU([]byte{1}), 0, 0, 0, 2, 1),
		append(JoinNALU([]byte{5}), 0, 0, 0, 0),
		append(JoinNALU([]byte{5}), 0, 0),
		append([]byte{0, 0, 0, 0}, JoinNALU([]byte{1, 2})...),
		append(append(JoinNALU([]byte{1, 2}), 0, 0, 0, 0), JoinNALU([]byte{1, 3})...),
	} {
		t.Run(hex.EncodeToString(payload), func(t *testing.T) {
			called := false
			handler := RepairAVCC(&core.Codec{Name: core.CodecH264}, func(*rtp.Packet) { called = true })
			require.NotPanics(t, func() { handler(&rtp.Packet{Payload: payload}) })
			require.False(t, called)
		})
	}
}

// A unit of nothing but SEI carries no picture: once the SEI is stripped
// there is nothing left to hand on.
func TestRepairAVCCDropsSEIOnly(t *testing.T) {
	for _, payload := range [][]byte{
		JoinNALU([]byte{6}),
		JoinNALU([]byte{6, 1, 2}),
	} {
		called := false
		handler := RepairAVCC(&core.Codec{Name: core.CodecH264}, func(*rtp.Packet) { called = true })
		require.NotPanics(t, func() { handler(&rtp.Packet{Payload: payload}) })
		require.False(t, called)
	}
}

func TestRepairAVCCValidPayload(t *testing.T) {
	for _, test := range []struct {
		name            string
		input, expected []byte
	}{
		{"P frame", JoinNALU([]byte{1, 2}), JoinNALU([]byte{1, 2})},
		{"I frame", JoinNALU([]byte{5, 2}), JoinNALU([]byte{0x67}, []byte{0x68}, []byte{5, 2})},
		{"I frame with parameter sets", JoinNALU([]byte{0x67}, []byte{0x68}, []byte{5, 2}), JoinNALU([]byte{0x67}, []byte{0x68}, []byte{5, 2})},
		{"SEI and P frame", JoinNALU([]byte{6, 2}, []byte{1, 3}), JoinNALU([]byte{1, 3})},
		{"SEI and I frame", JoinNALU([]byte{6, 2}, []byte{5, 3}), JoinNALU([]byte{0x67}, []byte{0x68}, []byte{5, 3})},
		{"multiple NALUs", JoinNALU([]byte{1, 2}, []byte{1, 3}), JoinNALU([]byte{1, 2}, []byte{1, 3})},
	} {
		t.Run(test.name, func(t *testing.T) {
			codec := &core.Codec{Name: core.CodecH264, FmtpLine: testFmtp}
			packet := &rtp.Packet{Header: rtp.Header{Timestamp: 1234, SequenceNumber: 12, Marker: true}, Payload: test.input}
			var got *rtp.Packet
			RepairAVCC(codec, func(actual *rtp.Packet) { got = actual })(packet)
			require.NotNil(t, got)
			require.Equal(t, test.expected, got.Payload)
			require.Equal(t, packet.Header, got.Header)
		})
	}
}

// Every consumer of a track gets the same packet, and the GOP cache keeps it
// for the next consumer. The repair works on a copy: the packet stays as the
// camera sent it, whatever the consumer before did with it.
func TestRepairAVCCLeavesSharedPacketUntouched(t *testing.T) {
	for _, input := range [][]byte{
		JoinNALU([]byte{6, 2}, []byte{1, 3}),
		JoinNALU([]byte{5, 2}),
		JoinNALU([]byte{6, 2}, []byte{5, 3}),
	} {
		original := bytes.Clone(input)
		packet := &rtp.Packet{Header: rtp.Header{Timestamp: 1234}, Payload: input}

		var outputs [][]byte
		for range 2 {
			codec := &core.Codec{Name: core.CodecH264, FmtpLine: testFmtp}
			RepairAVCC(codec, func(actual *rtp.Packet) {
				outputs = append(outputs, actual.Payload)
			})(packet)
		}

		require.Equal(t, original, packet.Payload, "the shared packet is untouched")
		require.Len(t, outputs, 2)
		require.Equal(t, outputs[0], outputs[1], "both consumers get the same repair")
	}
}

// The real fan-out: two consumers on one track repair the same packets on
// their own goroutines. Run with -race.
func TestRepairAVCCConsumersShareTrack(t *testing.T) {
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, FmtpLine: testFmtp}
	media := &core.Media{Kind: core.KindVideo, Codecs: []*core.Codec{codec}}
	track := core.NewReceiver(media, codec)

	const frames = 100
	expected := JoinNALU([]byte{0x67}, []byte{0x68}, []byte{5, 3})

	var mu sync.Mutex
	var repaired int
	done := make(chan struct{}, 2)
	for range 2 {
		var received int
		sender := core.NewSender(media, codec)
		sender.UseGOP = false
		sender.Handler = RepairAVCC(codec, func(packet *rtp.Packet) {
			if bytes.Equal(expected, packet.Payload) {
				mu.Lock()
				repaired++
				mu.Unlock()
			}
			if received++; received == frames {
				done <- struct{}{}
			}
		})
		sender.WithParent(track).Start()
		t.Cleanup(sender.Close)
	}

	for i := range frames {
		track.Input(&rtp.Packet{Header: rtp.Header{Timestamp: uint32(i)}, Payload: JoinNALU([]byte{6, 2}, []byte{5, 3})})
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the consumers didn't get every frame")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2*frames, repaired)
}
