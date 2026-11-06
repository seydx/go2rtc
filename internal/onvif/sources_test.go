package onvif

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/onvif"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

const (
	fmtpH264High = "packetization-mode=1;sprop-parameter-sets=Z2QAM6wVFKAoAPGQ,aO48sA==" // High, 2560x1920
	fmtpH264Base = "packetization-mode=1;sprop-parameter-sets=Z0IAHvQCgC3I,aM48gA=="     // Baseline, 1280x720
	fmtpH265Wide = "sprop-sps=QgEBAWAAAAMAAAMAAAMAAAMAmaAAoAgBaH+KrTuiS7/8AAQABbAgApMuADN/mAE="
)

// stubSource is how a source of the scheme stub: answers
type stubSource struct {
	codec   *core.Codec
	packets []*rtp.Packet // sent again and again while it runs
	err     error         // the dial fails
	block   chan struct{} // the dial waits for it
	dials   atomic.Int32
}

var stubs sync.Map // stream name => *stubSource

func init() {
	streams.HandleFunc("onvifstub", func(source string) (core.Producer, error) {
		v, _ := stubs.Load(strings.TrimPrefix(source, "onvifstub:"))
		stub := v.(*stubSource)
		stub.dials.Add(1)
		if stub.block != nil {
			<-stub.block
		}
		if stub.err != nil {
			return nil, stub.err
		}
		return &stubCamera{
			Connection: core.Connection{Medias: []*core.Media{{
				Kind:      core.KindVideo,
				Direction: core.DirectionRecvonly,
				Codecs:    []*core.Codec{stub.codec.Clone()},
			}}},
			packets: stub.packets,
			stopped: make(chan struct{}),
		}, nil
	})
}

type stubCamera struct {
	core.Connection
	packets []*rtp.Packet
	stopped chan struct{}
	once    sync.Once
}

func (c *stubCamera) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	receiver := core.NewReceiver(media, codec)
	c.Receivers = append(c.Receivers, receiver)
	return receiver, nil
}

func (c *stubCamera) Start() error {
	if len(c.packets) == 0 {
		<-c.stopped
		return nil
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for seq := uint16(0); ; {
		select {
		case <-c.stopped:
			return nil
		case <-ticker.C:
		}
		for _, packet := range c.packets {
			clone := *packet
			clone.SequenceNumber = seq
			clone.Timestamp = uint32(seq) * 3000
			seq++
			for _, receiver := range c.Receivers {
				receiver.Input(&clone)
			}
		}
	}
}

func (c *stubCamera) Stop() error {
	c.once.Do(func() { close(c.stopped) })
	return c.Connection.Stop()
}

// newStub adds a stream of a stub source
func newStub(t *testing.T, name string, stub *stubSource) *stubSource {
	stubs.Store(name, stub)
	_, err := streams.New(name, "onvifstub:"+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		streams.Delete(name)
		stubs.Delete(name)
	})
	return stub
}

// freshProbes forgets the probes and the kept videos of earlier tests
func freshProbes(t *testing.T) {
	reset := func() {
		probesMu.Lock()
		probes = map[string]*probe{}
		probesMu.Unlock()
		keptVideosMu.Lock()
		keptVideos = map[string]keptVideoOf{}
		keptVideosMu.Unlock()
	}
	reset()
	timeout, retry := probeTimeout, probeRetry
	t.Cleanup(func() {
		reset()
		probeTimeout, probeRetry = timeout, retry
	})
}

func useConfig(t *testing.T, configs ...sourceConfig) {
	setSourceConfigs(configs)
	t.Cleanup(func() { setSourceConfigs(nil) })
}

func h264Codec(fmtp string) *core.Codec {
	return &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96, FmtpLine: fmtp}
}

func h265Codec(fmtp string) *core.Codec {
	return &core.Codec{Name: core.CodecH265, ClockRate: 90000, PayloadType: 96, FmtpLine: fmtp}
}

