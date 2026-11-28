package mpegts

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

const (
	testPMT   = 0x1000
	testVideo = 0x100
	testAudio = 0x101
)

// tsPacket is one 188 byte packet with payload on pid. As a muxer does, the
// adaptation field fills up a short payload.
func tsPacket(pid uint16, start bool, payload []byte) []byte {
	if n := len(payload); n < PacketSize-4 {
		return tsPacketAF(pid, start, byte(PacketSize-5-n), payload)
	}
	b := psiPacket(pid, start, payload)
	b[3] = 0x10 // payload only
	return b
}

// tsPacketAF puts an adaptation field of afSize bytes in front of the payload
func tsPacketAF(pid uint16, start bool, afSize byte, payload []byte) []byte {
	b := psiPacket(pid, start, nil)
	b[3] = 0x30 // adaptation field and payload
	b[4] = afSize
	if afSize > 0 {
		b[5] = 0 // no flags, stuffing follows
	}
	copy(b[5+int(afSize):], payload)
	return b
}

// psiPacket is one 188 byte packet with payload on pid. A table section is
// followed by 0xFF.
func psiPacket(pid uint16, start bool, payload []byte) []byte {
	b := bytes.Repeat([]byte{0xFF}, PacketSize)
	b[0] = SyncByte
	b[1] = byte(pid>>8) & 0x1F
	if start {
		b[1] |= 0x40
	}
	b[2] = byte(pid)
	b[3] = 0x10 // payload only
	copy(b[4:], payload)
	return b
}

// pcrPacket carries an adaptation field and nothing else, ex. the PCR of a
// stream between its frames
func pcrPacket(pid uint16) []byte {
	b := psiPacket(pid, false, nil)
	b[3] = 0x20 // adaptation field only
	b[4] = PacketSize - 5
	b[5] = 0x10 // PCR flag
	copy(b[6:], []byte{0, 0, 0x12, 0x34, 0x7E, 0})
	return b
}

// section is the pointer field, table ID, section length, body and CRC32. The
// demuxer doesn't check the CRC, zeros do.
func section(tableID byte, body []byte) []byte {
	size := len(body) + 4
	b := append([]byte{0, tableID, 0xB0 | byte(size>>8), byte(size)}, body...)
	return append(b, 0, 0, 0, 0)
}

func pat(pmtPIDs ...uint16) []byte {
	body := []byte{0x00, 0x01, 0xC1, 0, 0}
	for i, pid := range pmtPIDs {
		body = append(body, 0, byte(i+1), 0xE0|byte(pid>>8), byte(pid))
	}
	return section(tableIDPAT, body)
}

func pmt(streams ...stream) []byte {
	body := []byte{0x00, 0x01, 0xC1, 0, 0, 0xE1, 0x00, 0xF0, 0x00}
	for _, s := range streams {
		body = append(body, s.streamType, 0xE0|byte(s.pid>>8), byte(s.pid), 0xF0, 0x00)
	}
	return section(tableIDPMT, body)
}

// pes is the start of a unit with a PTS, its length set: it ends with this packet
func pes(streamID byte, payload []byte) []byte {
	b := []byte{0, 0, 1, streamID, 0, byte(3 + 5 + len(payload)), 0x80, 0x80, 5}
	b = append(b, make([]byte, 5)...)
	WriteTime(b[9:], 90000)
	return append(b, payload...)
}

var (
	videoUnit = []byte{0, 0, 0, 1, 0x65, 0x88, 0x84}
	audioUnit = []byte{0xD5, 0xD5, 0xD5, 0xD5}
)

func videoPacket() []byte {
	return tsPacket(testVideo, true, pes(0xE0, videoUnit))
}

func audioPacket() []byte {
	return tsPacket(testAudio, true, pes(0xC0, audioUnit))
}

func demux(t *testing.T, packets ...[]byte) []*rtp.Packet {
	t.Helper()
	d := NewDemuxer()
	rd := bytes.NewReader(bytes.Join(packets, nil))
	var pkts []*rtp.Packet
	for {
		pkt, err := d.ReadPacket(rd)
		if errors.Is(err, io.EOF) {
			return pkts
		}
		require.NoError(t, err)
		pkts = append(pkts, pkt)
	}
}

