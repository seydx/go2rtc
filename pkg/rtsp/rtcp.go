package rtsp

import (
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// RFC 3550 requires RTP senders to send periodic RTCP Sender Reports.
// The NTP<->RTP mapping in them is the only way for a receiver to sync
// the clocks of multiple tracks (lipsync) and to map a stream onto its
// own wallclock. Some receivers (ex. UniFi Protect) log clock warnings,
// lose A/V sync or drift the recording timeline when SRs are missing.
// https://github.com/AlexxIT/go2rtc/issues/2303
const (
	srInterval  = 2500 * time.Millisecond // media senders commonly use 2..5s
	srMaxDrift  = 30 * time.Second        // hard re-anchor beyond this
	srSlewBand  = 150 * time.Millisecond  // ignore drift smaller than this
	srSlewStep  = 5 * time.Millisecond    // max timeline correction per report
	srStartBias = 100 * time.Millisecond  // assumed pipeline latency of the first frame

	srJump   = time.Second // RTP vs wallclock disagreement that starts a new timeline
	srWarmup = srInterval  // no jump detection while a consumer starts (GOP replay, startup burst)
	// divisor: the live edge decays by elapsed/srEdgeDecay (elapsed capped
	// at srMaxDrift), which is the slew rate (5ms per 2.5s), see jump
	srEdgeDecay = srInterval / srSlewStep
)

type senderReport struct {
	clockRate uint32
	packets   uint32
	octets    uint32
	anchorNTP time.Time // wallclock moment that maps to anchorTS
	anchorTS  uint32
	last      time.Time

	// timeline of the packets handed to the consumer, see jump
	start  time.Time // first packet
	prevTS uint32    // previous packet
	edgeTS uint32    // live edge: the packet least delayed relative to its RTP time
	edgeAt time.Time // when the live edge packet was handled
}

func (s *senderReport) count(packet *rtp.Packet) {
	s.packets++
	s.octets += uint32(len(packet.Payload))
}

// marshal returns an interleaved framed compound RTCP packet (SR+SDES)
// for the given RTP timestamp when a report is due, otherwise nil.
//
// The NTP<->RTP mapping is anchored once and then advanced along the RTP
// timeline, slewed only a few ms per report toward wallclock. Stamping
// each report with the current wallclock instead would jitter the mapping
// on every delivery stall or burst, which receivers translate into stream
// clock jumps (UniFi Protect logs "streamClock regression" / "Back time"
// DTS corrections on every such report).
func (s *senderReport) marshal(channel uint8, ssrc, ts uint32, now time.Time) []byte {
	if now.Sub(s.last) < srInterval {
		return nil
	}
	s.last = now

	if s.anchorNTP.IsZero() {
		// the first frame was produced slightly in the past, and the small
		// bias makes later slew corrections mostly forward, which receivers
		// tolerate better than backward corrections
		s.anchorNTP = now.Add(-srStartBias)
	} else {
		// int32 diff handles timestamp wraparound and reordering
		diff := int32(ts - s.anchorTS)
		s.anchorNTP = s.anchorNTP.Add(s.duration(diff))

		// absorb long-term clock drift between producer and wallclock
		if drift := now.Sub(s.anchorNTP); drift > srMaxDrift || drift < -srMaxDrift {
			s.anchorNTP = now.Add(-srStartBias)
		} else if drift > srSlewBand {
			s.anchorNTP = s.anchorNTP.Add(srSlewStep)
		} else if drift < -srSlewBand {
			s.anchorNTP = s.anchorNTP.Add(-srSlewStep)
		}
	}
	s.anchorTS = ts

	return s.report(channel, ssrc, ts)
}

// jump tracks the RTP timeline of the packets handed to the consumer and
// reports whether the packet with timestamp ts, handled at now, starts a new
// one. A new timeline needs a new mapping before its first packet: receivers
// map every packet with the last report they got (FFmpeg rtpdec does once a
// session has two tracks), and with the old anchor the first frames after a
// producer reconnect land hours or a random int32 apart from the rest.
//
// It is self-contained on purpose. Producer swaps, in-connection reconnects
// and cameras that restart their RTP clock all look the same here, and the
// sender queue may still hold packets of the old timeline when a swap
// happens elsewhere.
//
// Three shapes count as a jump:
//   - a backward RTP step beyond srJump, which B-frame reordering never
//     comes close to
//   - a packet more than srJump ahead of the live edge, the packet that
//     arrived least delayed relative to its RTP time. Stalls, bursts and
//     sender drops only ever make packets later than that edge, so only a
//     forward RTP step that wallclock cannot explain gets past it. The edge
//     decays at the slew rate so a producer clock that runs slow does not
//     blind it.
//   - a packet more than srMaxDrift behind the edge, which marshal would
//     hard re-anchor anyway, only after the packet instead of before it
//
// Media that falls behind wallclock by less without stepping backward is no
// jump: the mapping stays, and marshal slews it as before. Late delivery
// looks like that (a stall, or a sender queue that stays full and drops,
// whose packets never catch up), and so does a timeline that resumes where
// it stopped, ex. the Tapo reset fix in handleRawPacket after an in-connection
// reconnect. Re-anchoring late packets would be the report jitter receivers
// turn into clock jumps, so a resumed timeline keeps the old mapping:
// timestamps stay continuous, each track lags wallclock by its own outage
// until the slew absorbs it.
func (s *senderReport) jump(ts uint32, now time.Time) bool {
	if s.start.IsZero() {
		s.start, s.prevTS, s.edgeTS, s.edgeAt = now, ts, ts, now
		return false
	}

	step := s.duration(int32(ts - s.prevTS))
	s.prevTS = ts

	// the decay only has to follow a slow clock while packets flow (the edge
	// is re-expressed every srInterval); capped, a track that pauses for
	// minutes does not decay its way into a jump
	since := now.Sub(s.edgeAt)
	ahead := s.duration(int32(ts-s.edgeTS)) - since + min(since, srMaxDrift)/srEdgeDecay

	if step < -srJump || ahead > srJump || ahead < -srMaxDrift {
		// the new timeline brings its own edge; while the consumer warms
		// up it is only followed (the live packets queued during a GOP
		// replay can be ahead of it by a second, a startup burst likewise)
		s.edgeTS, s.edgeAt = ts, now
		return now.Sub(s.start) >= srWarmup
	}

	if ahead >= 0 {
		s.edgeTS, s.edgeAt = ts, now
	} else if since >= srInterval {
		// keep the decayed edge, but expressed relative to this packet so
		// the int32 differences above never overflow
		s.edgeTS = ts + uint32(s.ticks(-ahead))
		s.edgeAt = now
	}
	return false
}

// reanchor maps ts to now exactly like the first report of a session does
// and returns that report right away, regardless of srInterval. It returns
// nil while there is no mapping yet: the first regular report anchors it.
func (s *senderReport) reanchor(channel uint8, ssrc, ts uint32, now time.Time) []byte {
	if s.anchorNTP.IsZero() {
		return nil
	}
	s.last = now
	s.anchorNTP = now.Add(-srStartBias)
	s.anchorTS = ts

	return s.report(channel, ssrc, ts)
}

// report returns the interleaved framed SR+SDES for the current mapping
func (s *senderReport) report(channel uint8, ssrc, ts uint32) []byte {
	sr := rtcp.SenderReport{
		SSRC:        ssrc,
		NTPTime:     ntpTime(s.anchorNTP),
		RTPTime:     ts,
		PacketCount: s.packets,
		OctetCount:  s.octets,
	}
	sd := rtcp.SourceDescription{
		Chunks: []rtcp.SourceDescriptionChunk{{
			Source: ssrc,
			Items:  []rtcp.SourceDescriptionItem{{Type: rtcp.SDESCNAME, Text: "go2rtc"}},
		}},
	}

	data, err := rtcp.Marshal([]rtcp.Packet{&sr, &sd})
	if err != nil {
		return nil
	}

	b := make([]byte, 4, 4+len(data))
	b[0] = '$'
	b[1] = channel
	b[2] = byte(len(data) >> 8)
	b[3] = byte(len(data))
	return append(b, data...)
}

// duration converts RTP ticks to time
func (s *senderReport) duration(ticks int32) time.Duration {
	return time.Duration(ticks) * time.Second / time.Duration(s.clockRate)
}

// ticks converts time to RTP ticks
func (s *senderReport) ticks(d time.Duration) int64 {
	return int64(d) * int64(s.clockRate) / int64(time.Second)
}

// ntpTime converts wallclock time to a 64-bit fixed point NTP timestamp
func ntpTime(t time.Time) uint64 {
	secs := uint64(t.Unix()) + 2208988800 // seconds between 1900 and 1970
	frac := (uint64(t.Nanosecond()) << 32) / 1e9
	return secs<<32 | frac
}
