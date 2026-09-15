package streams

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/probe"
	"github.com/AlexxIT/go2rtc/pkg/rtsp"
	"github.com/stretchr/testify/require"
)

func micProbe() *probe.Probe {
	query, _ := url.ParseQuery("video&audio&microphone")
	return probe.Create("probe", query)
}

// talkWired reports whether the stream's producer has a backchannel sender
// that actually writes to the camera (not just a mixer consumers talk into).
func talkWired(s *Stream) bool {
	for _, p := range streamProducers(s) {
		p.mu.RLock()
		conn := p.conn
		p.mu.RUnlock()
		if rc, ok := conn.(*rtsp.Conn); ok && len(rc.Senders) > 0 {
			return true
		}
	}
	return false
}

// zombieMixer: a mixer consumers talk into that no camera sender reads from.
func zombieMixer(s *Stream) bool {
	for _, p := range streamProducers(s) {
		p.mu.RLock()
		mixer := p.mixer
		conn := p.conn
		p.mu.RUnlock()
		if mixer == nil {
			continue
		}
		if rc, ok := conn.(*rtsp.Conn); !ok || len(rc.Senders) == 0 {
			return true
		}
	}
	return false
}

func streamProducers(s *Stream) []*Producer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Producer(nil), s.producers...)
}

// holdTalkSlot occupies the camera's single backchannel slot from a foreign
// session, like another client talking to the camera.
func holdTalkSlot(t *testing.T, cam *fakeCamera) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", cam.ln.Addr().String())
	require.NoError(t, err)
	_, _ = fmt.Fprintf(conn, "SETUP rtsp://%s/stream/trackID=5 RTSP/1.0\r\nCSeq: 1\r\nTransport: RTP/AVP/TCP;unicast;interleaved=10-11\r\n\r\n", cam.ln.Addr())
	require.True(t, waitUntil(3*time.Second, cam.talkSlotHeld))
	return conn
}

// Dahua/Amcrest have a single talk slot. Two preloads of the same camera
// that both ask for the microphone (camera.ui preloads main and sub stream
// this way) start in parallel and race for it. The loser used to get stuck
// in a session that could not start and reconnected without backoff: its
// video died and the camera got hammered with ~1000 sessions per second.
func TestMicPreloadsOfOneCameraDoNotStorm(t *testing.T) {
	// firmwares differ in how they refuse a second talk session: failing its
	// SETUP (documented for Dahua/Amcrest) or not offering the media at all
	for _, omit := range []bool{false, true} {
		t.Run(fmt.Sprintf("omitBusyMedia=%v", omit), func(t *testing.T) {
			testMicPreloadsOfOneCamera(t, omit)
		})
	}
}

func testMicPreloadsOfOneCamera(t *testing.T, omitBusyMedia bool) {
	registerBackchannelRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)
	cam.backchannel.Store(true)
	cam.bcOmitBusy.Store(omitBusyMedia)

	suffix := fmt.Sprintf("_%v", omitBusyMedia)
	main, err := New("bc_race_main"+suffix, cam.BackchannelURL())
	require.NoError(t, err)
	sub, err := New("bc_race_sub"+suffix, cam.BackchannelURL())
	require.NoError(t, err)

	done := make(chan struct{}, 2)
	for _, name := range []string{"bc_race_main" + suffix, "bc_race_sub" + suffix} {
		go func() {
			_ = AddPreload(name, "video&audio&microphone")
			done <- struct{}{}
		}()
		t.Cleanup(func() { _ = DelPreload(name) })
	}
	<-done
	<-done

	time.Sleep(4 * time.Second)

	require.Less(t, cam.dialCount.Load(), int32(20), "a busy talk slot must not turn into a reconnect storm")
	require.True(t, receiverActive(main), "the stream that lost the talk slot must still deliver video")
	require.True(t, receiverActive(sub), "the stream that won the talk slot must deliver video")
	require.NotEqual(t, talkWired(main), talkWired(sub), "exactly one stream can hold the camera's single talk slot")
	require.False(t, zombieMixer(main), "no mixer may be left that nothing sends to the camera")
	require.False(t, zombieMixer(sub), "no mixer may be left that nothing sends to the camera")
}