// payloads leaves out the stream types packet each PMT gives
func payloads(pkts []*rtp.Packet, streamType byte) (b [][]byte) {
	for _, pkt := range pkts {
		if pkt.PayloadType == streamType {
			b = append(b, pkt.Payload)
		}
	}
	return
}

func TestDemuxerRoundTrip(t *testing.T) {
	m := NewMuxer()
	video := m.AddTrack(StreamTypeH264, ClockRate)
	audio := m.AddTrack(StreamTypePCMATapo, ClockRate)

	var ts []byte
	ts = append(ts, m.GetHeader()...)

	var frames [][]byte
	for i := range 3 {
		// big enough to take several packets
		frame := append([]byte{0, 0, 0, 0, 0x65}, bytes.Repeat([]byte{byte(i + 1)}, 1000)...)
		frame[3] = byte(len(frame) - 4)
		frame[2] = byte((len(frame) - 4) >> 8)
		frames = append(frames, frame)
		ts = append(ts, m.GetPayload(video, uint32(i*3000), frame)...)
		ts = append(ts, m.GetPayload(audio, uint32(i*3000), audioUnit)...)
	}

	pkts := demux(t, ts)
	require.Equal(t, byte(StreamTypeMetadata), pkts[0].PayloadType)
	require.ElementsMatch(t, []byte{StreamTypeH264, StreamTypePCMATapo}, pkts[0].Payload)

	var expected [][]byte
	for _, frame := range frames {
		expected = append(expected, annexb.EncodeToAVCC(annexb.DecodeAVCCWithAUD(frame)))
	}
	require.Equal(t, expected, payloads(pkts, StreamTypeH264))
	require.Equal(t, [][]byte{audioUnit, audioUnit, audioUnit}, payloads(pkts, StreamTypePCMATapo))
}

// Tapo announces its audio in a later PMT than the video. Before, every PMT
// after the first was ignored: the stream had no sound.
func TestDemuxerLaterPMTAddsStream(t *testing.T) {
	pkts := demux(t,
		psiPacket(0, true, pat(testPMT)),
		psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264})),
		videoPacket(), audioPacket(),
		psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264}, stream{testAudio, StreamTypePCMUTapo})),
		videoPacket(), audioPacket(),
	)
	require.Len(t, payloads(pkts, StreamTypeH264), 2)
	require.Equal(t, [][]byte{audioUnit}, payloads(pkts, StreamTypePCMUTapo))
	require.Len(t, payloads(pkts, StreamTypeMetadata), 1, "the stream types come once")
}

// Consumers take the packets of a stream by its type: a second stream of a
// type that is there already would interleave with the first one.
func TestDemuxerLaterPMTKeepsOneStreamPerType(t *testing.T) {
	pkts := demux(t,
		psiPacket(0, true, pat(testPMT)),
		psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264})),
		psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264}, stream{0x200, StreamTypeH264})),
		videoPacket(),
		tsPacket(0x200, true, pes(0xE0, []byte{0, 0, 0, 1, 0x65, 0x99})),
	)
	require.Equal(t, [][]byte{{0, 0, 0, 3, 0x65, 0x88, 0x84}}, payloads(pkts, StreamTypeH264))
}

// Tapo cameras have changed their audio from PCMA to PCMU with a firmware: a
// stream whose type changes goes on with the new one.
func TestDemuxerLaterPMTChangesStreamType(t *testing.T) {
	pkts := demux(t,
		psiPacket(0, true, pat(testPMT)),
		psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264}, stream{testAudio, StreamTypePCMATapo})),
		audioPacket(),
		psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264}, stream{testAudio, StreamTypePCMUTapo})),
		audioPacket(),
	)
	require.Equal(t, [][]byte{audioUnit}, payloads(pkts, StreamTypePCMATapo))
	require.Equal(t, [][]byte{audioUnit}, payloads(pkts, StreamTypePCMUTapo))
}

// The streams come from the last program in the PAT, as they always did, even
// if the PMT of another program comes first.
func TestDemuxerReadsTheLastProgram(t *testing.T) {
	pkts := demux(t,
		psiPacket(0, true, pat(testPMT, testPMT+1)),
		psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264})),
		tsPacket(0x200, true, pes(0xE0, []byte{0, 0, 0, 1, 0x65, 0x99})),
		psiPacket(testPMT+1, true, pmt(stream{0x200, StreamTypeH264})),
		psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264})),
		videoPacket(),
		tsPacket(0x200, true, pes(0xE0, []byte{0, 0, 0, 1, 0x65, 0x99})),
	)
	require.Equal(t, [][]byte{{0, 0, 0, 2, 0x65, 0x99}}, payloads(pkts, StreamTypeH264))
}

