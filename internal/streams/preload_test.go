package streams

import (
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
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
	require.True(t, waitUntil(10*time.Second, func() bool { return p.Attached() && receiverActive(stream) }))
	require.Len(t, p.cons.Senders, 2, "video and audio negotiated")

	// camera reboots without audio: video is swapped, audio gets parked
	dials := cam.dialCount.Load()
	cam.noAudio.Store(true)
	cam.dropConns()
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > dials }))
	require.True(t, waitUntil(30*time.Second, func() bool { return receiverActive(stream) }), "video must recover")

	// the parked audio track keeps the preload's sender open instead of closing it
	require.True(t, p.Attached(), "partial reconnect must not close the preload's tracks")
	require.Equal(t, "connected", p.cons.Senders[1].State())

	// camera reboots with audio again: the parked track is re-negotiated
	setups := cam.setupCount.Load()
	dials = cam.dialCount.Load()
	cam.noAudio.Store(false)
	cam.dropConns()
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > dials }))
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.setupCount.Load() >= setups+2 }), "video and audio must both be set up again")
	require.True(t, waitUntil(30*time.Second, func() bool { return receiverActive(stream) }))
	require.True(t, p.Attached())
	require.Equal(t, "connected", p.cons.Senders[1].State())
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
	require.Empty(t, stream.consumers)
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
			return sender.Packets
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
	require.True(t, waitUntil(10*time.Second, func() bool { return p.Attached() && receiverActive(stream) }))
	require.Len(t, p.cons.Senders, 1, "camera without audio serves video only")

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