type profileXML struct {
	Token    string `xml:"token,attr"`
	Name     string `xml:"Name"`
	Source   string `xml:"VideoSourceConfiguration>SourceToken"`
	Encoding string `xml:"VideoEncoderConfiguration>Encoding"`
	Width    int    `xml:"VideoEncoderConfiguration>Resolution>Width"`
	Height   int    `xml:"VideoEncoderConfiguration>Resolution>Height"`
}

func getProfiles(t *testing.T) map[string]profileXML {
	var res struct {
		Profiles []profileXML `xml:"Body>GetProfilesResponse>Profiles"`
	}
	unmarshal(t, request(t, "GetProfiles", ""), &res)
	profiles := map[string]profileXML{}
	for _, p := range res.Profiles {
		profiles[p.Token] = p
	}
	return profiles
}

// requestStatus sends an operation and returns the status with the body
func requestStatus(t *testing.T, operation, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest("POST", "/onvif/media_service", strings.NewReader(
		`<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>`+
			`<trt:`+operation+` xmlns:trt="http://www.onvif.org/ver10/media/wsdl">`+body+`</trt:`+operation+`>`+
			`</s:Body></s:Envelope>`,
	))
	req.Host = "192.168.1.2:1984"
	w := httptest.NewRecorder()
	onvifDeviceService(w, req)
	return w.Code, w.Body.Bytes()
}

// A profile told H264 1920x1080 for every stream: UniFi Protect showed an
// H.265 stream black and couldn't tell the main stream from the sub stream.
// The profiles tell what the streams send, a stream that never ran is dialed
// once for it, then the stream remembers.
func TestProfilesTellTheStreams(t *testing.T) {
	freshProbes(t)
	wide := newStub(t, "wide", &stubSource{codec: h265Codec(fmtpH265Wide)})
	base := newStub(t, "base", &stubSource{codec: h264Codec(fmtpH264Base)})

	for range 2 {
		profiles := getProfiles(t)
		require.Equal(t, profileXML{"wide", "wide", "wide", "H265", 5120, 1440}, profiles["wide"])
		require.Equal(t, profileXML{"base", "base", "base", "H264", 1280, 720}, profiles["base"])
	}
	require.Equal(t, int32(1), wide.dials.Load(), "dialed once, then remembered")
	require.Equal(t, int32(1), base.dials.Load(), "dialed once, then remembered")

	var encoder struct {
		Encoding string `xml:"Body>GetVideoEncoderConfigurationResponse>Configuration>Encoding"`
	}
	unmarshal(t, request(t, "GetVideoEncoderConfiguration", "<trt:ConfigurationToken>wide</trt:ConfigurationToken>"), &encoder)
	require.Equal(t, "H265", encoder.Encoding)
}

const (
	spsHigh, ppsHigh = "Z2QAM6wVFKAoAPGQ", "aO48sA==" // High, 2560x1920
	spsBase, ppsBase = "Z0IAHvQCgC3I", "aM48gA=="     // Baseline, 1280x720
)

var idr = []byte{0x65, 0x88, 0x84, 0x00, 0x33}

func decode(s string) []byte {
	b, _ := base64.StdEncoding.DecodeString(s)
	return b
}

// keyframeRTP is a keyframe with its parameter sets, one NAL unit a packet
func keyframeRTP(sps, pps string) []*rtp.Packet {
	nalu := func(payload []byte, marker bool) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, Marker: marker}, Payload: payload}
	}
	return []*rtp.Packet{nalu(decode(sps), false), nalu(decode(pps), false), nalu(idr, true)}
}

// keyframeAVCC is a keyframe with its parameter sets in one packet, as Tapo
// sends it: its SDP is made up, without parameter sets
func keyframeAVCC(sps, pps string) []*rtp.Packet {
	return []*rtp.Packet{{Payload: h264.JoinNALU(decode(sps), decode(pps), idr)}}
}

func avccCodec() *core.Codec {
	return &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
}

// viewer is a consumer that passes the packets on as they come, like the
// RTSP server does for Tapo: it never reads their parameter sets
type viewer struct {
	core.Connection
}