// A packet of nothing but an adaptation field, ex. a PCR, carries 183 bytes of
// it. It used to end the stream with "wrong adaptation size".
func TestDemuxerSkipsAdaptationFieldOnlyPackets(t *testing.T) {
	pkts := demux(t,
		psiPacket(0, true, pat(testPMT)),
		psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264})),
		videoPacket(),
		pcrPacket(testVideo),
		videoPacket(),
	)
	require.Len(t, payloads(pkts, StreamTypeH264), 2)
}

// Nothing a camera sends can read past the packet: a panic there takes the
// whole process down. A damaged packet is dropped, the stream goes on.
func TestDemuxerSurvivesMalformedPackets(t *testing.T) {
	bad := func(b ...byte) []byte { return b }
	header := []byte{0x00, 0x01, 0xC1, 0, 0}

	for name, packets := range map[string][][]byte{
		"PAT pointer past the packet": {psiPacket(0, true, bad(0xB7))},
		"PAT pointer of 255":          {psiPacket(0, true, bad(0xFF))},
		"PAT length of 1023":          {psiPacket(0, true, bad(0, 0, 0xB3, 0xFF))},
		"PAT length past the packet":  {psiPacket(0, true, append(bad(0, 0, 0xB0, 0xF0), header...))},
		"PAT length too short":        {psiPacket(0, true, bad(0, 0, 0xB0, 0x02, 0, 1))},
		"PAT continuation":            {psiPacket(0, false, pat(testPMT+1))},
		"PAT of another table":        {psiPacket(0, true, append([]byte{0, 0x02}, pat(testPMT + 1)[2:]...))},
		"PMT pointer past the packet": {psiPacket(testPMT, true, bad(0xB7))},
		"PMT program info too long":   {psiPacket(testPMT, true, section(tableIDPMT, append(header, 0xE1, 0, 0xF3, 0xFF)))},
		"PMT program info of 256":     {psiPacket(testPMT, true, section(tableIDPMT, append(header, 0xE1, 0, 0xF1, 0x00, 0x1B, 0xE1, 0x01, 0xF0, 0)))},
		"PMT ES info too long":        {psiPacket(testPMT, true, section(tableIDPMT, append(header, 0xE1, 0, 0xF0, 0, 0x1B, 0xE1, 0x01, 0xF3, 0xFF)))},
		"PMT continuation":            {psiPacket(testPMT, false, pmt(stream{testAudio, StreamTypePCMUTapo}))},
		"PMT of another table":        {psiPacket(testPMT, true, append([]byte{0, 0x03}, pmt(stream{testAudio, StreamTypePCMUTapo})[2:]...))},
		"PMT length too short":        {psiPacket(testPMT, true, bad(0, 0x02, 0xB0, 0x05, 0, 1, 0xC1, 0, 0))},
		"PES header past the packet":  {tsPacket(testVideo, true, bad(0, 0, 1, 0xE0, 0, 0, 0x80, 0x80, 0xFF))},
		"PES PTS in a short header":   {tsPacket(testVideo, true, bad(0, 0, 1, 0xE0, 0, 0, 0x80, 0x80, 2))},
		"PES DTS in a short header":   {tsPacket(testVideo, true, bad(0, 0, 1, 0xE0, 0, 0, 0x80, 0xC0, 5))},
		"PES shorter than its header": {tsPacket(testVideo, true, bad(0, 0, 1, 0xE0, 0, 2, 0x80, 0x80, 5))},
		"PES after a long adaptation": {tsPacketAF(testVideo, true, PacketSize-6, bad(0))},
	} {
		t.Run(name, func(t *testing.T) {
			// before the first PMT, and after it
			before := append(append([][]byte{psiPacket(0, true, pat(testPMT))}, packets...),
				psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264})), videoPacket())
			after := append(append([][]byte{
				psiPacket(0, true, pat(testPMT)),
				psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264})),
			}, packets...), videoPacket())

			for _, ts := range [][][]byte{before, after} {
				var pkts []*rtp.Packet
				require.NotPanics(t, func() { pkts = demux(t, ts...) })
				require.Equal(t, [][]byte{{0, 0, 0, 3, 0x65, 0x88, 0x84}}, payloads(pkts, StreamTypeH264))
				require.Equal(t, [][]byte{{StreamTypeH264}}, payloads(pkts, StreamTypeMetadata))
			}
		})
	}
}

