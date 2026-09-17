package cui

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api/ws"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/probe"
	"github.com/stretchr/testify/require"
)

type fakeConn struct {
	core.Connection
	hangup chan struct{}
	once   sync.Once
}

func (c *fakeConn) Start() error {
	<-c.hangup
	return nil
}

func (c *fakeConn) Stop() error {
	c.drop()
	return c.Connection.Stop()
}

func (c *fakeConn) drop() {
	c.once.Do(func() { close(c.hangup) })
}

// fakeCamera answers dials with a single video media in its current codec.
type fakeCamera struct {
	codec   atomic.Value // string
	offline atomic.Bool
	conn    atomic.Pointer[fakeConn]
}

func newFakeCamera(scheme, codec string) *fakeCamera {
	cam := &fakeCamera{}
	cam.codec.Store(codec)
	streams.HandleFunc(scheme, func(string) (core.Producer, error) {
		if cam.offline.Load() {
			return nil, errors.New("camera offline")
		}
		conn := &fakeConn{
			Connection: core.Connection{
				FormatName: "fake",
				Medias: []*core.Media{{
					Kind:      core.KindVideo,
					Direction: core.DirectionRecvonly,
					Codecs:    []*core.Codec{{Name: cam.codec.Load().(string), ClockRate: 90000, PayloadType: core.PayloadTypeRAW}},
				}},
			},
			hangup: make(chan struct{}),
		}
		cam.conn.Store(conn)
		return conn, nil
	})
	return cam
}

type message struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// stateView decodes a stream state; producers stay raw, they only marshal
type stateView struct {
	Status    string                  `json:"status"`
	Error     string                  `json:"error"`
	Producers json.RawMessage         `json:"producers"`
	Consumers []streams.ConsumerState `json:"consumers"`
	Preload   *streams.PreloadState   `json:"preload"`
}

type streamValue struct {
	Name  string     `json:"name"`
	State *stateView `json:"state"`
}

// recorder is a ws.Transport whose writes can be held back, like a client that
// does not read.
type recorder struct {
	tr   *ws.Transport
	msgs chan message
	gate sync.RWMutex
}

func newRecorder() *recorder {
	r := &recorder{msgs: make(chan message, 1024)}
	r.tr = &ws.Transport{Request: httptest.NewRequest("GET", "/api/ws", nil)}
	r.tr.OnWrite(func(msg any) error {
		r.gate.RLock()
		defer r.gate.RUnlock()
		b, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		var m message
		if err = json.Unmarshal(b, &m); err != nil {
			return err
		}
		r.msgs <- m
		return nil
	})
	return r
}

func (r *recorder) next(t *testing.T, msgType string) message {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m := <-r.msgs:
			if m.Type == msgType {
				return m
			}
		case <-timeout:
			t.Fatalf("no %s message", msgType)
		}
	}
}

func (r *recorder) nextStream(t *testing.T, name string, cond func(*stateView) bool) *stateView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case m := <-r.msgs:
			if m.Type != "cui/stream" {
				continue
			}
			var v streamValue
			require.NoError(t, json.Unmarshal(m.Value, &v))
			if v.Name == name && cond(v.State) {
				return v.State
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("no matching cui/stream for %s", name)
	return nil
}

func subscribed(t *testing.T) *recorder {
	t.Helper()
	r := newRecorder()
	require.NoError(t, subscribe(r.tr, &ws.Message{Type: "cui/subscribe"}))
	t.Cleanup(r.tr.Close)
	return r
}

func newStream(t *testing.T, name, source string) *streams.Stream {
	t.Helper()
	stream, err := streams.New(name, source)
	require.NoError(t, err)
	t.Cleanup(func() { streams.Delete(name) })
	return stream
}

func attach(t *testing.T, stream *streams.Stream) *probe.Probe {
	t.Helper()
	cons := probe.Create("test", url.Values{"video": {""}})
	require.NoError(t, stream.AddConsumer(cons))
	return cons
}

