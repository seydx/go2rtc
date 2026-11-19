package onvif

import (
	"html"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DiscoveryGroup = "239.255.255.250"
	DiscoveryPort  = 3702

	probeAction = "http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe"
)

// Probe is the WS-Discovery probe of a client looking for devices
type Probe struct {
	MessageID string
	Types     []string
	Scopes    []string
}

// ParseProbe reads a WS-Discovery message, nil for anything but a probe
func ParseProbe(b []byte) *Probe {
	if strings.TrimSpace(FindTagValue(b, "Action")) != probeAction {
		return nil
	}
	return &Probe{
		MessageID: strings.TrimSpace(FindTagValue(b, "MessageID")),
		Types:     strings.Fields(FindTagValue(b, "Types")),
		Scopes:    strings.Fields(FindTagValue(b, "Scopes")),
	}
}

// Matches tells if a device with scopes answers the probe: it has every type
// and every scope the probe asks for, a scope by its leading path segments
func (p *Probe) Matches(scopes []string) bool {
	for _, t := range p.Types {
		if i := strings.LastIndexByte(t, ':'); i >= 0 {
			t = t[i+1:]
		}
		if t != "NetworkVideoTransmitter" && t != "Device" {
			return false
		}
	}
	for _, want := range p.Scopes {
		want = strings.TrimSuffix(want, "/")
		if !hasScope(scopes, want) {
			return false
		}
	}
	return true
}

func hasScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want || strings.HasPrefix(scope, want+"/") {
			return true
		}
	}
	return false
}

// the AppSequence of the answers: InstanceId changes with every start
var instanceID = strconv.FormatInt(time.Now().Unix(), 10)
var messageNumber atomic.Uint64

// ProbeMatchesResponse answers a probe for one device: endpoint is its stable
// urn:uuid, xaddr the address of its device service
func ProbeMatchesResponse(relatesTo, endpoint, xaddr string, scopes []string) []byte {
	return []byte(`<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:dn="http://www.onvif.org/ver10/network/wsdl" xmlns:tds="http://www.onvif.org/ver10/device/wsdl">` +
		`<s:Header>` +
		`<a:MessageID>urn:uuid:` + UUID() + `</a:MessageID>` +
		`<a:RelatesTo>` + html.EscapeString(relatesTo) + `</a:RelatesTo>` +
		`<a:To>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</a:To>` +
		`<a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/ProbeMatches</a:Action>` +
		`<d:AppSequence InstanceId="` + instanceID + `" MessageNumber="` + strconv.FormatUint(messageNumber.Add(1), 10) + `"/>` +
		`</s:Header>` +
		`<s:Body><d:ProbeMatches><d:ProbeMatch>` +
		`<a:EndpointReference><a:Address>` + html.EscapeString(endpoint) + `</a:Address></a:EndpointReference>` +
		`<d:Types>dn:NetworkVideoTransmitter tds:Device</d:Types>` +
		`<d:Scopes>` + html.EscapeString(strings.Join(scopes, " ")) + `</d:Scopes>` +
		`<d:XAddrs>` + html.EscapeString(xaddr) + `</d:XAddrs>` +
		`<d:MetadataVersion>1</d:MetadataVersion>` +
		`</d:ProbeMatch></d:ProbeMatches></s:Body></s:Envelope>`)
}

// sentProbes are the message IDs of the probes go2rtc sends itself: its own
// ONVIF server doesn't answer them, a camera of go2rtc is no camera to add
var sentProbes sync.Map

func IsSentProbe(messageID string) bool {
	_, ok := sentProbes.Load(messageID)
	return ok
}
