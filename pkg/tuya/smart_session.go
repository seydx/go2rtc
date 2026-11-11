package tuya

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

var errSessionLost = errors.New("session expired")

// Tuya refuses a second login within seconds, a failed login is not repeated before this
var loginRetryDelay = 30 * time.Second

type smartSession struct {
	client *http.Client

	mu       sync.Mutex
	logins   int
	mqttsUrl string
	err      error
	failedAt time.Time
}

var (
	smartSessionsMu sync.Mutex
	smartSessions   = map[string]*smartSession{}
)

func getSmartSession(host, email, password string) *smartSession {
	key := host + "\n" + email + "\n" + password

	smartSessionsMu.Lock()
	defer smartSessionsMu.Unlock()

	s := smartSessions[key]
	if s == nil {
		s = &smartSession{client: CreateHTTPClientWithSession()}
		smartSessions[key] = s
	}
	return s
}

func (s *smartSession) state() (logins int, mqttsUrl string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins, s.mqttsUrl
}

// login runs passwordLogin unless another caller logged in after the state seen
func (s *smartSession) login(seen int, passwordLogin func() (string, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.logins != seen {
		return nil
	}
	if s.err != nil && time.Since(s.failedAt) < loginRetryDelay {
		return s.err
	}

	mqttsUrl, err := passwordLogin()
	if err != nil {
		s.err, s.failedAt = err, time.Now()
		return err
	}

	s.logins++
	s.mqttsUrl, s.err = mqttsUrl, nil
	return nil
}

func smartApiError(code, msg string) error {
	switch code {
	case "USER_SESSION_LOSS", "USER_SESSION_INVALID":
		return errSessionLost
	}
	return errors.New(msg)
}
