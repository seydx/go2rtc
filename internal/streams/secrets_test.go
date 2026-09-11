package streams

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// Credentials written literally into a source must never reach the streams API.
// They are registered when a producer is created, before any dial, and again
// when a source is updated in place or fills a template.
func TestApiStreamsMasksLiteralSourceCredentials(t *testing.T) {
	HandleFunc("stubcloud", func(string) (core.Producer, error) { return nil, errors.New("stub") })

	const name, tmplName = "masked_source", "masked_template"
	t.Cleanup(func() {
		Delete(name)
		Delete(tmplName)
	})

	get := func(src string) string {
		w := httptest.NewRecorder()
		apiStreams(w, httptest.NewRequest("GET", "/api/streams?src="+src, nil))
		require.Equal(t, http.StatusOK, w.Code)
		return w.Body.String()
	}
	list := func() string {
		w := httptest.NewRecorder()
		apiStreams(w, httptest.NewRequest("GET", "/api/streams", nil))
		require.Equal(t, http.StatusOK, w.Code)
		return w.Body.String()
	}

	// new stream
	_, err := New(name, "stubcloud:?client_id=STREAMS-CLIENT-ID&client_secret=STREAMS-SECRET-NEW#noBackchannel")
	require.NoError(t, err)
	body := get(name)
	require.Contains(t, body, "STREAMS-CLIENT-ID", "the producer url must be part of the response")
	require.NotContains(t, body, "STREAMS-SECRET-NEW")
	require.NotContains(t, list(), "STREAMS-SECRET-NEW")

	// source updated in place
	_, err = New(name, "stubcloud:?client_id=STREAMS-CLIENT-ID&refresh_token=STREAMS-TOKEN-UPDATED")
	require.NoError(t, err)
	body = get(name)
	require.Contains(t, body, "STREAMS-CLIENT-ID")
	require.NotContains(t, body, "STREAMS-TOKEN-UPDATED")

	// template filled by a request (ex. from Home Assistant)
	_, err = New(tmplName, "stubcloud:{input}")
	require.NoError(t, err)
	_, err = Patch(tmplName, "stubcloud:?token=STREAMS-TEMPLATE-TOKEN")
	require.NoError(t, err)
	body = get(tmplName)
	require.Contains(t, body, "stubcloud:")
	require.NotContains(t, body, "STREAMS-TEMPLATE-TOKEN")
}
