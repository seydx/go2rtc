package streams

import (
	"sync/atomic"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

func boundTo(s *Stream, cons core.Consumer) ([]*core.Receiver, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tracks, ok := s.bound[cons]
	return tracks, ok
}

func evictHooked(s *Stream, cons core.Consumer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.evictHooks[cons]
	return ok
}

// A renegotiation (WebRTC, ex. audio enabled later) attaches the consumer to
// more tracks. It stays registered once, bound to the tracks of both.
func TestRenegotiatedMediasAddToTheConsumer(t *testing.T) {
	registerTestRTSPHandler()

	cam := newFakeCamera(t)
	stream, err := New("renegotiate_adds", cam.URL())
	require.NoError(t, err)

	cons := queryProbe(t, "video")
	stream.OnEvict(cons, func() {})
	require.NoError(t, stream.AddConsumer(cons))
	t.Cleanup(func() { stream.RemoveConsumer(cons) })

	require.NoError(t, stream.AddConsumerMedias(cons, queryProbe(t, "audio").GetMedias()))

	require.Len(t, streamConsumers(stream), 1)
	tracks, _ := boundTo(stream, cons)
	require.Len(t, tracks, 2, "bound to the video and the audio")
	require.True(t, evictHooked(stream, cons))
}

// A renegotiation the stream can't serve leaves the consumer as it was: it
// keeps watching, and the stream can still evict it.
func TestFailedRenegotiationKeepsTheConsumer(t *testing.T) {
	registerTestRTSPHandler()

	cam := newFakeCamera(t)
	cam.noAudio.Store(true)
	stream, err := New("renegotiate_fails", cam.URL())
	require.NoError(t, err)

	cons := queryProbe(t, "video")
	stream.OnEvict(cons, func() {})
	require.NoError(t, stream.AddConsumer(cons))
	t.Cleanup(func() { stream.RemoveConsumer(cons) })

	require.Error(t, stream.AddConsumerMedias(cons, queryProbe(t, "audio").GetMedias()))

	require.True(t, registered(stream, cons))
	tracks, _ := boundTo(stream, cons)
	require.Len(t, tracks, 1)
	require.True(t, evictHooked(stream, cons), "the stream can still evict it")
}

// A consumer removed while it renegotiated was stopped before it got the new
// tracks. They must not keep the camera running for nobody.
func TestRenegotiationOfRemovedConsumer(t *testing.T) {
	registerTestRTSPHandler()

	cam := newFakeCamera(t)
	stream, err := New("renegotiate_removed", cam.URL())
	require.NoError(t, err)

	var evicted atomic.Bool
	cons := queryProbe(t, "video")
	stream.OnEvict(cons, func() { evicted.Store(true) })
	require.NoError(t, stream.AddConsumer(cons))
	stream.RemoveConsumer(cons)

	require.ErrorIs(t, stream.AddConsumerMedias(cons, queryProbe(t, "audio").GetMedias()), errConsumerGone)

	require.False(t, registered(stream, cons))
	_, bound := boundTo(stream, cons)
	require.False(t, bound)
	for _, prod := range streamProducers(stream) {
		require.False(t, prod.hasReaders(), "nobody reads the camera")
	}
	require.False(t, evicted.Load())
}
