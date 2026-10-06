package core_test

import (
	"bytes"
	"fmt"
	"math"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func lookFFmpeg(t *testing.T) string {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found")
	}
	return bin
}

// pcmuStats returns the DC offset, the RMS level in dBFS and the length of
// the span between the first and the last sample above the noise floor.
func pcmuStats(data []byte) (dc, level float64, n int) {
	samples := make([]float64, len(data))
	first, last := -1, -1
	for i, b := range data {
		samples[i] = float64(pcm.PCMUtoPCM(b)) / 32768
		if math.Abs(samples[i]) > 0.01 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return 0, math.Inf(-1), 0
	}

	var sum, sumSq float64
	span := samples[first : last+1]
	for _, s := range span {
		sum += s
		sumSq += s * s
	}
	n = len(span)
	return sum / float64(n), 10 * math.Log10(sumSq/float64(n)), n
}

// A viewer talks while the other WebRTC viewers only watch. Their microphones
// send nothing, and the camera must hear the talker as it is: no offset from
// the filler of the idle inputs, no level drop per extra viewer.
func TestMixerTalkerNextToSilentParents(t *testing.T) {
	bin := lookFFmpeg(t)

	pcmu := &core.Codec{Name: core.CodecPCMU, ClockRate: 8000}
	pcma := &core.Codec{Name: core.CodecPCMA, ClockRate: 8000, PayloadType: 8}
	opus := &core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2, PayloadType: 111}
	l16 := &core.Codec{Name: core.CodecPCML, ClockRate: 16000, Channels: 1, PayloadType: 96}
	media := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{pcmu}}

	tone := make([]byte, 2*8000)
	for i := range tone {
		tone[i] = pcm.PCMtoPCMU(int16(0.25 * 32767 * math.Sin(2*math.Pi*440*float64(i)/8000)))
	}
	_, toneLevel, _ := pcmuStats(tone)
	silence := bytes.Repeat([]byte{0xFF}, 160)

	cases := []struct {
		watchers int
		codec    *core.Codec
	}{
		{0, pcmu},
		{1, pcmu},
		{3, pcmu},
		{1, pcma},
		{1, opus},
		{1, l16},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d %s watchers", tc.watchers, tc.codec.Name), func(t *testing.T) {
			mixer := core.NewRTPMixer(bin, media, pcmu)
			t.Cleanup(mixer.Close)

			talker := core.NewReceiver(media, pcmu)
			mixer.AddParentWithCodec(&talker.Node, pcmu)
			for range tc.watchers {
				watcher := core.NewReceiver(media, tc.codec)
				mixer.AddParentWithCodec(&watcher.Node, tc.codec)
			}

			var mu sync.Mutex
			var heard []byte
			tap := core.NewSender(media, pcmu)
			tap.Handler = func(packet *core.Packet) {
				mu.Lock()
				heard = append(heard, packet.Payload...)
				mu.Unlock()
			}
			tap.Node.WithParent(&mixer.Node)
			tap.Start()
			t.Cleanup(tap.Close)

			heardLen := func() int {
				mu.Lock()
				defer mu.Unlock()
				return len(heard)
			}
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			send := func(payload []byte) {
				<-ticker.C
				talker.Input(&core.Packet{Header: rtp.Header{Version: 2}, Payload: append([]byte(nil), payload...)})
			}

			// ffmpeg is up once the talker's silence comes out
			for deadline := time.Now().Add(10 * time.Second); heardLen() == 0; {
				require.True(t, time.Now().Before(deadline), "the mixer never sent anything")
				send(silence)
			}
			mu.Lock()
			heard = nil
			mu.Unlock()

			for off := 0; off+160 <= len(tone); off += 160 {
				send(tone[off : off+160])
			}
			for deadline := time.Now().Add(5 * time.Second); heardLen() < len(tone)+4000; {
				require.True(t, time.Now().Before(deadline), "the camera must get the talk")
				send(silence)
			}

			mu.Lock()
			out := append([]byte(nil), heard...)
			mu.Unlock()

			dc, level, n := pcmuStats(out)
			require.Greater(t, n, len(tone)/2, "the camera must get the talk")
			require.InDelta(t, 0, dc, 0.01, "DC offset in FS")
			require.InDelta(t, toneLevel, level, 1, "level in dBFS")
		})
	}
}
