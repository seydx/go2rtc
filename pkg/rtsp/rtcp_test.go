package rtsp

import (
	"net"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func parseSR(t *testing.T, b []byte, channel uint8) *rtcp.SenderReport {
	t.Helper()

	require.NotNil(t, b)
	require.Equal(t, byte('$'), b[0])
	require.Equal(t, channel, b[1])
	require.Equal(t, len(b)-4, int(b[2])<<8|int(b[3]))

	packets, err := rtcp.Unmarshal(b[4:])
	require.NoError(t, err)
	require.Len(t, packets, 2)

	sd, ok := packets[1].(*rtcp.SourceDescription)
	require.True(t, ok)
	require.Equal(t, rtcp.SDESCNAME, sd.Chunks[0].Items[0].Type)

	sr, ok := packets[0].(*rtcp.SenderReport)
	require.True(t, ok)
	return sr
}

func ntpToTime(ntp uint64) time.Time {
	secs := int64(ntp>>32) - 2208988800
	frac := int64((ntp & 0xFFFFFFFF) * 1e9 >> 32)
	return time.Unix(secs, frac)
}

func TestSenderReportBasic(t *testing.T) {
	now := time.Unix(1700000000, 0)
	s := senderReport{clockRate: 90000}

	s.count(&rtp.Packet{Payload: make([]byte, 100)})
	s.count(&rtp.Packet{Payload: make([]byte, 50)})

	sr := parseSR(t, s.marshal(3, 0x11223344, 1000, now), 3)
	require.Equal(t, uint32(0x11223344), sr.SSRC)
	require.Equal(t, uint32(1000), sr.RTPTime)
	require.Equal(t, uint32(2), sr.PacketCount)
	require.Equal(t, uint32(150), sr.OctetCount)

	// first report maps the RTP timestamp slightly into the past
	require.WithinDuration(t, now.Add(-srStartBias), ntpToTime(sr.NTPTime), time.Millisecond)
}

func TestSenderReportInterval(t *testing.T) {
	now := time.Unix(1700000000, 0)
	s := senderReport{clockRate: 90000}

	require.NotNil(t, s.marshal(1, 1, 0, now))
	require.Nil(t, s.marshal(1, 1, 3000, now.Add(time.Second)))
	require.NotNil(t, s.marshal(1, 1, 90000*3, now.Add(3*time.Second)))
}

func TestSenderReportMappingConsistency(t *testing.T) {
	// the NTP<->RTP mapping must advance along the RTP timeline even when
	// wallclock delivery is bursty, otherwise receivers see clock jumps
	const clockRate = 90000
	base := time.Unix(1700000000, 0)
	s := senderReport{clockRate: clockRate}

	first := parseSR(t, s.marshal(1, 1, 0, base), 1)

	for i := 1; i <= 10; i++ {
		ts := uint32(i) * 3 * clockRate // 3s of media per report
		mediaTime := time.Duration(i) * 3 * time.Second
		// delivery jitter oscillates around zero and stays inside the
		// slew dead band, so it must never disturb the mapping
		jitter := time.Duration(1-2*(i%2)) * 40 * time.Millisecond
		now := base.Add(mediaTime + jitter)

		sr := parseSR(t, s.marshal(1, 1, ts, now), 1)

		ntpDelta := ntpToTime(sr.NTPTime).Sub(ntpToTime(first.NTPTime))
		require.InDelta(t, mediaTime, ntpDelta, float64(time.Millisecond),
			"report %d: NTP advance must equal RTP advance", i)
	}
}

func TestSenderReportSlewBounds(t *testing.T) {
	const clockRate = 8000
	now := time.Unix(1700000000, 0)
	s := senderReport{clockRate: clockRate}

	prev := parseSR(t, s.marshal(1, 1, 0, now), 1)

	// producer runs 1% slow vs wallclock: drift accumulates, correction
	// must stay bounded to srSlewStep per report
	ts := uint32(0)
	for i := 1; i <= 5; i++ {
		ts += 3 * clockRate
		now = now.Add(3*time.Second + 500*time.Millisecond) // way past dead band

		sr := parseSR(t, s.marshal(1, 1, ts, now), 1)

		mediaTime := 3 * time.Second
		ntpDelta := ntpToTime(sr.NTPTime).Sub(ntpToTime(prev.NTPTime))
		require.LessOrEqual(t, (ntpDelta - mediaTime).Abs(), srSlewStep+time.Millisecond,
			"report %d: correction exceeds slew step", i)
		prev = sr
	}
}

func TestSenderReportHardReset(t *testing.T) {
	const clockRate = 90000
	now := time.Unix(1700000000, 0)
	s := senderReport{clockRate: clockRate}

	parseSR(t, s.marshal(1, 1, 0, now), 1)

	// producer stalled for a minute with no RTP advance: drift exceeds
	// srMaxDrift, mapping must re-anchor to wallclock
	now = now.Add(time.Minute)
	sr := parseSR(t, s.marshal(1, 1, 3000, now), 1)
	require.WithinDuration(t, now.Add(-srStartBias), ntpToTime(sr.NTPTime), time.Millisecond)
}

func TestSenderReportTimestampWraparound(t *testing.T) {
	const clockRate = 90000
	now := time.Unix(1700000000, 0)
	s := senderReport{clockRate: clockRate}

	start := uint32(0xFFFFFFFF - clockRate) // 1s before wrap
	first := parseSR(t, s.marshal(1, 1, start, now), 1)

	now = now.Add(3 * time.Second)
	sr := parseSR(t, s.marshal(1, 1, start+3*clockRate, now), 1) // wrapped

	ntpDelta := ntpToTime(sr.NTPTime).Sub(ntpToTime(first.NTPTime))
	require.InDelta(t, 3*time.Second, ntpDelta, float64(time.Millisecond))
}

func TestNTPTime(t *testing.T) {
	// known value: 1900-01-01 maps to 0
	require.Equal(t, uint64(0), ntpTime(time.Unix(-2208988800, 0)))
	// half second fraction
	require.Equal(t, uint64(2208988800)<<32|1<<31, ntpTime(time.Unix(0, 5e8)))
}

func TestSenderReportUDPRouting(t *testing.T) {
	// for transport=udp the framed report must arrive unframed on the
	// RTCP socket of the pair (odd channel)
	rtpConn, rtcpConn, err := ListenUDPPair()
	require.NoError(t, err)
	defer rtpConn.Close()
	defer rtcpConn.Close()

	sink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer sink.Close()

	c := &Conn{
		Transport: "udp",
		udpConn:   []*net.UDPConn{rtpConn, rtcpConn},
		udpAddr: []*net.UDPAddr{
			sink.LocalAddr().(*net.UDPAddr), // RTP (unused here)
			sink.LocalAddr().(*net.UDPAddr), // RTCP
		},
	}

	s := senderReport{clockRate: 90000}
	b := s.marshal(1, 0x42, 90000, time.Unix(1700000000, 0))
	require.NoError(t, c.writeInterleavedData(b))

	_ = sink.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1500)
	n, _, err := sink.ReadFromUDP(buf)
	require.NoError(t, err)

	packets, err := rtcp.Unmarshal(buf[:n])
	require.NoError(t, err)
	sr, ok := packets[0].(*rtcp.SenderReport)
	require.True(t, ok)
	require.Equal(t, uint32(0x42), sr.SSRC)
}

