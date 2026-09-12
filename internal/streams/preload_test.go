package streams

import (
	"slices"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/probe"
	"github.com/stretchr/testify/require"
)

func TestPreloadRetriesUntilCameraReachable(t *testing.T) {
	registerTestRTSPHandler()

	cam := newFakeCamera(t)
	cam.reject.Store(true)

	stream, err := New("preload_retry", cam.URL())
	require.NoError(t, err)

	// camera is down at boot: the preload must be registered anyway
	require.NoError(t, AddPreload("preload_retry", "video"))
	t.Cleanup(func() { _ = DelPreload("preload_retry") })

	p := GetPreload("preload_retry")
	require.NotNil(t, p)
	require.False(t, p.Attached())
	require.Error(t, p.Err())

	// camera comes up: the supervisor attaches without anyone asking
	cam.reject.Store(false)
	require.True(t, waitUntil(15*time.Second, p.Attached), "preload must attach once the camera is reachable")
	require.NoError(t, p.Err())
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }), "preload must keep the producer running")

	require.NoError(t, DelPreload("preload_retry"))
	require.Nil(t, GetPreload("preload_retry"))
	require.True(t, waitUntil(5*time.Second, func() bool { return !stream.producers[0].hasReaders() }), "producer must be released after DelPreload")
}

func TestPreloadSurvivesPartialReconnectAndHeals(t *testing.T) {
	registerTestRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)

	stream, err := New("preload_partial", cam.URL())
	require.NoError(t, err)

	require.NoError(t, AddPreload("preload_partial", "video&audio"))
	t.Cleanup(func() { _ = DelPreload("preload_partial") })

	p := GetPreload("preload_partial")
	waitForCodecs(t, p, core.CodecH264, core.CodecPCMU)
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))

	// camera reboots without audio: video is swapped, audio gets parked
	dials := cam.dialCount.Load()
	cam.noAudio.Store(true)
	cam.dropConns()
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > dials }))
	require.True(t, waitUntil(30*time.Second, func() bool { return receiverActive(stream) }), "video must recover")

	// the parked audio track keeps the preload's sender open instead of closing it
	require.True(t, p.Attached(), "partial reconnect must not close the preload's tracks")
	audio := preloadSender(p, core.CodecPCMU)
	require.NotNil(t, audio, "the audio sender must still be there")
	require.Equal(t, "connected", audio.State())

	// camera reboots with audio again: the parked track is re-negotiated
	setups := cam.setupCount.Load()
	dials = cam.dialCount.Load()
	cam.noAudio.Store(false)
	cam.dropConns()
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > dials }))
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.setupCount.Load() >= setups+2 }), "video and audio must both be set up again")
	require.True(t, waitUntil(30*time.Second, func() bool { return receiverActive(stream) }))
	require.True(t, p.Attached())
	audio = preloadSender(p, core.CodecPCMU)
	require.NotNil(t, audio, "the audio track must be live again")
	require.Equal(t, "connected", audio.State())
}

func TestPreloadReattachesAfterProducerStop(t *testing.T) {
	registerTestRTSPHandler()

	cam := newFakeCamera(t)

	stream, err := New("preload_stop", cam.URL())
	require.NoError(t, err)

	require.NoError(t, AddPreload("preload_stop", "video"))
	t.Cleanup(func() { _ = DelPreload("preload_stop") })

	p := GetPreload("preload_stop")
	require.True(t, waitUntil(10*time.Second, func() bool { return p.Attached() && receiverActive(stream) }))

	// something tears the producer down underneath the preload
	stream.producers[0].stop()
	require.False(t, p.Attached(), "stopped producer closes the preload's senders")

	require.True(t, waitUntil(preloadCheckInterval+15*time.Second, func() bool { return p.Attached() && receiverActive(stream) }), "supervisor must re-attach and restart the producer")
}

