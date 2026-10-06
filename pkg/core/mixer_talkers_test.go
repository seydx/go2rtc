package core

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// countingFFmpeg returns an ffmpeg that logs every start, and a counter.
func countingFFmpeg(t *testing.T) (string, func() int) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell wrapper")
	}
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found")
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "starts")
	wrapper := filepath.Join(dir, "ffmpeg")
	script := fmt.Sprintf("#!/bin/sh\necho >> '%s'\nexec '%s' \"$@\"\n", log, bin)
	require.NoError(t, os.WriteFile(wrapper, []byte(script), 0o755))

	return wrapper, func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "\n")
	}
}

func (m *RTPMixer) ffmpegRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ffmpegCmd != nil
}

// Every WebRTC viewer is a parent, whether it talks or not. Watching viewers
// joining and leaving must not start ffmpeg, and it runs only while somebody
// talks.
func TestMixerRunsFFmpegOnlyForTalkers(t *testing.T) {
	bin, starts := countingFFmpeg(t)

	pcmu := &Codec{Name: CodecPCMU, ClockRate: 8000}
	opus := &Codec{Name: CodecOpus, ClockRate: 48000, Channels: 2, PayloadType: 111}
	media := &Media{Kind: KindAudio, Direction: DirectionSendonly, Codecs: []*Codec{pcmu}}

	m := NewRTPMixer(bin, media, pcmu)
	t.Cleanup(m.Close)

	var viewers []*Receiver
	for range 4 {
		r := NewReceiver(media, opus)
		m.AddParentWithCodec(&r.Node, opus)
		viewers = append(viewers, r)
	}
	time.Sleep(300 * time.Millisecond)
	require.Zero(t, starts(), "viewers that only watch start no ffmpeg")

	talker := viewers[0]
	talk := func() {
		talker.Input(&Packet{Header: rtp.Header{Version: 2, PayloadType: opus.PayloadType}, Payload: []byte{0xF8, 0xFF, 0xFE}})
	}
	talk()
	require.Eventually(t, m.ffmpegRunning, 5*time.Second, 10*time.Millisecond, "an Opus talker needs ffmpeg")

	viewers[3].Close()
	for range 15 {
		time.Sleep(20 * time.Millisecond)
		talk()
	}
	require.Equal(t, 1, starts(), "a watching viewer leaving restarts nothing")

	require.Eventually(t, func() bool { return !m.ffmpegRunning() }, talkerTimeout+2*time.Second, 50*time.Millisecond,
		"ffmpeg stops once nobody talks")

	for _, r := range viewers[:3] {
		r.Close()
	}
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, starts())
}

// A talker that keeps talking never wakes the mixer again, so a failed ffmpeg
// start has to be retried on its own.
func TestMixerRetriesFailedStart(t *testing.T) {
	bin, starts := countingFFmpeg(t)
	missing := filepath.Join(t.TempDir(), "ffmpeg")

	pcmu := &Codec{Name: CodecPCMU, ClockRate: 8000}
	opus := &Codec{Name: CodecOpus, ClockRate: 48000, Channels: 2, PayloadType: 111}
	media := &Media{Kind: KindAudio, Direction: DirectionSendonly, Codecs: []*Codec{pcmu}}

	m := NewRTPMixer(missing, media, pcmu)
	t.Cleanup(m.Close)

	talker := NewReceiver(media, opus)
	m.AddParentWithCodec(&talker.Node, opus)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				talker.Input(&Packet{Header: rtp.Header{Version: 2, PayloadType: opus.PayloadType}, Payload: []byte{0xF8, 0xFF, 0xFE}})
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})

	time.Sleep(300 * time.Millisecond)
	require.False(t, m.ffmpegRunning(), "ffmpeg can't start without its binary")

	require.NoError(t, os.Symlink(bin, missing))
	require.Eventually(t, m.ffmpegRunning, restartDelay+3*time.Second, 20*time.Millisecond,
		"the start is retried while the talker keeps talking")
	require.Eventually(t, func() bool { return starts() == 1 }, 2*time.Second, 10*time.Millisecond)
}
