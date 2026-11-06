package onvif

import (
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/onvif"
	"github.com/AlexxIT/go2rtc/pkg/yaml"
)

// sourceConfig is a video source of the config: the input of a camera, its
// streams are the profiles, the highest quality first
type sourceConfig struct {
	Token    string   `yaml:"token"` // the first profile without one
	Name     string   `yaml:"name"`
	Profiles []string `yaml:"profiles"`
}

// sourceConfigs are the video sources of the config: only their streams are
// profiles, in their order. Nil without video_sources in the config, then
// every stream is a profile. Never changed, only replaced.
var sourceConfigs []sourceConfig
var sourceConfigsMu sync.Mutex

// setSourceConfigs takes the video sources of the config, nil without any. A
// stream is the profile of one source only: the client asks for it by its name.
func setSourceConfigs(configs []sourceConfig) {
	var list []sourceConfig
	if configs != nil {
		list = []sourceConfig{} // an empty list: no profiles at all
	}
	seen := map[string]bool{}
	for _, cfg := range configs {
		var profiles []string
		for _, name := range cfg.Profiles {
			if name == "" || seen[name] {
				log.Warn().Str("stream", name).Msg("[onvif] skip a profile listed twice or without name")
				continue
			}
			seen[name] = true
			profiles = append(profiles, name)
		}
		if len(profiles) == 0 {
			continue
		}
		cfg.Profiles = profiles
		if cfg.Token == "" {
			cfg.Token = profiles[0]
		}
		list = append(list, cfg)
	}

	sourceConfigsMu.Lock()
	sourceConfigs = list
	sourceConfigsMu.Unlock()
}

// reloadConfig takes the video sources and the listener of the config the API
// saved: camera.ui adds and removes its cameras and moves or stops the server
// without a restart. Open connections stay.
func reloadConfig(data []byte) {
	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Warn().Err(err).Msg("[onvif] settings of the saved config")
		return
	}
	setSourceConfigs(cfg.Mod.VideoSources)
	listen(cfg.Mod.Listen)
}

// configs returns the video sources the server offers: the ones of the config,
// or one per stream, in a stable order (clients take the cameras by their
// position, ex. Dahua its channels)
func configs() []sourceConfig {
	sourceConfigsMu.Lock()
	list := sourceConfigs
	sourceConfigsMu.Unlock()
	if list != nil {
		return list
	}
	names := streams.GetAllNames()
	slices.Sort(names)
	configs := make([]sourceConfig, len(names))
	for i, name := range names {
		configs[i] = sourceConfig{Token: name, Profiles: []string{name}}
	}
	return configs
}

// profileName: the name of the source with its position, or the stream name
func (cfg *sourceConfig) profileName(i int) string {
	switch {
	case cfg.Name == "":
		return cfg.Profiles[i]
	case len(cfg.Profiles) == 1:
		return cfg.Name
	}
	return cfg.Name + " " + strconv.Itoa(i+1)
}

