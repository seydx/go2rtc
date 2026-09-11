package streams

import (
	"errors"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
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
