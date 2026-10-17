package xiaomi

import (
	"bytes"
	"crypto/rc4"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/xiaomi"
	"github.com/stretchr/testify/require"
)

const (
	testUser      = "123"
	testPassToken = "pass"
)

// fakeCloud answers the requests of xiaomi.Cloud in place of the Xiaomi
// servers: the login with the passToken, the STS that sets the serviceToken
// and the API, which takes only the serviceToken of the last login.
type fakeCloud struct {
	ssecurity []byte

	mu       sync.Mutex
	logins   int
	token    string // the serviceToken the API takes
	requests int
	status   int // the API answers every request with it, if set

	held chan chan struct{} // the API holds requests with an expired token here, if set
}

func useFakeCloud(t *testing.T) *fakeCloud {
	f := &fakeCloud{ssecurity: []byte("0123456789abcdef")}

	transport := http.DefaultTransport
	http.DefaultTransport = f
	tokens, clouds = map[string]string{testUser: testPassToken}, nil

	t.Cleanup(func() {
		http.DefaultTransport = transport
		tokens, clouds = nil, nil
	})
	return f
}

// expire makes the API reject the serviceToken of the last login
func (f *fakeCloud) expire() {
	f.mu.Lock()
	f.token = ""
	f.mu.Unlock()
}

func (f *fakeCloud) count() (logins, requests int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins, f.requests
}

func (f *fakeCloud) RoundTrip(req *http.Request) (*http.Response, error) {
	res := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: req}

	var body string

	switch {
	case req.URL.Host == "account.xiaomi.com" && req.URL.Path == "/pass/serviceLogin":
		if !strings.Contains(req.Header.Get("Cookie"), "passToken="+testPassToken) {
			body = `&&&START&&&{"code":70016}`
			break
		}
		b, _ := json.Marshal(map[string]any{
			"ssecurity": f.ssecurity,
			"passToken": testPassToken,
			"location":  "https://sts.api.io.mi.com/sts?d=1",
		})
		body = "&&&START&&&" + string(b)

	case req.URL.Host == "sts.api.io.mi.com":
		f.mu.Lock()
		f.logins++
		f.token = fmt.Sprintf("token%d", f.logins)
		token := f.token
		f.mu.Unlock()

		res.Header.Add("Set-Cookie", "userId="+testUser)
		res.Header.Add("Set-Cookie", "cUserId=c"+testUser)
		res.Header.Add("Set-Cookie", "serviceToken="+token)

	case strings.HasSuffix(req.URL.Host, "api.io.mi.com"):
		f.mu.Lock()
		f.requests++
		valid := f.token != "" && strings.Contains(req.Header.Get("Cookie"), "serviceToken="+f.token)
		status, held := f.status, f.held
		f.mu.Unlock()

		if status != 0 {
			res.StatusCode = status
			break
		}
		if !valid {
			if held != nil {
				release := make(chan struct{})
				held <- release
				<-release
			}
			res.StatusCode = http.StatusUnauthorized
			body = "auth err"
			break
		}

		if err := req.ParseForm(); err != nil {
			return nil, err
		}
		nonce, err := base64.StdEncoding.DecodeString(req.PostForm.Get("_nonce"))
		if err != nil {
			return nil, err
		}
		body = f.encrypt(nonce, `{"code":0,"result":{"ok":true}}`)

	default:
		return nil, fmt.Errorf("unexpected request %s", req.URL)
	}

	res.Status = fmt.Sprintf("%d %s", res.StatusCode, http.StatusText(res.StatusCode))
	res.Body = io.NopCloser(strings.NewReader(body))
	return res, nil
}

// encrypt is what the API does with its answer: RC4 with the signed nonce,
// the first 1024 bytes of the key stream dropped
func (f *fakeCloud) encrypt(nonce []byte, s string) string {
	key := sha256.Sum256(append(bytes.Clone(f.ssecurity), nonce...))
	cipher, _ := rc4.NewCipher(key[:])
	cipher.XORKeyStream(make([]byte, 1024), make([]byte, 1024))
	b := make([]byte, len(s))
	cipher.XORKeyStream(b, []byte(s))
	return base64.StdEncoding.EncodeToString(b)
}

