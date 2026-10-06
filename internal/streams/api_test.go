package streams

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

func TestApiSchemes(t *testing.T) {
	// Setup: Register some test handlers and redirects
	HandleFunc("rtsp", func(url string) (core.Producer, error) { return nil, nil })
	HandleFunc("rtmp", func(url string) (core.Producer, error) { return nil, nil })
	RedirectFunc("http", func(url string) (string, error) { return "", nil })

	t.Run("GET request returns schemes", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/schemes", nil)
		w := httptest.NewRecorder()

		apiSchemes(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "application/json", w.Header().Get("Content-Type"))

		var schemes []string
		err := json.Unmarshal(w.Body.Bytes(), &schemes)
		require.NoError(t, err)
		require.NotEmpty(t, schemes)

		// Check that our test schemes are in the response
		require.Contains(t, schemes, "rtsp")
		require.Contains(t, schemes, "rtmp")
		require.Contains(t, schemes, "http")
	})
}

func TestApiSchemesNoDuplicates(t *testing.T) {
	// Setup: Register a scheme in both handlers and redirects
	HandleFunc("duplicate", func(url string) (core.Producer, error) { return nil, nil })
	RedirectFunc("duplicate", func(url string) (string, error) { return "", nil })

	req := httptest.NewRequest("GET", "/api/schemes", nil)
	w := httptest.NewRecorder()

	apiSchemes(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var schemes []string
	err := json.Unmarshal(w.Body.Bytes(), &schemes)
	require.NoError(t, err)

	// Count occurrences of "duplicate"
	count := 0
	for _, scheme := range schemes {
		if scheme == "duplicate" {
			count++
		}
	}

	// Should only appear once
	require.Equal(t, 1, count, "scheme 'duplicate' should appear exactly once")
}

// A revoked share deletes its viewer through the API. The handler must close
// the viewer's connection too, or the client attaches again on it.
func TestDeleteConsumerEndsItsConnection(t *testing.T) {
	registerTestRTSPHandler()
	cam := newFakeCamera(t)
	stream, err := New("api_delete_consumer", cam.URL())
	require.NoError(t, err)

	for _, by := range []string{"tag", "id"} {
		t.Run(by, func(t *testing.T) {
			var disconnected atomic.Bool
			cons := queryProbe(t, "video")
			cons.Tag = "share_api_delete"
			stream.OnEvict(cons, func() { disconnected.Store(true) })
			require.NoError(t, stream.AddConsumer(cons))
			t.Cleanup(func() { stream.RemoveConsumer(cons) })

			query := "tag=" + cons.Tag
			if by == "id" {
				query = "id=" + strconv.FormatUint(uint64(cons.ID), 10)
			}
			w := httptest.NewRecorder()
			apiConsumer(w, httptest.NewRequest("DELETE", "/api/consumer?"+query, nil))
			require.Equal(t, http.StatusOK, w.Code)

			require.False(t, registered(stream, cons))
			require.True(t, disconnected.Load(), "the handler must close the viewer's connection")
		})
	}
}