func (v *viewer) AddTrack(media *core.Media, _ *core.Codec, track *core.Receiver) error {
	sender := core.NewSender(media, track.Codec)
	sender.Handler = func(*rtp.Packet) {}
	sender.WithParent(track).Start()
	v.Senders = append(v.Senders, sender)
	return nil
}

func addViewer(t *testing.T, stream *streams.Stream) *viewer {
	v := &viewer{core.Connection{
		ID: core.NewID(), FormatName: "viewer", Medias: core.ParseQuery(url.Values{"video": {""}}),
	}}
	require.NoError(t, stream.AddConsumer(v))
	return v
}

// requireNoSPS checks that the offers of the stream lack the parameter sets
func requireNoSPS(t *testing.T, stream *streams.Stream, state string) {
	offers := streams.OffersOf(stream)
	require.Equal(t, state, offers.State)
	require.Len(t, offers.Video, 1)
	require.False(t, hasSPS(offers.Video[0].Codec, offers.Video[0].Fmtp), offers.Video[0].Fmtp)
}

// Many cameras send the parameter sets with the keyframes only, not in the
// SDP: the probe waits for the first keyframe
func TestProbeReadsTheKeyframe(t *testing.T) {
	freshProbes(t)
	newStub(t, "keyframe", &stubSource{codec: h264Codec("packetization-mode=1"), packets: keyframeRTP(spsHigh, ppsHigh)})

	require.Equal(t, profileXML{"keyframe", "keyframe", "keyframe", "H264", 2560, 1920}, getProfiles(t)["keyframe"])
}

// A stream that runs already, ex. for a recording, without its parameter
// sets in the offers (Tapo with the RTSP server): the profile told the
// placeholders. The probe joins the running stream for the next keyframe,
// it doesn't dial the camera again.
func TestProfileOfRunningStream(t *testing.T) {
	freshProbes(t)
	stub := newStub(t, "running", &stubSource{codec: avccCodec(), packets: keyframeAVCC(spsHigh, ppsHigh)})
	stream := streams.Get("running")

	v := addViewer(t, stream)
	t.Cleanup(func() { stream.RemoveConsumer(v) })
	requireNoSPS(t, stream, streams.OffersLive)

	require.Equal(t, profileXML{"running", "running", "running", "H264", 2560, 1920}, getProfiles(t)["running"])
	require.Equal(t, int32(1), stub.dials.Load(), "the probe joined the running stream")
}

// Live test with a Tapo: after a viewer of the RTSP server the profile of its
// sub stream told 1920x1080 instead of 640x360. Every session of Tapo starts
// without the parameter sets. The profile keeps the size it told, until the
// stream gets another source.
func TestProfileKeepsItsSize(t *testing.T) {
	freshProbes(t)
	stub := newStub(t, "tapo", &stubSource{codec: avccCodec(), packets: keyframeAVCC(spsHigh, ppsHigh)})
	stream := streams.Get("tapo")

	require.Equal(t, profileXML{"tapo", "tapo", "tapo", "H264", 2560, 1920}, getProfiles(t)["tapo"])
	require.Equal(t, int32(1), stub.dials.Load())

	// a viewer: a new session, without the parameter sets
	stream.RemoveConsumer(addViewer(t, stream))
	require.Equal(t, int32(2), stub.dials.Load())
	requireNoSPS(t, stream, streams.OffersCached)

	require.Equal(t, profileXML{"tapo", "tapo", "tapo", "H264", 2560, 1920}, getProfiles(t)["tapo"])
	require.Equal(t, int32(2), stub.dials.Load(), "kept, not dialed again")

	// the camera sends H.265 now: the H.264 size is not its size
	stub.codec, stub.packets = &core.Codec{Name: core.CodecH265, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}, nil
	stream.RemoveConsumer(addViewer(t, stream))
	probeTimeout = 100 * time.Millisecond
	require.Equal(t, profileXML{"tapo", "tapo", "tapo", "H265", 1920, 1080}, getProfiles(t)["tapo"])

	// another camera: its size is read anew
	stubs.Store("tapo_other", &stubSource{codec: avccCodec(), packets: keyframeAVCC(spsBase, ppsBase)})
	t.Cleanup(func() { stubs.Delete("tapo_other") })
	stream.SetSource("onvifstub:tapo_other")
	stream.RemoveConsumer(addViewer(t, stream))
	requireNoSPS(t, stream, streams.OffersCached)

	require.Equal(t, profileXML{"tapo", "tapo", "tapo", "H264", 1280, 720}, getProfiles(t)["tapo"])
}

