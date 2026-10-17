package wyze

import (
	"bytes"
	"encoding/base64"
	"io"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/tutk"
	"github.com/stretchr/testify/require"
)

func decode(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

var (
	// Wyze Cam v3 (WYZE_CAKP2JFUS) asked for HD, AlexxIT/go2rtc#2573: the SPS
	// of the frames from before the change, then the one after it
	sd264 = decode("J00AMudAUBf8s1AQEB8AAAMAAQAAAwAoMTADqYAFfl//wKA=")
	hd264 = decode("J00AKedAPAET8s1AQEB8AAADAAQAAAMAoMkABpeAAnjX//Ao")
	// x265, Main profile, at 640x360 and 1920x1080
	sd265 = decode("QgEBAWAAAAMAkAAAAwAAAwA/oAUCAWlllZpJMrwFoCAAAAMAIAAAAwAh")
	hd265 = decode("QgEBAWAAAAMAkAAAAwAAAwB4oAPAgBDlllZpJMrwFoCAAAADAIAAAAMAhA==")
)

func annexB(nals ...[]byte) []byte {
	var b []byte
	for _, nal := range nals {
		b = append(append(b, 0, 0, 0, 1), nal...)
	}
	return b
}

var pps264 = []byte{0x68, 0xce, 0x3c, 0x80}

func keyframe(codec byte, sps []byte) *tutk.Packet {
	payload := annexB(sps, pps264, []byte{0x65, 0x88, 0x84, 0x00})
	if codec == tutk.CodecH265 {
		vps := []byte{0x40, 0x01, 0x0c, 0x01, 0xff, 0xff}
		payload = annexB(vps, sps, []byte{0x44, 0x01, 0xc1, 0x72}, []byte{0x26, 0x01, 0xaf, 0x00})
	}
	return &tutk.Packet{Codec: codec, IsKeyframe: true, Payload: payload}
}

func pframe(codec byte) *tutk.Packet {
	payload := annexB([]byte{0x41, 0x9a, 0x00, 0x11})
	if codec == tutk.CodecH265 {
		payload = annexB([]byte{0x02, 0x01, 0xd0, 0x11})
	}
	return &tutk.Packet{Codec: codec, Payload: payload}
}

func audio() *tutk.Packet {
	return &tutk.Packet{Codec: tutk.CodecPCMU, SampleRate: 8000, Channels: 1, Payload: make([]byte, 160)}
}

// camera sends the packets one after the other, wait apart
func camera(wait time.Duration, packets ...*tutk.Packet) func() (*tutk.Packet, error) {
	return func() (*tutk.Packet, error) {
		if len(packets) == 0 {
			return nil, io.EOF
		}
		time.Sleep(wait)
		pkt := packets[0]
		packets = packets[1:]
		return pkt, nil
	}
}

func spsOf(t *testing.T, codec *core.Codec) []byte {
	t.Helper()
	require.NotNil(t, codec)
	if codec.Name == core.CodecH265 {
		_, sps, _ := h265.GetParameterSet(codec.Fmtp())
		return sps
	}
	sps, _ := h264.GetParameterSet(codec.Fmtp())
	return sps
}

func TestIsSD(t *testing.T) {
	s := h264.DecodeSPS(sd264)
	require.Equal(t, [2]uint16{640, 360}, [2]uint16{s.Width(), s.Height()})
	s = h264.DecodeSPS(hd264)
	require.Equal(t, [2]uint16{1920, 1080}, [2]uint16{s.Width(), s.Height()})
	s2 := h265.DecodeSPS(sd265)
	require.Equal(t, [2]uint16{640, 360}, [2]uint16{s2.Width(), s2.Height()})
	s2 = h265.DecodeSPS(hd265)
	require.Equal(t, [2]uint16{1920, 1080}, [2]uint16{s2.Width(), s2.Height()})

	for _, test := range []struct {
		name  string
		codec *core.Codec
		sd    bool
	}{
		{"h264 sd", h264.AVCCToCodec(h264.JoinNALU(sd264, pps264)), true},
		{"h264 hd", h264.AVCCToCodec(h264.JoinNALU(hd264, pps264)), false},
		{"h265 sd", h265.AVCCToCodec(h264.JoinNALU(sd265)), true},
		{"h265 hd", h265.AVCCToCodec(h264.JoinNALU(hd265)), false},
		{"h264 without sps", &core.Codec{Name: core.CodecH264}, false},
		{"h265 without sps", &core.Codec{Name: core.CodecH265}, false},
		{"h265 with a broken sps", &core.Codec{Name: core.CodecH265, FmtpLine: "sprop-sps=Qg=="}, false},
		{"mjpeg", &core.Codec{Name: core.CodecJPEG}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.sd, isSD(test.codec))
		})
	}
}

