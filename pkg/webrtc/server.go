package webrtc

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

func (c *Conn) SetOffer(offer string) (err error) {
	c.offer = offer

	sd := &sdp.SessionDescription{}
	if err = sd.Unmarshal([]byte(offer)); err != nil {
		return
	}

	// create transceivers with opposite direction
	for _, md := range sd.MediaDescriptions {
		var mid string
		var tr *webrtc.RTPTransceiver
		for _, attr := range md.Attributes {
			switch attr.Key {
			case core.DirectionSendRecv:
				tr, _ = c.pc.AddTransceiverFromTrack(NewTrack(md.MediaName.Media))
			case core.DirectionSendonly:
				tr, _ = c.pc.AddTransceiverFromKind(
					webrtc.NewRTPCodecType(md.MediaName.Media),
					webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
				)
			case core.DirectionRecvonly:
				tr, _ = c.pc.AddTransceiverFromTrack(
					NewTrack(md.MediaName.Media),
					webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly},
				)
			case "mid":
				mid = attr.Value
			}
		}

		if mid != "" && tr != nil {
			_ = tr.SetMid(mid)
		}
	}

	c.Medias = UnmarshalMedias(sd.MediaDescriptions)

	return
}

func (c *Conn) GetAnswer() (answer string, err error) {
	// we need to process remote offer after we create transeivers
	desc := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: c.offer}
	if err = c.pc.SetRemoteDescription(desc); err != nil {
		return "", err
	}

	// disable transceivers if we don't have track, make direction=inactive
transeivers:
	for _, tr := range c.pc.GetTransceivers() {
		for _, sender := range c.Senders {
			if sender.Media.ID == tr.Mid() {
				continue transeivers
			}
		}

		switch tr.Direction() {
		case webrtc.RTPTransceiverDirectionSendrecv:
			_ = tr.Sender().Stop()             // don't know if necessary
			_ = tr.SetSender(tr.Sender(), nil) // set direction to recvonly
		case webrtc.RTPTransceiverDirectionSendonly:
			_ = tr.Stop()
		}
	}

	if desc, err = c.pc.CreateAnswer(nil); err != nil {
		return
	}
	if err = c.pc.SetLocalDescription(desc); err != nil {
		return
	}

	return c.pc.LocalDescription().SDP, nil
}

const (
	// a STUN request from an address without a route out (ULA, link-local) only ends at pion's 5 s timeout
	gatherTimeout = 1500 * time.Millisecond
	// answers of the servers that are reachable come in a burst, what is still missing after this waits on a dead route
	gatherQuiet = 250 * time.Millisecond
)

// GetCompleteAnswer - get SDP answer with candidates inside
func (c *Conn) GetCompleteAnswer(candidates []string, filter func(*webrtc.ICECandidate) bool) (string, error) {
	var mu sync.Mutex
	var once sync.Once
	var quiet *time.Timer
	var done = make(chan struct{})
	finish := func() { once.Do(func() { close(done) }) }
	slowest := slowestCandidateType(c.pc.GetConfiguration().ICEServers)

	c.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			finish()
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if filter == nil || filter(candidate) {
			candidates = append(candidates, candidate.ToJSON().Candidate)
		}
		if quiet != nil {
			quiet.Reset(gatherQuiet)
		} else if candidate.Typ == slowest {
			quiet = time.AfterFunc(gatherQuiet, finish)
		}
	})

	answer, err := c.GetAnswer()
	if err != nil {
		return "", err
	}

	select {
	case <-done:
	case <-time.After(gatherTimeout):
	}

	// candidates that come in later don't touch the answer
	mu.Lock()
	gathered := slices.Clone(candidates)
	mu.Unlock()

	sd := &sdp.SessionDescription{}
	if err = sd.Unmarshal([]byte(answer)); err != nil {
		return "", err
	}

	md := sd.MediaDescriptions[0]

	for _, candidate := range gathered {
		md.WithPropertyAttribute(candidate)
	}

	// pion dials out from its active TCP candidates by itself, the far side cannot connect to them
	for _, md := range sd.MediaDescriptions {
		md.Attributes = slices.DeleteFunc(md.Attributes, func(attr sdp.Attribute) bool {
			return strings.HasPrefix(attr.Key, "candidate") && strings.Contains(attr.Key+attr.Value, "tcptype active")
		})
	}

	b, err := sd.Marshal()
	if err != nil {
		return "", err
	}

	return string(b), nil
}

func slowestCandidateType(servers []webrtc.ICEServer) webrtc.ICECandidateType {
	typ := webrtc.ICECandidateTypeHost
	for _, server := range servers {
		for _, url := range server.URLs {
			if strings.HasPrefix(url, "turn") {
				return webrtc.ICECandidateTypeRelay
			}
			if strings.HasPrefix(url, "stun") {
				typ = webrtc.ICECandidateTypeSrflx
			}
		}
	}
	return typ
}