func request(t *testing.T) error {
	t.Helper()
	res, err := cloudRequest(testUser, "de", "/v2/device/miss_get_vendor", "{}")
	if err == nil {
		require.JSONEq(t, `{"ok":true}`, string(res))
	}
	return err
}

// The serviceToken of a session expires after about ten hours. Every dial asks
// the cloud first: the cached session used to fail every dial with 401 from
// then on, until a restart.
func TestCloudRequestLogsInAgainWhenTheSessionExpired(t *testing.T) {
	f := useFakeCloud(t)

	require.NoError(t, request(t))
	require.NoError(t, request(t))
	logins, _ := f.count()
	require.Equal(t, 1, logins, "the session is kept")

	f.expire()
	require.NoError(t, request(t))
	require.NoError(t, request(t))
	logins, requests := f.count()
	require.Equal(t, 2, logins)
	require.Equal(t, 5, requests, "the request is sent once more")
}

// Several cameras run into the expired session at once. The first one logs in
// again, the others use its new session: a session that is fresh by now is
// not thrown away for the expired one of another camera.
func TestCloudRequestRenewsTheSessionOnce(t *testing.T) {
	f := useFakeCloud(t)
	require.NoError(t, request(t))

	const cameras = 5
	held := make(chan chan struct{}, cameras)
	f.mu.Lock()
	f.held = held
	f.mu.Unlock()
	f.expire()

	errs := make(chan error, cameras)
	for range cameras {
		go func() {
			_, err := cloudRequest(testUser, "de", "/v2/device/miss_get_vendor", "{}")
			errs <- err
		}()
	}

	var releases []chan struct{}
	for range cameras {
		select {
		case release := <-held:
			releases = append(releases, release)
		case <-time.After(5 * time.Second):
			t.Fatal("the cameras didn't reach the cloud")
		}
	}

	// the first camera gets its 401 and logs in again, then the others
	close(releases[0])
	require.Eventually(t, func() bool {
		logins, _ := f.count()
		return logins == 2
	}, 5*time.Second, time.Millisecond)
	for _, release := range releases[1:] {
		close(release)
	}

	for range cameras {
		select {
		case err := <-errs:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("a camera didn't get an answer")
		}
	}
	logins, _ := f.count()
	require.Equal(t, 2, logins, "one new login for all of them")
}

// A 401 after the new login is no reason for another one: the request fails
// and the next dial tries again.
func TestCloudRequestLogsInAgainOnlyOnce(t *testing.T) {
	f := useFakeCloud(t)
	f.status = http.StatusUnauthorized

	err := request(t)
	require.ErrorIs(t, err, xiaomi.ErrUnauthorized)
	logins, requests := f.count()
	require.Equal(t, 2, logins)
	require.Equal(t, 2, requests)
}

// Any other error of the cloud keeps the session.
func TestCloudRequestKeepsTheSessionOnOtherErrors(t *testing.T) {
	f := useFakeCloud(t)
	require.NoError(t, request(t))

	f.mu.Lock()
	f.status = http.StatusInternalServerError
	f.mu.Unlock()

	err := request(t)
	require.EqualError(t, err, "500 Internal Server Error")
	require.NotErrorIs(t, err, xiaomi.ErrUnauthorized)
	logins, _ := f.count()
	require.Equal(t, 1, logins)
}

// A passToken the account server no longer takes fails the new login. The
// failure isn't kept: the next dial logs in once more.
func TestCloudRequestRetriesAFailedLogin(t *testing.T) {
	f := useFakeCloud(t)
	require.NoError(t, request(t))

	f.expire()
	cloudsMu.Lock()
	tokens[testUser] = "revoked"
	cloudsMu.Unlock()
	require.Error(t, request(t))

	cloudsMu.Lock()
	tokens[testUser] = testPassToken // logged in again in the UI
	cloudsMu.Unlock()
	require.NoError(t, request(t))
	logins, _ := f.count()
	require.Equal(t, 2, logins)
}
