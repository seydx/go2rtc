package streams

import (
	"encoding/json"
	"errors"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/probe"
	"github.com/stretchr/testify/require"
)

func TestRecursion(t *testing.T) {
	HandleFunc("stubproto", func(string) (core.Producer, error) { return nil, errors.New("stub") }) // bypass HasProducer

	const src = "rtsp://localhost:8554/from_yaml?video"
	// a leftover alias would make New treat "from_yaml" as an alias on the next run
	t.Cleanup(func() {
		Delete("from_yaml")
		Delete(src)
	})

	// create stream with some source
	stream1, err := New("from_yaml", "stubproto://does_not_matter")
	require.NoError(t, err)
	require.Same(t, stream1, Get("from_yaml"))

	// ask another unnamed stream that links go2rtc
	query, err := url.ParseQuery("src=" + src)
	require.NoError(t, err)
	stream2, err := GetOrPatch(query)
	require.NoError(t, err)

	// check stream is same
	require.Equal(t, stream1, stream2)
	// check stream urls is same
	require.Equal(t, stream1.producers[0].url, stream2.producers[0].url)
	// the link is registered as an alias, not as a second stream; the streams
	// map is shared by the whole package, so check by name instead of by size
	require.Same(t, stream1, Get(src))
}

func TestTempate(t *testing.T) {
	HandleFunc("rtsp", func(url string) (core.Producer, error) { return nil, nil })              // bypass HasProducer
	HandleFunc("ffmpeg", func(string) (core.Producer, error) { return nil, errors.New("stub") }) // bypass HasProducer
	t.Cleanup(func() { Delete("camera.from_hass") })

	// config from yaml
	stream1, err := New("camera.from_hass", "ffmpeg:{input}#video=copy")
	require.NoError(t, err)
	// request from hass
	stream2, err := Patch("camera.from_hass", "rtsp://example.com")
	require.NoError(t, err)

	require.Equal(t, stream1, stream2)
	require.Equal(t, "ffmpeg:rtsp://example.com#video=copy", stream1.producers[0].url)
}

type stubConsumer struct {
	core.Consumer
	stopped atomic.Bool
}

func (c *stubConsumer) GetMedias() []*core.Media { return nil }
func (c *stubConsumer) IsClosed() bool           { return c.stopped.Load() }
func (c *stubConsumer) Stop() error              { c.stopped.Store(true); return nil }

func TestSetSourcesKeepsUnchangedProducers(t *testing.T) {
	s := NewStream([]string{"rtsp://127.0.0.1/main#gop=1", "ffmpeg:x#audio=opus"})
	companion := s.producers[1]
	cons := &stubConsumer{}
	s.consumers = append(s.consumers, cons)

	require.False(t, s.setSources([]string{"rtsp://127.0.0.1/main#gop=1", "ffmpeg:x#audio=opus"}))
	require.False(t, cons.stopped.Load())
	require.Len(t, s.consumers, 1)

	require.True(t, s.setSources([]string{"rtsp://127.0.0.1/main#gop=1#noBackchannel", "ffmpeg:x#audio=opus"}))
	require.Len(t, s.producers, 2)
	require.False(t, s.producers[0].backchannelEnabled)
	require.Same(t, companion, s.producers[1])
	require.True(t, cons.stopped.Load())
	require.Empty(t, s.consumers)
}

func TestNewUpdatesExistingStreamInPlace(t *testing.T) {
	HandleFunc("stubproto", func(string) (core.Producer, error) { return nil, errors.New("stub") })

	first, err := New("in_place_test", "stubproto://cam#gop=1")
	require.NoError(t, err)
	second, err := New("in_place_test", "stubproto://cam#gop=1#noBackchannel")
	require.NoError(t, err)
	require.Same(t, first, second)
	require.False(t, second.producers[0].backchannelEnabled)
}

func TestNewLeavesAliasedStreamAlone(t *testing.T) {
	HandleFunc("stubproto", func(string) (core.Producer, error) { return nil, errors.New("stub") })

	shared, err := New("alias_origin", "stubproto://cam")
	require.NoError(t, err)
	streamsMu.Lock()
	streams["alias_name"] = shared
	streamsMu.Unlock()

	replaced, err := New("alias_name", "stubproto://other")
	require.NoError(t, err)
	require.NotSame(t, shared, replaced)
	require.Equal(t, "stubproto://cam", shared.producers[0].source)
	require.Same(t, shared, Get("alias_origin"))
}

// A stream without templates, like camera.ui's camera plus exec companion,
// swaps only its primary source on a patch. Setting every producer to the new
// source collapsed the companion into a second copy of the camera.
func TestSetSourceKeepsCompanion(t *testing.T) {
	s := NewStream([]string{"rtsp://127.0.0.1/old", "ffmpeg:cam#audio=opus#requirePrevAudio"})
	companion := s.producers[1]
	cons := &stubConsumer{}
	s.consumers = append(s.consumers, cons)

	s.SetSource("rtsp://127.0.0.1/new")

	require.Len(t, s.producers, 2)
	require.Equal(t, "rtsp://127.0.0.1/new", s.producers[0].url)
	require.Same(t, companion, s.producers[1], "the companion keeps its producer")
	require.Equal(t, "ffmpeg:cam#audio=opus", companion.url)
	require.True(t, companion.requirePrevAudio)
	require.True(t, cons.stopped.Load(), "consumers reconnect against the new source")
	require.Empty(t, s.consumers)
}

