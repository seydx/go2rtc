package cui

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type Client struct {
	URL string
}

const (
	maxRetries  = 10
	retryDelay  = 1 * time.Second
	httpTimeout = 10 * time.Second
)

func NewClient(rawURL string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	query := u.Query()
	useInsecure := query.Get("insecure") == "1"

	var client http.Client

	if !useInsecure {
		u.Scheme = "https"
		tr := http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client = http.Client{Transport: &tr, Timeout: httpTimeout}
	} else {
		u.Scheme = "http"
		client = http.Client{Timeout: httpTimeout}
	}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		streamURL, err := fetchStreamURL(&client, u.String())
		if err == nil {
			return &Client{URL: streamURL}, nil
		}

		lastErr = err

		if attempt < maxRetries {
			time.Sleep(retryDelay)
		}
	}

	return nil, fmt.Errorf("failed after %d attempts: %w", maxRetries, lastErr)
}

var errNoStreamURL = errors.New("no streamUrl in response")

func fetchStreamURL(client *http.Client, url string) (string, error) {
	res, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return "", errors.New(res.Status)
	}

	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", err
	}

	if streamURL, ok := body["streamUrl"]; ok {
		return streamURL, nil
	}

	return "", errNoStreamURL
}