func TestPreloadNegotiatesBeforeFirstClient(t *testing.T) {
	registerTestRTSPHandler()

	cam := newFakeCamera(t)
	cam.reject.Store(true)

	stream, err := New("preload_first", cam.URL())
	require.NoError(t, err)

	require.NoError(t, AddPreload("preload_first", "video&audio"))
	t.Cleanup(func() { _ = DelPreload("preload_first") })
	p := GetPreload("preload_first")
	require.False(t, p.Attached())

	// camera is back and a video-only client beats the supervisor to it
	cam.reject.Store(false)
	dials, setups := cam.dialCount.Load(), cam.setupCount.Load()

	cons := newProbeConsumer()
	require.NoError(t, stream.AddConsumer(cons))
	t.Cleanup(func() { stream.RemoveConsumer(cons) })

	// the preload went first: one session, negotiated with video+audio,
	// and the client reused its video track — no reconnect needed
	require.True(t, p.Attached(), "preload must be attached before the client")
	require.Equal(t, int32(1), cam.dialCount.Load()-dials, "exactly one camera session")
	require.Equal(t, int32(2), cam.setupCount.Load()-setups, "session holds the preload's video+audio")
	require.Len(t, cons.Senders, 1)
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))
}

func TestPreloadOwnsTheDial(t *testing.T) {
	registerTestRTSPHandler()

	cam := newFakeCamera(t)
	cam.reject.Store(true)

	stream, err := New("preload_owns", cam.URL())
	require.NoError(t, err)

	require.NoError(t, AddPreload("preload_owns", "video"))
	t.Cleanup(func() { _ = DelPreload("preload_owns") })

	// camera still down: the client gets the preload's error and the camera
	// sees exactly one dial (the preload's), not a second one from the client
	dials := cam.dialCount.Load()
	err = stream.AddConsumer(newProbeConsumer())
	require.ErrorContains(t, err, "preload")
	require.Equal(t, int32(1), cam.dialCount.Load()-dials)
	require.Empty(t, streamConsumers(stream))
}

func TestEnsurePreloadDoesNotWaitForRunningAttach(t *testing.T) {
	stream := NewStream(nil)
	p := &Preload{name: "busy_preload", stream: stream, stop: make(chan struct{})}
	preloadsMu.Lock()
	preloads[p.name] = p
	preloadsMu.Unlock()
	t.Cleanup(func() {
		preloadsMu.Lock()
		delete(preloads, p.name)
		preloadsMu.Unlock()
	})

	p.attachMu.Lock()
	defer p.attachMu.Unlock()

	done := make(chan error, 1)
	go func() { done <- ensurePreload(stream, newProbeConsumer()) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("ensurePreload must not block on an attach in progress")
	}
}

func preloadAudioPackets(p *Preload) int {
	p.mu.Lock()
	cons := p.cons
	p.mu.Unlock()
	if cons == nil {
		return 0
	}
	for _, sender := range cons.Senders {
		if sender.Codec != nil && core.GetKind(sender.Codec.Name) == core.KindAudio {
			_, packets, _ := sender.Stats()
			return packets
		}
	}
	return -1
}

// A camera that starts advertising audio mid-life (microphone switched on)
// must reach the preload without a restart.
func TestPreloadWidensWhenAudioAppears(t *testing.T) {
	registerTestRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)
	cam.noAudio.Store(true)

	stream, err := New("preload_widen", cam.URL())
	require.NoError(t, err)
	require.NoError(t, AddPreload("preload_widen", "video&audio"))
	t.Cleanup(func() { _ = DelPreload("preload_widen") })

	p := GetPreload("preload_widen")
	waitForCodecs(t, p, core.CodecH264)
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }), "camera without audio serves video only")

	// microphone switched on: the camera session comes back offering audio
	cam.noAudio.Store(false)
	cam.dropConns()

	require.True(t, waitUntil(30*time.Second, func() bool { return preloadAudioPackets(p) > 0 }),
		"the preload must widen to audio and the track must carry packets")
	require.True(t, p.Attached())

	// the widened state is stable: no further re-attach happens
	p.mu.Lock()
	cons := p.cons
	p.mu.Unlock()
	time.Sleep(preloadCheckInterval + time.Second)
	p.mu.Lock()
	same := p.cons == cons
	p.mu.Unlock()
	require.True(t, same, "the supervisor must settle once fully served")
}

