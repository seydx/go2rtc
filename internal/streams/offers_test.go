package streams

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func offerNames(codecs []*OfferCodec) []string {
	names := make([]string, 0, len(codecs))
	for _, codec := range codecs {
		names = append(names, offerKey(codec))
	}
	return names
}

func cameraMedias(audio bool, talk bool) []*core.Media {
	medias := []*core.Media{
		{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{{Name: core.CodecH264, ClockRate: 90000, FmtpLine: "packetization-mode=1;profile-level-id=640033", PayloadType: 96}}},
	}
	if audio {
		medias = append(medias, &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{{Name: core.CodecAAC, ClockRate: 16000, Channels: 1, FmtpLine: "config=1408", PayloadType: 97}}})
	}
	if talk {
		medias = append(medias, &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecPCMU, ClockRate: 8000}, {Name: core.CodecPCMA, ClockRate: 8000, PayloadType: 8}}})
	}
	return medias
}

func connectedProducer(source string, medias []*core.Media) *Producer {
	prod := NewProducer(source)
	prod.conn = &stubProducer{medias: medias}
	prod.state = stateStart
	return prod
}

// companion like camera.ui configures it: transcodes the camera audio for
// clients that cannot play it, only when the camera has audio
const companionSource = "offers:camera#cameraui#audio=pcma#audio=opus#audio=aac#noVideo#noBackchannel#requirePrevAudio"

func registerOfferHandler() {
	HandleOffers("offers", func(url string) []*core.Media {
		var medias []*core.Media
		for _, value := range ParseQuery(url[strings.IndexByte(url, '#')+1:])["audio"] {
			codec := &core.Codec{}
			switch value {
			case "pcma":
				codec = &core.Codec{Name: core.CodecPCMA, ClockRate: 8000, Channels: 1}
			case "opus":
				codec = &core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2}
			case "aac":
				codec = &core.Codec{Name: core.CodecAAC}
			}
			medias = append(medias, &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}})
		}
		return medias
	})
}

func TestOffersCollectCodecsAcrossProducers(t *testing.T) {
	registerOfferHandler()

	camera := connectedProducer("rtsp://camera/stream", cameraMedias(true, true))
	companion := NewProducer(companionSource) // not running

	stream := &Stream{producers: []*Producer{camera, companion}}
	offers := OffersOf(stream)

	data, err := json.Marshal(stream)
	require.NoError(t, err)
	require.Contains(t, string(data), `"offers":{"state":"live"`)

	require.Equal(t, OffersLive, offers.State)
	require.Equal(t, []string{"H264/90000/0"}, offerNames(offers.Video))
	require.Equal(t, "packetization-mode=1;profile-level-id=640033", offers.Video[0].Fmtp)
	// the companion's aac keeps the camera's rate, so it is the camera codec again
	require.Equal(t, []string{"MPEG4-GENERIC/16000/1", "PCMA/8000/1", "OPUS/48000/2"}, offerNames(offers.Audio))
	require.Equal(t, "config=1408", offers.Audio[0].Fmtp)

	require.NotNil(t, offers.Backchannel)
	require.Equal(t, []string{"PCMU/8000/1", "PCMA/8000/1"}, offerNames(offers.Backchannel.Codecs))
	require.True(t, offers.Backchannel.Transcode)
}

func TestOffersListARunningCompanionOnce(t *testing.T) {
	registerOfferHandler()

	camera := connectedProducer("rtsp://camera/stream", cameraMedias(true, false))
	companion := connectedProducer(companionSource, []*core.Media{
		{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{{Name: core.CodecPCMA, ClockRate: 8000, Channels: 1}}},
		{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{{Name: core.CodecOpus, ClockRate: 48000, Channels: 2}}},
		{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{{Name: core.CodecAAC, ClockRate: 16000, Channels: 1}}},
	})

	offers := OffersOf(&Stream{producers: []*Producer{camera, companion}})

	require.Equal(t, []string{"MPEG4-GENERIC/16000/1", "PCMA/8000/1", "OPUS/48000/2"}, offerNames(offers.Audio))
	require.Equal(t, "config=1408", offers.Audio[0].Fmtp, "the primary's entry wins a duplicate")
	require.Nil(t, offers.Backchannel)
}

