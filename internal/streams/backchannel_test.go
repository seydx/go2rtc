package streams

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/probe"
	"github.com/AlexxIT/go2rtc/pkg/rtsp"
	"github.com/pion/rtp"
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

func mixerCodec(s *Stream) *core.Codec {
	for _, p := range streamProducers(s) {
		p.mu.RLock()
		mixer := p.mixer
		p.mu.RUnlock()
		if mixer != nil {
			return mixer.Codec
		}
	}
	return nil
}

// A camera offers several talk codecs. The one it encodes its own audio in
// is the natural pick: a talker in that codec passes through the mixer
// without ffmpeg. Wildcard microphones (the preload asks for ANY) used to
// take the first offered codec instead — AAC 48 kHz on an Amcrest, which
// forced a transcode for every talker.
func TestTalkCodecFollowsCameraAudio(t *testing.T) {
	registerBackchannelRTSPHandler()

	cases := []struct {
		name    string
		aac     bool // camera audio is AAC/16000, which the talk media doesn't offer
		noAudio bool
		want    string
		rate    uint32
	}{
		{name: "camera audio offered for talk", want: core.CodecPCMU, rate: 8000},
		{name: "camera audio not offered for talk", aac: true, want: core.CodecAAC, rate: 48000},
		{name: "camera without audio", noAudio: true, want: core.CodecAAC, rate: 48000},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cam := newFakeCamera(t)
			cam.backchannel.Store(true)
			cam.aac.Store(tc.aac)
			cam.noAudio.Store(tc.noAudio)

			stream, err := New(fmt.Sprintf("talk_codec_%d", i), cam.BackchannelURL())
			require.NoError(t, err)

			cons := micProbe()
			require.NoError(t, stream.AddConsumer(cons))
			t.Cleanup(func() { stream.RemoveConsumer(cons) })

			require.True(t, talkWired(stream))
			codec := mixerCodec(stream)
			require.NotNil(t, codec)
			require.Equal(t, tc.want, codec.Name)
			require.Equal(t, tc.rate, codec.ClockRate)
		})
	}
}

// watcherProbe takes video and audio but can't talk, like a recorder or an
// MSE tile.
func watcherProbe() *probe.Probe {
	query, _ := url.ParseQuery("video&audio")
	return probe.Create("probe", query)
}

// talkSettled: no producer waits for a first talk packet anymore. Read under
// the producer lock, so talkWired can look at the conn's senders afterwards.
func talkSettled(s *Stream) bool {
	for _, p := range streamProducers(s) {
		p.mu.RLock()
		pending := p.talkPending
		p.mu.RUnlock()
		if pending {
			return false
		}
	}
	return true
}

// talk sends one packet into the microphone of a consumer.
func talk(cons *probe.Probe) {
	for _, receiver := range cons.Receivers {
		if receiver.Codec.IsAudio() || receiver.Codec.Name == core.CodecAny {
			receiver.Input(&rtp.Packet{Header: rtp.Header{Version: 2}, Payload: make([]byte, 160)})
		}
	}
}

// Without a preload nothing sets talk up before PLAY. A viewer that could talk
// must not re-dial the session the recording and every other viewer share
// just by joining: the talk track is set up once somebody talks.
func TestTalkCapableViewerDoesNotRedialPlayingCamera(t *testing.T) {
	registerBackchannelRTSPHandler()

	cam := newFakeCamera(t)
	cam.backchannel.Store(true)

	stream, err := New("talk_on_demand", cam.BackchannelURL())
	require.NoError(t, err)

	recorder := watcherProbe()
	require.NoError(t, stream.AddConsumer(recorder))
	t.Cleanup(func() { stream.RemoveConsumer(recorder) })
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))
	dials, plays := cam.dialCount.Load(), cam.plays.Load()

	viewer := micProbe()
	require.NoError(t, stream.AddConsumer(viewer))
	t.Cleanup(func() { stream.RemoveConsumer(viewer) })
	require.Equal(t, dials, cam.dialCount.Load(), "joining must not re-dial the camera")
	require.Equal(t, plays, cam.plays.Load())
	require.False(t, talkWired(stream))

	talk(viewer)
	require.True(t, waitUntil(10*time.Second, func() bool { return talkSettled(stream) }))
	require.True(t, talkWired(stream), "talking sets up the talk track")
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }), "video goes on")
	require.False(t, zombieMixer(stream))

	// a second viewer joins a session that already has talk
	second := micProbe()
	dials = cam.dialCount.Load()
	require.NoError(t, stream.AddConsumer(second))
	t.Cleanup(func() { stream.RemoveConsumer(second) })
	talk(second)
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, dials, cam.dialCount.Load(), "talk is set up once per session")
}

