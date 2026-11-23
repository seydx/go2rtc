package streams

import (
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/probe"
	"github.com/stretchr/testify/require"
)

// keyframeConn is a source whose tracks ask it for a keyframe, like an RTSP
// source with #keyframe= does. The URL lists its medias: video, audio, talk.
type keyframeConn struct {
	core.Connection

	asked chan string // the kind of each track asked for a keyframe
	stop  chan struct{}
	once  sync.Once
}

func registerKeyframeConn(scheme string, asked chan string) {
	HandleFunc(scheme, func(rawURL string) (core.Producer, error) {
		c := &keyframeConn{asked: asked, stop: make(chan struct{})}
		c.ID = core.NewID()
		c.FormatName = scheme

		_, kinds, _ := strings.Cut(rawURL, ":")
		for kind := range strings.SplitSeq(kinds, "+") {
			switch kind {
			case "video":
				c.Medias = append(c.Medias, &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly,
					Codecs: []*core.Codec{{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}}})
			case "audio":
				c.Medias = append(c.Medias, &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly,
					Codecs: []*core.Codec{{Name: core.CodecPCMA, ClockRate: 8000}}})
			case "talk":
				c.Medias = append(c.Medias, &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly,
					Codecs: []*core.Codec{{Name: core.CodecPCMA, ClockRate: 8000}}})
			}
		}
		return c, nil
	})
}

func (c *keyframeConn) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	track, err := c.Connection.GetTrack(media, codec)
	if err != nil {
		return nil, err
	}
	kind := media.Kind
	track.OnKeyframe(func() { c.asked <- kind })
	return track, nil
}

func (c *keyframeConn) AddTrack(*core.Media, *core.Codec, *core.Receiver) error {
	return nil
}

func (c *keyframeConn) Start() error {
	<-c.stop
	return nil
}

func (c *keyframeConn) Stop() error {
	c.once.Do(func() { close(c.stop) })
	return c.Connection.Stop()
}

// A viewer that joins a running source asks it for a keyframe for its video,
// so it doesn't wait for the next one of the camera. The first viewer starts
// the source, the camera begins with a keyframe anyway. The fork binds the
// tracks in three places, one per way the stream picks its sources.
func TestNewViewerOfRunningSourceAsksForKeyframe(t *testing.T) {
	asked := make(chan string, 10)
	registerKeyframeConn("fkkey", asked)

	for _, tc := range []struct {
		name    string
		sources []string
		viewer  string
	}{
		{"one source has all", []string{"fkkey:video+audio"}, "video&audio"},
		{"video and audio from one, talk from another", []string{"fkkey:video+audio", "fkkey:talk"}, "video&audio&microphone"},
		{"video and audio from two", []string{"fkkey:video", "fkkey:audio"}, "video&audio"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStream(tc.sources)
			query, err := url.ParseQuery(tc.viewer)
			require.NoError(t, err)

			first := probe.Create("first", query)
			require.NoError(t, s.AddConsumer(first))
			t.Cleanup(func() { s.RemoveConsumer(first) })

			// the first viewer finds the source stopped
			time.Sleep(50 * time.Millisecond)
			require.Empty(t, asked)

			second := probe.Create("second", query)
			require.NoError(t, s.AddConsumer(second))
			t.Cleanup(func() { s.RemoveConsumer(second) })

			select {
			case kind := <-asked:
				require.Equal(t, core.KindVideo, kind)
			case <-time.After(time.Second):
				t.Fatal("no keyframe request")
			}

			// for the video only
			time.Sleep(50 * time.Millisecond)
			require.Empty(t, asked)
		})
	}
}
