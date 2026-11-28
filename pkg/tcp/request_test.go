package tcp

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// Older Axis firmware (ex. M5054 on 6.53.4) knows neither ECDHE nor GCM, only
// the CBC suites with RSA key exchange (#2383). A camera is reached by its IP
// with the insecure config, it must offer them.
func TestDoRSACBCCipherSuites(t *testing.T) {
	for _, suite := range []uint16{tls.TLS_RSA_WITH_AES_128_CBC_SHA, tls.TLS_RSA_WITH_AES_256_CBC_SHA} {
		t.Run(tls.CipherSuiteName(suite), func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("ok"))
			}))
			srv.TLS = &tls.Config{MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{suite}}
			srv.StartTLS()
			t.Cleanup(srv.Close)

			req, err := http.NewRequest("GET", srv.URL, nil) // https://127.0.0.1:port
			require.NoError(t, err)
			res, err := Do(req)
			require.NoError(t, err)
			t.Cleanup(func() { _ = res.Body.Close() })

			require.Equal(t, uint16(suite), res.TLS.CipherSuite)
			body, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			require.Equal(t, "ok", string(body))
		})
	}
}