// A camera that never offers audio must not make the preload re-negotiate.
func TestSilentCameraDoesNotChurnThePreload(t *testing.T) {
	registerTestRTSPHandler()

	cam := newFakeCamera(t)
	cam.noAudio.Store(true)

	stream, err := New("preload_silent", cam.URL())
	require.NoError(t, err)
	require.NoError(t, AddPreload("preload_silent", "video&audio"))
	t.Cleanup(func() { _ = DelPreload("preload_silent") })

	p := GetPreload("preload_silent")
	require.True(t, waitUntil(10*time.Second, func() bool { return p.Attached() && receiverActive(stream) }))

	p.mu.Lock()
	cons := p.cons
	p.mu.Unlock()
	dials := cam.dialCount.Load()

	time.Sleep(preloadCheckInterval + 3*time.Second)

	p.mu.Lock()
	same := p.cons == cons
	p.mu.Unlock()
	require.True(t, same, "no re-attach for a kind nobody offers")
	require.Equal(t, dials, cam.dialCount.Load(), "no new camera sessions")
	require.True(t, p.Attached())
}

// codecNames of the producer's current receivers.
func codecNames(s *Stream) []string {
	s.mu.Lock()
	producers := append([]*Producer(nil), s.producers...)
	s.mu.Unlock()

	var names []string
	for _, prod := range producers {
		prod.mu.RLock()
		for _, recv := range prod.receivers {
			names = append(names, recv.Codec.Name)
		}
		prod.mu.RUnlock()
	}
	return names
}

// preloadCodecs reads the preload's current consumer codecs under its lock.
func preloadCodecs(p *Preload) []string {
	p.mu.Lock()
	cons := p.cons
	p.mu.Unlock()
	if cons == nil {
		return nil
	}
	return senderCodecs(cons)
}

// waitForCodecs waits until the preload serves exactly these codecs. Order is
// not compared: the query is a map, so the tracks come out in any order. The
// first attach can also come back with fewer tracks (a SETUP racing the
// camera) and gets widened by the supervisor, so never judge one snapshot.
func waitForCodecs(t *testing.T, p *Preload, codecs ...string) {
	t.Helper()
	want := slices.Sorted(slices.Values(codecs))
	require.True(t, waitUntil(30*time.Second, func() bool {
		return slices.Equal(slices.Sorted(slices.Values(preloadCodecs(p))), want)
	}), "preload must serve %v, got %v", want, preloadCodecs(p))
}

// preloadSender returns the preload's sender for that codec, or nil.
func preloadSender(p *Preload, codec string) *core.Sender {
	p.mu.Lock()
	cons := p.cons
	p.mu.Unlock()
	if cons == nil {
		return nil
	}
	for _, sender := range cons.Senders {
		if sender.Codec != nil && sender.Codec.Name == codec {
			return sender
		}
	}
	return nil
}

func senderCodecs(cons *probe.Probe) []string {
	var names []string
	for _, sender := range cons.Senders {
		names = append(names, sender.Codec.Name)
	}
	return names
}

