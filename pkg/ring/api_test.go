package ring

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccessTokenRequestsSkipAuthAndSession(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()

		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("hardware_id") != "hid" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path == "/expired" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ticket":"abc"}`))
	}))
	defer srv.Close()

	api, err := NewRestClient(AccessTokenAuth{AccessToken: "token", HardwareID: "hid"}, nil)
	require.NoError(t, err)

	body, err := api.Request("POST", srv.URL+"/ticket", nil)
	require.NoError(t, err)
	require.JSONEq(t, `{"ticket":"abc"}`, string(body))

	_, err = api.Request("GET", srv.URL+"/expired", nil)
	require.ErrorContains(t, err, "401")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"/ticket", "/expired"}, paths)
}
