package onvif

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

type ONVIFDevice struct {
	URL       string
	Name      string
	Hardware  string
	UUID      string
	IPAddress string
}

func FindTagValue(b []byte, tag string) string {
	re := regexp.MustCompile(`(?s)<(?:[\w-]+:)?` + tag + `[^>]*>([^<]+)</(?:[\w-]+:)?` + tag + `>`)
	m := re.FindSubmatch(b)
	if len(m) != 2 {
		return ""
	}
	return string(m[1])
}

// UUID - generate something like 44302cbf-0d18-4feb-79b3-33b575263da3
func UUID() string {
	s := core.RandString(32, 16)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func extractScopeValue(scope, prefix string) string {
	value := strings.TrimPrefix(scope, prefix)
	return value
}

func DiscoveryONVIFDevices() ([]ONVIFDevice, error) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}

	defer conn.Close()

	// https://www.onvif.org/wp-content/uploads/2016/12/ONVIF_Feature_Discovery_Specification_16.07.pdf
	// 5.3 Discovery Procedure:
	msg := `<?xml version="1.0" ?>
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

	addr := &net.UDPAddr{
		IP:   net.IP{239, 255, 255, 250},
		Port: 3702,
	}

	if _, err = conn.WriteTo([]byte(msg), addr); err != nil {
		return nil, err
	}

	if err = conn.SetReadDeadline(time.Now().Add(time.Second * 3)); err != nil {
		return nil, err
	}

	// Map to deduplicate devices by their UUID
	deviceMap := make(map[string]*ONVIFDevice)

	b := make([]byte, 8192)
	for {
		n, addr, err := conn.ReadFromUDP(b)
		if err != nil {
			break
		}

		// Ignore non-ONVIF devices
		response := string(b[:n])
		if !strings.Contains(response, "onvif") {
			continue
		}

		// Extract UUID - this is often in EndpointReference
		uuid := FindTagValue(b[:n], "EndpointReference")
		if uuid == "" {
			// Alternatively look in Address
			uuid = FindTagValue(b[:n], "Address")
		}

		// Fallback to IP address if UUID is empty
		if uuid == "" {
			uuid = "unknown-" + addr.IP.String()
		}

		// Extract XAddrs
		xaddrs := FindTagValue(b[:n], "XAddrs")
		if xaddrs == "" {
			continue
		}

		// Get device from map or create new one
		_, exists := deviceMap[uuid]
		if !exists {
			firstURL := strings.Fields(xaddrs)[0]

			// Fix buggy URLs
			if strings.HasPrefix(firstURL, "http://0.0.0.0") {
				firstURL = "http://" + addr.IP.String() + firstURL[14:]
			}

			device := &ONVIFDevice{
				IPAddress: addr.IP.String(),
				UUID:      uuid,
				URL:       firstURL,
			}
			deviceMap[uuid] = device

			// Extract additional information from scopes
			scopes := FindTagValue(b[:n], "Scopes")
			if scopes != "" {
				scopesList := strings.Fields(scopes)

				for _, scope := range scopesList {
					scope = strings.TrimSpace(scope)

					// Extract name
					if strings.Contains(scope, "onvif://www.onvif.org/name/") {
						device.Name = extractScopeValue(scope, "onvif://www.onvif.org/name/")
						// Decode URL-encoded name
						if decodedName, err := url.QueryUnescape(device.Name); err == nil {
							device.Name = decodedName
						}
					}

					// Extract hardware
					if strings.Contains(scope, "onvif://www.onvif.org/hardware/") {
						device.Hardware = extractScopeValue(scope, "onvif://www.onvif.org/hardware/")
					}
				}
			}

			if device.Name == "" {
				device.Name = device.IPAddress
			}
		}

	}

	var devices []ONVIFDevice
	for _, device := range deviceMap {
		devices = append(devices, *device)
	}

	return devices, nil
}

func DiscoveryStreamingURLs() ([]string, error) {
	devices, err := DiscoveryONVIFDevices()
	if err != nil {
		return nil, err
	}

	var urls []string
	for _, device := range devices {
		urls = append(urls, device.URL)
	}

	return urls, nil
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