// pkt is one packet as the consumer handler sees it: RTP time and the
// wallclock offset (from the session start) at which it is handled
type pkt struct {
	ts uint32
	at time.Duration
}

// feedJumps runs the packets through jump and returns the indexes it
// reported as the start of a new timeline
func feedJumps(s *senderReport, base time.Time, pkts []pkt) []int {
	var jumps []int
	for i, p := range pkts {
		if s.jump(p.ts, base.Add(p.at)) {
			jumps = append(jumps, i)
		}
	}
	return jumps
}

// live appends packets of a source with the given clock rate and packet
// interval, stamped by a producer clock running at speed (1 = wallclock),
// handled with a deterministic jitter of up to ±jitter
func live(pkts []pkt, clockRate uint32, ts uint32, at, dur, every time.Duration, speed float64, jitter time.Duration) ([]pkt, uint32, time.Duration) {
	seed := uint32(len(pkts)*7919 + 1)
	for t := time.Duration(0); t < dur; t += every {
		seed = seed*1664525 + 1013904223
		j := time.Duration(int64(seed>>8)%int64(2*jitter+1)) - jitter
		if jitter == 0 {
			j = 0
		}
		pts := ts + uint32(float64(t)*speed*float64(clockRate)/float64(time.Second))
		pkts = append(pkts, pkt{ts: pts, at: at + t + jitter + j})
	}
	ts += uint32(float64(dur) * speed * float64(clockRate) / float64(time.Second))
	return pkts, ts, at + dur
}