// The payload of a unit whose header doesn't add up is garbage, up to the
// start of the next unit.
func TestDemuxerDropsDamagedUnit(t *testing.T) {
	for name, start := range map[string][]byte{
		"start code":                {0, 0, 2, 0xE0, 0, 0, 0x80, 0x80, 5},
		"PTS in a short header":     {0, 0, 1, 0xE0, 0, 0, 0x80, 0x80, 2},
		"shorter than its header":   {0, 0, 1, 0xE0, 0, 2, 0x80, 0x80, 5},
		"header past the packet":    {0, 0, 1, 0xE0, 0, 0, 0x80, 0x00, 0xFF},
		"header longer than packet": {0, 0, 1, 0xE0, 0, 0, 0x80, 0x00, 0xB0},
		"length shorter than header": func() []byte {
			b := pes(0xE0, []byte{0, 0, 0, 1, 0x41, 0x9A})
			b[5] = 2
			return b
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			pkts := demux(t,
				psiPacket(0, true, pat(testPMT)),
				psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264})),
				tsPacket(testVideo, true, start),
				tsPacket(testVideo, false, []byte{0, 0, 0, 1, 0x41, 0x9A}),
				videoPacket(),
			)
			require.Equal(t, [][]byte{{0, 0, 0, 3, 0x65, 0x88, 0x84}}, payloads(pkts, StreamTypeH264))
		})
	}
}

// A PMT whose lengths don't add up gives no streams at all: a part of them would
// end the probe of the producer with them.
func TestDemuxerWaitsForAValidPMT(t *testing.T) {
	header := []byte{0x00, 0x01, 0xC1, 0, 0, 0xE1, 0, 0xF0, 0}
	pkts := demux(t,
		psiPacket(0, true, pat(testPMT)),
		psiPacket(testPMT, true, section(tableIDPMT, append(header, 0x1B, 0xE1, 0x00, 0xF0, 0, 0x0F, 0xE1, 0x01, 0xF0, 0x20))),
		psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264}, stream{testAudio, StreamTypePCMUTapo})),
		videoPacket(),
	)
	require.Len(t, pkts, 2)
	require.ElementsMatch(t, []byte{StreamTypeH264, StreamTypePCMUTapo}, pkts[0].Payload)
}

// The last four bytes of a section are its CRC32. An entry that runs into
// them is cut short: what is read of it is no program or stream, the section
// gives none.
func TestDemuxerSectionEntriesLeaveTheCRC(t *testing.T) {
	header := []byte{0x00, 0x01, 0xC1, 0, 0}
	pmtHeader := append(header, 0xE1, 0, 0xF0, 0)

	for name, packets := range map[string][][]byte{
		"PAT entry cut short": {
			psiPacket(0, true, pat(testPMT)),
			psiPacket(0, true, section(tableIDPAT, append(header, 0, 1, 0xE0|byte(testPMT>>8), byte(testPMT&0xFF), 0, 2))),
			psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264})),
		},
		"PMT without program info": {
			psiPacket(0, true, pat(testPMT)),
			psiPacket(testPMT, true, section(tableIDPMT, header)),
			psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264})),
		},
		"PMT stream entry cut short": {
			psiPacket(0, true, pat(testPMT)),
			psiPacket(testPMT, true, section(tableIDPMT, append(pmtHeader, 0x1B, 0xE1, 0x00, 0xF0, 0, 0x0F, 0xE1, 0x01))),
			psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264})),
		},
	} {
		t.Run(name, func(t *testing.T) {
			pkts := demux(t, append(packets, videoPacket())...)
			require.Equal(t, [][]byte{{StreamTypeH264}}, payloads(pkts, StreamTypeMetadata))
			require.Len(t, payloads(pkts, StreamTypeH264), 1)
		})
	}
}

