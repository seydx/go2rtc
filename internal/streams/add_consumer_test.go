package streams

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// A viewer waits for a camera that is slow to answer while camera.ui puts the
// stream again with one more source (unmuting adds the audio companion).
func TestSourcesGrowingDuringSlowDialDoNotCrash(t *testing.T) {
	HandleFunc("stubslowfail", func(string) (core.Producer, error) {
		time.Sleep(300 * time.Millisecond)
		return nil, errors.New("dial timeout")
	})

	const name = "sources_grow_during_dial"
	t.Cleanup(func() { Delete(name) })
	stream, err := New(name, "stubslowfail://cam")
	require.NoError(t, err)

	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		_ = stream.AddConsumer(micProbe())
	}()

	time.Sleep(50 * time.Millisecond)
	_, err = New(name, "stubslowfail://cam", "stubslowfail://companion")
	require.NoError(t, err)

	select {
	case r := <-done:
		require.Nil(t, r, "attaching must not panic")
	case <-time.After(10 * time.Second):
		t.Fatal("AddConsumer did not return")
	}
	require.Zero(t, stream.pending.Load(), "a finished attach must not count as pending")
}

type gatedConn struct {
	core.Connection
	stopped chan struct{}
	once    sync.Once
}

func (c *gatedConn) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	receiver := core.NewReceiver(media, codec)
	c.Receivers = append(c.Receivers, receiver)
	return receiver, nil
}

func (c *gatedConn) Start() error {
	<-c.stopped
	return nil
}

func (c *gatedConn) Stop() error {
	c.once.Do(func() { close(c.stopped) })
	return c.Connection.Stop()
}

// Consumers of a stream whose sources change reconnect against the new set.
// One that was still attaching must not slip in bound to the old set.
func TestAttachAcrossSourceChangeIsTurnedAway(t *testing.T) {
	release := make(chan struct{})
	var dials atomic.Int32
	var conns sync.Map
	HandleFunc("stubgated", func(url string) (core.Producer, error) {
		dials.Add(1)
		<-release
		conn := &gatedConn{
			Connection: core.Connection{Medias: []*core.Media{{
				Kind:      core.KindVideo,
				Direction: core.DirectionRecvonly,
				Codecs:    []*core.Codec{{Name: core.CodecH264, ClockRate: 90000}},
			}}},
			stopped: make(chan struct{}),
		}
		conns.Store(url, conn)
		return conn, nil
	})

	const name = "attach_across_source_change"
	t.Cleanup(func() { Delete(name) })
	stream, err := New(name, "stubgated://cam")
	require.NoError(t, err)

	cons := newProbeConsumer()
	result := make(chan error, 1)
	go func() { result <- stream.AddConsumer(cons) }()
	require.True(t, waitUntil(time.Second, func() bool { return dials.Load() == 1 }))

	_, err = New(name, "stubgated://cam", "stubgated://companion")
	require.NoError(t, err)
	close(release)

	select {
	case err = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("AddConsumer did not return")
	}
	require.ErrorIs(t, err, errSourcesChanged)
	require.Empty(t, streamConsumers(stream))
	require.False(t, cons.IsActive(), "the turned away consumer must not hold tracks")

	conn, ok := conns.Load("stubgated://cam")
	require.True(t, ok)
	require.True(t, waitUntil(time.Second, func() bool {
		select {
		case <-conn.(*gatedConn).stopped:
			return true
		default:
			return false
		}
	}), "a camera session nobody reads must be closed")
}