// A camera whose video codec is reconfigured (h264 -> h265) must not leave
// its consumers parked on the old track: that track can never be served
// again, so the preload has to notice and re-negotiate the new codec.
func TestPreloadHealsAfterCodecChange(t *testing.T) {
	registerTestRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)

	stream, err := New("preload_codec_change", cam.URL())
	require.NoError(t, err)

	require.NoError(t, AddPreload("preload_codec_change", "video&audio"))
	t.Cleanup(func() { _ = DelPreload("preload_codec_change") })

	p := GetPreload("preload_codec_change")
	waitForCodecs(t, p, core.CodecH264, core.CodecPCMU)
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))

	// camera is switched to h265 and drops the session
	dials := cam.dialCount.Load()
	cam.h265.Store(true)
	cam.dropConns()
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > dials }))

	// the producer must follow the camera...
	require.True(t, waitUntil(30*time.Second, func() bool {
		return slices.Contains(codecNames(stream), core.CodecH265)
	}), "producer must pick up the new codec")
	require.NotContains(t, codecNames(stream), core.CodecH264, "the h264 track must not linger")

	// ...and the preload must end up serving it, not a dead h264 track
	require.True(t, waitUntil(30*time.Second, func() bool {
		return slices.Contains(preloadCodecs(p), core.CodecH265)
	}), "preload must re-negotiate the new codec")
	require.True(t, p.Attached())
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))
}

// Same reconfiguration on a video-only stream: no track can be moved at all,
// which must not be mistaken for a wedged camera and retried forever.
func TestReconnectSwapsWhenEveryTrackIsStale(t *testing.T) {
	registerTestRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)
	cam.noAudio.Store(true)

	stream, err := New("codec_change_video_only", cam.URL())
	require.NoError(t, err)

	cons := newProbeConsumer()
	require.NoError(t, stream.AddConsumer(cons))
	t.Cleanup(func() { stream.RemoveConsumer(cons) })
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))

	dials := cam.dialCount.Load()
	cam.h265.Store(true)
	cam.dropConns()
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > dials }))

	// the stale h264 track is released instead of parked forever
	require.True(t, waitUntil(30*time.Second, func() bool { return !cons.IsActive() }), "stale track must be released")
	require.Empty(t, codecNames(stream))

	// and the camera is not hammered by a reconnect loop that can never match
	dials = cam.dialCount.Load()
	time.Sleep(5 * time.Second)
	require.Equal(t, dials, cam.dialCount.Load(), "no reconnect loop after the swap")

	// a fresh consumer negotiates the codec the camera offers now
	next := newProbeConsumer()
	require.NoError(t, stream.AddConsumer(next))
	t.Cleanup(func() { stream.RemoveConsumer(next) })
	require.Equal(t, []string{core.CodecH265}, senderCodecs(next))
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))
}

// The stale-track rule is about media kind, not about video: an audio codec
// swapped on the camera (PCMU -> AAC) has to be dropped and re-negotiated
// the same way, while the untouched video track keeps serving its consumers.
func TestPreloadHealsAfterAudioCodecChange(t *testing.T) {
	registerTestRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)

	stream, err := New("preload_audio_codec", cam.URL())
	require.NoError(t, err)

	require.NoError(t, AddPreload("preload_audio_codec", "video&audio"))
	t.Cleanup(func() { _ = DelPreload("preload_audio_codec") })

	p := GetPreload("preload_audio_codec")
	waitForCodecs(t, p, core.CodecH264, core.CodecPCMU)
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))

	// a video-only client that must survive the audio reconfiguration
	videoOnly := newProbeConsumer()
	require.NoError(t, stream.AddConsumer(videoOnly))
	t.Cleanup(func() { stream.RemoveConsumer(videoOnly) })

	// camera keeps its video codec but switches audio to AAC
	dials := cam.dialCount.Load()
	cam.aac.Store(true)
	cam.dropConns()
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > dials }))

	require.True(t, waitUntil(30*time.Second, func() bool {
		return slices.Contains(codecNames(stream), core.CodecAAC)
	}), "producer must pick up the new audio codec")
	require.NotContains(t, codecNames(stream), core.CodecPCMU, "the pcmu track must not linger")
	require.Contains(t, codecNames(stream), core.CodecH264, "video is untouched by an audio change")

	require.True(t, waitUntil(30*time.Second, func() bool {
		return slices.Contains(preloadCodecs(p), core.CodecAAC)
	}), "preload must re-negotiate the new audio codec")

	// the video-only consumer never lost its track
	require.True(t, videoOnly.IsActive(), "an unrelated video consumer must not be disturbed")
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))
}
