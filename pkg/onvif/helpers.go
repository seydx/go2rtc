package onvif

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

type DiscoveryDevice struct {
	URL      string
	Name     string
	Hardware string
}

func FindTagValue(b []byte, tag string) string {
	re := regexp.MustCompile(`(?s)<(?:\w+:)?` + tag + `\b[^>]*>([^<]+)`)
	m := re.FindSubmatch(b)
	if len(m) != 2 {
		return ""
	}
	return string(m[1])
}

var (
	profileBlockRe = regexp.MustCompile(`(?s)<(?:\w+:)?Profiles\b.*?</(?:\w+:)?Profiles>`)
	profileTokenRe = regexp.MustCompile(`token="([^"]+)"`)
	encoderBlockRe = regexp.MustCompile(`(?s)<(?:\w+:)?VideoEncoderConfiguration\b.*?</(?:\w+:)?VideoEncoderConfiguration>`)
)

// parseProfiles extracts the token plus video encoding/resolution from a
// GetProfiles response. Encoding and resolution are read from each profile's
// VideoEncoderConfiguration block so the camera-reported codec (H264/H265/JPEG)
// stays mapped to the correct token — consumers use this to keep MJPEG profiles
// out of the decode/detection roles.
func parseProfiles(b []byte) []Profile {
	var profiles []Profile
	for _, block := range profileBlockRe.FindAll(b, -1) {
		m := profileTokenRe.FindSubmatch(block)
		if m == nil {
			continue
		}
		p := Profile{Token: string(m[1])}
		if enc := encoderBlockRe.Find(block); enc != nil {
			p.Encoding = FindTagValue(enc, "Encoding")
			p.Width = parseInt(FindTagValue(enc, "Width"))
			p.Height = parseInt(FindTagValue(enc, "Height"))
		}
		profiles = append(profiles, p)
	}
	return profiles
}

func parseInt(s string) int {
	i, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return i
}

// UUID - generate something like 44302cbf-0d18-4feb-79b3-33b575263da3
func UUID() string {
	s := core.RandString(32, 16)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// DiscoveryStreamingDevices probes every usable IPv4 interface for WS-Discovery
// devices. An unbound socket multicasts only via the OS default route, which on
// multi-homed desktops is often a VPN or virtual adapter. Windows Firewall also
// drops unsolicited unicast replies to a multicast probe, so each interface
// additionally gets a directed probe sweep of its subnet: replies to those pass
// the stateful firewall without any rule.
func DiscoveryStreamingDevices(timeout time.Duration) ([]DiscoveryDevice, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		seen    = map[string]bool{}
		devices []DiscoveryDevice
	)

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}

			wg.Add(1)
			go func(iface net.Interface, ipnet *net.IPNet) {
				defer wg.Done()
				for _, device := range probeInterface(iface, ipnet, timeout) {
					mu.Lock()
					if !seen[device.URL] {
						seen[device.URL] = true
						devices = append(devices, device)
					}
					mu.Unlock()
				}
			}(iface, ipnet)
		}
	}

	wg.Wait()
	return devices, nil
}

func probeInterface(iface net.Interface, ipnet *net.IPNet, timeout time.Duration) []DiscoveryDevice {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ipnet.IP})
	if err != nil {
		return nil
	}

	defer conn.Close()

	// binding the source IP alone doesn't pin multicast egress on every platform
	_ = ipv4.NewPacketConn(conn).SetMulticastInterface(&iface)

	msg := []byte(probeMessage())

	multicast := &net.UDPAddr{
		IP:   net.IP{239, 255, 255, 250},
		Port: 3702,
	}
	_, _ = conn.WriteToUDP(msg, multicast)

	for _, host := range sweepHosts(ipnet) {
		_, _ = conn.WriteToUDP(msg, &net.UDPAddr{IP: host, Port: 3702})
	}

	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	var devices []DiscoveryDevice

	b := make([]byte, 8192)
	for {
		n, addr, err := conn.ReadFromUDP(b)
		if err != nil {
			break
		}

		//log.Printf("[onvif] discovery response addr=%s:\n%s", addr, b[:n])

		// ignore printers, etc
		if !strings.Contains(string(b[:n]), "onvif") {
			continue
		}

		device := DiscoveryDevice{
			URL: FindTagValue(b[:n], "XAddrs"),
		}

		if device.URL == "" {
			continue
		}

		// XAddrs may list several space-separated URLs (IPv4 + IPv6),
		// keep the first IPv4 one
		if fields := strings.Fields(device.URL); len(fields) > 1 {
			device.URL = fields[0]
			for _, field := range fields {
				if !strings.Contains(field, "[") {
					device.URL = field
					break
				}
			}
		}

		// fix some buggy cameras
		// <wsdd:XAddrs>http://0.0.0.0:8080/onvif/device_service</wsdd:XAddrs>
		if s, ok := strings.CutPrefix(device.URL, "http://0.0.0.0"); ok {
			device.URL = "http://" + addr.IP.String() + s
		}

		// try to find the camera name and model (hardware)
		scopes := FindTagValue(b[:n], "Scopes")
		device.Name = findScope(scopes, "onvif://www.onvif.org/name/")
		device.Hardware = findScope(scopes, "onvif://www.onvif.org/hardware/")

		devices = append(devices, device)
	}

	return devices
}

