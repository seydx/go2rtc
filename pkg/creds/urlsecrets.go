package creds

import (
	"net/url"
	"strings"
)

// secretQueryKeys are query parameters whose values are credentials in the
// source handlers: cloud tokens and passwords (nest, ring, tuya, hass, yandex,
// webtorrent) and device keys (wyze enr, xiaomi client_private and sign).
// Identifiers (client_id, device_id, uid, mac) and public keys stay readable.
var secretQueryKeys = map[string]bool{
	"password":       true,
	"pass":           true,
	"pwd":            true,
	"secret":         true,
	"client_secret":  true,
	"refresh_token":  true,
	"access_token":   true,
	"token":          true,
	"x_token":        true,
	"api_key":        true,
	"key":            true,
	"auth":           true,
	"sign":           true,
	"enr":            true,
	"client_private": true,
}

// schemeSecretQueryKeys are credentials only for one scheme, their names are
// too generic to mask for every source (ex. roborock s and k).
var schemeSecretQueryKeys = map[string]map[string]bool{
	"roborock": {"s": true, "k": true},
}

// A registered secret is replaced everywhere it appears, so a short value (a
// port, a channel number) would mask unrelated text. Real credentials are longer.
const minSecretLen = 8

// AddURLSecrets registers the credential query parameters of a source as
// secrets. Values written literally into a source URL were not registered
// anywhere, so they reached the log and the streams API in clear text.
//
// A source can be a command line (exec, ffmpeg) with several URLs, so every
// whitespace separated token is checked. A #fragment holds go2rtc params and
// is not part of the value. Values are registered as written and decoded,
// because logs show the encoded form.
func AddURLSecrets(source string) {
	for _, token := range strings.Fields(source) {
		token = strings.Trim(token, `"'`)
		token, _, _ = strings.Cut(token, "#")

		i := strings.IndexByte(token, '?')
		if i < 0 {
			continue
		}

		scheme, _, _ := strings.Cut(token[:i], ":")
		schemeKeys := schemeSecretQueryKeys[strings.ToLower(scheme)]

		for _, pair := range strings.Split(token[i+1:], "&") {
			key, value, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			if s, err := url.QueryUnescape(key); err == nil {
				key = s
			}
			key = strings.ToLower(key)
			if !secretQueryKeys[key] && !schemeKeys[key] {
				continue
			}

			addURLSecret(value)
			if s, err := url.QueryUnescape(value); err == nil && s != value {
				addURLSecret(s)
			}
		}
	}
}

func addURLSecret(value string) {
	if len(value) >= minSecretLen {
		AddSecret(value)
	}
}
