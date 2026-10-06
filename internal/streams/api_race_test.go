package streams

import (
	"net/url"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/probe"
	"github.com/stretchr/testify/require"
)

// camera.ui polls the stream state while viewers attach. A track the session
// hasn't set up yet re-dials the camera on the same connection, and the first
// talk packet adds the talk track: both rewrite what the API marshals, the
// session info, the tracks and the mixer's children. Meaningful under -race.
func TestAPIReadsWhileTracksAreSetUp(t *testing.T) {
	registerBackchannelRTSPHandler()

	cam := newFakeCamera(t)
	cam.backchannel.Store(true)

	const name = "api_reads_track_setup"
	stream, err := New(name, cam.BackchannelURL())
	require.NoError(t, err)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = stream.MarshalJSON()
			_ = StateOf(name, stream)
			_ = StatsOf(stream)
		}
	}()

	query, _ := url.ParseQuery("video")
	video := probe.Create("probe", query)
	require.NoError(t, stream.AddConsumer(video))
	t.Cleanup(func() { stream.RemoveConsumer(video) })
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))

	// audio isn't set up yet: the playing session is set up again
	query, _ = url.ParseQuery("audio")
	audio := probe.Create("probe", query)
	require.NoError(t, stream.AddConsumer(audio))
	t.Cleanup(func() { stream.RemoveConsumer(audio) })

	mic := micProbe()
	require.NoError(t, stream.AddConsumer(mic))
	t.Cleanup(func() { stream.RemoveConsumer(mic) })
	talk(mic)
	require.True(t, waitUntil(10*time.Second, func() bool { return talkSettled(stream) && talkWired(stream) }))
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))

	close(stop)
	<-done
}
