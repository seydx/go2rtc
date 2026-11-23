package mp4

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// silentCam has a video track and sends nothing
type silentCam struct {
	core.Connection
	started chan struct{}
	stop    chan struct{}
	once    sync.Once
}

func (c *silentCam) Start() error {
	close(c.started)
	<-c.stop
	return nil
}

func (c *silentCam) Stop() error {
	c.once.Do(func() { close(c.stop) })
	return c.Connection.Stop()
}

// newSilentStream returns a channel that gets each camera when it starts
func newSilentStream(t *testing.T, name string) chan *silentCam {
	t.Helper()
	started := make(chan *silentCam, 1)
	streams.HandleFunc(name, func(string) (core.Producer, error) {
		cam := &silentCam{
			Connection: core.Connection{Medias: []*core.Media{{
				Kind:      core.KindVideo,
				Direction: core.DirectionRecvonly,
				Codecs:    []*core.Codec{{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}},
			}}},
			started: make(chan struct{}),
			stop:    make(chan struct{}),
		}
		go func() {
			<-cam.started
			started <- cam
		}()
		return cam, nil
	})
	_, err := streams.New(name, name+"://cam")
	require.NoError(t, err)
	return started
}

// camera.ui gives a snapshot 30s, a camera that wakes up slowly needs them
func TestKeyframeTimeoutDefault(t *testing.T) {
	require.Equal(t, 30*time.Second, keyframeTimeout)
}

// A camera that sends no frame doesn't keep the request and its viewer
// forever: after ?timeout= the request fails and the viewer leaves the stream.
func TestKeyframeWithoutFrameTimesOut(t *testing.T) {
	newSilentStream(t, "kfsilent")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/frame.mp4?src=kfsilent&timeout=200ms", nil)

	start := time.Now()
	handlerKeyframe(w, r)
	require.Less(t, time.Since(start), 5*time.Second)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Eventually(t, func() bool { return consumerCount(t, "kfsilent") == 0 },
		time.Second, 10*time.Millisecond, "the viewer must be removed from the stream")
}

// A client that gives up doesn't leave its viewer in the stream.
func TestKeyframeClientGone(t *testing.T) {
	started := newSilentStream(t, "kfgone")

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/api/frame.mp4?src=kfgone", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handlerKeyframe(httptest.NewRecorder(), r)
		close(done)
	}()

	// the viewer has its track once the camera runs
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the camera must start")
	}
	require.Equal(t, 1, consumerCount(t, "kfgone"))
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the request must end with the client")
	}
	require.Eventually(t, func() bool { return consumerCount(t, "kfgone") == 0 },
		time.Second, 10*time.Millisecond, "the viewer must be removed from the stream")
}
