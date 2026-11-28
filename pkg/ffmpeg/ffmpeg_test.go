package ffmpeg

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/shell"
	"github.com/stretchr/testify/require"
)

func TestArgsBinWithSpaces(t *testing.T) {
	args := &Args{
		Bin:    `C:\Program Files\camera.ui\resources\app\node_modules\node-av\binary\ffmpeg.exe`,
		Global: "-hide_banner",
		Input:  "-i -",
		Codecs: []string{"-c:v mjpeg"},
		Output: "-f mjpeg -",
	}

	require.Equal(
		t,
		`"C:\Program Files\camera.ui\resources\app\node_modules\node-av\binary\ffmpeg.exe" -hide_banner -i - -c:v mjpeg -f mjpeg -`,
		args.String(),
	)

	cmd := shell.QuoteSplit(args.String())
	require.Equal(t, `C:\Program Files\camera.ui\resources\app\node_modules\node-av\binary\ffmpeg.exe`, cmd[0])
	require.Equal(t, []string{"-hide_banner", "-i", "-", "-c:v", "mjpeg", "-f", "mjpeg", "-"}, cmd[1:])
}

func TestQuoteBin(t *testing.T) {
	require.Equal(t, "ffmpeg", QuoteBin("ffmpeg"))
	require.Equal(t, "/usr/bin/ffmpeg", QuoteBin("/usr/bin/ffmpeg"))
	require.Equal(t, `"/opt/my ffmpeg/ffmpeg"`, QuoteBin("/opt/my ffmpeg/ffmpeg"))
	require.Equal(t, `"C:\Program Files\ffmpeg.exe"`, QuoteBin(`"C:\Program Files\ffmpeg.exe"`))
	require.Equal(t, `'C:\Program Files\ff"mpeg.exe'`, QuoteBin(`C:\Program Files\ff"mpeg.exe`))
}

func TestUnquoteBin(t *testing.T) {
	require.Equal(t, "ffmpeg", UnquoteBin("ffmpeg"))
	require.Equal(t, `C:\Program Files\ffmpeg.exe`, UnquoteBin(`"C:\Program Files\ffmpeg.exe"`))
	require.Equal(t, `C:\Program Files\ffmpeg.exe`, UnquoteBin(`'C:\Program Files\ffmpeg.exe'`))
	require.Equal(t, `C:\Program Files\ffmpeg".exe`, UnquoteBin(`C:\Program Files\ffmpeg".exe`))
	require.Equal(t, `"`, UnquoteBin(`"`))
}

// The test from upstream (#2423).
func TestBinWithSpaces(t *testing.T) {
	args := Args{
		Bin:    `C:\Program Files\Symcon\ffmpeg.exe`,
		Global: "-hide_banner",
		Input:  "-i rtsp://example.com",
		Output: "-f mjpeg -",
	}

	s := args.String()
	require.Equal(t, `"C:\Program Files\Symcon\ffmpeg.exe" -hide_banner -i rtsp://example.com -f mjpeg -`, s)

	// shell.QuoteSplit is how internal/exec parses this string back
	split := shell.QuoteSplit(s)
	require.Equal(t, `C:\Program Files\Symcon\ffmpeg.exe`, split[0])
	require.Equal(t, "-hide_banner", split[1])
}