// The talk slot is taken when the viewer starts talking. No mixer may be left
// that nothing sends to the camera, and talking on must not re-dial it: the
// session goes on without talk, like a mic preload that lost the slot.
func TestTalkOnBusySlotLeavesNoZombieMixer(t *testing.T) {
	registerBackchannelRTSPHandler()

	cam := newFakeCamera(t)
	cam.backchannel.Store(true)

	stream, err := New("talk_on_demand_busy", cam.BackchannelURL())
	require.NoError(t, err)

	recorder := watcherProbe()
	require.NoError(t, stream.AddConsumer(recorder))
	t.Cleanup(func() { stream.RemoveConsumer(recorder) })
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))

	viewer := micProbe()
	require.NoError(t, stream.AddConsumer(viewer))
	t.Cleanup(func() { stream.RemoveConsumer(viewer) })

	foreign := holdTalkSlot(t, cam)
	t.Cleanup(func() { _ = foreign.Close() })

	talk(viewer)
	require.True(t, waitUntil(10*time.Second, func() bool { return talkSettled(stream) }))
	require.Positive(t, cam.bcSetupFails.Load())
	require.False(t, zombieMixer(stream), "no mixer may be left that nothing sends to the camera")
	require.True(t, waitUntil(15*time.Second, func() bool { return receiverActive(stream) }), "video goes on")

	dials := cam.dialCount.Load()
	talk(viewer)
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, dials, cam.dialCount.Load(), "talking on must not re-dial the camera")
	require.False(t, zombieMixer(stream))
}

type slowTalkConn struct {
	core.Connection
	entered chan struct{}
	release chan struct{}
}

func (c *slowTalkConn) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	return core.NewReceiver(media, codec), nil
}

func (c *slowTalkConn) AddTrack(*core.Media, *core.Codec, *core.Receiver) error {
	close(c.entered)
	<-c.release
	return nil
}

func (c *slowTalkConn) Start() error { select {} }

// A viewer asks for a track the session hasn't set up while the first talk
// still re-dials the camera. Waiting for its turn must not hold the producer
// lock: status, stats and every other attach read it meanwhile.
func TestTrackRequestDuringTalkSetupDoesNotHang(t *testing.T) {
	pcmu := &core.Codec{Name: core.CodecPCMU, ClockRate: 8000}
	talkMedia := &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{pcmu}}
	videoMedia := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{{Name: core.CodecH264, ClockRate: 90000}}}

	conn := &slowTalkConn{
		Connection: core.Connection{Medias: []*core.Media{videoMedia, talkMedia}},
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	release := sync.OnceFunc(func() { close(conn.release) })
	t.Cleanup(release)
	p := &Producer{conn: conn, state: stateStart, mixingEnabled: true, url: "slowtalk://cam"}

	mic := core.NewReceiver(talkMedia, pcmu)
	require.NoError(t, p.AddTrack(talkMedia, pcmu, mic))

	mic.Input(&rtp.Packet{Header: rtp.Header{Version: 2}, Payload: make([]byte, 160)})
	select {
	case <-conn.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("talking must set up the talk track")
	}

	got := make(chan struct{})
	go func() {
		_, _ = p.GetTrack(videoMedia, videoMedia.Codecs[0])
		close(got)
	}()
	time.Sleep(100 * time.Millisecond)

	read := make(chan struct{})
	go func() {
		_ = p.State()
		close(read)
	}()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("the producer lock is held while GetTrack waits for the talk setup")
	}

	release()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("GetTrack hangs behind the talk setup")
	}
	require.True(t, waitUntil(time.Second, func() bool {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return !p.talkPending && p.mixer != nil
	}), "talk is set up")
}