func TestOffersFollowThePrimaryForEachKind(t *testing.T) {
	registerOfferHandler()

	t.Run("camera without audio", func(t *testing.T) {
		camera := connectedProducer("rtsp://camera/stream", cameraMedias(false, true))
		offers := OffersOf(&Stream{producers: []*Producer{camera, NewProducer(companionSource)}})

		require.Empty(t, offers.Audio, "the companion needs camera audio")
		require.NotNil(t, offers.Backchannel)
	})

	t.Run("audio and talk hidden on the camera", func(t *testing.T) {
		camera := connectedProducer("rtsp://camera/stream#noAudio#noBackchannel", cameraMedias(true, true))
		offers := OffersOf(&Stream{producers: []*Producer{camera, NewProducer(companionSource)}})

		require.Equal(t, []string{"H264/90000/0"}, offerNames(offers.Video))
		require.Empty(t, offers.Audio)
		require.Nil(t, offers.Backchannel)
	})

	t.Run("no mixing", func(t *testing.T) {
		camera := connectedProducer("rtsp://camera/stream#noMix", cameraMedias(true, true))
		offers := OffersOf(&Stream{producers: []*Producer{camera}})

		require.False(t, offers.Backchannel.Transcode)
	})
}

func TestOffersStateWithoutASession(t *testing.T) {
	registerOfferHandler()

	never := NewProducer("rtsp://camera/stream")
	offers := OffersOf(&Stream{producers: []*Producer{never, NewProducer(companionSource)}})
	require.Equal(t, OffersUnknown, offers.State)
	require.Empty(t, offers.Video)
	require.Empty(t, offers.Audio)
	require.Nil(t, offers.Backchannel)

	// a stopped producer keeps what its last session offered
	sleeping := NewProducer("rtsp://camera/stream")
	sleeping.lastMedias = cameraMedias(true, true)
	offers = OffersOf(&Stream{producers: []*Producer{sleeping, NewProducer(companionSource)}})
	require.Equal(t, OffersCached, offers.State)
	require.Equal(t, []string{"MPEG4-GENERIC/16000/1", "PCMA/8000/1", "OPUS/48000/2"}, offerNames(offers.Audio))
	require.NotNil(t, offers.Backchannel)

	// another source is another camera, its last session says nothing
	sleeping.SetSource("rtsp://other/stream")
	require.Equal(t, OffersUnknown, OffersOf(&Stream{producers: []*Producer{sleeping}}).State)
}

// A real session: offers go live on connect, stay cached after the producer
// stops, follow a codec change, and are part of the stream JSON and the
// camera.ui state.
func TestOffersFollowTheCameraSession(t *testing.T) {
	registerTestRTSPHandler()
	speedUpWatchdog(t)

	cam := newFakeCamera(t)

	stream, err := New("offers_session", cam.URL())
	require.NoError(t, err)
	require.Equal(t, OffersUnknown, OffersOf(stream).State)

	cons := newProbeConsumer()
	require.NoError(t, stream.AddConsumer(cons))
	require.True(t, waitUntil(10*time.Second, func() bool { return receiverActive(stream) }))

	offers := OffersOf(stream)
	require.Equal(t, OffersLive, offers.State)
	require.Equal(t, []string{"H264/90000/0"}, offerNames(offers.Video))
	require.Equal(t, core.CodecPCMU, offers.Audio[0].Codec)

	require.Equal(t, OffersLive, StateOf("offers_session", stream).Offers.State)

	// camera reconfigured: the only consumer is evicted and the producer
	// released, the offers still show the codec the camera sends now
	dials := cam.dialCount.Load()
	cam.h265.Store(true)
	cam.dropConns()
	require.True(t, waitUntil(30*time.Second, func() bool { return cam.dialCount.Load() > dials }))
	require.True(t, waitUntil(30*time.Second, func() bool {
		o := OffersOf(stream)
		return o.State == OffersCached && len(o.Video) == 1 && o.Video[0].Codec == core.CodecH265
	}), "offers must follow the codec change, not keep the dropped one")

	// the next client connects live on the new codec
	next := newProbeConsumer()
	require.NoError(t, stream.AddConsumer(next))
	offers = OffersOf(stream)
	require.Equal(t, OffersLive, offers.State)
	require.Equal(t, core.CodecH265, offers.Video[0].Codec)

	// nobody watches anymore, the producer stops: the last session stays known
	stream.RemoveConsumer(next)
	require.True(t, waitUntil(10*time.Second, func() bool { return OffersOf(stream).State == OffersCached }))
	require.Equal(t, core.CodecH265, OffersOf(stream).Video[0].Codec)
}

