package tuya

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSmartSessionLogsInOnce(t *testing.T) {
	s := &smartSession{}
	var logins atomic.Int32

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, s.login(0, func() (string, error) {
				logins.Add(1)
				time.Sleep(20 * time.Millisecond)
				return "ssl://broker:8883", nil
			}))
		}()
	}
	wg.Wait()

	require.Equal(t, int32(1), logins.Load())
	seen, url := s.state()
	require.Equal(t, 1, seen)
	require.Equal(t, "ssl://broker:8883", url)
}

func TestSmartSessionLogsInAgainOnlyOnce(t *testing.T) {
	s := &smartSession{logins: 1}
	var logins int
	login := func() (string, error) {
		logins++
		return "", nil
	}

	require.NoError(t, s.login(1, login))
	// a second stream saw the same lost session, the first one already logged in again
	require.NoError(t, s.login(1, login))
	require.Equal(t, 1, logins)
}

func TestSmartSessionWaitsAfterFailedLogin(t *testing.T) {
	old := loginRetryDelay
	t.Cleanup(func() { loginRetryDelay = old })
	loginRetryDelay = time.Hour

	s := &smartSession{}
	refused := errors.New("too many requests")
	var logins int
	login := func() (string, error) {
		logins++
		return "", refused
	}

	require.ErrorIs(t, s.login(0, login), refused)
	require.ErrorIs(t, s.login(0, login), refused)
	require.Equal(t, 1, logins)

	loginRetryDelay = 0
	require.ErrorIs(t, s.login(0, login), refused)
	require.Equal(t, 2, logins)
}

func TestLoadConfigsLogsInAgainAfterSessionLoss(t *testing.T) {
	server := newFakeSmartServer(t)

	newClient := func() *TuyaSmartApiClient {
		c, err := NewTuyaSmartApiClient("protect-eu.ismartlife.me", "session-loss@example.com", "secret", "device")
		require.NoError(t, err)
		c.httpClient.Transport = server
		return c
	}

	_, hub, err := newClient().loadConfigs()
	require.NoError(t, err)
	require.Equal(t, "ssl://m1.tuyaeu.com:8883", hub.Url)

	_, _, err = newClient().loadConfigs()
	require.NoError(t, err)
	require.Equal(t, int32(1), server.logins.Load())

	server.expireSession()

	_, _, err = newClient().loadConfigs()
	require.NoError(t, err)
	require.Equal(t, int32(2), server.logins.Load())
}

type fakeSmartServer struct {
	mux    *http.ServeMux
	logins atomic.Int32
	valid  atomic.Int32
}

func newFakeSmartServer(t *testing.T) *fakeSmartServer {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)

	s := &fakeSmartServer{mux: http.NewServeMux()}

	s.mux.HandleFunc("/api/login/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"success": true, "result": map[string]any{
			"token": "token", "pbKey": base64.StdEncoding.EncodeToString(der),
		}})
	})
	s.mux.HandleFunc("/api/private/email/login", func(w http.ResponseWriter, r *http.Request) {
		sid := s.logins.Add(1)
		s.valid.Store(sid)
		http.SetCookie(w, &http.Cookie{Name: "s-sid", Value: sidValue(sid), Path: "/"})
		writeJSON(w, map[string]any{"success": true, "result": map[string]any{
			"domain": map[string]any{"mobileMqttsUrl": "m1.tuyaeu.com", "mqttsPort": 8883},
		}})
	})
	s.mux.HandleFunc("/api/jarvis/config", s.withSession(map[string]any{
		"motoId": "moto", "skill": `{"webrtc":3}`, "p2pConfig": map[string]any{"ices": []any{}},
	}))
	s.mux.HandleFunc("/api/jarvis/mqtt", s.withSession(map[string]any{"msid": "msid", "password": "pw"}))

	return s
}

func (s *fakeSmartServer) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func (s *fakeSmartServer) expireSession() {
	s.valid.Store(0)
}

func (s *fakeSmartServer) withSession(result any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("s-sid")
		if err != nil || cookie.Value != sidValue(s.valid.Load()) || r.Header.Get("X-Requested-With") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"success": false, "errorCode": "USER_SESSION_LOSS", "errorMsg": "Not login"})
			return
		}
		writeJSON(w, map[string]any{"success": true, "result": result})
	}
}

func sidValue(n int32) string {
	return string(rune('a' + n))
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}
