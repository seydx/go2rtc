package onvif

import (
	"errors"
	"net"
	"strconv"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/onvif"
	"golang.org/x/net/ipv4"
)

// discoveryAddress: on the group address the port is shared with the other
// listeners of the group on the host
var discoveryAddress = net.JoinHostPort(onvif.DiscoveryGroup, strconv.Itoa(onvif.DiscoveryPort))

var discoveryConn net.PacketConn
var discoveryMu sync.Mutex

// discover answers the WS-Discovery probes of clients while a device listens:
// UniFi Protect and Hass find the devices without an address typed in
func discover(on bool) {
	discoveryMu.Lock()
	defer discoveryMu.Unlock()

	if on == (discoveryConn != nil) {
		return
	}
	if !on {
		_ = discoveryConn.Close()
		discoveryConn = nil
		return
	}

	conn, err := net.ListenPacket("udp4", discoveryAddress)
	if err != nil {
		log.Warn().Err(err).Msg("[onvif] discovery")
		return
	}
	joinGroup(conn)
	discoveryConn = conn
	go answerProbes(conn)
}

// joinGroup joins the group on every interface: without one the socket gets
// the probes of the interface of the default route only
func joinGroup(conn net.PacketConn) {
	p := ipv4.NewPacketConn(conn)
	group := &net.UDPAddr{IP: net.ParseIP(onvif.DiscoveryGroup)}
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagMulticast != 0 {
			_ = p.JoinGroup(&iface, group)
		}
	}
}

func answerProbes(conn net.PacketConn) {
	b := make([]byte, 8192)
	for {
		n, addr, err := conn.ReadFrom(b)
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			continue // Windows reads the ICMP of a client gone before the answer as an error
		}

		probe := onvif.ParseProbe(b[:n])
		if probe == nil || onvif.IsSentProbe(probe.MessageID) {
			continue
		}
		for _, msg := range probeMatches(probe, addr.(*net.UDPAddr)) {
			_, _ = conn.WriteTo(msg, addr)
		}
	}
}

// probeMatches are the answers of the devices to the probe of client, one
// message each: UniFi Protect reads one device of a message
func probeMatches(probe *onvif.Probe, client *net.UDPAddr) (msgs [][]byte) {
	local := localIP(client)
	for _, l := range listeners() {
		addr := l.addr.(*net.TCPAddr)
		ip := addr.IP
		switch {
		case ip.IsUnspecified():
			if local == nil {
				continue
			}
			ip = local
		case ip.To4() == nil || ip.IsLoopback() != client.IP.IsLoopback():
			continue // the client can't reach it
		}

		id := l.device.identity("")
		if !probe.Matches(id.scopes) {
			continue
		}
		xaddr := "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(addr.Port)) + onvif.PathDevice
		msgs = append(msgs, onvif.ProbeMatchesResponse(probe.MessageID, id.endpoint, xaddr, id.scopes))
	}
	return
}

// localIP is the address of the host on the route to client
func localIP(client *net.UDPAddr) net.IP {
	conn, err := net.DialUDP("udp4", nil, client)
	if err != nil {
		return nil
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP
}