func TestStaleSDUntil(t *testing.T) {
	now := time.Now()
	for quality, until := range map[byte]time.Time{
		0:                   now.Add(staleSPSTimeout),
		FrameSize720P:       now.Add(staleSPSTimeout),
		FrameSize2K:         now.Add(staleSPSTimeout),
		FrameSizeFloodlight: now.Add(staleSPSTimeout),
		FrameSize360P:       {}, // SD asked for: an SD SPS is the right one
	} {
		require.Equal(t, until, staleSDUntil(quality, true, now), "quality %d", quality)
		// the change failed, the camera stays at the profile it has
		require.Equal(t, time.Time{}, staleSDUntil(quality, false, now), "quality %d", quality)
	}
}

// The frames on their way when SetResolution is sent still have the SD frame
// size. Their SPS used to become the codec of the stream, while the stream
// went on in HD: hardware decoders decoded nothing.
func TestProbeSkipsTheSPSFromBeforeTheChange(t *testing.T) {
	for name, test := range map[string]struct {
		codec  byte
		sd, hd []byte
	}{
		"h264": {tutk.CodecH264, sd264, hd264},
		"h265": {tutk.CodecH265, sd265, hd265},
	} {
		t.Run(name, func(t *testing.T) {
			read := camera(0, keyframe(test.codec, test.sd), audio(), pframe(test.codec), keyframe(test.codec, test.hd), audio())
			vcodec, acodec, _, err := probeCodecs(read, true, time.Now().Add(time.Minute), false)
			require.NoError(t, err)
			require.Equal(t, test.hd, spsOf(t, vcodec))
			require.Equal(t, core.CodecPCMU, acodec.Name)

			// without audio the HD keyframe ends the probe
			read = camera(0, keyframe(test.codec, test.sd), keyframe(test.codec, test.hd))
			vcodec, acodec, _, err = probeCodecs(read, false, time.Now().Add(time.Minute), false)
			require.NoError(t, err)
			require.Equal(t, test.hd, spsOf(t, vcodec))
			require.Nil(t, acodec)
		})
	}
}

// A camera that stays at SD isn't waited for any longer than the frames of
// the change take: its SPS is taken without the next keyframe. The probe
// would time out with a long GOP otherwise.
func TestProbeTakesTheSDOfACameraThatStaysAtSD(t *testing.T) {
	packets := []*tutk.Packet{keyframe(tutk.CodecH264, sd264)}
	for range 100 {
		packets = append(packets, audio(), pframe(tutk.CodecH264))
	}

	start := time.Now()
	vcodec, acodec, _, err := probeCodecs(camera(5*time.Millisecond, packets...), true, start.Add(100*time.Millisecond), false)
	require.NoError(t, err)
	require.Equal(t, sd264, spsOf(t, vcodec))
	require.NotNil(t, acodec)
	require.Less(t, time.Since(start), time.Second)
}

// Asked for SD, or without a change, the first SPS is the right one.
func TestProbeTakesTheFirstSPSWithoutAChange(t *testing.T) {
	read := camera(0, keyframe(tutk.CodecH264, sd264), audio(), keyframe(tutk.CodecH264, hd264))
	vcodec, _, _, err := probeCodecs(read, true, time.Time{}, false)
	require.NoError(t, err)
	require.Equal(t, sd264, spsOf(t, vcodec))

	// without audio the keyframe ends the probe, nothing after it is waited for
	vcodec, _, _, err = probeCodecs(camera(0, keyframe(tutk.CodecH264, sd264)), false, time.Time{}, false)
	require.NoError(t, err)
	require.Equal(t, sd264, spsOf(t, vcodec))
}

// An HD SPS is taken right away, the window is for SD only.
func TestProbeTakesAnHDSPSRightAway(t *testing.T) {
	read := camera(0, keyframe(tutk.CodecH264, hd264), audio(), keyframe(tutk.CodecH264, sd264))
	vcodec, _, _, err := probeCodecs(read, true, time.Now().Add(time.Minute), false)
	require.NoError(t, err)
	require.Equal(t, hd264, spsOf(t, vcodec))
}

// Every error of the camera ends the probe.
func TestProbeFailsWithTheCamera(t *testing.T) {
	_, _, _, err := probeCodecs(camera(0, keyframe(tutk.CodecH264, sd264)), true, time.Now().Add(time.Minute), false)
	require.ErrorIs(t, err, io.EOF)
	require.True(t, bytes.HasPrefix([]byte(err.Error()), []byte("wyze: probe: ")))
}

// With ?verbose=true the probe printed the name of the audio codec, also of a
// camera without audio: a nil pointer took the process down.
func TestPrintProbedWithoutAudio(t *testing.T) {
	video := &core.Codec{Name: core.CodecH264}
	for _, intercom := range []bool{false, true} {
		require.NotPanics(t, func() { printProbed(video, nil, intercom) })
		require.NotPanics(t, func() { printProbed(video, &core.Codec{Name: core.CodecPCMU}, intercom) })
	}
}
