package onvif

import (
	"net/url"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/pion/rtp"
)

// probeTimeout keeps a probe within the patience of a client: UniFi Protect
// gives up on a request after 10 s
var probeTimeout = 5 * time.Second

// probeRetry: a stream whose probe failed is not dialed again before
var probeRetry = time.Minute

type probe struct {
	done chan struct{}
	at   time.Time
	ok   bool
}

var probes = map[string]*probe{}
var probesMu sync.Mutex

// probeOnce reads the video of a stream: a prober joins the running stream,
// or dials it, and waits for its parameter sets. The stream keeps them in its
// offers. A stream whose probe failed (offline, no keyframe in time) is not
// probed again for probeRetry. It reports if the stream was probed, by it or
// by another request it waited for. The key is the stream with its sources.
func probeOnce(key string, stream *streams.Stream) bool {
	probesMu.Lock()
	if p := probes[key]; p != nil {
		select {
		case <-p.done:
			if !p.ok && time.Since(p.at) < probeRetry {
				probesMu.Unlock()
				return false
			}
		default:
			probesMu.Unlock()
			<-p.done // another request probes it
			return true
		}
	}
	p := &probe{done: make(chan struct{}), at: time.Now()}
	probes[key] = p
	probesMu.Unlock()

	p.ok = runProbe(stream, p.at.Add(probeTimeout))
	close(p.done)
	return true
}

// runProbe attaches a prober until the stream told its video, at most until
// deadline: it reports if it did. The prober leaves the stream also when the
// probe gave up on it, a dial that hangs leaves it when it returns.
func runProbe(stream *streams.Stream, deadline time.Time) bool {
	cons := newProber()
	added := make(chan error, 1)
	go func() {
		added <- stream.AddConsumer(cons)
	}()

	timeout := time.NewTimer(time.Until(deadline))
	defer timeout.Stop()

	select {
	case err := <-added:
		if err != nil {
			log.Debug().Err(err).Msg("[onvif] probe")
			return false
		}
	case <-timeout.C:
		go func() {
			if <-added == nil {
				stream.RemoveConsumer(cons)
			}
		}()
		return false
	}
	defer stream.RemoveConsumer(cons)

	// an SDP without parameter sets: they come with the first keyframe
	select {
	case <-cons.params:
		return true
	case <-timeout.C:
		return false
	}
}

// prober is a consumer of the video of a stream, it waits for its SPS
type prober struct {
	core.Connection
	params chan struct{}
	once   sync.Once
}

func newProber() *prober {
	return &prober{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "onvif",
			Medias:     core.ParseQuery(url.Values{"video": {""}}),
		},
		params: make(chan struct{}),
	}
}

func (p *prober) AddTrack(media *core.Media, _ *core.Codec, track *core.Receiver) error {
	codec := track.Codec
	sender := core.NewSender(media, codec)
	sender.Handler = func(*rtp.Packet) {}

	switch {
	case hasSPS(codec.Name, codec.Fmtp()):
		p.paramsKnown()
	case codec.Name == core.CodecH264 || codec.Name == core.CodecH265:
		// the depacketizer writes the parameter sets of the first keyframe
		// to the codec of the stream, its offers have them from then on
		handler := func(*rtp.Packet) {
			if hasSPS(codec.Name, codec.Fmtp()) {
				p.paramsKnown()
			}
		}
		switch {
		case codec.Name == core.CodecH264 && codec.IsRTP():
			handler = h264.RTPDepay(codec, handler)
		case codec.Name == core.CodecH264:
			handler = h264.RepairAVCC(codec, handler)
		case codec.IsRTP():
			handler = h265.RTPDepay(codec, handler)
		default:
			handler = h265.RepairAVCC(codec, handler)
		}
		sender.Handler = handler
	default:
		p.paramsKnown() // a video without parameter sets, nothing to wait for
	}

	sender.WithParent(track).Start()
	p.Senders = append(p.Senders, sender)
	return nil
}

func (p *prober) paramsKnown() {
	p.once.Do(func() { close(p.params) })
}

// hasSPS tells if the fmtp of a codec has its SPS, the size of its video
func hasSPS(codec, fmtp string) bool {
	switch codec {
	case core.CodecH264:
		sps, _ := h264.GetParameterSet(fmtp)
		return len(sps) > 0
	case core.CodecH265:
		_, sps, _ := h265.GetParameterSet(fmtp)
		return len(sps) > 2
	}
	return false
}
