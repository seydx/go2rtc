package onvif

import (
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/mjpeg"
	"github.com/AlexxIT/go2rtc/internal/rtsp"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/onvif"
	"github.com/rs/zerolog"
)

func Init() {
	var cfg struct {
		Mod struct {
			Listen string `yaml:"listen"`
		} `yaml:"onvif"`
	}

	app.LoadConfig(&cfg)

	log = app.GetLogger("onvif")

	streams.HandleFunc("onvif", streamOnvif)

	// ONVIF client autodiscovery
	api.HandleFunc("api/onvif", apiOnvif)

	// ONVIF server: plain HTTP on a listener of its own, off without one. On
	// the API it was wherever the API was, and the API came along with it.
	if cfg.Mod.Listen == "" {
		return
	}

	ln, err := net.Listen("tcp", cfg.Mod.Listen)
	if err != nil {
		log.Error().Err(err).Msg("[onvif] listen")
		return
	}

	log.Info().Str("addr", cfg.Mod.Listen).Msg("[onvif] listen")

	go serve(ln)
}

var log zerolog.Logger

const snapshotPath = "/onvif/snapshot"

func serve(ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("/onvif/", onvifDeviceService) // the services on all suburls
	mux.HandleFunc(snapshotPath, onvifSnapshot)

	server := http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := server.Serve(ln); err != nil {
		log.Error().Err(err).Msg("[onvif] serve")
	}
}

// onvifSnapshot serves the snapshot URI for the streams of the config only:
// for any other src the snapshot handler creates a stream with it as source,
// and the ONVIF port has no login.
func onvifSnapshot(w http.ResponseWriter, r *http.Request) {
	if streams.Get(r.URL.Query().Get("src")) == nil {
		http.Error(w, api.StreamNotFound, http.StatusNotFound)
		return
	}
	mjpeg.HandlerKeyframe(w, r)
}

func streamOnvif(rawURL string) (core.Producer, error) {
	client, err := onvif.NewClient(rawURL)
	if err != nil {
		return nil, err
	}

	uri, err := client.GetURI()
	if err != nil {
		return nil, err
	}

	// Forward query params from the onvif:// URL to the resolved RTSP URL,
	// except onvif-specific keys (subtype/snapshot/timeout) that are only used
	// for SOAP discovery. This lets users pass things like `transport=udp` to
	// work around camera firmware bugs in TCP-interleaved mode.
	if u, err := url.Parse(onvif.SanitizeQuery(rawURL)); err == nil {
		extra := u.Query()
		extra.Del("subtype")
		extra.Del("snapshot")
		extra.Del("timeout")
		if encoded := extra.Encode(); encoded != "" {
			if strings.Contains(uri, "?") {
				uri += "&" + encoded
			} else {
				uri += "?" + encoded
			}
		}
	}

	// Append hash-based arguments to the retrieved URI
	if i := strings.IndexByte(rawURL, '#'); i > 0 {
		uri += rawURL[i:]
	}

	log.Debug().Msgf("[onvif] new uri=%s", uri)

	if err = streams.Validate(uri); err != nil {
		return nil, err
	}

	return streams.GetProducer(uri)
}