// A stream that has no H.264 or H.265 to tell (MJPEG) gets placeholders, it
// is not probed with every request
func TestProfileOfOtherVideo(t *testing.T) {
	freshProbes(t)
	stub := newStub(t, "mjpeg", &stubSource{codec: &core.Codec{Name: core.CodecJPEG, ClockRate: 90000, PayloadType: 26}})

	for range 3 {
		require.Equal(t, profileXML{"mjpeg", "mjpeg", "mjpeg", "H264", 1920, 1080}, getProfiles(t)["mjpeg"])
	}
	require.Equal(t, int32(1), stub.dials.Load())
}

// A stream that sends no keyframe within the probe failed: it is not probed
// again with the next request
func TestProbeWithoutKeyframe(t *testing.T) {
	freshProbes(t)
	probeTimeout = 300 * time.Millisecond
	frame := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, Marker: true}, Payload: []byte{0x41, 0x9a, 0x00}}
	stub := newStub(t, "nokey", &stubSource{codec: h264Codec("packetization-mode=1"), packets: []*rtp.Packet{frame}})

	for range 2 {
		require.Equal(t, profileXML{"nokey", "nokey", "nokey", "H264", 1920, 1080}, getProfiles(t)["nokey"])
	}
	require.Equal(t, int32(1), stub.dials.Load())
}

// A camera that is offline gets placeholders and doesn't fail the list. It
// is not dialed with every request again.
func TestProbeOfflineCamera(t *testing.T) {
	freshProbes(t)
	offline := newStub(t, "offline", &stubSource{err: errors.New("offline")})
	newStub(t, "online", &stubSource{codec: h265Codec(fmtpH265Wide)})

	for range 3 {
		profiles := getProfiles(t)
		require.Equal(t, profileXML{"offline", "offline", "offline", "H264", 1920, 1080}, profiles["offline"])
		require.Equal(t, 5120, profiles["online"].Width)
	}
	require.Equal(t, int32(1), offline.dials.Load())

	probeRetry = 0 // later on it is dialed again
	getProfiles(t)
	require.Equal(t, int32(2), offline.dials.Load())
}

// A camera that doesn't answer doesn't hold the request: the client gives up
// after a while (UniFi Protect after 10 s)
func TestProbeGivesUp(t *testing.T) {
	freshProbes(t)
	probeTimeout = 200 * time.Millisecond

	block := make(chan struct{})
	newStub(t, "hanging", &stubSource{codec: h264Codec(fmtpH264High), block: block})

	start := time.Now()
	require.Equal(t, profileXML{"hanging", "hanging", "hanging", "H264", 1920, 1080}, getProfiles(t)["hanging"])
	require.Less(t, time.Since(start), 2*time.Second)

	// the dial returns after all: the prober leaves the stream, it stops
	// (unknown before the dial, live while the prober stays)
	close(block)
	stream := streams.Get("hanging")
	require.Eventually(t, func() bool {
		return streams.OffersOf(stream).State == streams.OffersCached
	}, 2*time.Second, 10*time.Millisecond)
}