func TestSenderReportJumpSteadyState(t *testing.T) {
	// none of these are a new timeline: a report on any of them would be
	// the SR jitter receivers turn into clock jumps
	const clockRate = 90000
	const frame = 40 * time.Millisecond // 25 fps
	base := time.Unix(1700000000, 0)

	t.Run("jitter", func(t *testing.T) {
		pkts, _, _ := live(nil, clockRate, 1234, 0, 5*time.Minute, frame, 1, 80*time.Millisecond)
		require.Empty(t, feedJumps(&senderReport{clockRate: clockRate}, base, pkts))
	})

	t.Run("b-frames", func(t *testing.T) {
		// decode order I P B B P B B: the B-frames step back up to 2 frames
		var pkts []pkt
		order := []int{0, 3, 1, 2, 6, 4, 5}
		for gop := 0; gop < 500; gop++ {
			for i, o := range order {
				n := gop*len(order) + o
				at := time.Duration(gop*len(order)+i) * frame
				pkts = append(pkts, pkt{ts: uint32(n) * 3600, at: at})
			}
		}
		require.Empty(t, feedJumps(&senderReport{clockRate: clockRate}, base, pkts))
	})

	t.Run("stall and burst", func(t *testing.T) {
		pkts, ts, at := live(nil, clockRate, 0, 0, 10*time.Second, frame, 1, 0)
		// 5s nothing, then the backlog arrives within 50ms
		for i := 0; i < 125; i++ {
			pkts = append(pkts, pkt{ts: ts + uint32(i)*3600, at: at + 5*time.Second + time.Duration(i)*400*time.Microsecond})
		}
		ts += 125 * 3600
		pkts, _, _ = live(pkts, clockRate, ts, at+5*time.Second+50*time.Millisecond, 10*time.Second, frame, 1, 0)
		require.Empty(t, feedJumps(&senderReport{clockRate: clockRate}, base, pkts))
	})

	t.Run("sender drops", func(t *testing.T) {
		// a slow consumer: packets queue up 4s late, then the full queue
		// drops 3s of them; the first packet after the gap is still late
		pkts, ts, at := live(nil, clockRate, 0, 0, 10*time.Second, frame, 1, 0)
		pkts, ts, at = live(pkts, clockRate, ts, at+4*time.Second, 5*time.Second, frame, 1, 0)
		ts += 3 * clockRate
		pkts, _, _ = live(pkts, clockRate, ts, at, 10*time.Second, frame/4, 1, 0)
		require.Empty(t, feedJumps(&senderReport{clockRate: clockRate}, base, pkts))
	})

	t.Run("gop replay", func(t *testing.T) {
		// replayGOP squeezes the cached frames into 10ms steps; the live
		// packets queued meanwhile follow up to a replay duration ahead
		// (worst case here: one step, and the replay overshot its budget)
		var pkts []pkt
		for i := 0; i < 100; i++ {
			pkts = append(pkts, pkt{ts: uint32(i) * 900, at: time.Duration(i) * 10 * time.Millisecond})
		}
		pkts, _, _ = live(pkts, clockRate, 99*900+clockRate*13/10, time.Second, time.Minute, frame, 1, 0)
		require.Empty(t, feedJumps(&senderReport{clockRate: clockRate}, base, pkts))
	})

	t.Run("silence", func(t *testing.T) {
		// audio stops for 2 minutes while its clock keeps running
		const audioRate = 16000
		pkts, ts, at := live(nil, audioRate, 0, 0, 10*time.Second, 64*time.Millisecond, 1, 0)
		pkts, _, _ = live(pkts, audioRate, ts+2*60*audioRate, at+2*time.Minute, 10*time.Second, 64*time.Millisecond, 1, 0)
		require.Empty(t, feedJumps(&senderReport{clockRate: audioRate}, base, pkts))
	})

	t.Run("long silence", func(t *testing.T) {
		// the edge decay must not add up to a jump over a long pause: a
		// clock that kept running, or one that ran slow meanwhile, is
		// still the same timeline
		for _, rate := range []uint32{8000, clockRate} {
			for _, gap := range []time.Duration{10 * time.Minute, time.Hour} {
				for _, speed := range []float64{1, 0.999} {
					pkts, ts, at := live(nil, rate, 0, 0, 10*time.Second, 20*time.Millisecond, speed, 0)
					ts += uint32(float64(gap) * speed * float64(rate) / float64(time.Second))
					pkts, _, _ = live(pkts, rate, ts, at+gap, 10*time.Second, 20*time.Millisecond, speed, 0)
					require.Empty(t, feedJumps(&senderReport{clockRate: rate}, base, pkts), "rate %d gap %v speed %v", rate, gap, speed)
				}
			}
		}
	})

	t.Run("clock drift", func(t *testing.T) {
		for _, speed := range []float64{0.999, 1.002} {
			pkts, _, _ := live(nil, clockRate, 0, 0, 30*time.Minute, frame, speed, 20*time.Millisecond)
			require.Empty(t, feedJumps(&senderReport{clockRate: clockRate}, base, pkts), "speed %v", speed)
		}
	})

	t.Run("stamped without gap", func(t *testing.T) {
		// a camera that resumes its clock where it stopped (ex. the Tapo
		// reset fix in handleRawPacket) stays on one timeline
		pkts, ts, at := live(nil, clockRate, 0, 0, 10*time.Second, frame, 1, 0)
		pkts, _, _ = live(pkts, clockRate, ts+clockRate/10, at+3*time.Second, 10*time.Second, frame, 1, 0)
		require.Empty(t, feedJumps(&senderReport{clockRate: clockRate}, base, pkts))
	})
}

