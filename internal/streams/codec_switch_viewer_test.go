package streams

import (
	"errors"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/AlexxIT/go2rtc/pkg/probe"
	"github.com/stretchr/testify/require"
)

func consumerServed(cons *mp4.Consumer) bool {
	if len(cons.Senders) == 0 {
		return false
	}
	for _, sender := range cons.Senders {
		if sender.State() == "closed" || !sender.Attached() {
			return false
		}
	}
	return true
}

func registered(s *Stream, cons core.Consumer) bool {
	return slices.Contains(streamConsumers(s), cons)
}

func queryProbe(t *testing.T, query string) *probe.Probe {
	t.Helper()
	cons := probe.Create("test", mustQuery(t, query))
	return cons
}

// A viewer that is already watching when the camera switches h264 -> h265
// negotiated a codec that no longer exists (MSE init segment). The stream
// evicts it and its handler's hook ends the connection, so the client
// reconnects and negotiates h265.
func TestViewerOnReconfiguredCameraIsEvicted(t *testing.T) {
	registerTestRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)
	cam.noAudio.Store(true)

	stream, err := New("viewer_codec_change", cam.URL())
	require.NoError(t, err)

	var evicted atomic.Bool
	cons := mp4.NewConsumer(nil)
	stream.OnEvict(cons, func() { evicted.Store(true) })
	require.NoError(t, stream.AddConsumer(cons))
	t.Cleanup(func() { stream.RemoveConsumer(cons) })
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))
	require.True(t, consumerServed(cons), "viewer starts with a live h264 track")

	dials := cam.dialCount.Load()
	cam.h265.Store(true)
	cam.dropConns()
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > dials }))

	require.True(t, waitUntil(30*time.Second, evicted.Load), "the handler must be told to end the connection")
	require.False(t, registered(stream, cons), "the viewer is no longer registered on the stream")
	require.False(t, consumerServed(cons))

	// the client reconnects and gets the new codec
	next := mp4.NewConsumer(nil)
	require.NoError(t, stream.AddConsumer(next))
	t.Cleanup(func() { stream.RemoveConsumer(next) })
	require.Equal(t, core.CodecH265, next.Senders[0].Codec.Name)
}

// An audio codec change evicts the consumers that took audio. A consumer that
// only takes video keeps its (moved) track and is left alone.
func TestAudioCodecChangeEvictsOnlyAudioConsumers(t *testing.T) {
	registerTestRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)

	stream, err := New("audio_codec_evict", cam.URL())
	require.NoError(t, err)

	var withAudioEvicted, videoOnlyEvicted atomic.Bool

	withAudio := queryProbe(t, "video&audio")
	stream.OnEvict(withAudio, func() { withAudioEvicted.Store(true) })
	require.NoError(t, stream.AddConsumer(withAudio))
	t.Cleanup(func() { stream.RemoveConsumer(withAudio) })

	videoOnly := queryProbe(t, "video")
	stream.OnEvict(videoOnly, func() { videoOnlyEvicted.Store(true) })
	require.NoError(t, stream.AddConsumer(videoOnly))
	t.Cleanup(func() { stream.RemoveConsumer(videoOnly) })

	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))

	dials := cam.dialCount.Load()
	cam.aac.Store(true)
	cam.dropConns()
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > dials }))

	require.True(t, waitUntil(30*time.Second, withAudioEvicted.Load), "the consumer with the stale audio track must be evicted")
	require.False(t, registered(stream, withAudio))

	require.False(t, videoOnlyEvicted.Load(), "a video-only consumer is not affected by an audio codec change")
	require.True(t, registered(stream, videoOnly))
	require.True(t, videoOnly.IsActive(), "the video-only consumer keeps its moved track")
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))
}

// Reconnects that keep the codecs must never evict anyone: the same codec
// moves the tracks, a camera that comes back without audio parks the audio.
func TestReconnectWithoutCodecChangeEvictsNobody(t *testing.T) {
	cases := []struct {
		name   string
		change func(cam *fakeCamera)
	}{
		{name: "same codecs", change: func(*fakeCamera) {}},
		{name: "audio gone", change: func(cam *fakeCamera) { cam.noAudio.Store(true) }},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registerTestRTSPHandler()
			speedUpWatchdog(t)

			cam := newFakeCamera(t)
			stream, err := New("no_evict_"+string(rune('a'+i)), cam.URL())
			require.NoError(t, err)

			var evicted atomic.Bool
			cons := queryProbe(t, "video&audio")
			stream.OnEvict(cons, func() { evicted.Store(true) })
			require.NoError(t, stream.AddConsumer(cons))
			t.Cleanup(func() { stream.RemoveConsumer(cons) })
			require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))

			dials := cam.dialCount.Load()
			tc.change(cam)
			cam.dropConns()
			require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > dials }))
			require.True(t, waitUntil(30*time.Second, func() bool { return receiverActive(stream) }), "video recovers")

			time.Sleep(time.Second)
			require.False(t, evicted.Load(), "nobody is evicted when the codecs did not change")
			require.True(t, registered(stream, cons))
			require.True(t, cons.IsActive())
		})
	}
}