// The video sources of the config: only their streams are profiles, in their
// order, the profiles of one source share it. A stream go2rtc doesn't have is
// left out, a stream listed twice is the profile of its first source.
func TestVideoSourcesOfConfig(t *testing.T) {
	freshProbes(t)
	newStub(t, "front_main", &stubSource{codec: h265Codec(fmtpH265Wide)})
	newStub(t, "front_sub", &stubSource{codec: h264Codec(fmtpH264Base)})
	newStub(t, "back", &stubSource{codec: h264Codec(fmtpH264High)})
	newStub(t, "unlisted", &stubSource{codec: h264Codec(fmtpH264High)})
	useConfig(t,
		sourceConfig{Name: "Front Door", Profiles: []string{"front_main", "front_sub", "front_gone"}},
		sourceConfig{Profiles: []string{"back"}},
		sourceConfig{Token: "nothing", Profiles: []string{"gone"}},
		sourceConfig{Profiles: []string{"front_sub"}},
	)

	var res struct {
		Profiles []profileXML `xml:"Body>GetProfilesResponse>Profiles"`
	}
	unmarshal(t, request(t, "GetProfiles", ""), &res)
	require.Equal(t, []profileXML{
		{"front_main", "Front Door 1", "front_main", "H265", 5120, 1440},
		{"front_sub", "Front Door 2", "front_main", "H264", 1280, 720},
		{"back", "back", "back", "H264", 2560, 1920},
	}, res.Profiles)

	var sources struct {
		Sources []struct {
			Token string `xml:"token,attr"`
			Width int    `xml:"Resolution>Width"`
		} `xml:"Body>GetVideoSourcesResponse>VideoSources"`
	}
	unmarshal(t, request(t, "GetVideoSources", ""), &sources)
	require.Len(t, sources.Sources, 2)
	require.Equal(t, "front_main", sources.Sources[0].Token)
	require.Equal(t, 5120, sources.Sources[0].Width, "the size of its main stream")
	require.Equal(t, "back", sources.Sources[1].Token)

	var config struct {
		Token string `xml:"Body>GetVideoSourceConfigurationResponse>Configuration>SourceToken"`
	}
	unmarshal(t, request(t, "GetVideoSourceConfiguration", "<trt:ConfigurationToken>front_main</trt:ConfigurationToken>"), &config)
	require.Equal(t, "front_main", config.Token)

	status, b := requestStatus(t, "GetStreamUri", "<trt:ProfileToken>front_sub</trt:ProfileToken>")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, string(b), "/front_sub</tt:Uri>")

	// what is not a profile or a source gets a fault
	for _, tc := range []struct{ operation, body, subcode string }{
		{"GetProfile", "<trt:ProfileToken>unlisted</trt:ProfileToken>", "ter:NoProfile"},
		{"GetStreamUri", "<trt:ProfileToken>unlisted</trt:ProfileToken>", "ter:NoProfile"},
		{"GetSnapshotUri", "<trt:ProfileToken>unlisted</trt:ProfileToken>", "ter:NoProfile"},
		{"GetStreamUri", "<trt:ProfileToken>front_gone</trt:ProfileToken>", "ter:NoProfile"},
		{"GetVideoEncoderConfigurationOptions", "<trt:ProfileToken>unlisted</trt:ProfileToken>", "ter:NoProfile"},
		{"GetVideoEncoderConfiguration", "<trt:ConfigurationToken>unlisted</trt:ConfigurationToken>", "ter:NoConfig"},
		{"GetVideoSourceConfiguration", "<trt:ConfigurationToken>front_sub</trt:ConfigurationToken>", "ter:NoConfig"},
		{"GetVideoSourceConfiguration", "<trt:ConfigurationToken>nothing</trt:ConfigurationToken>", "ter:NoConfig"},
	} {
		status, b := requestStatus(t, tc.operation, tc.body)
		require.Equal(t, http.StatusBadRequest, status, tc.operation)
		var fault struct {
			Subcode string `xml:"Body>Fault>Code>Subcode>Subcode>Value"`
		}
		unmarshal(t, b, &fault)
		require.Equal(t, tc.subcode, fault.Subcode, "%s %s", tc.operation, tc.body)
	}
}

