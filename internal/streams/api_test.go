package streams

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/AlexxIT/go2rtc/internal/app"
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

// camera.ui writes a preload into the config as "video&audio&microphone" and
// sends the same query when it starts. The API stores it the way it came, so
// the config file stays as it is.
func TestPreloadIsStoredTheWayItWasSent(t *testing.T) {
	registerTestRTSPHandler()
	cam := newFakeCamera(t)
	_, err := New("api_preload_stored", cam.URL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = DelPreload("api_preload_stored") })

	config := "preload:\n  api_preload_stored: video&audio&microphone\n"
	path := filepath.Join(t.TempDir(), "go2rtc.yaml")
	require.NoError(t, os.WriteFile(path, []byte(config), 0644))
	configPath := app.ConfigPath
	app.ConfigPath = path
	t.Cleanup(func() { app.ConfigPath = configPath })

	put := func(query string) {
		w := httptest.NewRecorder()
		apiPreload(w, httptest.NewRequest("PUT", "/api/preload?src=api_preload_stored&"+query, nil))
		require.Equal(t, http.StatusOK, w.Code)
	}

	put("video&audio&microphone&other=1")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, config, string(data))

	put("video&audio=opus,pcma")
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "preload:\n  api_preload_stored: video&audio=opus,pcma\n", string(data))
	require.Equal(t, []string{"opus,pcma"}, GetPreload("api_preload_stored").query["audio"])
}