// A mic preload reconnects while the camera still holds its previous talk
// session (dropped without TEARDOWN, freed only after the session timeout).
func TestReconnectWithBusyTalkSlotDoesNotStorm(t *testing.T) {
	registerBackchannelRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)
	cam.backchannel.Store(true)
	cam.bcHoldDrop.Store(int64(3 * time.Second))

	stream, err := New("bc_reconnect_busy", cam.BackchannelURL())
	require.NoError(t, err)
	require.NoError(t, AddPreload("bc_reconnect_busy", "video&audio&microphone"))
	t.Cleanup(func() { _ = DelPreload("bc_reconnect_busy") })

	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) && talkWired(stream) }))

	dials := cam.dialCount.Load()
	cam.dropConns()

	time.Sleep(5 * time.Second)
	require.Less(t, cam.dialCount.Load()-dials, int32(20), "reconnecting against a busy talk slot must not storm the camera")
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }), "video must recover even while the talk slot is busy")

	// The slot was still held by the dropped session, so talk could not be
	// wired. The mixer keeps the preload's microphone and waits: it is not a
	// dead end. Once the camera released the slot, the next reconnect wires
	// talk again. (It is not forced earlier on purpose: a reconnect only for
	// talk would interrupt every viewer and the recording.)
	require.False(t, cam.talkSlotHeld(), "the camera released the dropped session's slot by now")
	cam.bcHoldDrop.Store(0)
	cam.dropConns()
	require.True(t, waitUntil(15*time.Second, func() bool { return talkWired(stream) && receiverActive(stream) }), "the next reconnect must wire talk again")
	require.False(t, zombieMixer(stream))
}

// A talk request that cannot get the slot must not leave a mixer behind:
// later microphone consumers would attach to it, report success and talk
// into nothing.
func TestMicConsumerWithBusySlotLeavesNoZombieMixer(t *testing.T) {
	registerBackchannelRTSPHandler()

	cam := newFakeCamera(t)
	cam.backchannel.Store(true)

	stream, err := New("bc_zombie", cam.BackchannelURL())
	require.NoError(t, err)

	foreign := holdTalkSlot(t, cam)

	cons := micProbe()
	require.NoError(t, stream.AddConsumer(cons), "video and audio still work without the talk slot")
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }), "a busy talk slot must not kill the stream")
	require.False(t, talkWired(stream))
	require.False(t, zombieMixer(stream), "no mixer may be left that nothing sends to the camera")

	// the other client stops talking: a new session gets the slot
	_ = foreign.Close()
	require.True(t, waitUntil(3*time.Second, func() bool { return !cam.talkSlotHeld() }))
	stream.RemoveConsumer(cons)

	again := micProbe()
	require.NoError(t, stream.AddConsumer(again))
	t.Cleanup(func() { stream.RemoveConsumer(again) })
	require.True(t, talkWired(stream), "once the slot is free, talk must work again")
}

// dyingProducer offers a video track, but its session ends the moment it
// starts: a camera that accepts SETUP and drops PLAY.
type dyingProducer struct {
	core.Connection
}

func (d *dyingProducer) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	receiver := core.NewReceiver(media, codec)
	d.Receivers = append(d.Receivers, receiver)
	return receiver, nil
}

func (d *dyingProducer) Start() error { return errors.New("session died") }

// Whatever makes a session die right after it started — a talk slot, a
// firmware bug — the producer must back off instead of reconnecting in a
// tight loop.
func TestWorkerBacksOffWhenSessionDiesAtOnce(t *testing.T) {
	var dials atomic.Int32
	HandleFunc("dying", func(string) (core.Producer, error) {
		dials.Add(1)
		return &dyingProducer{Connection: core.Connection{Medias: []*core.Media{{
			Kind:      core.KindVideo,
			Direction: core.DirectionRecvonly,
			Codecs:    []*core.Codec{{Name: core.CodecH264, ClockRate: 90000}},
		}}}}, nil
	})

	stream, err := New("dying_session", "dying://camera")
	require.NoError(t, err)

	cons := newProbeConsumer()
	require.NoError(t, stream.AddConsumer(cons))
	t.Cleanup(func() { stream.RemoveConsumer(cons) })

	time.Sleep(3 * time.Second)
	require.Less(t, dials.Load(), int32(10), "a session that dies at once must be retried with backoff")
}
