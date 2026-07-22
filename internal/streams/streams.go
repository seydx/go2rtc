package streams

import (
	"errors"
	"maps"
	"net/url"
	"sync"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/rs/zerolog"
)

var ffmpegBin string

func Init() {
	var cfg struct {
		Streams map[string]any    `yaml:"streams"`
		Publish map[string]any    `yaml:"publish"`
		Preload map[string]string `yaml:"preload"`
		FFmpeg  map[string]string `yaml:"ffmpeg"`
	}

	app.LoadConfig(&cfg)

	log = app.GetLogger("streams")

	ffmpegBin = cfg.FFmpeg["bin"]
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg" // Default fallback
	}

	for name, item := range cfg.Streams {
		streams[name] = NewStream(item)
	}

	api.HandleFunc("api/streams", apiStreams)
	api.HandleFunc("api/consumer", apiConsumer)
	api.HandleFunc("api/streams.dot", apiStreamsDOT)
	api.HandleFunc("api/preload", apiPreload)
	api.HandleFunc("api/schemes", apiSchemes)

	// Store deferred config — StartPreloads() must be called after all modules are initialized
	deferredPublish = cfg.Publish
	deferredPreload = cfg.Preload
}

// deferred config from Init() — started by StartPreloads() after all modules are ready
var deferredPublish map[string]any
var deferredPreload map[string]string

// StartPreloads starts publish and preload streams. Must be called after all
// protocol handlers (rtsp, tapo, ffmpeg, etc.) have been registered via Init().
func StartPreloads() {
	if deferredPublish == nil && len(deferredPreload) == 0 {
		return
	}

	go func() {
		// Publish streams in parallel (same rationale as preloads)
		if len(deferredPublish) > 0 {
			var pubWg sync.WaitGroup
			pubWg.Add(len(deferredPublish))
			for name, dst := range deferredPublish {
				go func(name string, dst any) {
					defer pubWg.Done()
					if stream := Get(name); stream != nil {
						Publish(stream, dst)
					}
				}(name, dst)
			}
			pubWg.Wait()
		}

		count := len(deferredPreload)
		if count == 0 {
			return
		}

		log.Debug().Int("count", count).Msg("[preload] starting all preloads")

		// Start all preloads in parallel — each stream dials a different camera,
		// trackMu per-producer prevents concurrent protocol ops on the same connection.
		var wg sync.WaitGroup
		wg.Add(count)

		for name, query := range deferredPreload {
			go func(name, query string) {
				defer wg.Done()
				if err := AddPreload(name, query); err != nil {
					log.Error().Err(err).Str("name", name).Msg("[preload] failed")
				}
			}(name, query)
		}

		wg.Wait()

		log.Debug().Int("count", count).Msg("[preload] all preloads started")

		// Clear references
		deferredPublish = nil
		deferredPreload = nil
	}()
}

func New(name string, sources ...string) (*Stream, error) {
	decodedSources := make([]string, len(sources))

	for i, source := range sources {
		if !HasProducer(source) {
			return nil, errors.New("streams: source not supported")
		}

		if err := Validate(source); err != nil {
			return nil, err
		}

		decoded, err := DecodeExecSource(source)
		if err != nil {
			log.Error().Err(err).Msg("Failed to decode base64 exec command")
			return nil, err
		}
		decodedSources[i] = decoded
	}

	stream := NewStream(decodedSources)

	streamsMu.Lock()
	streams[name] = stream
	streamsMu.Unlock()

	return stream, nil
}

func Patch(name string, source string) (*Stream, error) {
	streamsMu.Lock()
	defer streamsMu.Unlock()

	// check if source links to some stream name from go2rtc
	if u, err := url.Parse(source); err == nil && u.Scheme == "rtsp" && len(u.Path) > 1 {
		rtspName := u.Path[1:]
		if stream, ok := streams[rtspName]; ok {
			if streams[name] != stream {
				// link (alias) streams[name] to streams[rtspName]
				streams[name] = stream
			}
			return stream, nil
		}
	}

	if stream, ok := streams[source]; ok {
		if name != source {
			// link (alias) streams[name] to streams[source]
			streams[name] = stream
		}
		return stream, nil
	}

	// check if src has supported scheme
	if !HasProducer(source) {
		return nil, errors.New("streams: source not supported")
	}

	if err := Validate(source); err != nil {
		return nil, err
	}

	// check an existing stream with this name
	if stream, ok := streams[name]; ok {
		stream.SetSource(source)
		return stream, nil
	}

	// create new stream with this name
	stream := NewStream(source)
	streams[name] = stream
	return stream, nil
}

func GetOrPatch(query url.Values) (*Stream, error) {
	// check if src param exists
	source := query.Get("src")
	if source == "" {
		return nil, errors.New("streams: source empty")
	}

	// check if src is stream name
	if stream := Get(source); stream != nil {
		return stream, nil
	}

	// check if name param provided
	if name := query.Get("name"); name != "" {
		return Patch(name, source)
	}

	// return new stream with src as name
	return Patch(source, source)
}

var log zerolog.Logger

// streams map

var streams = map[string]*Stream{}
var streamsMu sync.Mutex

func Get(name string) *Stream {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	return streams[name]
}

func Delete(name string) {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	delete(streams, name)
}

func GetAllNames() []string {
	streamsMu.Lock()
	names := make([]string, 0, len(streams))
	for name := range streams {
		names = append(names, name)
	}
	streamsMu.Unlock()
	return names
}

func GetAllSources() map[string][]string {
	streamsMu.Lock()
	sources := make(map[string][]string, len(streams))
	for name, stream := range streams {
		sources[name] = stream.Sources()
	}
	streamsMu.Unlock()
	return sources
}

func GetAll() map[string]*Stream {
	streamsMu.Lock()
	result := make(map[string]*Stream, len(streams))
	maps.Copy(result, streams)
	streamsMu.Unlock()
	return result
}

func KillConsumer(id uint32) bool {
	for _, s := range GetAll() {
		if s.RemoveConsumerByID(id) {
			return true
		}
	}
	return false
}

func KillConsumersByTag(tag string) int {
	if tag == "" {
		return 0
	}
	n := 0
	for _, s := range GetAll() {
		n += s.RemoveConsumersByTag(tag)
	}
	return n
}