func onvifDeviceService(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	operation := onvif.GetRequestAction(b)
	if operation == "" {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}

	log.Trace().Msgf("[onvif] server request %s %s:\n%s", r.Method, r.RequestURI, b)

	const protocol = "http" // the listener of the ONVIF server has no TLS

	switch operation {
	case onvif.ServiceGetServiceCapabilities, // important for Hass
		onvif.DeviceGetNetworkInterfaces, // important for Hass
		onvif.DeviceGetSystemDateAndTime, // important for Hass
		onvif.DeviceSetSystemDateAndTime, // return just OK
		onvif.DeviceGetDiscoveryMode,
		onvif.DeviceGetDNS,
		onvif.DeviceGetHostname,
		onvif.DeviceGetNetworkDefaultGateway,
		onvif.DeviceGetNetworkProtocols,
		onvif.DeviceGetNTP,
		onvif.DeviceGetScopes,
		onvif.MediaGetVideoEncoderConfiguration,
		onvif.MediaGetVideoEncoderConfigurations,
		onvif.MediaGetAudioEncoderConfigurations,
		onvif.MediaGetVideoEncoderConfigurationOptions,
		onvif.MediaGetAudioSources,
		onvif.MediaGetAudioSourceConfigurations:
		b = onvif.StaticResponse(operation)

	case onvif.DeviceGetCapabilities:
		// important for Hass: Media section
		b = onvif.GetCapabilitiesResponse(protocol, r.Host)

	case onvif.DeviceGetServices:
		b = onvif.GetServicesResponse(protocol, r.Host)

	case onvif.DeviceGetDeviceInformation:
		// important for Hass: SerialNumber (unique server ID)
		b = onvif.GetDeviceInformationResponse("", "go2rtc", app.Version, r.Host)

	case onvif.MediaGetVideoSources:
		b = onvif.GetVideoSourcesResponse(profileNames())

	case onvif.MediaGetProfiles:
		// important for Hass: H264 codec, width, height
		b = onvif.GetProfilesResponse(profileNames())

	case onvif.MediaGetProfile:
		b = onvif.GetProfileResponse(findToken(b, "ProfileToken"))

	case onvif.MediaGetVideoSourceConfigurations:
		// important for Happytime Onvif Client
		b = onvif.GetVideoSourceConfigurationsResponse(profileNames())

	case onvif.MediaGetVideoSourceConfiguration:
		b = onvif.GetVideoSourceConfigurationResponse(findToken(b, "ConfigurationToken"))

	case onvif.MediaGetStreamUri:
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host // in case of Host without port
		}

		// the RTSP server finds the stream by the decoded path
		uri := "rtsp://" + host + ":" + rtsp.Port + "/" + url.PathEscape(findToken(b, "ProfileToken"))
		b = onvif.GetStreamUriResponse(uri)

	case onvif.MediaGetSnapshotUri:
		uri := protocol + "://" + r.Host + snapshotPath + "?src=" + url.QueryEscape(findToken(b, "ProfileToken"))
		b = onvif.GetSnapshotUriResponse(uri)

	default:
		http.Error(w, "unsupported operation", http.StatusBadRequest)
		log.Warn().Msgf("[onvif] unsupported operation: %s", operation)
		log.Debug().Msgf("[onvif] unsupported request:\n%s", b)
		return
	}

	log.Trace().Msgf("[onvif] server response:\n%s", b)

	w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
	if _, err = w.Write(b); err != nil {
		log.Error().Err(err).Caller().Send()
	}
}

// profileNames returns the names of the streams in a stable order: clients
// take the cameras by their position in the list (ex. Dahua its channels)
func profileNames() []string {
	names := streams.GetAllNames()
	slices.Sort(names)
	return names
}

// findToken returns the stream name a token of the request stands for
func findToken(b []byte, tag string) string {
	return html.UnescapeString(onvif.FindTagValue(b, tag))
}

func apiOnvif(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("src")

	var items []*api.Source

	if src == "" {
		// optional ?timeout=SECONDS - WS-Discovery response window (default 5s,
		// max 60s), slow cameras may not answer within the default window
		var timeout time.Duration
		if s := r.URL.Query().Get("timeout"); s != "" {
			sec, err := strconv.Atoi(s)
			if err != nil || sec < 1 || sec > 60 {
				http.Error(w, "invalid timeout, expected seconds in range 1..60", http.StatusBadRequest)
				return
			}
			timeout = time.Duration(sec) * time.Second
		}

		devices, err := onvif.DiscoveryStreamingDevices(timeout)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		for _, device := range devices {
			u, err := url.Parse(device.URL)
			if err != nil {
				log.Warn().Str("url", device.URL).Msg("[onvif] broken")
				continue
			}

			if u.Scheme != "http" {
				log.Warn().Str("url", device.URL).Msg("[onvif] unsupported")
				continue
			}

			u.Scheme = "onvif"
			u.User = url.UserPassword("user", "pass")

			if u.Path == onvif.PathDevice {
				u.Path = ""
			}

			items = append(items, &api.Source{
				Name: u.Host,
				URL:  u.String(),
				Info: device.Name + " " + device.Hardware,
			})
		}
	} else {
		client, err := onvif.NewClient(src)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if l := log.Trace(); l.Enabled() {
			b, _ := client.MediaRequest(onvif.MediaGetProfiles)
			l.Msgf("[onvif] src=%s profiles:\n%s", src, b)
		}

		name, err := client.GetName()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		profiles, err := client.GetProfiles()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// src may already carry a query (or stale subtype/snapshot when probing a
		// stored source URL), rebuild it cleanly before appending our own params
		base, sep := src, "?"
		if u, err := url.Parse(onvif.SanitizeQuery(src)); err == nil {
			q := u.Query()
			q.Del("subtype")
			q.Del("snapshot")
			q.Del("timeout")
			u.RawQuery = q.Encode()
			base = u.String()
			if u.RawQuery != "" {
				sep = "&"
			}
		}

		for i, p := range profiles {
			items = append(items, &api.Source{
				Name:     name + " stream" + strconv.Itoa(i),
				URL:      base + sep + "subtype=" + p.Token,
				Encoding: p.Encoding,
				Width:    p.Width,
				Height:   p.Height,
			})
		}

		if len(profiles) > 0 && client.HasSnapshots() {
			items = append(items, &api.Source{
				Name: name + " snapshot",
				URL:  base + sep + "subtype=" + profiles[0].Token + "&snapshot",
			})
		}
	}

	api.ResponseSources(w, items)
}