// go test -fuzz FuzzDemuxer ./pkg/mpegts
func FuzzDemuxer(f *testing.F) {
	f.Add(bytes.Join([][]byte{
		psiPacket(0, true, pat(testPMT)),
		psiPacket(testPMT, true, pmt(stream{testVideo, StreamTypeH264}, stream{testAudio, StreamTypePCMUTapo})),
		videoPacket(), audioPacket(), pcrPacket(testVideo),
		tsPacketAF(testVideo, true, 100, pes(0xE0, videoUnit)),
		tsPacket(testVideo, false, videoUnit),
		videoPacket(),
	}, nil))
	f.Add(bytes.Join([][]byte{
		psiPacket(0, true, pat(testPMT, testPMT+1)),
		psiPacket(testPMT+1, true, pmt(stream{testVideo, StreamTypeAAC}, stream{testAudio, StreamTypePrivateOPUS})),
		tsPacket(testVideo, true, pes(0xC0, []byte{0xFF, 0xF1, 0x50, 0x80, 0x01, 0x7F, 0xFC, 1, 2})),
		tsPacket(testAudio, true, pes(0xC0, []byte{0x7F, 0xE0, 0x02, 1, 2})),
		psiPacket(testPMT, true, pmt(stream{0x200, StreamTypeH265})),
		tsPacket(0x200, true, pes(0xE0, []byte{0, 0, 0, 1, 0x26, 0x01, 0xAF})),
		tsPacket(0x200, true, pes(0xE0, []byte{0, 0, 0, 1, 0x02, 0x01, 0xAF})),
	}, nil))

	f.Fuzz(func(t *testing.T, b []byte) {
		d := NewDemuxer()
		rd := bytes.NewReader(b)
		for {
			if _, err := d.ReadPacket(rd); err != nil {
				return
			}
		}
	})
}

// The tests from upstream (#2587), on their own packet builders

func TestDemuxerReadsLatePMT(t *testing.T) {
	pat := testPATPacket()
	firstPMT := testPMTPacket(testStream{pid: 0x100, streamType: StreamTypeH264})
	d := NewDemuxer()

	pkt, err := d.ReadPacket(bytes.NewReader(append(pat, firstPMT...)))
	require.NoError(t, err)
	require.Equal(t, []byte{StreamTypeH264}, pkt.Payload)

	latePMT := testPMTPacket(
		testStream{pid: 0x100, streamType: StreamTypeH264},
		testStream{pid: 0x101, streamType: StreamTypeH264},
		testStream{pid: 0x91, streamType: StreamTypePCMUTapo},
	)
	_, err = d.ReadPacket(bytes.NewReader(latePMT))
	require.ErrorIs(t, err, io.EOF)
	require.Contains(t, d.pes, uint16(0x91))
	require.Equal(t, byte(StreamTypePCMUTapo), d.pes[0x91].StreamType)
	// a second H.264 stream would interleave with the first one in its track
	require.NotContains(t, d.pes, uint16(0x101))

	audioPES := []byte{0, 0, 1, 0xC0, 0, 0, 0x80, 0, 0, 0x11, 0x22}
	audio := testTSPacket(0x91, true, audioPES)
	pkt, err = d.ReadPacket(bytes.NewReader(append(audio, audio...)))
	require.NoError(t, err)
	require.Equal(t, byte(StreamTypePCMUTapo), pkt.PayloadType)
	require.Equal(t, []byte{0x11, 0x22}, pkt.Payload[:2])
}

// The streams come from the last program in the PAT, a stream of another
// program is added
func TestDemuxerReadsEveryPATProgram(t *testing.T) {
	d := NewDemuxer()
	pat := testPATPacket(0x1000, 0x1001)
	firstPMT := testPMTPacketAt(0x1001, testStream{pid: 0x100, streamType: StreamTypeH264})

	pkt, err := d.ReadPacket(bytes.NewReader(append(pat, firstPMT...)))
	require.NoError(t, err)
	require.Equal(t, []byte{StreamTypeH264}, pkt.Payload)

	secondPMT := testPMTPacketAt(0x1000, testStream{pid: 0x101, streamType: StreamTypeAAC})
	_, err = d.ReadPacket(bytes.NewReader(secondPMT))
	require.ErrorIs(t, err, io.EOF)
	require.Contains(t, d.pes, uint16(0x101))
	require.Equal(t, byte(StreamTypeAAC), d.pes[0x101].StreamType)
}

