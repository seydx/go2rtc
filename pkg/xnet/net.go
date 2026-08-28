package xnet

import (
	"net"
	"strconv"
	"strings"
)

// container bridges by interface name: docker0, br-xxxx (docker networks),
// hassio (supervisor), podman/cni/flannel/cali (podman, k8s), virbr (libvirt),
// lxc/lxd. Matching by name instead of the 172.16.0.0/12 range keeps LANs in
// that range (172.21.x.x is not rare) usable for WebRTC and mDNS.
var containerInterfacePrefixes = []string{
	"docker", "br-", "veth", "hassio", "podman", "cni", "flannel", "cali", "virbr", "lxc", "lxd",
}

func IsContainerInterface(name string) bool {
	for _, prefix := range containerInterfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// ContainerIPs returns the addresses of container bridge interfaces, which
// are unreachable from the LAN.
func ContainerIPs() (ips []net.IP) {
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if !IsContainerInterface(iface.Name) {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if v, ok := addr.(*net.IPNet); ok {
				ips = append(ips, v.IP)
			}
		}
	}
	return
}

func IsContainerIP(ip net.IP) bool {
	for _, cip := range ContainerIPs() {
		if cip.Equal(ip) {
			return true
		}
	}
	return false
}

// ParseUnspecifiedPort will return port if address is unspecified
// ex. ":8555" or "0.0.0.0:8555"
func ParseUnspecifiedPort(address string) int {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return 0
	}

	if host != "" && host != "0.0.0.0" && host != "[::]" {
		return 0
	}

	i, _ := strconv.Atoi(port)
	return i
}

func IPNets(ipFilter func(ip net.IP) bool) ([]*net.IPNet, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var nets []*net.IPNet

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, _ := iface.Addrs() // range on nil slice is OK
		for _, addr := range addrs {
			switch v := addr.(type) {
			case *net.IPNet:
				ip := v.IP.To4()
				if ip == nil {
					continue
				}
				if ipFilter != nil && !ipFilter(ip) {
					continue
				}
				nets = append(nets, v)
			}
		}
	}

	return nets, nil
}