// camera.ui saves the config with its cameras as video sources through the
// API, go2rtc doesn't restart for it: the profiles follow the saved config
func TestVideoSourcesFollowTheSavedConfig(t *testing.T) {
	freshProbes(t)
	newStub(t, "cam_main", &stubSource{codec: h264Codec(fmtpH264High)})
	newStub(t, "cam_sub", &stubSource{codec: h264Codec(fmtpH264Base)})
	newStub(t, "preview", &stubSource{codec: h264Codec(fmtpH264Base)})
	t.Cleanup(func() { setSourceConfigs(nil) })

	tokens := func() []string {
		var res struct {
			Profiles []profileXML `xml:"Body>GetProfilesResponse>Profiles"`
		}
		unmarshal(t, request(t, "GetProfiles", ""), &res)
		var tokens []string
		for _, p := range res.Profiles {
			tokens = append(tokens, p.Token)
		}
		return tokens
	}

	camera := []byte("onvif:\n  video_sources:\n    - name: Cam\n      profiles: [cam_main, cam_sub]\n")
	reloadConfig(camera)
	require.Equal(t, []string{"cam_main", "cam_sub"}, tokens())

	// a config that is no YAML changes nothing
	reloadConfig([]byte("onvif: ["))
	require.Equal(t, []string{"cam_main", "cam_sub"}, tokens())

	// no cameras: no profiles, not every stream
	reloadConfig([]byte("onvif:\n  video_sources: []\n"))
	require.Empty(t, tokens())
	var sources struct {
		Sources []struct{} `xml:"Body>GetVideoSourcesResponse>VideoSources"`
	}
	unmarshal(t, request(t, "GetVideoSources", ""), &sources)
	require.Empty(t, sources.Sources)

	// without video sources: every stream
	reloadConfig([]byte("streams: {}\n"))
	require.Subset(t, tokens(), []string{"cam_main", "cam_sub", "preview"})

	// the requests read the video sources while a save replaces them
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				requestStatus(t, "GetProfiles", "")
			}
		}
	})
	for range 50 {
		reloadConfig(camera)
		reloadConfig([]byte("onvif:\n  video_sources: []\n"))
	}
	close(stop)
	wg.Wait()
}

func unmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	require.NoError(t, xml.Unmarshal(b, v), "%s", b)
}

// children are the names of the elements directly in the first element at
// path (below Body), in their order: the schema of ONVIF is a sequence
func children(t *testing.T, b []byte, path ...string) []string {
	t.Helper()
	want := append([]string{"Envelope", "Body"}, path...)
	var stack, names []string
	found := false
	d := xml.NewDecoder(bytes.NewReader(b))
	for {
		token, err := d.Token()
		if err == io.EOF {
			require.True(t, found, "no %s in %s", strings.Join(path, "/"), b)
			return names
		}
		require.NoError(t, err, "%s", b)
		switch token := token.(type) {
		case xml.StartElement:
			if found && len(stack) == len(want) && slices.Equal(stack, want) {
				names = append(names, token.Name.Local)
			}
			stack = append(stack, token.Name.Local)
			if !found && slices.Equal(stack, want) {
				found = true
			}
		case xml.EndElement:
			if found && slices.Equal(stack, want) {
				return names
			}
			stack = stack[:len(stack)-1]
		}
	}
}

