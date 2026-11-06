package xnet

import (
	"net"
	"sync"
)

// Listener is a TCP listener that moves to another address
type Listener struct {
	mu   sync.Mutex
	addr string
	ln   net.Listener
}

// Listen closes the listener and opens one on address, when address is not the
// current one. It returns the new listener to serve, nil for none: the address
// is the same or empty. Open connections stay.
func (l *Listener) Listen(address string) (net.Listener, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if address == l.addr {
		return nil, nil
	}

	if l.ln != nil {
		_ = l.ln.Close()
		l.ln, l.addr = nil, ""
	}

	if address == "" {
		return nil, nil
	}

	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	l.ln, l.addr = ln, address
	return ln, nil
}
