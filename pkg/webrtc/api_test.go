package webrtc

import (
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// The video codecs offer the RTCP feedback browsers rely on: NACK and PLI for
// lost packets and keyframe requests, FIR and REMB. pion adds NACK, PLI and
// transport-cc itself, so a mixed up entry of ours would not go missing but
// show up as a feedback type that doesn't exist (ex. "pli nack").
func TestRegisterDefaultCodecsVideoFeedback(t *testing.T) {
	m := &webrtc.MediaEngine{}
	require.NoError(t, RegisterDefaultCodecs(m))

	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	defer pc.Close()

	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
	require.NoError(t, err)

	offer, err := pc.CreateOffer(nil)
	require.NoError(t, err)

	feedback := map[string][]string{} // payload type -> "type parameter"
	for _, line := range strings.Split(offer.SDP, "\r\n") {
		if s, ok := strings.CutPrefix(line, "a=rtcp-fb:"); ok {
			pt, fb, _ := strings.Cut(s, " ")
			feedback[pt] = append(feedback[pt], fb)
		}
	}

	// valid types, see webrtc.RTCPFeedback
	valid := map[string]bool{"ack": true, "ccm": true, "nack": true, "goog-remb": true, "transport-cc": true}
	for _, pt := range []string{"96", "97", "98", "100"} {
		for _, fb := range []string{"goog-remb", "ccm fir", "nack", "nack pli"} {
			require.Contains(t, feedback[pt], fb, "payload type %s", pt)
		}
		for _, fb := range feedback[pt] {
			typ, _, _ := strings.Cut(fb, " ")
			require.True(t, valid[typ], "payload type %s: feedback %q", pt, fb)
		}
	}
}
