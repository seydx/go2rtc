package rtsp

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A playing session sends OPTIONS 5 s before the session timeout of the
// camera, every #keepalive=N s, or never with #keepalive=-1: some cameras
// break the stream when they get a request while streaming (#2746).
func TestKeepalive(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sessionTimeout int // of the camera, in s
		keepalive      int
		options        bool
	}{
		{"from the session timeout", 6, 0, true},
		{"own interval", 60, 1, true},
		{"disabled", 6, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cam := newFakeCamera(t, -1)
			cam.sessionTimeout = tc.sessionTimeout
			defer cam.ln.Close()

			client := NewClient("rtsp://" + cam.ln.Addr().String() + "/stream")
			client.Keepalive = tc.keepalive

			require.Nil(t, client.Dial())
			require.Nil(t, client.Describe())

			video := client.GetMedias()[0]
			_, err := client.GetTrack(video, video.Codecs[0])
			require.Nil(t, err)

			// the session ends before the test, nothing of it reads on afterwards
			ended := make(chan struct{})
			go func() {
				_ = client.Start()
				close(ended)
			}()
			defer func() {
				_ = client.Stop()
				<-ended
			}()

			require.Equal(t, 0, waitPlayed(t, cam))

			// the camera sends no packets, the session ends 5 s after PLAY
			options := func() bool { return slices.Contains(cam.methods(0), MethodOptions) }
			if tc.options {
				require.Eventually(t, options, 3*time.Second, 10*time.Millisecond, "%v", cam.methods(0))
			} else {
				require.Never(t, options, 3*time.Second, 50*time.Millisecond, "%v", cam.methods(0))
			}
		})
	}
}
