package core

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// Talk setup failed while viewers were attached: the mixer is closed with its
// parents still on it, and they leave one by one later.
func TestClosedMixerStaysStoppedWhenParentsLeave(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found")
	}

	pcmu := &Codec{Name: CodecPCMU, ClockRate: 8000}
	opus := &Codec{Name: CodecOpus, ClockRate: 48000, Channels: 2, PayloadType: 111}
	media := &Media{Kind: KindAudio, Direction: DirectionSendonly, Codecs: []*Codec{pcmu}}

	m := NewRTPMixer(bin, media, pcmu)
	var parents []*Receiver
	for range 3 {
		r := NewReceiver(media, opus)
		m.AddParentWithCodec(&r.Node, opus)
		parents = append(parents, r)
	}

	m.Close()
	for _, r := range parents {
		r.Close()
	}

	m.mu.Lock()
	cmd := m.ffmpegCmd
	m.mu.Unlock()
	if cmd != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	require.Nil(t, cmd, "ffmpeg runs on a closed mixer")
}

// The last viewer left, the next one talks into the same mixer and still
// gets transcoded.
func TestMixerWithoutParentsTakesNewTalker(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found")
	}

	pcmu := &Codec{Name: CodecPCMU, ClockRate: 8000}
	opus := &Codec{Name: CodecOpus, ClockRate: 48000, Channels: 2, PayloadType: 111}
	media := &Media{Kind: KindAudio, Direction: DirectionSendonly, Codecs: []*Codec{pcmu}}

	m := NewRTPMixer(bin, media, pcmu)
	first := NewReceiver(media, opus)
	m.AddParentWithCodec(&first.Node, opus)
	first.Close()

	next := NewReceiver(media, opus)
	m.AddParentWithCodec(&next.Node, opus)

	m.mu.Lock()
	running := m.ffmpegCmd != nil
	m.mu.Unlock()
	m.stopFFmpeg()
	require.True(t, running, "the Opus talker needs ffmpeg")
}