// The camera drops the session while a viewer could talk but hasn't yet. The
// reconnect re-dials anyway and sets talk up on the way: the first talk packet
// afterwards must not set it up a second time, that would re-dial again.
func TestReconnectWhileTalkPendingSetsTalkUpOnce(t *testing.T) {
	registerBackchannelRTSPHandler()

	cam := newFakeCamera(t)
	cam.backchannel.Store(true)

	stream, err := New("talk_pending_reconnect", cam.BackchannelURL())
	require.NoError(t, err)

	recorder := watcherProbe()
	require.NoError(t, stream.AddConsumer(recorder))
	t.Cleanup(func() { stream.RemoveConsumer(recorder) })
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))

	viewer := micProbe()
	require.NoError(t, stream.AddConsumer(viewer))
	t.Cleanup(func() { stream.RemoveConsumer(viewer) })
	require.False(t, talkSettled(stream), "talk waits for the first packet")

	dials := cam.dialCount.Load()
	cam.dropConns()
	require.True(t, waitUntil(15*time.Second, func() bool {
		return cam.dialCount.Load() > dials && talkSettled(stream) && talkWired(stream) && receiverActive(stream)
	}), "the reconnect sets talk up")

	dials, setups := cam.dialCount.Load(), cam.bcSetups.Load()
	talk(viewer)
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, dials, cam.dialCount.Load(), "talking must not re-dial the camera")
	require.Equal(t, setups, cam.bcSetups.Load(), "talk is set up once")
	require.True(t, talkWired(stream))
	require.False(t, zombieMixer(stream))
}

// browserViewer is a WebRTC live view: its offer always carries a microphone.
func browserViewer() *probe.Probe {
	query, _ := url.ParseQuery("video=h264&audio=opus,pcmu,pcma&microphone=opus/48000/2,pcmu,pcma")
	return probe.Create("probe", query)
}

// countFFmpegStarts points the mixers at an ffmpeg that logs each start.
func countFFmpegStarts(t *testing.T) func() int {
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

	old := ffmpegBin
	ffmpegBin = wrapper
	t.Cleanup(func() { ffmpegBin = old })

	return func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "\n")
	}
}

// Every WebRTC viewer is a talk parent of the mixer. Tiles that only watch
// must not start or restart its ffmpeg as they come and go: it runs while
// viewers talk at once.
func TestWatchingViewersStartNoMixerFFmpeg(t *testing.T) {
	registerBackchannelRTSPHandler()
	starts := countFFmpegStarts(t)

	cam := newFakeCamera(t)
	cam.backchannel.Store(true)

	stream, err := New("mixer_watchers", cam.BackchannelURL())
	require.NoError(t, err)
	require.NoError(t, AddPreload("mixer_watchers", "video&audio&microphone"))
	t.Cleanup(func() { _ = DelPreload("mixer_watchers") })
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) && talkWired(stream) }))

	var viewers []*probe.Probe
	for range 4 {
		viewer := browserViewer()
		require.NoError(t, stream.AddConsumer(viewer))
		t.Cleanup(func() { stream.RemoveConsumer(viewer) })
		viewers = append(viewers, viewer)
		time.Sleep(100 * time.Millisecond)
	}
	stream.RemoveConsumer(viewers[3])
	time.Sleep(300 * time.Millisecond)
	require.Zero(t, starts(), "watching viewers start no ffmpeg")

	talk(viewers[0])
	talk(viewers[1])
	require.True(t, waitUntil(5*time.Second, func() bool { return starts() == 1 }), "two talkers are mixed")
}
