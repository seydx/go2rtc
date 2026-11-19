package onvif

import (
	"encoding/xml"
	"testing"

	"github.com/stretchr/testify/require"
)

func probeWith(types, scopes string) []byte {
	return []byte(`<?xml version="1.0"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery">` +
		`<s:Header><a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action><a:MessageID>urn:uuid:1</a:MessageID></s:Header>` +
		`<s:Body><d:Probe><d:Types>` + types + `</d:Types><d:Scopes>` + scopes + `</d:Scopes></d:Probe></s:Body></s:Envelope>`)
}

// UniFi Protect probes for dn:NetworkVideoTransmitter, ONVIF Device Manager
// for tds:Device, go2rtc for anything. A probe with scopes finds the devices
// with all of them, a scope by its leading path segments.
func TestProbeMatches(t *testing.T) {
	scopes := []string{"onvif://www.onvif.org/name/Garden", "onvif://www.onvif.org/Profile/Streaming"}
	for _, c := range []struct {
		types, scopes string
		match         bool
	}{
		{"", "", true},
		{"dn:NetworkVideoTransmitter", "", true},
		{"tds:Device", "", true},
		{"dn:NetworkVideoTransmitter tds:Device", "", true},
		{"wsdp:Device pub:Computer", "", false},
		{"", "onvif://www.onvif.org/name", true},
		{"", "onvif://www.onvif.org/name/Garden", true},
		{"", "onvif://www.onvif.org/name/Garden onvif://www.onvif.org/Profile/Streaming", true},
		{"", "onvif://www.onvif.org/name/Gard", false},
		{"", "onvif://www.onvif.org/name/Garden onvif://www.onvif.org/location", false},
	} {
		probe := ParseProbe(probeWith(c.types, c.scopes))
		require.NotNil(t, probe)
		require.Equal(t, "urn:uuid:1", probe.MessageID)
		require.Equal(t, c.match, probe.Matches(scopes), "types %q scopes %q", c.types, c.scopes)
	}

	// the probe go2rtc sends itself, without types and scopes
	probe := ParseProbe([]byte(probeMessage("urn:uuid:2")))
	require.NotNil(t, probe)
	require.True(t, probe.Matches(scopes))

	// the answers of other devices are no probes
	require.Nil(t, ParseProbe(ProbeMatchesResponse("urn:uuid:2", "urn:uuid:3", "http://192.168.1.2:2006/onvif/device_service", scopes)))
}

// A name is any text: the answer stays valid XML and tells the scopes as they are
func TestProbeMatchesResponse(t *testing.T) {
	scopes := []string{"onvif://www.onvif.org/name/Tom%20&%20Jerry", "onvif://www.onvif.org/Profile/Streaming"}
	b := ProbeMatchesResponse("urn:uuid:<1>", "urn:uuid:3", "http://192.168.1.2:2006/onvif/device_service", scopes)

	var res struct {
		Action    string `xml:"Header>Action"`
		RelatesTo string `xml:"Header>RelatesTo"`
		Endpoint  string `xml:"Body>ProbeMatches>ProbeMatch>EndpointReference>Address"`
		Types     string `xml:"Body>ProbeMatches>ProbeMatch>Types"`
		Scopes    string `xml:"Body>ProbeMatches>ProbeMatch>Scopes"`
		XAddrs    string `xml:"Body>ProbeMatches>ProbeMatch>XAddrs"`
	}
	require.NoError(t, xml.Unmarshal(b, &res), "%s", b)
	require.Equal(t, "http://schemas.xmlsoap.org/ws/2005/04/discovery/ProbeMatches", res.Action)
	require.Equal(t, "urn:uuid:<1>", res.RelatesTo)
	require.Equal(t, "urn:uuid:3", res.Endpoint)
	require.Equal(t, "dn:NetworkVideoTransmitter tds:Device", res.Types)
	require.Equal(t, "onvif://www.onvif.org/name/Tom%20&%20Jerry onvif://www.onvif.org/Profile/Streaming", res.Scopes)
	require.Equal(t, "http://192.168.1.2:2006/onvif/device_service", res.XAddrs)
}
