package streams

import (
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/probe"
	"github.com/stretchr/testify/require"
)

// fakeCompanionConn mirrors internal/ffmpeg.Producer exactly:
// core.Connection embed (default GetTrack creates placeholder receivers),
// Start feeds the placeholder as a relay child of the inner producer's
// track, Stop detaches the relay before the inner teardown.
type fakeCompanionConn struct {
	core.Connection

	feed    chan struct{}
	stopped atomic.Bool
	sent    atomic.Int32
	inner   *core.Receiver
	innerMu sync.Mutex
}

func newFakeCompanionConn() *fakeCompanionConn {
	c := &fakeCompanionConn{feed: make(chan struct{})}
	c.ID = core.NewID()
	c.FormatName = "fakecompanion"
	c.Medias = []*core.Media{{
		Kind:      core.KindAudio,
		Direction: core.DirectionRecvonly,
		Codecs:    []*core.Codec{{Name: core.CodecOpus, ClockRate: 48000}},
	}}
	return c
}

func (c *fakeCompanionConn) Start() error {
	inner := core.NewReceiver(c.Medias[0], c.Medias[0].Codecs[0])
	c.innerMu.Lock()
	c.inner = inner
	c.innerMu.Unlock()

	inner.AttachRelay(&c.Receivers[0].Node) // same as internal/ffmpeg Producer.Start

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.feed:
			return errors.New("companion inner died")
		case <-ticker.C:
			if c.stopped.Load() {
				return nil
			}
			c.sent.Add(1)
			inner.Input(&core.Packet{Payload: []byte("0123456789")})
		}
	}
}

func (c *fakeCompanionConn) Stop() error {
	c.stopped.Store(true)
	c.innerMu.Lock()
	inner := c.inner
	c.innerMu.Unlock()
	if inner != nil {
		inner.RemoveChild(&c.Receivers[0].Node) // detach the relay first, like the real Stop
		inner.Close()                           // exec teardown closes the inner conn's receivers
	}
	return nil
}

func (c *fakeCompanionConn) Interrupt() error {
	return c.Stop()
}

type companionLog struct {
	mu    sync.Mutex
	conns []*fakeCompanionConn
}

func registerCompanion(scheme string) *companionLog {
	l := &companionLog{}
	HandleFunc(scheme, func(string) (core.Producer, error) {
		c := newFakeCompanionConn()
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
		return c, nil
	})
	return l
}

func (l *companionLog) get(i int) *fakeCompanionConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i >= len(l.conns) {
		return nil
	}
	return l.conns[i]
}

func audioViewer(name string) *probe.Probe {
	query, _ := url.ParseQuery("audio")
	return probe.Create(name, query)
}

func viewerBytes(v *probe.Probe) int {
	return v.Send
}

// A second viewer joining while the companion runs must get audio too.
func TestCompanionSecondViewerGetsAudio(t *testing.T) {
	log := registerCompanion("fkcomp1")
	s := NewStream("fkcomp1:x")

	viewerA := audioViewer("viewerA")
	require.NoError(t, s.AddConsumer(viewerA))
	require.True(t, waitUntil(2*time.Second, func() bool { return viewerBytes(viewerA) > 0 }),
		"viewer A never got audio")

	viewerB := audioViewer("viewerB")
	require.NoError(t, s.AddConsumer(viewerB))

	require.True(t, waitUntil(2*time.Second, func() bool { return viewerBytes(viewerB) > 0 }),
		"viewer B got no audio while viewer A had %d bytes (companion conn0 sent=%d)",
		viewerBytes(viewerA), log.get(0).sent.Load())
}

// A failed consumer attach on the same stream must not tear down the
// companion under a viewer that is actively listening.
func TestFailedProbeDoesNotKillCompanionAudio(t *testing.T) {
	log := registerCompanion("fkcomp2")
	s := NewStream("fkcomp2:x")

	viewerA := audioViewer("viewerA")
	require.NoError(t, s.AddConsumer(viewerA))
	require.True(t, waitUntil(2*time.Second, func() bool { return viewerBytes(viewerA) > 0 }),
		"viewer A never got audio")

	videoProbe := probe.Create("videoProbe", func() url.Values { q, _ := url.ParseQuery("video"); return q }())
	require.Error(t, s.AddConsumer(videoProbe), "video-only attach on an audio stream should fail")

	require.False(t, log.get(0).stopped.Load(),
		"the failed attach stopped the companion although viewer A is listening")

	before := viewerBytes(viewerA)
	require.True(t, waitUntil(2*time.Second, func() bool { return viewerBytes(viewerA) > before }),
		"viewer A's audio died after the failed attach (sender state=%s attached=%v)",
		senderState(viewerA), viewerA.IsActive())
}

// When the companion's inner producer dies and the producer reconnects,
// an attached viewer must end up on the new run's track.
func TestCompanionReconnectKeepsViewer(t *testing.T) {
	log := registerCompanion("fkcomp3")
	s := NewStream("fkcomp3:x")

	viewerA := audioViewer("viewerA")
	require.NoError(t, s.AddConsumer(viewerA))
	require.True(t, waitUntil(2*time.Second, func() bool { return viewerBytes(viewerA) > 0 }),
		"viewer A never got audio")

	close(log.get(0).feed) // inner producer dies -> worker reconnects

	require.True(t, waitUntil(3*time.Second, func() bool {
		c := log.get(1)
		return c != nil && c.sent.Load() > 5
	}), "companion never reconnected")

	before := viewerBytes(viewerA)
	require.True(t, waitUntil(2*time.Second, func() bool { return viewerBytes(viewerA) > before }),
		"viewer A stayed silent after the companion reconnect (sender state=%s attached=%v)",
		senderState(viewerA), viewerA.IsActive())
}

// The last viewer's teardown cascades up the node graph; it must not rip
// the relay off the running companion, a viewer joining right after has to
// get audio.
func TestViewerCycleKeepsCompanionFeeding(t *testing.T) {
	registerCompanion("fkcomp4")
	s := NewStream("fkcomp4:x")

	viewerA := audioViewer("viewerA")
	require.NoError(t, s.AddConsumer(viewerA))
	require.True(t, waitUntil(2*time.Second, func() bool { return viewerBytes(viewerA) > 0 }),
		"viewer A never got audio")

	_ = viewerA.Stop() // sender close cascades before the streams layer reacts

	viewerB := audioViewer("viewerB")
	require.NoError(t, s.AddConsumer(viewerB))
	require.True(t, waitUntil(2*time.Second, func() bool { return viewerBytes(viewerB) > 0 }),
		"viewer B got no audio after viewer A left")
}

func senderState(p *probe.Probe) string {
	if len(p.Senders) == 0 {
		return "none"
	}
	return p.Senders[0].State()
}
