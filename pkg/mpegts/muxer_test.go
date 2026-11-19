package mpegts

import (
	"bytes"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMuxerHeader(t *testing.T) {
	m := NewMuxer()
	require.Equal(t, uint16(pes0PID), m.AddTrack(StreamTypeH264))
	require.Equal(t, uint16(pes0PID+1), m.AddTrack(StreamTypeAAC))

	d := NewDemuxer()
	_, err := d.ReadPacket(bytes.NewReader(m.GetHeader()))
	require.NoError(t, err)
	require.Len(t, d.pes, 2)
	require.Equal(t, byte(StreamTypeH264), d.pes[pes0PID].StreamType)
	require.Equal(t, byte(StreamTypeAAC), d.pes[pes0PID+1].StreamType)
}

// A track can be added after the sender of another track has started,
// e.g. when the audio source of a stream appears later than the video.
func TestMuxerAddTrackWhileWriting(t *testing.T) {
	for range 100 {
		m := NewMuxer()
		pid := m.AddTrack(StreamTypeH264)

		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ts := uint32(0); ; ts += 3000 {
				select {
				case <-stop:
					return
				default:
					_ = m.GetPayload(pid, ts, []byte{0, 0, 0, 1, 0x65})
				}
			}
		}()

		m.AddTrack(StreamTypeAAC)
		_ = m.GetHeader()

		close(stop)
		wg.Wait()
	}
}

// H265 reaches the muxer without any repair in front of it: a unit with a
// damaged length prefix must not take the process down (HLS, mpegts).
func TestMuxerSurvivesMalformedAVCC(t *testing.T) {
	for _, streamType := range []byte{StreamTypeH264, StreamTypeH265} {
		m := NewMuxer()
		pid := m.AddTrack(streamType)

		for _, payload := range [][]byte{
			{0, 0, 0, 2, 0x26, 0x01, 0},                      // a byte after the last unit
			{0, 0, 0, 2, 0x26, 0x01, 0, 0, 0},                // three bytes after it
			{0x80, 0, 0, 0, 0x26, 0x01},                      // negative as a 32 bit int
			{0, 0, 0, 2, 0x26, 0x01, 0, 0, 0, 9, 0x02, 0x01}, // the last unit too long
		} {
			require.NotPanics(t, func() { m.GetPayload(pid, 0, payload) })
		}
	}
}
