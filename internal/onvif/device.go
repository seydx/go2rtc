package onvif

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/url"
	"os"
)

// device is what a listener of the ONVIF server serves: all video sources, or
// a source with a listener of its own as a camera of its own. UniFi Protect
// takes a device as one camera, it knows no channels.
type device struct {
	listen string // of the source, empty for the device of all sources
}

// sources are the video sources of the device, none once its source left the
// config (its listener closes)
func (d device) sources() []sourceConfig {
	list := configs()
	if d.listen == "" {
		return list
	}
	for _, cfg := range list {
		if cfg.Listen == d.listen {
			return []sourceConfig{cfg}
		}
	}
	return []sourceConfig{}
}

// identity is how clients tell the devices apart. UniFi Protect takes the MAC
// of GetNetworkInterfaces, else the one of the IP: the same for every port, it
// joined the devices of a host into one camera. Hass takes the MAC, else the
// serial number.
type identity struct {
	model, serial, mac, endpoint string
	scopes                       []string
}

func (d device) identity(host string) identity {
	if d.listen != "" {
		for _, cfg := range configs() {
			if cfg.Listen == d.listen {
				return sourceIdentity(&cfg)
			}
		}
	}
	hostname, _ := os.Hostname()
	return identity{
		model:    "go2rtc",
		serial:   host,
		endpoint: endpointOf("go2rtc\n" + hostname),
		scopes: []string{
			"onvif://www.onvif.org/name/go2rtc",
			"onvif://www.onvif.org/location/github",
			"onvif://www.onvif.org/Profile/Streaming",
			"onvif://www.onvif.org/type/Network_Video_Transmitter",
		},
	}
}

// sourceIdentity: the name of the source is its model, Protect names a camera
// "<manufacturer> <model>"
func sourceIdentity(cfg *sourceConfig) identity {
	name := cfg.Name
	if name == "" {
		name = cfg.Token
	}
	mac := macOf(cfg.Token)
	return identity{
		model:    name,
		serial:   cfg.Token,
		mac:      mac,
		endpoint: endpointOf(cfg.Token),
		scopes: []string{
			"onvif://www.onvif.org/name/" + url.PathEscape(name),
			"onvif://www.onvif.org/hardware/go2rtc",
			"onvif://www.onvif.org/MAC/" + mac,
			"onvif://www.onvif.org/Profile/Streaming",
			"onvif://www.onvif.org/type/Network_Video_Transmitter",
		},
	}
}

// macOf is a locally administered unicast MAC of key, the way UniFi Protect
// makes one of a serial number
func macOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	sum[0] = sum[0]&0xfe | 0x02
	return net.HardwareAddr(sum[:6]).String()
}

// endpointOf is the WS-Discovery address of a device, a UUID of key
func endpointOf(key string) string {
	sum := sha256.Sum256([]byte("endpoint\n" + key))
	sum[6] = sum[6]&0x0f | 0x50
	sum[8] = sum[8]&0x3f | 0x80
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}