// A consumer removed by its own handler (mse/stop, client close) is not an
// eviction: the hook must not fire, the handler keeps its connection.
func TestRemovedConsumerDoesNotFireEvictHook(t *testing.T) {
	registerTestRTSPHandler()

	cam := newFakeCamera(t)
	stream, err := New("no_evict_on_remove", cam.URL())
	require.NoError(t, err)

	var evicted atomic.Bool
	cons := queryProbe(t, "video")
	stream.OnEvict(cons, func() { evicted.Store(true) })
	require.NoError(t, stream.AddConsumer(cons))

	stream.RemoveConsumer(cons)
	require.False(t, evicted.Load())

	// its bookkeeping is gone with it
	stream.mu.Lock()
	_, hooked := stream.evictHooks[cons]
	_, bound := stream.bound[cons]
	stream.mu.Unlock()
	require.False(t, hooked)
	require.False(t, bound)
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	query, err := url.ParseQuery(raw)
	require.NoError(t, err)
	return query
}

type stoppableStub struct{ stubProducer }

func (*stoppableStub) Stop() error { return nil }

// A reconnect can drop a track in the moment a consumer attaches to it. The
// consumer must not be registered on a track nothing will ever feed: the
// attach fails, so the client retries against the new codec.
func TestAttachToDroppedTrackFails(t *testing.T) {
	video := &core.Media{
		Kind:      core.KindVideo,
		Direction: core.DirectionRecvonly,
		Codecs:    []*core.Codec{{Name: core.CodecH264, ClockRate: 90000}},
	}
	dropped := core.NewReceiver(video, video.Codecs[0])
	dropped.Close() // dropped by the reconnect after GetTrack handed it out

	prod := NewProducer("stub://camera")
	prod.conn = &stoppableStub{stubProducer{medias: []*core.Media{video}}}
	prod.state = stateTracks
	prod.receivers = []*core.Receiver{dropped}

	stream := &Stream{producers: []*Producer{prod}}
	prod.stream = stream

	var evicted atomic.Bool
	cons := queryProbe(t, "video")
	stream.OnEvict(cons, func() { evicted.Store(true) })

	require.ErrorIs(t, stream.AddConsumer(cons), errTrackGone)
	require.False(t, registered(stream, cons))
	require.False(t, cons.IsActive())

	stream.mu.Lock()
	_, hooked := stream.evictHooks[cons]
	stream.mu.Unlock()
	require.False(t, hooked, "the hook of a failed attach must not linger")
}

// loopCompanion is an exec companion like `ffmpeg:<stream>#audio=opus#requirePrevAudio`:
// it reads the camera audio of its own stream through a loopback consumer and
// ends when that input closes, the way ffmpeg exits on EOF.
type loopCompanion struct {
	core.Connection
	loop    *probe.Probe
	stream  *Stream
	stopped chan struct{}
	once    sync.Once
}

func (c *loopCompanion) Start() error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopped:
			return nil
		case <-ticker.C:
			if !c.loop.IsActive() {
				return errors.New("loopback input closed")
			}
			if len(c.Receivers) > 0 {
				c.Receivers[0].Input(&core.Packet{Payload: []byte("opus-frame")})
			}
		}
	}
}

func (c *loopCompanion) Stop() error {
	c.once.Do(func() { close(c.stopped) })
	// ffmpeg exits and the RTSP server removes the loopback consumer from its
	// own goroutine once the TCP connection closes — never synchronously from
	// within Stop, which producers call while holding their lock
	go c.stream.RemoveConsumer(c.loop)
	return c.Connection.Stop()
}

// An audio codec change evicts the companion's loopback consumer, which ends
// the companion. It must restart once against the new camera audio, keep its
// own viewers (its opus output did not change) and not loop.
func TestCompanionRestartsOnceAfterAudioCodecChange(t *testing.T) {
	registerTestRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)

	var dials atomic.Int32
	HandleFunc("loopcomp", func(string) (core.Producer, error) {
		s := Get("companion_loop")
		loop := queryProbe(t, "audio")
		loop.Source = "loopcomp://x" // like the RTSP loopback: never read the companion itself
		if err := s.AddConsumer(loop); err != nil {
			return nil, err
		}
		c := &loopCompanion{loop: loop, stream: s, stopped: make(chan struct{})}
		c.Medias = []*core.Media{{
			Kind:      core.KindAudio,
			Direction: core.DirectionRecvonly,
			Codecs:    []*core.Codec{{Name: core.CodecOpus, ClockRate: 48000, Channels: 2}},
		}}
		dials.Add(1)
		return c, nil
	})

	stream, err := New("companion_loop", cam.URL(), "loopcomp://x#noVideo#noBackchannel#audio=opus#requirePrevAudio")
	require.NoError(t, err)

	var viewerEvicted atomic.Bool
	viewer := queryProbe(t, "audio=opus")
	stream.OnEvict(viewer, func() { viewerEvicted.Store(true) })
	require.NoError(t, stream.AddConsumer(viewer))
	t.Cleanup(func() { stream.RemoveConsumer(viewer) })
	require.True(t, waitUntil(10*time.Second, func() bool { return viewer.Bytes() > 0 }), "viewer gets the companion's audio")
	require.Equal(t, int32(1), dials.Load())

	camDials := cam.dialCount.Load()
	cam.aac.Store(true)
	cam.dropConns()
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > camDials }))

	require.True(t, waitUntil(30*time.Second, func() bool { return dials.Load() == 2 }), "the companion must restart against the new camera audio")

	before := viewer.Bytes()
	require.True(t, waitUntil(10*time.Second, func() bool { return viewer.Bytes() > before }), "the viewer keeps getting the companion's audio")
	require.False(t, viewerEvicted.Load(), "the companion's output codec did not change, its viewer stays")
	require.True(t, registered(stream, viewer))

	time.Sleep(3 * time.Second)
	require.Equal(t, int32(2), dials.Load(), "the companion restarts once, it must not loop")
}
