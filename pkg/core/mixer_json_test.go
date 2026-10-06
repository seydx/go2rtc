package core

import (
	"testing"
)

// Talkers join (the mixer becomes their child via the camera's talk track)
// while the API marshals the mixer. Meaningful under -race.
func TestMixerMarshalWhileChildrenChange(t *testing.T) {
	pcmu := &Codec{Name: CodecPCMU, ClockRate: 8000}
	media := &Media{Kind: KindAudio, Direction: DirectionSendonly, Codecs: []*Codec{pcmu}}
	m := NewRTPMixer("ffmpeg", media, pcmu)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			_, _ = m.MarshalJSON()
		}
	}()
	for range 100 {
		m.AppendChild(&NewReceiver(media, pcmu).Node)
	}
	<-done
}
