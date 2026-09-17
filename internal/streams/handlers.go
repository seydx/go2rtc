package streams

import (
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

type Handler func(source string) (core.Producer, error)

// handlers and redirects are written by module Init() at startup, but tests
// register while streams already run — and a concurrent map read/write is a
// hard crash, not just a race.
var handlers = map[string]Handler{}
var redirectsMu sync.RWMutex

func HandleFunc(scheme string, handler Handler) {
	redirectsMu.Lock()
	handlers[scheme] = handler
	redirectsMu.Unlock()
}

func getHandler(scheme string) (Handler, bool) {
	redirectsMu.RLock()
	defer redirectsMu.RUnlock()
	handler, ok := handlers[scheme]
	return handler, ok
}

func getRedirect(scheme string) (Redirect, bool) {
	redirectsMu.RLock()
	defer redirectsMu.RUnlock()
	redirect, ok := redirects[scheme]
	return redirect, ok
}

func schemeNames() ([]string, []string) {
	redirectsMu.RLock()
	defer redirectsMu.RUnlock()
	h := make([]string, 0, len(handlers))
	for scheme := range handlers {
		h = append(h, scheme)
	}
	r := make([]string, 0, len(redirects))
	for scheme := range redirects {
		r = append(r, scheme)
	}
	return h, r
}

func SupportedSchemes() []string {
	handlerSchemes, redirectSchemes := schemeNames()
	uniqueKeys := make(map[string]struct{}, len(handlerSchemes)+len(redirectSchemes))
	for _, scheme := range handlerSchemes {
		uniqueKeys[scheme] = struct{}{}
	}
	for _, scheme := range redirectSchemes {
		uniqueKeys[scheme] = struct{}{}
	}
	resultKeys := make([]string, 0, len(uniqueKeys))
	for key := range uniqueKeys {
		resultKeys = append(resultKeys, key)
	}
	return resultKeys
}

func HasProducer(url string) bool {
	if i := strings.IndexByte(url, ':'); i > 0 {
		scheme := url[:i]

		if _, ok := getHandler(scheme); ok {
			return true
		}

		if _, ok := getRedirect(scheme); ok {
			return true
		}
	}

	return false
}

func GetProducer(url string) (core.Producer, error) {
	if i := strings.IndexByte(url, ':'); i > 0 {
		scheme := url[:i]

		if redirect, ok := getRedirect(scheme); ok {
			location, err := redirect(url)
			if err != nil {
				return nil, err
			}
			if location != "" {
				return GetProducer(location)
			}
		}

		if handler, ok := getHandler(scheme); ok {
			prod, err := handler(url)
			if prod == nil && err == nil {
				// every caller uses the producer on a nil error: a reconnect
				// would crash on it instead of backing off
				err = errors.New("streams: no producer for " + url)
			}
			return prod, err
		}
	}

	return nil, errors.New("streams: unsupported scheme: " + url)
}

// Redirect can return: location URL or error or empty URL and error
type Redirect func(url string) (string, error)

var redirects = map[string]Redirect{}

func RedirectFunc(scheme string, redirect Redirect) {
	redirectsMu.Lock()
	redirects[scheme] = redirect
	redirectsMu.Unlock()
}

func Location(url string) (string, error) {
	if i := strings.IndexByte(url, ':'); i > 0 {
		scheme := url[:i]

		if redirect, ok := getRedirect(scheme); ok {
			return redirect(url)
		}
	}

	return "", nil
}

// TODO: rework

type ConsumerHandler func(url string) (core.Consumer, func(), error)

var consumerHandlers = map[string]ConsumerHandler{}

func HandleConsumerFunc(scheme string, handler ConsumerHandler) {
	consumerHandlers[scheme] = handler
}

func GetConsumer(url string) (core.Consumer, func(), error) {
	if i := strings.IndexByte(url, ':'); i > 0 {
		scheme := url[:i]

		if handler, ok := consumerHandlers[scheme]; ok {
			return handler(url)
		}
	}

	return nil, nil, errors.New("streams: unsupported scheme: " + url)
}

var insecure = map[string]bool{}

func MarkInsecure(scheme string) {
	insecure[scheme] = true
}

var sanitize = regexp.MustCompile(`\s`)

func Validate(source string) error {
	// TODO: Review the entire logic of insecure sources
	if i := strings.IndexByte(source, ':'); i > 0 {
		if insecure[source[:i]] {
			return errors.New("streams: source from insecure producer")
		}
	}
	isBase64Exec := strings.HasPrefix(source, "exec:base64:")
	if sanitize.MatchString(source) && !isBase64Exec {
		return errors.New("streams: source with spaces may be insecure")
	}
	return nil
}

func DecodeExecSource(source string) (string, error) {
	if strings.HasPrefix(source, "exec:base64:") {
		encodedPart := strings.TrimPrefix(source, "exec:base64:")
		decodedBytes, err := base64.StdEncoding.DecodeString(encodedPart)
		if err != nil {
			return "", err
		}
		return "exec:" + string(decodedBytes), nil
	}
	return source, nil
}

func DecodeSources(sources ...string) ([]string, error) {
	decodedSources := make([]string, len(sources))

	for i, source := range sources {
		decodedSource, err := DecodeExecSource(source)
		if err != nil {
			log.Error().Err(err).Msg("Failed to decode base64 exec command")
			return nil, err
		}
		decodedSources[i] = decodedSource
	}

	return decodedSources, nil
}
