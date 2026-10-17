package h264

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeConfig(t *testing.T) {
	s := "01640033ffe1000c67640033ac1514a02800f19001000468ee3cb0"
	src, err := hex.DecodeString(s)
	require.Nil(t, err)

	profile, sps, pps := DecodeConfig(src)
	require.NotNil(t, profile)
	require.NotNil(t, sps)
	require.NotNil(t, pps)

	dst := EncodeConfig(sps, pps)
	require.Equal(t, src, dst)
}

// A config that ends after its SPS, without the count of its PPS, panicked:
// any FLV source could send one as its sequence header.
func TestDecodeConfigWithoutPPS(t *testing.T) {
	src, err := hex.DecodeString("01640033ffe1000c67640033ac1514a02800f190")
	require.Nil(t, err)

	var profile, sps, pps []byte
	require.NotPanics(t, func() { profile, sps, pps = DecodeConfig(src) })
	require.Equal(t, src[1:4], profile)
	require.Equal(t, src[8:], sps)
	require.Nil(t, pps)

	// no SPS either
	require.NotPanics(t, func() { profile, sps, pps = DecodeConfig(src[:6]) })
	require.Equal(t, src[1:4], profile)
	require.Nil(t, sps)
	require.Nil(t, pps)
}

func FuzzDecodeConfig(f *testing.F) {
	src, _ := hex.DecodeString("01640033ffe1000c67640033ac1514a02800f19001000468ee3cb0")
	f.Add(src)
	f.Add(src[:20])
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _, _ = DecodeConfig(b)
		_ = ConfigToCodec(b)
	})
}

func TestDecodeSPS(t *testing.T) {
	s := "Z0IAMukAUAHjQgAAB9IAAOqcCAA=" // Amcrest AD410
	b, err := base64.StdEncoding.DecodeString(s)
	require.Nil(t, err)

	sps := DecodeSPS(b)
	require.Equal(t, uint16(2560), sps.Width())
	require.Equal(t, uint16(1920), sps.Height())

	s = "R00AKZmgHgCJ+WEAAAMD6AAATiCE" // Sonoff
	b, err = base64.StdEncoding.DecodeString(s)
	require.Nil(t, err)

	sps = DecodeSPS(b)
	require.Equal(t, uint16(1920), sps.Width())
	require.Equal(t, uint16(1080), sps.Height())

	s = "Z01AMqaAKAC1kAA=" // Dahua
	b, err = base64.StdEncoding.DecodeString(s)
	require.Nil(t, err)

	sps = DecodeSPS(b)
	require.Equal(t, uint16(2560), sps.Width())
	require.Equal(t, uint16(1440), sps.Height())

	s = "Z2QAM6wVFKAoAPGQ" // Reolink
	b, err = base64.StdEncoding.DecodeString(s)
	require.Nil(t, err)

	sps = DecodeSPS(b)
	require.Equal(t, uint16(2560), sps.Width())
	require.Equal(t, uint16(1920), sps.Height())

	s = "Z2QAKKwa0AoAt03AQEBQAAADABAAAAMB6PFCKg==" // TP-Link
	b, err = base64.StdEncoding.DecodeString(s)
	require.Nil(t, err)

	sps = DecodeSPS(b)
	require.Equal(t, uint16(1280), sps.Width())
	require.Equal(t, uint16(720), sps.Height())

	s = "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=" // TP-Link sub
	b, err = base64.StdEncoding.DecodeString(s)
	require.Nil(t, err)

	sps = DecodeSPS(b)
	require.Equal(t, uint16(640), sps.Width())
	require.Equal(t, uint16(360), sps.Height())
}

func TestGetProfileLevelID(t *testing.T) {
	// OpenIPC https://github.com/OpenIPC
	s := "profile-level-id=0033e7; packetization-mode=1; "
	profile := GetProfileLevelID(s)
	require.Equal(t, "640029", profile)

	// Eufy T8400 https://github.com/AlexxIT/go2rtc/issues/155
	s = "packetization-mode=1;profile-level-id=276400"
	profile = GetProfileLevelID(s)
	require.Equal(t, "640029", profile)
}

func TestDecodeSPS2(t *testing.T) {
	s := "6764001fad84010c20086100430802184010c200843b50740932"
	b, err := hex.DecodeString(s)
	require.Nil(t, err)

	sps := DecodeSPS(b)
	require.Equal(t, uint16(928), sps.Width())
	require.Equal(t, uint16(576), sps.Height())

	s = "Z2QAHq2EAQwgCGEAQwgCGEAQwgCEO1BQF/yzcBAQFAAAD6AAAXcCEA==" // unknown
	b, err = base64.StdEncoding.DecodeString(s)
	require.Nil(t, err)

	sps = DecodeSPS(b)
	require.Equal(t, uint16(640), sps.Width())
	require.Equal(t, uint16(360), sps.Height())
}

func TestAVCCToCodec(t *testing.T) {
	s := "000000196764001fac2484014016ec0440000003004000000c23c60c920000000568ee32c8b0000000d365"
	b, _ := hex.DecodeString(s)
	codec := AVCCToCodec(b)
	require.Equal(t, "packetization-mode=1;profile-level-id=64001f;sprop-parameter-sets=Z2QAH6wkhAFAFuwEQAAAAwBAAAAMI8YMkg==,aO4yyLA=", codec.FmtpLine)
}