// describe describes the streams of the sources from what they send. A stream
// missing from go2rtc is left out, a source without any of its streams too.
func describe(configs []sourceConfig) []*onvif.VideoSource {
	videos := map[string]onvif.Video{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, cfg := range configs {
		for _, name := range cfg.Profiles {
			stream := streams.Get(name)
			if stream == nil {
				continue
			}
			// in parallel: a stream that never ran is dialed for it
			wg.Go(func() {
				video := describeStream(name, stream)
				mu.Lock()
				videos[name] = video
				mu.Unlock()
			})
		}
	}
	wg.Wait()

	var sources []*onvif.VideoSource
	for _, cfg := range configs {
		source := &onvif.VideoSource{Token: cfg.Token}
		for i, name := range cfg.Profiles {
			video, ok := videos[name]
			if !ok {
				continue
			}
			source.Profiles = append(source.Profiles, &onvif.MediaProfile{
				Token: name, Name: cfg.profileName(i), Source: source, Video: video,
			})
		}
		if len(source.Profiles) > 0 {
			sources = append(sources, source)
		}
	}
	return sources
}

// describeStream describes the video of a stream from its offers: what its
// source sends, or sent last time. A stream whose size is unknown is probed:
// a stream that never ran, or one that tells its parameter sets in-band only.
func describeStream(name string, stream *streams.Stream) onvif.Video {
	// a stream with other sources is another camera: what was probed or kept
	// of the stream is of its sources
	key := name + "\n" + strings.Join(stream.Sources(), "\n")

	offers := streams.OffersOf(stream)
	codec := videoOffer(offers)
	switch {
	case codec != nil && hasSPS(codec.Codec, codec.Fmtp):
		return keepVideo(key, codec)
	case codec == nil && offers.State != streams.OffersUnknown:
		return onvif.NewVideo("", "") // no H.264 or H.265 to describe (ex. MJPEG)
	}

	// Tapo sends its parameter sets with the keyframes only: every session
	// starts without them, and a consumer that passes the packets on (RTSP)
	// doesn't read them. The stream keeps the video it told before.
	if codec != nil {
		if video, ok := keptVideo(key, codec.Codec); ok {
			return video
		}
	}

	if probeOnce(key, stream) {
		if probed := videoOffer(streams.OffersOf(stream)); probed != nil {
			if codec = probed; hasSPS(codec.Codec, codec.Fmtp) {
				return keepVideo(key, codec)
			}
		}
	}
	if codec == nil {
		return onvif.NewVideo("", "")
	}
	return onvif.NewVideo(codec.Codec, codec.Fmtp)
}

// videoOffer is the first H.264 or H.265 video of offers
func videoOffer(offers *streams.Offers) *streams.OfferCodec {
	for _, codec := range offers.Video {
		if codec.Codec == core.CodecH264 || codec.Codec == core.CodecH265 {
			return codec
		}
	}
	return nil
}

// keptVideos are the videos the streams told with their parameter sets, by
// stream name and sources, with the codec they were told of
var keptVideos = map[string]keptVideoOf{}
var keptVideosMu sync.Mutex

type keptVideoOf struct {
	codec string
	video onvif.Video
}

func keepVideo(key string, codec *streams.OfferCodec) onvif.Video {
	video := onvif.NewVideo(codec.Codec, codec.Fmtp)
	keptVideosMu.Lock()
	keptVideos[key] = keptVideoOf{codec: codec.Codec, video: video}
	keptVideosMu.Unlock()
	return video
}

// keptVideo is the video the stream told before, unless its codec changed
func keptVideo(key, codec string) (onvif.Video, bool) {
	keptVideosMu.Lock()
	kept, ok := keptVideos[key]
	keptVideosMu.Unlock()
	if !ok || kept.codec != codec {
		return onvif.Video{}, false
	}
	return kept.video, true
}

func profilesOf(sources []*onvif.VideoSource) (profiles []*onvif.MediaProfile) {
	for _, source := range sources {
		profiles = append(profiles, source.Profiles...)
	}
	return
}

// findProfile describes the source of the profile with token, only that one
func findProfile(token string) *onvif.MediaProfile {
	for _, cfg := range configs() {
		if !slices.Contains(cfg.Profiles, token) {
			continue
		}
		for _, profile := range profilesOf(describe([]sourceConfig{cfg})) {
			if profile.Token == token {
				return profile
			}
		}
		return nil
	}
	return nil
}

func findSource(token string) *onvif.VideoSource {
	for _, cfg := range configs() {
		if cfg.Token != token {
			continue
		}
		if sources := describe([]sourceConfig{cfg}); len(sources) > 0 {
			return sources[0]
		}
		return nil
	}
	return nil
}

// optionSources are the video sources of the options a client asks for: the one
// of the configuration, the one of the profile, both have to match. Without a
// token all of them. Nil with the subcode of the fault.
func optionSources(configToken, profileToken string) ([]*onvif.VideoSource, string) {
	var source *onvif.VideoSource
	if profileToken != "" {
		profile := findProfile(profileToken)
		if profile == nil {
			return nil, "NoProfile"
		}
		source = profile.Source
	}
	if configToken != "" {
		if source == nil {
			source = findSource(configToken)
		}
		// the token of a source configuration is the one of its source
		if source == nil || source.Token != configToken {
			return nil, "NoConfig"
		}
	}
	if source != nil {
		return []*onvif.VideoSource{source}, ""
	}
	if sources := describe(configs()); len(sources) > 0 {
		return sources, ""
	}
	return nil, "NoConfig"
}

// isProfile tells if token is a profile, without describing it
func isProfile(token string) bool {
	for _, cfg := range configs() {
		if slices.Contains(cfg.Profiles, token) {
			return streams.Get(token) != nil
		}
	}
	return false
}