// The answers lacked elements the ONVIF schema requires (an OxDM structure
// check found them): MediaUri without InvalidAfterConnect, InvalidAfterReboot
// and Timeout, the encoder configurations without Multicast, DNS, NTP and the
// hostname without FromDHCP, and no network interface at all. A strict client
// rejects such an answer.
func TestRequiredElements(t *testing.T) {
	freshProbes(t)
	newStub(t, "front_main", &stubSource{codec: h265Codec(fmtpH265Wide)})
	newStub(t, "front_sub", &stubSource{codec: h264Codec(fmtpH264Base)})
	useConfig(t, sourceConfig{Profiles: []string{"front_main", "front_sub"}})

	mediaUri := []string{"Uri", "InvalidAfterConnect", "InvalidAfterReboot", "Timeout"}
	for _, operation := range []string{"GetStreamUri", "GetSnapshotUri"} {
		b := request(t, operation, "<trt:ProfileToken>front_sub</trt:ProfileToken>")
		require.Equal(t, mediaUri, children(t, b, operation+"Response", "MediaUri"), operation)

		// the schema says false and PT0S: valid for good
		res := parse(t, b)
		require.Equal(t, []string{"false"}, res.texts["InvalidAfterConnect"], operation)
		require.Equal(t, []string{"false"}, res.texts["InvalidAfterReboot"], operation)
		require.Equal(t, []string{"PT0S"}, res.texts["Timeout"], operation)
	}

	h264 := []string{"Name", "UseCount", "Encoding", "Resolution", "Quality", "RateControl", "H264", "Multicast", "SessionTimeout"}
	h265 := []string{"Name", "UseCount", "Encoding", "Resolution", "Quality", "RateControl", "Multicast", "SessionTimeout"}
	require.Equal(t, h265, children(t, request(t, "GetProfiles", ""), "GetProfilesResponse", "Profiles", "VideoEncoderConfiguration"))
	require.Equal(t, h264, children(t, request(t, "GetProfile", "<trt:ProfileToken>front_sub</trt:ProfileToken>"), "GetProfileResponse", "Profile", "VideoEncoderConfiguration"))
	require.Equal(t, h265, children(t, request(t, "GetVideoEncoderConfigurations", ""), "GetVideoEncoderConfigurationsResponse", "Configurations"))
	b := request(t, "GetVideoEncoderConfiguration", "<trt:ConfigurationToken>front_sub</trt:ConfigurationToken>")
	require.Equal(t, h264, children(t, b, "GetVideoEncoderConfigurationResponse", "Configuration"))
	require.Equal(t, []string{"Address", "Port", "TTL", "AutoStart"}, children(t, b, "GetVideoEncoderConfigurationResponse", "Configuration", "Multicast"))
	require.Equal(t, []string{"Type", "IPv4Address"}, children(t, b, "GetVideoEncoderConfigurationResponse", "Configuration", "Multicast", "Address"))

	for operation, path := range map[string][]string{
		"GetDNS":      {"GetDNSResponse", "DNSInformation"},
		"GetNTP":      {"GetNTPResponse", "NTPInformation"},
		"GetHostname": {"GetHostnameResponse", "HostnameInformation"},
	} {
		w := requestService(t, onvif.DeviceNamespace, operation)
		require.Equal(t, http.StatusOK, w.Code, operation)
		require.Equal(t, []string{"FromDHCP"}, children(t, w.Body.Bytes(), path...), operation)
	}

	w := requestService(t, onvif.DeviceNamespace, "GetNetworkInterfaces")
	require.Equal(t, http.StatusOK, w.Code)
	var interfaces struct {
		Interfaces []struct {
			Token   string `xml:"token,attr"`
			Enabled string `xml:"Enabled"`
			MAC     string `xml:"Info>HwAddress"`
		} `xml:"Body>GetNetworkInterfacesResponse>NetworkInterfaces"`
	}
	unmarshal(t, w.Body.Bytes(), &interfaces)
	require.Len(t, interfaces.Interfaces, 1)
	require.NotEmpty(t, interfaces.Interfaces[0].Token)
	require.Equal(t, "true", interfaces.Interfaces[0].Enabled)
	require.Empty(t, interfaces.Interfaces[0].MAC, "Hass would join the device with others of the MAC of the host")
}

type sourceOptions struct {
	Tokens []string `xml:"Body>GetVideoSourceConfigurationOptionsResponse>Options>VideoSourceTokensAvailable"`

	WidthMin  int `xml:"Body>GetVideoSourceConfigurationOptionsResponse>Options>BoundsRange>WidthRange>Min"`
	WidthMax  int `xml:"Body>GetVideoSourceConfigurationOptionsResponse>Options>BoundsRange>WidthRange>Max"`
	HeightMin int `xml:"Body>GetVideoSourceConfigurationOptionsResponse>Options>BoundsRange>HeightRange>Min"`
	HeightMax int `xml:"Body>GetVideoSourceConfigurationOptionsResponse>Options>BoundsRange>HeightRange>Max"`
}

