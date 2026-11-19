package onvif

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/onvif"
	"github.com/stretchr/testify/require"
)

// requestOf sends an action of a service to the device d
func requestOf(t *testing.T, d device, namespace, operation, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest("POST", "/onvif/device_service", strings.NewReader(soap(namespace, operation, body)))
	req.Host = "192.168.1.2:2006"
	w := httptest.NewRecorder()
	d.service(w, req)
	return w.Code, w.Body.Bytes()
}

func soap(namespace, operation, body string) string {
	return `<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>` +
		`<ns0:` + operation + ` xmlns:ns0="` + namespace + `">` + body + `</ns0:` + operation + `>` +
		`</s:Body></s:Envelope>`
}

type deviceInfo struct {
	Manufacturer string `xml:"Body>GetDeviceInformationResponse>Manufacturer"`
	Model        string `xml:"Body>GetDeviceInformationResponse>Model"`
	Serial       string `xml:"Body>GetDeviceInformationResponse>SerialNumber"`
}

func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, ln.Close())
	return ln.Addr().String()
}

// localDiscovery answers the probes on a port of its own on the loopback, not
// on the WS-Discovery port of the host
func localDiscovery(t *testing.T) {
	address := discoveryAddress
	discoveryAddress = "127.0.0.1:0"
	t.Cleanup(func() { discoveryAddress = address })
}

// UniFi Protect takes an ONVIF device as one camera: of the device with all
// cameras as video sources it took the main stream of one camera, the sub
// stream of another and the snapshot of a third. A video source with a listen
// of its own is a device of its own, with its profiles, its name and its MAC.
func TestDeviceOfEachCamera(t *testing.T) {
	freshProbes(t)
	newStub(t, "garden_main", &stubSource{codec: h264Codec(fmtpH264High)})
	newStub(t, "garden_sub", &stubSource{codec: h264Codec(fmtpH264Base)})
	newStub(t, "door", &stubSource{codec: h264Codec(fmtpH264Base)})
	setManufacturer("camera.ui")
	t.Cleanup(func() { setManufacturer("") })
	useConfig(t,
		sourceConfig{Token: "cam1", Name: "Garden & Yard", Profiles: []string{"garden_main", "garden_sub"}, Listen: ":2006"},
		sourceConfig{Token: "cam2", Name: "Door", Profiles: []string{"door"}, Listen: ":2007"},
	)
	garden, door := device{listen: ":2006"}, device{listen: ":2007"}

	profiles := func(d device) (tokens []string) {
		var res struct {
			Profiles []profileXML `xml:"Body>GetProfilesResponse>Profiles"`
		}
		_, b := requestOf(t, d, onvif.MediaNamespace, "GetProfiles", "")
		unmarshal(t, b, &res)
		for _, p := range res.Profiles {
			tokens = append(tokens, p.Token)
		}
		return
	}
	require.Equal(t, []string{"garden_main", "garden_sub"}, profiles(garden))
	require.Equal(t, []string{"door"}, profiles(door))
	require.Equal(t, []string{"garden_main", "garden_sub", "door"}, profiles(device{}))

	// a profile of another camera is none of this one
	code, _ := requestOf(t, garden, onvif.MediaNamespace, "GetStreamUri", "<ns0:ProfileToken>door</ns0:ProfileToken>")
	require.Equal(t, http.StatusBadRequest, code)
	w := httptest.NewRecorder()
	garden.snapshot(w, httptest.NewRequest("GET", snapshotPath+"?src=door", nil))
	require.Equal(t, http.StatusNotFound, w.Code)

	// Protect names the camera "<manufacturer> <model>"
	var info deviceInfo
	_, b := requestOf(t, garden, onvif.DeviceNamespace, "GetDeviceInformation", "")
	unmarshal(t, b, &info)
	require.Equal(t, deviceInfo{Manufacturer: "camera.ui", Model: "Garden & Yard", Serial: "cam1"}, info)

	mac := func(d device) string {
		var res struct {
			MAC string `xml:"Body>GetNetworkInterfacesResponse>NetworkInterfaces>Info>HwAddress"`
		}
		_, b := requestOf(t, d, onvif.DeviceNamespace, "GetNetworkInterfaces", "")
		unmarshal(t, b, &res)
		return res.MAC
	}
	// the MAC of a camera stays: Protect would take it for another camera
	require.Equal(t, "fa:54:23:40:b7:f3", mac(garden))
	require.NotEqual(t, mac(garden), mac(door))
	require.Empty(t, mac(device{}))

	var scopes struct {
		Items []string `xml:"Body>GetScopesResponse>Scopes>ScopeItem"`
	}
	_, b = requestOf(t, garden, onvif.DeviceNamespace, "GetScopes", "")
	unmarshal(t, b, &scopes)
	require.Contains(t, scopes.Items, "onvif://www.onvif.org/name/Garden%20&%20Yard")
	require.Contains(t, scopes.Items, "onvif://www.onvif.org/MAC/fa:54:23:40:b7:f3")
}

