package cui

import (
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/cui"
	"github.com/rs/zerolog"
)

func Init() {
	log = app.GetLogger("cui")

	streams.HandleFunc("cui", streamCui)
}

var log zerolog.Logger

func streamCui(rawURL string) (core.Producer, error) {
	client, err := cui.NewClient(rawURL)
	if err != nil {
		return nil, err
	}

	log.Debug().Msgf("[cui] new uri=%s", client.URL)

	return streams.GetProducer(client.URL)
}