func TestSenderReportJumpDetect(t *testing.T) {
	const clockRate = 90000
	const frame = 40 * time.Millisecond
	base := time.Unix(1700000000, 0)

	run := func(t *testing.T, start uint32, jump int64, outage time.Duration) []int {
		t.Helper()
		pkts, ts, at := live(nil, clockRate, start, 0, 10*time.Second, frame, 1, 30*time.Millisecond)
		pkts, _, _ = live(pkts, clockRate, uint32(int64(ts)+jump), at+outage, 10*time.Second, frame, 1, 30*time.Millisecond)
		return feedJumps(&senderReport{clockRate: clockRate}, base, pkts)
	}
	first := int(10 * time.Second / frame) // index of the first packet after the outage

	for _, tc := range []struct {
		name   string
		start  uint32
		jump   time.Duration // RTP step across the outage
		outage time.Duration
	}{
		{"forward hours", 1000, 3 * time.Hour, time.Second},
		{"forward 10s", 1000, 10 * time.Second, time.Second},
		{"forward 3s quick", 1000, 3 * time.Second, 300 * time.Millisecond},
		{"backward 5s", 1000, -5 * time.Second, time.Second},
		{"backward hours", 0x80000000, -5 * time.Hour, 2 * time.Second},
		{"wrap", 0xFFFFFFFF - clockRate, 2 * time.Hour, time.Second},
		{"behind beyond max drift", 1000, 10 * time.Second, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jump := int64(tc.jump) * clockRate / int64(time.Second)
			require.Equal(t, []int{first}, run(t, tc.start, jump, tc.outage))
		})
	}

	t.Run("random bases", func(t *testing.T) {
		// what a camera or publisher restart typically looks like
		seed := uint32(42)
		for i := 0; i < 200; i++ {
			seed = seed*1664525 + 1013904223
			jump := int64(int32(seed))
			outage := time.Duration(1+i%3) * time.Second
			// only a new base that happens to land within srJump of
			// wallclock (ahead) or of the last packet (behind) is missed,
			// and then nothing needs fixing
			d := time.Duration(jump) * time.Second / clockRate
			if d > -srJump-frame && d < outage+srJump+frame {
				continue
			}
			require.Equal(t, []int{first}, run(t, seed, jump, outage), "jump %v outage %v", d, outage)
		}
	})
}

