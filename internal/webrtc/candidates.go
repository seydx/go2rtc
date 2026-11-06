package webrtc

import (
	"net"
	"strings"
	"sync"

	"github.com/AlexxIT/go2rtc/internal/api/ws"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/webrtc"
	"github.com/AlexxIT/go2rtc/pkg/xnet"
	pion "github.com/pion/webrtc/v4"
)

type Address struct {
	host     string
	Port     string
	Network  string
	Priority uint32
}

func (a *Address) Host() string {
	if a.host == "stun" {
		candidatesMu.Lock()
		servers := stuns
		candidatesMu.Unlock()
		ip, err := webrtc.GetCachedPublicIP(servers...)
		if err != nil {
			return ""
		}
		return ip.String()
	}
	return a.host
}

func (a *Address) Marshal() string {
	if host := a.Host(); host != "" {
		return webrtc.CandidateICE(a.Network, host, a.Port, a.Priority)
	}
	return ""
}

var candidatesMu sync.Mutex
var addresses []*Address
var configured int // the first addresses, the ones of the config
var stuns []string
var filters webrtc.Filters

func AddCandidate(network, address string) {
	candidatesMu.Lock()
	addCandidate(network, address)
	candidatesMu.Unlock()
}

func addCandidate(network, address string) {
	if network == "" {
		addCandidate("tcp", address)
		addCandidate("udp", address)
		return
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return
	}

	// start from 1, so manual candidates will be lower than built-in
	// and every next candidate will have a lower priority
	candidateIndex := 1 + len(addresses)

	priority := webrtc.CandidateHostPriority(network, candidateIndex)
	addresses = append(addresses, &Address{host, port, network, priority})
}

// setCandidates replaces the candidates of the config, the ones added at
// runtime (ngrok) stay after them
func setCandidates(network string, candidates []string, iceServers []pion.ICEServer, hosts []string) {
	candidatesMu.Lock()
	defer candidatesMu.Unlock()

	added := addresses[configured:]
	addresses = nil
	stuns = nil

	for _, candidate := range candidates {
		addCandidate(network, candidate)

		if strings.HasPrefix(candidate, "stun:") && stuns == nil {
			for _, ice := range iceServers {
				for _, url := range ice.URLs {
					if strings.HasPrefix(url, "stun:") {
						stuns = append(stuns, url[5:])
					}
				}
			}
		}
	}

	configured = len(addresses)

	for _, address := range added {
		addCandidate(address.Network, net.JoinHostPort(address.host, address.Port))
	}

	filters.Candidates = hosts
}

func GetCandidates() (candidates []string) {
	candidatesMu.Lock()
	list := addresses
	candidatesMu.Unlock()

	for _, address := range list {
		if candidate := address.Marshal(); candidate != "" {
			candidates = append(candidates, candidate)
		}
	}
	return
}

// FilterCandidate return true if candidate passed the check
func FilterCandidate(candidate *pion.ICECandidate) bool {
	if candidate == nil {
		return false
	}

	// remove container bridge addresses (docker0, br-xxxx, hassio, ...)
	if ip := net.ParseIP(candidate.Address); ip != nil && xnet.IsContainerIP(ip) {
		return false
	}

	candidatesMu.Lock()
	filters := filters
	candidatesMu.Unlock()

	// host candidate should be in the hosts list
	if candidate.Typ == pion.ICECandidateTypeHost && filters.Candidates != nil {
		if !core.Contains(filters.Candidates, candidate.Address) {
			return false
		}
	}

	if filters.Networks != nil {
		networkType := NetworkType(candidate.Protocol.String(), candidate.Address)
		if !core.Contains(filters.Networks, networkType) {
			return false
		}
	}

	return true
}

// NetworkType convert tcp/udp network to tcp4/tcp6/udp4/udp6
func NetworkType(network, host string) string {
	if strings.IndexByte(host, ':') >= 0 {
		return network + "6"
	} else {
		return network + "4"
	}
}

func asyncCandidates(tr *ws.Transport, cons *webrtc.Conn) {
	tr.WithContext(func(ctx map[any]any) {
		if candidates, ok := ctx["candidate"].([]string); ok {
			// process candidates that receive before this moment
			for _, candidate := range candidates {
				_ = cons.AddCandidate(candidate)
			}

			// remove already processed candidates
			delete(ctx, "candidate")
		}

		// set variable for process candidates after this moment
		ctx["webrtc"] = cons
	})

	for _, candidate := range GetCandidates() {
		log.Trace().Str("candidate", candidate).Msg("[webrtc] config")
		tr.Write(&ws.Message{Type: "webrtc/candidate", Value: candidate})
	}
}

func candidateHandler(tr *ws.Transport, msg *ws.Message) error {
	// process incoming candidate in sync function
	tr.WithContext(func(ctx map[any]any) {
		candidate := msg.String()
		log.Trace().Str("candidate", candidate).Msg("[webrtc] remote")

		if cons, ok := ctx["webrtc"].(*webrtc.Conn); ok {
			// if webrtc.Server already initialized - process candidate
			_ = cons.AddCandidate(candidate)
		} else {
			// or collect candidate and process it later
			list, _ := ctx["candidate"].([]string)
			ctx["candidate"] = append(list, candidate)
		}
	})

	return nil
}
