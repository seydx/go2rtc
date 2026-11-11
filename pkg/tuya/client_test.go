package tuya

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// A camera that refused a viewer is not asked again right away: every
// handshake freezes the streams it already serves.
func TestDialLeavesAFullCameraAlone(t *testing.T) {
	setBusy("full-camera")
	t.Cleanup(func() {
		busyMu.Lock()
		delete(busyUntil, "full-camera")
		busyMu.Unlock()
	})

	_, err := Dial("tuya://protect-eu.ismartlife.me?device_id=full-camera&email=a@example.com&password=b")
	require.ErrorIs(t, err, errNoFreeStream)
}

func TestDTLSCertificateIsRSAAndReused(t *testing.T) {
	first, err := dtlsCertificate()
	require.NoError(t, err)
	second, err := dtlsCertificate()
	require.NoError(t, err)
	require.Same(t, first, second)

	fingerprints, err := first.GetFingerprints()
	require.NoError(t, err)
	require.NotEmpty(t, fingerprints)
}

// Tuya HEVC cameras stamp 16kHz mic audio with an 8kHz clock, so reclockPCM
// rebuilds the timestamp from the payload size. Non-PCM codecs pass through.
func TestReclockPCM(t *testing.T) {
	pcml := &core.Codec{Name: core.CodecPCML, ClockRate: 16000, Channels: 1}

	var got []uint32
	handler := reclockPCM(pcml, func(p *rtp.Packet) { got = append(got, p.Timestamp) })

	// camera sends 640-byte frames (320 samples) stamped 160 ticks apart
	for i := 0; i < 3; i++ {
		handler(&rtp.Packet{
			Header:  rtp.Header{Timestamp: uint32(1000 + i*160)},
			Payload: make([]byte, 640),
		})
	}

	want := []uint32{1000, 1320, 1640}
	if len(got) != len(want) {
		t.Fatalf("got %d packets, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("packet %d: timestamp %d, want %d", i, got[i], want[i])
		}
	}

	// AAC has no fixed bytes per frame, timestamps must pass through untouched
	var raw []uint32
	aac := &core.Codec{Name: core.CodecAAC, ClockRate: 16000, Channels: 1}
	handler = reclockPCM(aac, func(p *rtp.Packet) { raw = append(raw, p.Timestamp) })
	for i := 0; i < 3; i++ {
		handler(&rtp.Packet{Header: rtp.Header{Timestamp: uint32(i * 160)}, Payload: []byte{1, 2, 3}})
	}
	for i := 0; i < 3; i++ {
		if raw[i] != uint32(i*160) {
			t.Errorf("AAC packet %d: timestamp %d changed, want %d", i, raw[i], i*160)
		}
	}
}