func TestOffersCarryTheH264ProfileAndLevel(t *testing.T) {
	// the SPS wins over a profile-level-id that says something else
	profile, level := h264ProfileLevel("packetization-mode=1;profile-level-id=640033;sprop-parameter-sets=Z0LAHpY1QKALdNwEBAQI,aM48gA==")
	require.Equal(t, "Baseline", profile)
	require.Equal(t, uint8(30), level)

	profile, level = h264ProfileLevel("packetization-mode=1;profile-level-id=640033")
	require.Equal(t, "High", profile)
	require.Equal(t, uint8(51), level)

	profile, level = h264ProfileLevel("packetization-mode=1")
	require.Empty(t, profile)
	require.Zero(t, level)

	camera := connectedProducer("rtsp://camera/stream", cameraMedias(false, false))
	offers := OffersOf(&Stream{producers: []*Producer{camera}})
	require.Equal(t, "High", offers.Video[0].Profile)
	require.Equal(t, uint8(51), offers.Video[0].Level)
}

func TestOffersCompleteWhatRTPFixes(t *testing.T) {
	codecs := addOfferCodecs(nil, []*core.Codec{
		{Name: core.CodecOpus},
		{Name: core.CodecPCMA},
		{Name: "G726-32"},
		{Name: core.CodecAAC, ClockRate: 16000},
		{Name: core.CodecH265, ClockRate: 90000},
	}, nil, true)

	require.Equal(t, []string{"OPUS/48000/2", "PCMA/8000/1", "G726-32/8000/1", "MPEG4-GENERIC/16000/1", "H265/90000/0"}, offerNames(codecs))
	require.Equal(t, uint8(8), codecs[1].PayloadType, "PCMA has a static payload type")
	require.Equal(t, []string{"opus", "pcm_alaw", "g726", "aac", "hevc"}, []string{codecs[0].FFmpeg, codecs[1].FFmpeg, codecs[2].FFmpeg, codecs[3].FFmpeg, codecs[4].FFmpeg})
}

func TestOffersDescribeThemselvesAsSDP(t *testing.T) {
	registerOfferHandler()

	camera := connectedProducer("rtsp://camera/stream", cameraMedias(true, true))
	offers := OffersOf(&Stream{producers: []*Producer{camera, NewProducer(companionSource)}})

	// the companion's opus is only known from its options: it gets the next free dynamic type
	opus := offers.Audio[2]
	require.Equal(t, core.CodecOpus, opus.Codec)
	require.Equal(t, uint8(98), opus.PayloadType)

	sdp := offers.SDP
	require.Contains(t, sdp, "m=video 0 RTP/AVP 96")
	require.Contains(t, sdp, "a=rtpmap:96 H264/90000")
	require.Contains(t, sdp, "a=fmtp:96 packetization-mode=1;profile-level-id=640033")
	require.Contains(t, sdp, "a=rtpmap:97 MPEG4-GENERIC/16000")
	require.Contains(t, sdp, "a=rtpmap:98 OPUS/48000/2")
	require.Contains(t, sdp, "a=rtpmap:0 PCMU/8000")
	require.Equal(t, 4, strings.Count(sdp, "a=sendonly"), "video, aac, pcma and opus")
	require.Equal(t, 2, strings.Count(sdp, "a=recvonly"), "one per talk codec")

	require.Empty(t, OffersOf(&Stream{producers: []*Producer{NewProducer("rtsp://camera/stream")}}).SDP)
}

// A tapo camera lists both codecs it might send and uses 255 as payload
// type. The running track tells which one it really is, and that stays
// known after the producer stopped.
func TestOffersNarrowToTheRunningTrack(t *testing.T) {
	h264 := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 255, FmtpLine: "packetization-mode=1;sprop-parameter-sets=Z2QAMqzSAJACjoQAAAMABAAAAwB6EA==,aOqPLA=="}
	h265 := &core.Codec{Name: core.CodecH265, ClockRate: 90000, PayloadType: 255}
	video := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{h264, h265}}

	camera := connectedProducer("tapo://camera", []*core.Media{video})
	offers := OffersOf(&Stream{producers: []*Producer{camera}})
	require.Equal(t, []string{"H264/90000/0", "H265/90000/0"}, offerNames(offers.Video), "without a track both are possible")

	// negotiated as H264 but silent, because the camera sends H265: proves nothing
	silent := core.NewReceiver(video, h264)
	camera.receivers = []*core.Receiver{silent}
	offers = OffersOf(&Stream{producers: []*Producer{camera}})
	require.Equal(t, []string{"H264/90000/0", "H265/90000/0"}, offerNames(offers.Video), "a silent track must not decide the codec")

	receiving := core.NewReceiver(video, h264)
	receiving.WriteRTP(&rtp.Packet{Payload: []byte{0x65, 0x88}})
	camera.receivers = []*core.Receiver{receiving}
	offers = OffersOf(&Stream{producers: []*Producer{camera}})
	require.Equal(t, []string{"H264/90000/0"}, offerNames(offers.Video))
	require.Equal(t, "High", offers.Video[0].Profile)
	require.Equal(t, uint8(96), offers.Video[0].PayloadType, "255 is no RTP payload type")
	require.Contains(t, offers.SDP, "a=rtpmap:96 H264/90000")

	camera.conn = nil
	camera.receivers = nil
	offers = OffersOf(&Stream{producers: []*Producer{camera}})
	require.Equal(t, OffersCached, offers.State)
	require.Equal(t, []string{"H264/90000/0"}, offerNames(offers.Video), "the narrowed codec stays known")
}

