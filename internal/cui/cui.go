package cui

import (
	"strings"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/cui"
	"github.com/rs/zerolog"
)

func Init() {
	log = app.GetLogger("cui")

	streams.RedirectFunc("cui", redirectCui)
}

var log zerolog.Logger

// redirectCui resolves a camera.ui stream to the URL its plugin serves. The
// options on the cui URL describe the source, not the lookup, so they travel
// on to the connection that is actually made.
func redirectCui(rawURL string) (string, error) {
	rawURL, rawQuery, _ := strings.Cut(rawURL, "#")

	client, err := cui.NewClient(rawURL)
	if err != nil {
		return "", err
	}

	log.Debug().Msgf("[cui] new uri=%s", client.URL)

	if rawQuery != "" {
		return client.URL + "#" + rawQuery, nil
	}
	return client.URL, nil
}
