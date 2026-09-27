package h265

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeSPS(t *testing.T) {
	s := "QgEBAWAAAAMAAAMAAAMAAAMAmaAAoAgBaH+KrTuiS7/8AAQABbAgApMuADN/mAE="
	b, err := base64.StdEncoding.DecodeString(s)
	require.Nil(t, err)

	sps := DecodeSPS(b)
	require.NotNil(t, sps)
	require.Equal(t, uint16(5120), sps.Width())
	require.Equal(t, uint16(1440), sps.Height())
}

func TestDecodeSPS2(t *testing.T) {
	s := "QgEBIUAAAAMAkAAAAwAAAwCWoAUCAWlnpbkShc1AQIC4QAAAAwBAAAAFFEn/eEAOpgAV+V8IBBA="
	b, err := base64.StdEncoding.DecodeString(s)
	require.Nil(t, err)

	sps := DecodeSPS(b)
	require.NotNil(t, sps)
	require.Equal(t, uint16(640), sps.Width())
	require.Equal(t, uint16(360), sps.Height())
}

func TestDecodeSPSConformanceWindow(t *testing.T) {
	// Hikvision DVR: coded 1920x1088, cropped to 1920x1080
	s := "QgEGIWAAAAMAAAMAAAMAAAMAewAAoAPAgBEHy7ve96clEVcqn1KS5uAgICAQ"
	b, err := base64.StdEncoding.DecodeString(s)
	require.Nil(t, err)

	sps := DecodeSPS(b)
	require.NotNil(t, sps)
	require.Equal(t, uint16(1920), sps.Width())
	require.Equal(t, uint16(1080), sps.Height())
}

// Fork: SPS from real encoders, the expected size is what ffprobe reports for
// the encoded frame. Hardware encoders code 1080 lines as 1088, x265 pads odd
// sizes to its 8 pixel minimum block, both crop with the conformance window.
func TestDecodeSPSConformanceWindowEncoders(t *testing.T) {
	tests := []struct {
		name          string
		sps           string
		width, height uint16
	}{
		{"x265 1920x1080", "QgEBAWAAAAMAkAAAAwAAAwB4oAPAgBDlllZpJMrwFoCAAAADAIAAAAMAhA==", 1920, 1080},
		{"x265 1918x1078", "QgEBAWAAAAMAkAAAAwAAAwB4oAPAgBDnVZZWaSTK8BaAgAAAAwCAAAADAIQ=", 1918, 1078},
		{"x265 426x240", "QgEBAWAAAAMAkAAAAwAAAwA8oA2IDxyeWVmkkyvAWgIAAAMAAgAAAwACEA==", 426, 240},
		{"x265 2688x1520", "QgEBAWAAAAMAkAAAAwAAAwCWoAFQIAXxZZWaSTK8BaAgAAADACAAAAMAIQ==", 2688, 1520},
		{"VideoToolbox 1920x1080", "QgEBAWAAAAMAsAAAAwAAAwB4oAPAgBEHy4gbuRZFL/y5/E/rAWoEBAQB", 1920, 1080},
		{"VideoToolbox 640x360", "QgEBAWAAAAMAsAAAAwAAAwA/oAUCAXHy4gbuRZFL/y5/E/rAWoEBAQBA", 640, 360},
		{"VideoToolbox 2592x1944", "QgEBAWAAAAMAsAAAAwAAAwCWoAFEIAeh8uICO5FkUv/Ln8T+sBagQEBAEA==", 2592, 1944},
		{"VideoToolbox 1280x720", "QgEBAWAAAAMAsAAAAwAAAwBdoAKAgC0WIG7kWRS/8ufxP6wFqBAQEAQ=", 1280, 720},
		{"VideoToolbox 3840x2160", "QgEBAWAAAAMAsAAAAwAAAwCZoAHgIAIcWIF7kWRS/8ufxP6wFqBAQEAQ", 3840, 2160},
		{"VideoToolbox 800x600", "QgEBAWAAAAMAsAAAAwAAAwBaoAZCAJh8uIG7kWRS/8ufxP6wFqBAQEAQ", 800, 600},
		{"VideoToolbox 1918x1078", "QgEBAWAAAAMAsAAAAwAAAwB4oAPAgBEHU2IG7kWRS/8ufxP6wFqBAQEAQA==", 1918, 1078},
		// cameras from issues #1591 (Dahua 5MP) and #1108 (Annke C800 sub stream)
		{"Dahua 2592x1944", "QgEBAUAAAAMAAAMAAAMAAAMAmaABRCAHofLlruRsGuVRNgQAAAMABAAAAwBIIA==", 2592, 1944},
		{"Annke 640x360", "QgEBAWAAAAMAAAMAAAMAAAMAmaAFAgFx8uKrTuiS7/8AAQABbAgBSZcADN/mAEA=", 640, 360},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := base64.StdEncoding.DecodeString(tt.sps)
			require.Nil(t, err)

			sps := DecodeSPS(b)
			require.NotNil(t, sps)
			require.Equal(t, tt.width, sps.Width())
			require.Equal(t, tt.height, sps.Height())
		})
	}
}

// Fork: the offsets count chroma samples (H.265 Table 6-1), a window that
// would crop the whole picture is invalid and ignored.
func TestSPSConformanceWindowChromaFormat(t *testing.T) {
	tests := []struct {
		chroma        uint32
		width, height uint16
	}{
		{0, 1920 - 3, 1088 - 4}, // monochrome
		{1, 1920 - 6, 1088 - 8}, // 4:2:0
		{2, 1920 - 6, 1088 - 4}, // 4:2:2
		{3, 1920 - 3, 1088 - 4}, // 4:4:4
	}
	for _, tt := range tests {
		sps := &SPS{
			chroma_format_idc:          tt.chroma,
			pic_width_in_luma_samples:  1920,
			pic_height_in_luma_samples: 1088,
			conformance_window_flag:    1,
			conf_win_left_offset:       1,
			conf_win_right_offset:      2,
			conf_win_top_offset:        0,
			conf_win_bottom_offset:     4,
		}
		require.Equal(t, tt.width, sps.Width(), "chroma_format_idc %d", tt.chroma)
		require.Equal(t, tt.height, sps.Height(), "chroma_format_idc %d", tt.chroma)
	}

	sps := &SPS{
		chroma_format_idc:          1,
		pic_width_in_luma_samples:  640,
		pic_height_in_luma_samples: 368,
		conformance_window_flag:    1,
		conf_win_right_offset:      320,
		conf_win_bottom_offset:     184,
	}
	require.Equal(t, uint16(640), sps.Width())
	require.Equal(t, uint16(368), sps.Height())
}