func TestOffersMarkWhatTheCameraSendsItself(t *testing.T) {
	registerOfferHandler()

	camera := connectedProducer("rtsp://camera/stream", cameraMedias(true, true))
	offers := OffersOf(&Stream{producers: []*Producer{camera, NewProducer(companionSource)}})

	native := map[string]bool{}
	for _, codec := range append(append([]*OfferCodec(nil), offers.Video...), offers.Audio...) {
		native[offerKey(codec)] = codec.Native
	}
	require.Equal(t, map[string]bool{
		"H264/90000/0":          true,
		"MPEG4-GENERIC/16000/1": true, // the companion's aac is the same codec
		"PCMA/8000/1":           false,
		"OPUS/48000/2":          false,
	}, native)
	for _, codec := range offers.Backchannel.Codecs {
		require.True(t, codec.Native)
	}
}

func TestOffersCarryTheH265ProfileAndLevel(t *testing.T) {
	profile, level := h265ProfileLevel("profile-id=1;sprop-vps=QAEMAf//AUAAAAMAAAMAAAMAAAMAmawJ;sprop-sps=QgEBAUAAAAMAAAMAAAMAAAMAmaABQCAFof5a7kbBrlUE;sprop-pps=RAHAc8BMkA==")
	require.Equal(t, "Main", profile)
	require.Equal(t, uint8(51), level)

	// same SPS as Main 10 at level 4.0: general_profile_idc 2, general_level_idc 120
	sps, _ := base64.StdEncoding.DecodeString("QgEBAUAAAAMAAAMAAAMAAAMAmaABQCAFof5a7kbBrlUE")
	sps[3] = sps[3]&^0x1F | 2
	sps[18] = 120 // after the four emulation prevention bytes
	profile, level = h265ProfileLevel("sprop-sps=" + base64.StdEncoding.EncodeToString(sps))
	require.Equal(t, "Main 10", profile)
	require.Equal(t, uint8(40), level)

	profile, level = h265ProfileLevel("profile-id=1")
	require.Empty(t, profile)
	require.Zero(t, level)
}

// Offers read the camera's live codecs, which are the receivers' codecs a
// depacketizer updates with SetFmtp from the first keyframe of every new
// consumer. Each offer must read the fmtp once: under the lock, and so that
// its profile describes the same fmtp it reports. Meaningful under -race.
func TestOffersReadFmtpWhileDepacketizersUpdateIt(t *testing.T) {
	const (
		declared = "packetization-mode=1;profile-level-id=640033"                   // High
		learned  = declared + ";sprop-parameter-sets=Z0LAHpY1QKALdNwEBAQI,aM48gA==" // Baseline in the SPS
	)

	medias := cameraMedias(true, false)
	codec := medias[0].Codecs[0]
	codec.FmtpLine = declared
	stream := &Stream{producers: []*Producer{connectedProducer("rtsp://camera/stream", medias)}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 2000 {
			if i%2 == 0 {
				codec.SetFmtp(learned)
			} else {
				codec.SetFmtp(declared)
			}
		}
	}()

	for {
		select {
		case <-done:
			return
		default:
		}

		video := OffersOf(stream).Video[0]
		want := "High"
		if strings.Contains(video.Fmtp, "sprop-parameter-sets=") {
			want = "Baseline"
		}
		require.Equal(t, want, video.Profile, "profile and fmtp of an offer come from the same read")
	}
}