// camera.ui gives its cameras a port each and moves them with a saved config,
// without a restart of go2rtc
func TestListenerOfEachCamera(t *testing.T) {
	localDiscovery(t)
	t.Cleanup(func() { reloadConfig([]byte("streams: {}\n")) })

	model := func(address string) string {
		t.Helper()
		res, err := http.Post("http://"+address+"/onvif/device_service", "application/soap+xml",
			strings.NewReader(soap(onvif.DeviceNamespace, "GetDeviceInformation", "")))
		require.NoError(t, err)
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		var info deviceInfo
		unmarshal(t, b, &info)
		return info.Model
	}
	config := func(garden, door string) []byte {
		return []byte("onvif:\n  video_sources:\n" +
			"    - {token: cam1, name: Garden, profiles: [garden], listen: \"" + garden + "\"}\n" +
			"    - {token: cam2, name: Door, profiles: [door], listen: \"" + door + "\"}\n")
	}
	first, second, third := freeAddress(t), freeAddress(t), freeAddress(t)

	reloadConfig(config(first, second))
	require.Equal(t, "Garden", model(first))
	require.Equal(t, "Door", model(second))

	// the cameras swap their ports
	reloadConfig(config(second, first))
	require.Equal(t, "Door", model(first))
	require.Equal(t, "Garden", model(second))

	reloadConfig(config(third, first))
	require.Equal(t, "Garden", model(third))
	_, err := net.Dial("tcp", second)
	require.Error(t, err)

	reloadConfig([]byte("streams: {}\n"))
	for _, address := range []string{first, third} {
		_, err = net.Dial("tcp", address)
		require.Error(t, err)
	}
}

type probeMatch struct {
	RelatesTo string `xml:"Header>RelatesTo"`
	Endpoint  string `xml:"Body>ProbeMatches>ProbeMatch>EndpointReference>Address"`
	Scopes    string `xml:"Body>ProbeMatches>ProbeMatch>Scopes"`
	XAddrs    string `xml:"Body>ProbeMatches>ProbeMatch>XAddrs"`
}

// probeOf is a probe the way UniFi Protect sends it
func probeOf(id, scopes string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:dn="http://www.onvif.org/ver10/network/wsdl">` +
		`<s:Header><a:Action s:mustUnderstand="1">http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action><a:MessageID>` + id + `</a:MessageID>` +
		`<a:ReplyTo><a:Address>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</a:Address></a:ReplyTo>` +
		`<a:To s:mustUnderstand="1">urn:schemas-xmlsoap-org:ws:2005:04:discovery</a:To></s:Header>` +
		`<s:Body><d:Probe><d:Types>dn:NetworkVideoTransmitter</d:Types><d:Scopes>` + scopes + `</d:Scopes></d:Probe></s:Body></s:Envelope>`)
}

// UniFi Protect and Hass find ONVIF cameras with a WS-Discovery probe to the
// multicast group: go2rtc answered none, its cameras had to be typed in. Every
// device answers with a message of its own, Protect reads one device of a
// message.
func TestDiscoveryOfEachCamera(t *testing.T) {
	localDiscovery(t)
	t.Cleanup(func() { reloadConfig([]byte("streams: {}\n")) })

	all, garden := freeAddress(t), freeAddress(t)
	reloadConfig([]byte("onvif:\n  listen: \"" + all + "\"\n  video_sources:\n" +
		"    - {token: cam1, name: Garden, profiles: [garden], listen: \"" + garden + "\"}\n"))

	discoveryMu.Lock()
	server := discoveryConn.LocalAddr()
	discoveryMu.Unlock()

	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer client.Close()

	ask := func(id, scopes string) (matches []probeMatch) {
		t.Helper()
		_, err := client.WriteTo(probeOf(id, scopes), server)
		require.NoError(t, err)
		b := make([]byte, 8192)
		for {
			_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			n, err := client.Read(b)
			if err != nil {
				return
			}
			var match probeMatch
			unmarshal(t, b[:n], &match)
			require.Equal(t, id, match.RelatesTo)
			matches = append(matches, match)
		}
	}

	matches := ask("urn:uuid:1", "")
	require.Len(t, matches, 2)
	require.Equal(t, "http://"+all+"/onvif/device_service", matches[0].XAddrs)
	require.Equal(t, "http://"+garden+"/onvif/device_service", matches[1].XAddrs)
	require.Contains(t, strings.Fields(matches[1].Scopes), "onvif://www.onvif.org/name/Garden")
	require.NotEqual(t, matches[0].Endpoint, matches[1].Endpoint)

	// a probe for a scope finds the devices with it only
	matches = ask("urn:uuid:2", "onvif://www.onvif.org/name/Garden")
	require.Len(t, matches, 1)
	require.Equal(t, "http://"+garden+"/onvif/device_service", matches[0].XAddrs)
	require.Empty(t, ask("urn:uuid:3", "onvif://www.onvif.org/name/Door"))

	// ONVIF off: no answers
	reloadConfig([]byte("streams: {}\n"))
	discoveryMu.Lock()
	require.Nil(t, discoveryConn)
	discoveryMu.Unlock()
}
