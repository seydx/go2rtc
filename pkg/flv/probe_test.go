package flv

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// an AVC header without SPS and PPS, like h264_v4l2m2m sends first (#2687)
var videoConfigWithoutParams = EncodeTag(TagVideo, 0, []byte{
	0x17, PacketTypeAVCHeader, 0, 0, 0,
	1, 0x42, 0xC0, 0x1E, 0xFF, 0xE0, 0x00, // version, profile, level, 0 SPS, 0 PPS
})

func videoCodec(t *testing.T, prod *Producer) *core.Codec {
	t.Helper()
	for _, media := range prod.Medias {
		if media.Kind == core.KindVideo {
			return media.Codecs[0]
		}
	}
	require.FailNow(t, "no video")
	return nil
}

// The parameter sets come with the second AVC header: the codec is taken from it.
func TestProbeWaitsForTheParameterSets(t *testing.T) {
	prod, err := Open(bytes.NewReader(stream(videoConfigWithoutParams, audioConfig, videoConfig, videoFrame, audioFrame)))
	require.NoError(t, err)
	require.Contains(t, videoCodec(t, prod).FmtpLine, "sprop-parameter-sets")
}

// An encoder that sends its parameter sets only in band never sends a complete
// header. Its video stays, with the header it has, as before.
func TestProbeKeepsVideoWithoutParameterSets(t *testing.T) {
	old := probeTimeout
	probeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { probeTimeout = old })

	rd, wr := io.Pipe()
	t.Cleanup(func() { _ = rd.Close() })
	go func() {
		if _, err := wr.Write(stream(videoConfigWithoutParams, audioConfig)); err != nil {
			return
		}
		for {
			if _, err := wr.Write(videoFrame); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	prod, err := Open(rd)
	require.NoError(t, err)
	codec := videoCodec(t, prod)
	require.Equal(t, core.CodecH264, codec.Name)
	require.NotContains(t, codec.FmtpLine, "sprop-parameter-sets")
}
