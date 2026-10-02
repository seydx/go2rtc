package image

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/stretchr/testify/require"
)

// Stop is called from another goroutine while Start polls the camera. The
// poller has to see it without a data race and end with nil, the producer
// treats a stopped session as no failure. Meaningful under -race.
func TestStopWhilePolling(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(10 * time.Millisecond) // a camera takes a moment per snapshot
		_, _ = w.Write([]byte{0xff, 0xd8, 0xff, 0xd9})
	}))
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL, nil)
	require.Nil(t, err)
	res, err := tcp.Do(req)
	require.Nil(t, err)

	prod, err := Open(res)
	require.Nil(t, err)
	media := prod.GetMedias()[0]
	_, err = prod.GetTrack(media, media.Codecs[0])
	require.Nil(t, err)

	done := make(chan error, 1)
	go func() { done <- prod.Start() }()

	// Let it poll a few times without synchronizing with it: waiting on the
	// request counter would order the poller's reads before Stop and hide the
	// race, and so would thousands of fast polls (the detector forgets them).
	time.Sleep(50 * time.Millisecond)
	require.Nil(t, prod.Stop())

	select {
	case err = <-done:
		require.Nil(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("polling went on after stop")
	}

	n := requests.Load()
	require.Greater(t, n, int32(1), "the poller never polled")
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, n, requests.Load(), "requests after start returned")
}
