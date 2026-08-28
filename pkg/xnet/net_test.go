package xnet

import "testing"

func TestIsContainerInterface(t *testing.T) {
	for _, name := range []string{"docker0", "br-1a2b3c4d5e6f", "veth1234", "hassio", "podman0", "cni0", "flannel.1", "virbr0", "lxdbr0"} {
		if !IsContainerInterface(name) {
			t.Errorf("%s should be a container interface", name)
		}
	}
	for _, name := range []string{"eth0", "eno1", "enp3s0", "wlan0", "en0", "bond0", "vlan10", "bridge0", "lo"} {
		if IsContainerInterface(name) {
			t.Errorf("%s should not be a container interface", name)
		}
	}
}
