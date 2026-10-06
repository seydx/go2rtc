package core

import (
	"bytes"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// loudPCMA is 20 ms of a full scale square wave, unmistakable for silence
var loudPCMA = bytes.Repeat([]byte{0xAA, 0x2A}, 80)

// ulaw decodes a G.711 µ-law sample
func ulaw(u byte) int {
	u = ^u
	magnitude := (int(u&0x0F)<<3+0x84)<<(u>>4&0x07) - 0x84
	if u&0x80 != 0 {
		return -magnitude
	}
	return magnitude
}

// cameraEar taps what the mixer sends to the camera and notes when it first
// carried talk instead of silence
type cameraEar struct {
	mu    sync.Mutex
	heard time.Time
}

func (e *cameraEar) heardAt() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.heard
}

func (e *cameraEar) talkHeard() bool {
	return !e.heardAt().IsZero()
}

func (e *cameraEar) forget() {
	e.mu.Lock()
	e.heard = time.Time{}
	e.mu.Unlock()
}

// talkMixer is a PCMU camera with one PCMA talker, ffmpeg transcodes its talk
func talkMixer(t *testing.T, bin string) (*RTPMixer, *Receiver, *cameraEar) {
	pcmu := &Codec{Name: CodecPCMU, ClockRate: 8000}
	pcma := &Codec{Name: CodecPCMA, ClockRate: 8000, PayloadType: 8}
	media := &Media{Kind: KindAudio, Direction: DirectionSendonly, Codecs: []*Codec{pcmu}}

	m := NewRTPMixer(bin, media, pcmu)
	t.Cleanup(m.Close)

	talker := NewReceiver(media, pcma)
	m.AddParentWithCodec(&talker.Node, pcma)

	ear := &cameraEar{}
	tap := NewSender(media, pcmu)
	tap.Handler = func(packet *Packet) {
		for _, b := range packet.Payload {
			if s := ulaw(b); s > 8000 || s < -8000 {
				ear.mu.Lock()
				if ear.heard.IsZero() {
					ear.heard = time.Now()
				}
				ear.mu.Unlock()
				return
			}
		}
	}
	tap.Node.WithParent(&m.Node)
	tap.Start()
	t.Cleanup(tap.Close)

	return m, talker, ear
}

func say(talker *Receiver) {
	talker.Input(&Packet{Header: rtp.Header{Version: 2, PayloadType: 8}, Payload: append([]byte(nil), loudPCMA...)})
}

// talk keeps the talker talking every 20 ms until the returned stop or the
// end of the test
func talk(t *testing.T, talker *Receiver) (stop func()) {
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				say(talker)
			}
		}
	}()
	stop = sync.OnceFunc(func() {
		close(quit)
		<-done
	})
	t.Cleanup(stop)
	return stop
}

// Push to talk: ffmpeg stops while the talker pauses longer than
// talkerTimeout, and talking again has to wake the mixer. Otherwise the talk
// would be held from then on and never reach the camera.
func TestMixerTalkerTalksAgainAfterPause(t *testing.T) {
	bin, starts := countingFFmpeg(t)
	m, talker, ear := talkMixer(t, bin)

	stop := talk(t, talker)
	require.Eventually(t, ear.talkHeard, 5*time.Second, 10*time.Millisecond, "the camera hears the talker")
	stop()

	require.Eventually(t, func() bool { return !m.ffmpegRunning() }, talkerTimeout+2*time.Second, 50*time.Millisecond,
		"ffmpeg stops during the pause")
	ear.forget()

	talk(t, talker)
	require.Eventually(t, ear.talkHeard, 5*time.Second, 10*time.Millisecond, "the camera hears the talker again")
	require.Equal(t, 2, starts())
}

// ffmpeg dies while somebody talks. The talker never paused, so it never
// wakes the mixer: the crash itself has to bring ffmpeg back.
func TestMixerRestartsCrashedFFmpeg(t *testing.T) {
	bin, starts := countingFFmpeg(t)
	m, talker, ear := talkMixer(t, bin)

	talk(t, talker)
	require.Eventually(t, ear.talkHeard, 5*time.Second, 10*time.Millisecond, "the camera hears the talker")

	m.mu.Lock()
	cmd := m.ffmpegCmd
	m.mu.Unlock()
	require.NotNil(t, cmd)
	require.NoError(t, cmd.Process.Kill())

	require.Eventually(t, func() bool { return starts() == 2 }, restartDelay+3*time.Second, 20*time.Millisecond,
		"a crashed ffmpeg is started again")
	ear.forget()
	require.Eventually(t, ear.talkHeard, 5*time.Second, 10*time.Millisecond, "the camera hears the talker again")
}

// ffmpeg starts with the first talk packet. What the talker says meanwhile is
// held and goes in as soon as ffmpeg answers, not only after listenTimeout:
// every talk would start a second late otherwise.
func TestMixerReleasesHeldTalkOnceFFmpegAnswers(t *testing.T) {
	bin, _ := countingFFmpeg(t)
	m, talker, ear := talkMixer(t, bin)

	talk(t, talker)
	require.Eventually(t, ear.talkHeard, 5*time.Second, 5*time.Millisecond, "the camera hears the talker")

	m.mu.Lock()
	started := time.Unix(0, m.startedAt)
	m.mu.Unlock()
	after := ear.heardAt().Sub(started)
	require.Less(t, after, listenTimeout, "the talk came out %v after ffmpeg started", after)
}

// Without ffmpeg nothing takes the held talk. However long the talker talks,
// the mixer holds no more than holdLimit packets of it.
func TestMixerHoldStaysBoundedWithoutFFmpeg(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "ffmpeg")
	m, talker, _ := talkMixer(t, missing)

	for range 4 * holdLimit {
		say(talker)
	}

	m.mu.Lock()
	held := len(m.held[talker.id])
	m.mu.Unlock()
	require.Equal(t, holdLimit, held)
}