func TestDemuxerMalformedPSINoPanic(t *testing.T) {
	validPAT := testPATPacket()
	validPMT := testPMTPacket(testStream{pid: 0x100, streamType: StreamTypeH264})

	badPATPointer := testTSPacket(0, true, []byte{0xFF})
	badPATLength := testPSIPacket(0, []byte{0x00, 0xBF, 0xFF})
	badPMTPointer := testTSPacket(0x1000, true, []byte{0xFF})
	badPMTLength := testPSIPacket(0x1000, []byte{0x02, 0xBF, 0xFF})
	badProgramInfo := testPMTPacket(testStream{pid: 0x100, streamType: StreamTypeH264})
	badProgramInfo[4+1+10], badProgramInfo[4+1+11] = 0xFF, 0xFF
	badESInfo := testPMTPacket(testStream{pid: 0x100, streamType: StreamTypeH264})
	badESInfo[4+1+15], badESInfo[4+1+16] = 0xFF, 0xFF
	continuation := testTSPacket(0x1000, false, []byte{0x02, 0xFF, 0xFF})

	tests := []struct {
		name   string
		packet []byte
	}{
		{"PAT pointer past packet", badPATPointer},
		{"PAT section past packet", badPATLength},
		{"PMT pointer past packet", badPMTPointer},
		{"PMT section past packet", badPMTLength},
		{"PMT program info past section", badProgramInfo},
		{"PMT ES info past section", badESInfo},
		{"PMT continuation", continuation},
	}

	for _, tt := range tests {
		t.Run(tt.name+" before first PMT", func(t *testing.T) {
			input := append(append([]byte{}, validPAT...), tt.packet...)
			_, err := NewDemuxer().ReadPacket(bytes.NewReader(input))
			require.ErrorIs(t, err, io.EOF)
		})

		t.Run(tt.name+" after first PMT", func(t *testing.T) {
			d := NewDemuxer()
			_, err := d.ReadPacket(bytes.NewReader(append(append([]byte{}, validPAT...), validPMT...)))
			require.NoError(t, err)

			_, err = d.ReadPacket(bytes.NewReader(tt.packet))
			require.ErrorIs(t, err, io.EOF)
		})
	}
}

type testStream struct {
	pid        uint16
	streamType byte
}

func testPATPacket(pmtPIDs ...uint16) []byte {
	if len(pmtPIDs) == 0 {
		pmtPIDs = []uint16{0x1000}
	}
	sectionLength := 9 + len(pmtPIDs)*4
	section := []byte{
		0x00, 0xB0 | byte(sectionLength>>8), byte(sectionLength), // table ID, section length
		0x00, 0x01, 0xC1, 0x00, 0x00, // transport stream and section header
	}
	for i, pid := range pmtPIDs {
		section = append(section, 0x00, byte(i+1), 0xE0|byte(pid>>8), byte(pid))
	}
	section = append(section, 0x00, 0x00, 0x00, 0x00) // CRC
	return testPSIPacket(0, section)
}

func testPMTPacket(streams ...testStream) []byte {
	return testPMTPacketAt(0x1000, streams...)
}

func testPMTPacketAt(pid uint16, streams ...testStream) []byte {
	sectionLength := 13 + len(streams)*5
	section := []byte{
		0x02, 0xB0 | byte(sectionLength>>8), byte(sectionLength),
		0x00, 0x01, 0xC1, 0x00, 0x00, // program and section header
		0xE1, 0x00, 0xF0, 0x00, // PCR PID and no program descriptors
	}
	for _, stream := range streams {
		section = append(section, stream.streamType, 0xE0|byte(stream.pid>>8), byte(stream.pid), 0xF0, 0x00)
	}
	section = append(section, 0x00, 0x00, 0x00, 0x00) // CRC
	return testPSIPacket(pid, section)
}

func testPSIPacket(pid uint16, section []byte) []byte {
	return testTSPacket(pid, true, append([]byte{0}, section...))
}

func testTSPacket(pid uint16, start bool, payload []byte) []byte {
	packet := bytes.Repeat([]byte{0xFF}, PacketSize)
	packet[0] = SyncByte
	packet[1] = byte(pid >> 8 & 0x1F)
	if start {
		packet[1] |= 0x40
	}
	packet[2] = byte(pid)
	packet[3] = 0x10 // payload only
	copy(packet[4:], payload)
	return packet
}