func TestSenderReportJumpWarmup(t *testing.T) {
	const clockRate = 90000
	const frame = 40 * time.Millisecond
	base := time.Unix(1700000000, 0)

	// a swap within the first srWarmup is only followed, the next is reported
	pkts, ts, at := live(nil, clockRate, 0, 0, time.Second, frame, 1, 0)
	pkts, ts, at = live(pkts, clockRate, ts+uint32(time.Hour.Seconds())*clockRate, at+500*time.Millisecond, 10*time.Second, frame, 1, 0)
	second := len(pkts)
	pkts, _, _ = live(pkts, clockRate, ts-uint32(time.Minute.Seconds())*clockRate, at+500*time.Millisecond, 10*time.Second, frame, 1, 0)

	require.Equal(t, []int{second}, feedJumps(&senderReport{clockRate: clockRate}, base, pkts))
}

func TestSenderReportJumpSlowClock(t *testing.T) {
	// an edge that never decayed would sit above a slow producer clock by
	// minutes after a while and swallow every forward jump of that size
	const clockRate = 90000
	const frame = 40 * time.Millisecond
	base := time.Unix(1700000000, 0)

	pkts, ts, at := live(nil, clockRate, 0, 0, time.Hour, frame, 0.999, 0) // 3.6s behind after an hour
	first := len(pkts)
	pkts, _, _ = live(pkts, clockRate, ts+2*clockRate, at+500*time.Millisecond, 10*time.Second, frame, 0.999, 0)

	require.Equal(t, []int{first}, feedJumps(&senderReport{clockRate: clockRate}, base, pkts))

	// a clock slower than the edge decays falls further behind it for
	// longer than int32 RTP differences reach (6.6h at 90kHz)
	pkts, _, _ = live(nil, clockRate, 0, 0, 8*time.Hour, time.Second, 0.9975, 0)
	require.Empty(t, feedJumps(&senderReport{clockRate: clockRate}, base, pkts))
}

func TestSenderReportReanchor(t *testing.T) {
	const clockRate = 90000
	now := time.Unix(1700000000, 0)
	s := senderReport{clockRate: clockRate}

	// nothing to re-anchor before the first report
	require.Nil(t, s.reanchor(1, 1, 5000, now))

	parseSR(t, s.marshal(1, 1, 0, now), 1)
	require.NotNil(t, s.marshal(1, 1, 3*clockRate, now.Add(3*time.Second)))

	// new timeline 3s later: mapped like the first report of a session
	// and reported at once, regardless of the interval
	now = now.Add(6 * time.Second)
	ts := uint32(0x9ABCDEF0)
	sr := parseSR(t, s.reanchor(1, 7, ts, now), 1)
	require.Equal(t, ts, sr.RTPTime)
	require.Equal(t, uint32(7), sr.SSRC)
	require.WithinDuration(t, now.Add(-srStartBias), ntpToTime(sr.NTPTime), time.Millisecond)

	// it counts as a report: the regular cadence restarts from here
	require.Nil(t, s.marshal(1, 7, ts+clockRate, now.Add(time.Second)))

	// and the mapping continues along the new timeline
	next := parseSR(t, s.marshal(1, 7, ts+3*clockRate, now.Add(3*time.Second)), 1)
	ntpDelta := ntpToTime(next.NTPTime).Sub(ntpToTime(sr.NTPTime))
	require.InDelta(t, 3*time.Second, ntpDelta, float64(time.Millisecond))
}