// GetVideoSourceConfigurationOptions is required of the media service, the
// server answered ActionNotSupported. The options are the size of the source:
// of the configuration, of the profile, of all sources without a token.
func TestVideoSourceConfigurationOptions(t *testing.T) {
	freshProbes(t)
	newStub(t, "front_main", &stubSource{codec: h265Codec(fmtpH265Wide)})
	newStub(t, "front_sub", &stubSource{codec: h264Codec(fmtpH264Base)})
	newStub(t, "back", &stubSource{codec: h264Codec(fmtpH264High)})
	newStub(t, "side", &stubSource{codec: h264Codec(fmtpH264Base)})
	useConfig(t,
		sourceConfig{Profiles: []string{"front_main", "front_sub"}},
		sourceConfig{Profiles: []string{"side"}},
		sourceConfig{Profiles: []string{"back"}},
	)

	options := func(body string) sourceOptions {
		t.Helper()
		var o sourceOptions
		unmarshal(t, request(t, "GetVideoSourceConfigurationOptions", body), &o)
		return o
	}
	front := sourceOptions{Tokens: []string{"front_main"}, WidthMin: 5120, WidthMax: 5120, HeightMin: 1440, HeightMax: 1440}
	back := sourceOptions{Tokens: []string{"back"}, WidthMin: 2560, WidthMax: 2560, HeightMin: 1920, HeightMax: 1920}

	require.Equal(t, front, options("<trt:ConfigurationToken>front_main</trt:ConfigurationToken>"))
	require.Equal(t, front, options("<trt:ProfileToken>front_sub</trt:ProfileToken>"), "the source of the profile")
	require.Equal(t, back, options("<trt:ProfileToken>back</trt:ProfileToken><trt:ConfigurationToken>back</trt:ConfigurationToken>"))
	// 5120x1440, 1280x720 and 2560x1920
	require.Equal(t, sourceOptions{Tokens: []string{"front_main", "side", "back"}, WidthMin: 1280, WidthMax: 5120, HeightMin: 720, HeightMax: 1920}, options(""))

	b := request(t, "GetVideoSourceConfigurationOptions", "")
	require.Equal(t, []string{"BoundsRange", "VideoSourceTokensAvailable", "VideoSourceTokensAvailable", "VideoSourceTokensAvailable"},
		children(t, b, "GetVideoSourceConfigurationOptionsResponse", "Options"))
	require.Equal(t, []string{"XRange", "YRange", "WidthRange", "HeightRange"},
		children(t, b, "GetVideoSourceConfigurationOptionsResponse", "Options", "BoundsRange"))

	for _, tc := range []struct{ body, subcode string }{
		{"<trt:ProfileToken>unlisted</trt:ProfileToken>", "ter:NoProfile"},
		{"<trt:ConfigurationToken>front_sub</trt:ConfigurationToken>", "ter:NoConfig"}, // a profile, not a source
		{"<trt:ProfileToken>back</trt:ProfileToken><trt:ConfigurationToken>front_main</trt:ConfigurationToken>", "ter:NoConfig"},
	} {
		status, b := requestStatus(t, "GetVideoSourceConfigurationOptions", tc.body)
		require.Equal(t, http.StatusBadRequest, status, tc.body)
		var fault struct {
			Subcode string `xml:"Body>Fault>Code>Subcode>Subcode>Value"`
		}
		unmarshal(t, b, &fault)
		require.Equal(t, tc.subcode, fault.Subcode, tc.body)
	}

	// no video source at all
	setSourceConfigs([]sourceConfig{})
	status, _ := requestStatus(t, "GetVideoSourceConfigurationOptions", "")
	require.Equal(t, http.StatusBadRequest, status)
}
