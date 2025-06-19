package streams

import (
	"net/url"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/preload"
)

var (
    preloads    = map[string]*preload.Preload{}
    preloadsMu  sync.Mutex
)

func (s *Stream) Preload(cons *preload.Preload, query url.Values) error {
	if !query.Has("silent") {
		if err := s.AddConsumer(cons); err != nil {
			return err
		}
	}
	return nil
}

func Preload(src string, rawQuery string) {
    preloadsMu.Lock()
    defer preloadsMu.Unlock()

	stream := Get(src)
	if stream == nil {
		return
	}

	if _, ok := preloads[src]; ok {
		return
	}

	query := ParseQuery(rawQuery)
	cons := preload.NewPreload(src, query)
	
	preloads[src] = cons 

	if err := stream.Preload(cons, query); err != nil {
		log.Error().Err(err).Caller().Send()
	}
}
