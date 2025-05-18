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
		client = http.Client{Transport: &tr, Timeout: 10 * time.Second}
	} else {
		u.Scheme = "http"
		client = http.Client{Timeout: 10 * time.Second}
	}

	res, err := client.Get(u.String())
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errors.New(res.Status)
	}

	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}

	if streamUrl, ok := body["streamUrl"]; ok {
		return &Client{URL: streamUrl}, nil
	}

	return nil, fmt.Errorf("no streamUrl found in response")
}
