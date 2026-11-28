package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// initSegment muxes the init of an H264 and an AAC track
func initSegment(t *testing.T) []byte {
	t.Helper()
	m := &Muxer{}
	m.AddTrack(&core.Codec{Name: core.CodecH264, ClockRate: 90000})
	m.AddTrack(&core.Codec{Name: core.CodecAAC, ClockRate: 16000, Channels: 1, FmtpLine: "config=1408"})
	init, err := m.GetInit()
	require.NoError(t, err)
	return init
}

// mdhdOffsets returns the position of the mdhd type of each track
func mdhdOffsets(init []byte) (offsets []int) {
	for i := 0; ; i += 4 {
		j := bytes.Index(init[i:], []byte("mdhd"))
		if j < 0 {
			return
		}
		i += j
		offsets = append(offsets, i)
	}
}

func probe(init []byte) (*Demuxer, []string) {
	d := &Demuxer{}
	var codecs []string
	for _, media := range d.Probe(init) {
		codecs = append(codecs, media.Codecs[0].Name)
	}
	return d, codecs
}

func TestDemuxerProbe(t *testing.T) {
	d, codecs := probe(initSegment(t))
	require.Equal(t, []string{core.CodecH264, core.CodecAAC}, codecs)
	require.Equal(t, map[uint32]float32{1: 1, 2: 1}, d.timeScales)
}

// A time scale of 0 made the scale of the track +Inf: every timestamp of it
// was broken (#2192). The track is left out.
func TestDemuxerProbeSkipsZeroTimeScale(t *testing.T) {
	init := initSegment(t)
	offsets := mdhdOffsets(init)
	require.Len(t, offsets, 2)
	// version and flags, creation and modification time (version 0)
	binary.BigEndian.PutUint32(init[offsets[1]+4+4+4+4:], 0)

	d, codecs := probe(init)
	require.Equal(t, []string{core.CodecH264}, codecs)
	require.Equal(t, map[uint32]float32{1: 1}, d.timeScales)
}

// A track without its own mdhd must not take the time scale of the track
// before it: the AAC track got 90000 of the video, its timestamps ran 5.6
// times too slow.
func TestDemuxerProbeTrackWithoutMdhd(t *testing.T) {
	init := initSegment(t)
	offsets := mdhdOffsets(init)
	require.Len(t, offsets, 2)
	copy(init[offsets[1]:], "xdhd") // an atom the demuxer doesn't know

	d, codecs := probe(init)
	require.Equal(t, []string{core.CodecH264}, codecs)
	require.Equal(t, map[uint32]float32{1: 1}, d.timeScales)
}