func TestSnapshotListsOnlyCameraUIStreams(t *testing.T) {
	newFakeCamera("fakesnap", core.CodecH264)
	newStream(t, "cui_snap_main", "fakesnap://cam")
	newStream(t, "preview_random", "fakesnap://cam")

	r := subscribed(t)
	m := r.next(t, "cui/snapshot")

	var v struct {
		Streams map[string]*stateView `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(m.Value, &v))
	require.Contains(t, v.Streams, "cui_snap_main")
	require.NotContains(t, v.Streams, "preview_random")
	require.Equal(t, "idle", v.Streams["cui_snap_main"].Status)
	require.Nil(t, v.Streams["cui_snap_main"].Preload)
}

func TestConsumersAndStatusFollowTheStream(t *testing.T) {
	newFakeCamera("fakecons", core.CodecH264)
	stream := newStream(t, "cui_cons_main", "fakecons://cam")

	r := subscribed(t)
	r.next(t, "cui/snapshot")

	cons := attach(t, stream)
	state := r.nextStream(t, "cui_cons_main", func(s *stateView) bool { return len(s.Consumers) == 1 })
	require.Equal(t, "connected", state.Status)
	require.Equal(t, "test", state.Consumers[0].FormatName)

	stream.RemoveConsumer(cons)
	state = r.nextStream(t, "cui_cons_main", func(s *stateView) bool { return len(s.Consumers) == 0 })
	require.Equal(t, "idle", state.Status)
}

func TestBurstCollapsesIntoOneMessage(t *testing.T) {
	newFakeCamera("fakeburst", core.CodecH264)
	stream := newStream(t, "cui_burst_main", "fakeburst://cam")

	r := subscribed(t)
	r.next(t, "cui/snapshot")

	for range 3 {
		attach(t, stream)
	}
	state := r.nextStream(t, "cui_burst_main", func(*stateView) bool { return true })
	require.Len(t, state.Consumers, 3, "one message carries the whole burst")
}

func TestReconnectFailureAndCodecChange(t *testing.T) {
	cam := newFakeCamera("fakeswitch", core.CodecH264)
	stream := newStream(t, "cui_switch_main", "fakeswitch://cam")

	r := subscribed(t)
	r.next(t, "cui/snapshot")

	attach(t, stream)
	state := r.nextStream(t, "cui_switch_main", func(s *stateView) bool { return s.Status == "connected" })
	require.Contains(t, string(state.Producers), "H264")

	cam.offline.Store(true)
	cam.conn.Load().drop()
	state = r.nextStream(t, "cui_switch_main", func(s *stateView) bool { return s.Status == "error" })
	require.Equal(t, "camera offline", state.Error)

	// the camera comes back with another codec: the viewer that negotiated
	// h264 is evicted, the next one gets h265
	cam.codec.Store(core.CodecH265)
	cam.offline.Store(false)
	r.nextStream(t, "cui_switch_main", func(s *stateView) bool { return len(s.Consumers) == 0 })

	attach(t, stream)
	r.nextStream(t, "cui_switch_main", func(s *stateView) bool {
		return s.Status == "connected" && s.Error == "" && strings.Contains(string(s.Producers), "H265")
	})
}

func TestDeletedStreamIsNull(t *testing.T) {
	newFakeCamera("fakedel", core.CodecH264)
	_, err := streams.New("cui_del_main", "fakedel://cam")
	require.NoError(t, err)

	r := subscribed(t)
	r.next(t, "cui/snapshot")

	streams.Delete("cui_del_main")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-r.msgs:
			if m.Type == "cui/stream" && string(m.Value) == `{"name":"cui_del_main","state":null}` {
				return
			}
		case <-deadline:
			t.Fatal("deleted stream was not reported")
		}
	}
}

func TestCredentialsAreMasked(t *testing.T) {
	newFakeCamera("fakecreds", core.CodecH264)
	newStream(t, "cui_creds_main", "fakecreds://admin:topsecret@10.0.0.2/stream")

	r := subscribed(t)
	m := r.next(t, "cui/snapshot")
	require.NotContains(t, string(m.Value), "topsecret")
}

// A client that does not read must never hold up the stream: hooks only mark
// the stream, and once the client reads again it gets the latest state.
func TestStalledSubscriberDoesNotBlockHooks(t *testing.T) {
	newFakeCamera("fakestall", core.CodecH264)
	stream := newStream(t, "cui_stall_main", "fakestall://cam")

	r := subscribed(t)
	r.next(t, "cui/snapshot")

	r.gate.Lock()
	var consumers []*probe.Probe
	start := time.Now()
	for range 50 {
		consumers = append(consumers, attach(t, stream))
	}
	for _, cons := range consumers[:49] {
		stream.RemoveConsumer(cons)
	}
	require.Less(t, time.Since(start), 2*time.Second, "hooks must not wait for the client")
	time.Sleep(3 * flushDelay)
	r.gate.Unlock()

	state := r.nextStream(t, "cui_stall_main", func(s *stateView) bool { return len(s.Consumers) == 1 })
	require.Equal(t, "connected", state.Status)
}

func TestStatsCountTraffic(t *testing.T) {
	old := statsInterval
	statsInterval = 200 * time.Millisecond
	t.Cleanup(func() { statsInterval = old })

	newFakeCamera("fakestats", core.CodecH264)
	stream := newStream(t, "cui_stats_main", "fakestats://cam")
	attach(t, stream)

	r := subscribed(t)
	m := r.next(t, "cui/stats")

	var v struct {
		Streams map[string]struct {
			Producers []struct {
				Receivers []json.RawMessage `json:"receivers"`
			} `json:"producers"`
			Consumers []struct {
				Senders []json.RawMessage `json:"senders"`
			} `json:"consumers"`
		} `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(m.Value, &v))
	s := v.Streams["cui_stats_main"]
	require.Len(t, s.Producers, 1)
	require.Len(t, s.Producers[0].Receivers, 1)
	require.Len(t, s.Consumers, 1)
	require.Len(t, s.Consumers[0].Senders, 1)
}

// A name that starts pointing at another stream object (an alias relinked by
// a patch, or a stream deleted and created again within one flush) must send
// the new stream's state: the name alone was already known, and the registry
// change only signals, it marks no stream.
func TestRelinkedNameSendsTheNewStream(t *testing.T) {
	newFakeCamera("relinka", core.CodecH264)
	newFakeCamera("relinkb", core.CodecH264)
	newStream(t, "cui_relink_a", "relinka://cam")
	b := newStream(t, "cui_relink_b", "relinkb://cam")
	attach(t, b) // b has a consumer, a has none

	_, err := streams.Patch("cui_relink_alias", "cui_relink_a")
	require.NoError(t, err)
	t.Cleanup(func() { streams.Delete("cui_relink_alias") })

	r := subscribed(t)
	r.next(t, "cui/snapshot")

	_, err = streams.Patch("cui_relink_alias", "cui_relink_b")
	require.NoError(t, err)
	require.Same(t, b, streams.Get("cui_relink_alias"))

	r.nextStream(t, "cui_relink_alias", func(v *stateView) bool { return v != nil && len(v.Consumers) == 1 })
}