func probeMessage() string {
	// https://www.onvif.org/wp-content/uploads/2016/12/ONVIF_Feature_Discovery_Specification_16.07.pdf
	// 5.3 Discovery Procedure:
	return `<?xml version="1.0" ?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
	<s:Header xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing">
		<a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action>
		<a:MessageID>urn:uuid:` + UUID() + `</a:MessageID>
		<a:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</a:To>
	</s:Header>
	<s:Body>
		<d:Probe xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery">
			<d:Types />
			<d:Scopes />
		</d:Probe>
	</s:Body>
</s:Envelope>`
}

// sweepHosts lists directed-probe targets: every host of the interface subnet,
// capped to the /24 around the interface IP so a wide mask doesn't turn into
// a huge sweep
func sweepHosts(ipnet *net.IPNet) []net.IP {
	ip4 := ipnet.IP.To4()
	ones, bits := ipnet.Mask.Size()
	if ip4 == nil || bits != 32 {
		return nil
	}
	if ones < 24 {
		ones = 24
	}
	if ones > 30 {
		return nil
	}

	base := ip4.Mask(net.CIDRMask(ones, 32))
	size := 1 << (32 - ones)

	hosts := make([]net.IP, 0, size-2)
	for i := 1; i < size-1; i++ {
		host := make(net.IP, 4)
		copy(host, base)
		host[3] += byte(i)
		if host.Equal(ip4) {
			continue
		}
		hosts = append(hosts, host)
	}

	return hosts
}

func findScope(s, prefix string) string {
	s = core.Between(s, prefix, " ")
	s, _ = url.QueryUnescape(s)
	return s
}

func atoi(s string) int {
	if s == "" {
		return 0
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return i
}

func GetPosixTZ(current time.Time) string {
	// Thanks to https://github.com/Path-Variable/go-posix-time
	_, offset := current.Zone()

	if current.IsDST() {
		_, end := current.ZoneBounds()
		endPlus1 := end.Add(time.Hour * 25)
		_, offset = endPlus1.Zone()
	}

	var prefix string
	if offset < 0 {
		prefix = "GMT+"
		offset = -offset / 60
	} else {
		prefix = "GMT-"
		offset = offset / 60
	}

	return prefix + fmt.Sprintf("%02d:%02d", offset/60, offset%60)
}

// SanitizeQuery repairs URLs where params were chained with '?' instead of '&'
// ("onvif://cam?timeout=10?subtype=X" — produced by older discovery versions and
// still present in stored source URLs). Every '?' after the first becomes '&'.
func SanitizeQuery(rawURL string) string {
	i := strings.IndexByte(rawURL, '?')
	if i < 0 || !strings.Contains(rawURL[i+1:], "?") {
		return rawURL
	}
	return rawURL[:i+1] + strings.ReplaceAll(rawURL[i+1:], "?", "&")
}

// GetPath returns the path of a service XAddr so it can be re-based onto the
// reachable host. A full URL yields its path (Hikvision serves /onvif/Media,
// Tapo /onvif/service, not the /onvif/media_service default); a bare absolute
// path passes through; anything empty, relative, or unparseable falls back to
// defPath.
func GetPath(urlOrPath, defPath string) string {
	if urlOrPath == "" {
		return defPath
	}
	if urlOrPath[0] == '/' {
		return urlOrPath
	}
	if u, err := url.Parse(urlOrPath); err == nil && u.Path != "" && u.Path[0] == '/' {
		return u.Path
	}
	return defPath
}