// Template producers get the input filled in; the others stay as they are.
func TestSetSourceFillsOnlyTemplates(t *testing.T) {
	s := NewStream([]string{"ffmpeg:{input}#video=copy", "ffmpeg:cam#audio=opus#requirePrevAudio"})
	companion := s.producers[1]

	s.SetSource("rtsp://example.com")

	require.Equal(t, "ffmpeg:rtsp://example.com#video=copy", s.producers[0].url)
	require.Same(t, companion, s.producers[1])
	require.Equal(t, "ffmpeg:cam#audio=opus", companion.url)

	// the template survives a second patch
	s.SetSource("rtsp://example.org")
	require.Equal(t, "ffmpeg:rtsp://example.org#video=copy", s.producers[0].url)
}

// A named GetOrPatch repeats the patch on every client connect: the same
// input must not replace producers or disconnect anyone.
func TestSetSourceSameInputChangesNothing(t *testing.T) {
	s := NewStream("rtsp://127.0.0.1/cam")
	s.SetSource("rtsp://127.0.0.1/cam#gop=1")
	prod := s.producers[0]
	cons := &stubConsumer{}
	s.consumers = append(s.consumers, cons)

	s.SetSource("rtsp://127.0.0.1/cam#gop=1")

	require.Same(t, prod, s.producers[0])
	require.False(t, cons.stopped.Load())
	require.Len(t, s.consumers, 1)
}

// Producer url and flags are read without locks all over the package, so a
// patch must never rewrite a producer that others can already see.
// Meaningful under -race.
func TestSetSourceDoesNotRaceReaders(t *testing.T) {
	s := NewStream([]string{"rtsp://127.0.0.1/a", "ffmpeg:cam#audio=opus#requirePrevAudio"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			if i%2 == 0 {
				s.SetSource("rtsp://127.0.0.1/b#noAudio")
			} else {
				s.SetSource("rtsp://127.0.0.1/a")
			}
		}
	}()

	for {
		select {
		case <-done:
			return
		default:
		}
		_ = s.Sources()
		_, _ = s.MarshalJSON()
		for _, prod := range streamProducers(s) {
			_ = prod.GetMedias()
			_ = prod.hasHiddenMedias()
		}
	}
}

// pushedConn is a pushed peer that takes talk too, like a WebRTC ingest
type pushedConn struct {
	gatedConn
	talks atomic.Int32
}

func (c *pushedConn) AddTrack(*core.Media, *core.Codec, *core.Receiver) error {
	c.talks.Add(1)
	return nil
}

func pushStream(t *testing.T, name string) (*Stream, *pushedConn) {
	t.Cleanup(func() { Delete(name) })
	stream, err := New(name)
	require.NoError(t, err)

	conn := &pushedConn{gatedConn: gatedConn{
		Connection: core.Connection{Medias: []*core.Media{
			{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{{Name: core.CodecH264, ClockRate: 90000}}},
			{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{{Name: core.CodecPCMU, ClockRate: 8000}}},
			{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecPCMU, ClockRate: 8000}}},
		}},
		stopped: make(chan struct{}),
	}}
	stream.AddProducer(conn)
	t.Cleanup(func() {
		stream.RemoveProducer(conn)
		_ = conn.Stop()
	})
	return stream, conn
}

// A pushed source (RTSP or RTMP publish, WebRTC ingest, HTTP POST) has no
// source with #flags. Its producer came without their defaults: video, audio
// and talk were hidden, the API listed no medias, and every consumer of the
// stream got "streams: unknown error".
func TestAddProducerOffersAllMedias(t *testing.T) {
	stream, conn := pushStream(t, "add_producer_offers_all_medias")

	prod := streamProducers(stream)[0]
	require.Equal(t, conn.Medias, prod.GetMedias())

	b, err := json.Marshal(prod)
	require.NoError(t, err)
	var info struct {
		Medias []string `json:"medias"`
	}
	require.NoError(t, json.Unmarshal(b, &info))
	require.Len(t, info.Medias, len(conn.Medias))

	query, _ := url.ParseQuery("video&audio")
	cons := probe.Create("viewer", query)
	require.NoError(t, stream.AddConsumer(cons))
	t.Cleanup(func() { stream.RemoveConsumer(cons) })
	require.Len(t, cons.Senders, 2)
}

// Talk to a pushed peer goes through the mixer, as with any source: the
// talkers share its one talk track.
func TestAddProducerMixesTalk(t *testing.T) {
	stream, conn := pushStream(t, "add_producer_mixes_talk")

	for range 2 {
		cons := micProbe()
		require.NoError(t, stream.AddConsumer(cons))
		t.Cleanup(func() { stream.RemoveConsumer(cons) })
		require.Len(t, cons.Senders, 2)
		require.Len(t, cons.Receivers, 1)
	}
	require.Equal(t, int32(1), conn.talks.Load())
}
