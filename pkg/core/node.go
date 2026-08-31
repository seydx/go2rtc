package core

import (
	"sync"

	"github.com/pion/rtp"
)

//type Packet struct {
//	Payload     []byte
//	Timestamp   uint32 // PTS if DTS == 0 else DTS
//	Composition uint32 // CTS = PTS-DTS (for support B-frames)
//	Sequence    uint16
//}

type Packet = rtp.Packet

// HandlerFunc - process input packets (just like http.HandlerFunc)
type HandlerFunc func(packet *Packet)

// Filter - a decorator for any HandlerFunc
type Filter func(handler HandlerFunc) HandlerFunc

// Node - Receiver or Sender or Filter (transform)
type Node struct {
	Codec   *Codec
	Input   HandlerFunc
	Output  HandlerFunc
	Forward HandlerFunc

	id     uint32
	childs []*Node
	parent *Node
	// movedTo is set by MoveNode on a node that is retired for good.
	// A child attaching afterwards belongs on the successor: the topology
	// swap and the consumer attach race, and the loser must not end up on a
	// node nothing feeds anymore.
	movedTo *Node

	owner any

	mu sync.Mutex
}

func (n *Node) SetOwner(owner any) *Node {
	n.owner = owner
	return n
}

func (n *Node) GetOwner() any {
	return n.owner
}

func (n *Node) WithParent(parent *Node) *Node {
	parent.AppendChild(n)
	return n
}

func (n *Node) AppendChild(child *Node) {
	n.mu.Lock()
	if moved := n.movedTo; moved != nil {
		n.mu.Unlock()
		moved.AppendChild(child)
		return
	}
	n.childs = append(n.childs, child)
	n.mu.Unlock()

	child.mu.Lock()
	child.parent = n
	child.mu.Unlock()
}

// AttachRelay adds child to n's childs without linking child's parent.
// A relay keeps its own lifecycle: the upward close cascade from the
// relay's last consumer must not detach it from n, and n's teardown must
// not close the consumers riding on it (the owner detaches it explicitly
// via RemoveChild instead).
func (n *Node) AttachRelay(child *Node) {
	n.mu.Lock()
	n.childs = append(n.childs, child)
	n.mu.Unlock()
}

// Attached reports whether the node is still listed as a child of its
// parent. It turns false once the parent was closed underneath it (Close
// only detaches children, it doesn't close them) — the way to tell a live
// sender from an orphaned one.
func (n *Node) Attached() bool {
	n.mu.Lock()
	parent := n.parent
	n.mu.Unlock()

	if parent == nil {
		return false
	}

	parent.mu.Lock()
	defer parent.mu.Unlock()
	for _, child := range parent.childs {
		if child == n {
			return true
		}
	}
	return false
}

func (n *Node) RemoveChild(child *Node) {
	n.mu.Lock()
	for i, ch := range n.childs {
		if ch == child {
			n.childs = append(n.childs[:i], n.childs[i+1:]...)
			break
		}
	}
	n.mu.Unlock()
}

func (n *Node) Close() {
	if parent := n.parent; parent != nil {
		parent.RemoveChild(n)

		if len(parent.childs) == 0 {
			parent.Close()
		}
	} else {
		for _, child := range n.childs {
			// Skip closing mixers - they manage their own lifecycle
			// Mixers are closed by RemoveParent when the last parent is removed
			if _, isMixer := child.owner.(*RTPMixer); isMixer {
				continue
			}
			child.Close()
		}
	}
}

// MoveNode rewires src's children onto dst and marks src as retired:
// children attaching later are handed to dst, so an attach racing the swap
// cannot end up on a node nothing feeds anymore. src must be discarded.
func MoveNode(dst, src *Node) {
	src.mu.Lock()
	childs := src.childs
	src.childs = nil
	src.movedTo = dst
	src.mu.Unlock()

	// append: dst may already carry children of its own, and they stay
	dst.mu.Lock()
	dst.childs = append(dst.childs, childs...)
	dst.mu.Unlock()

	for _, child := range childs {
		child.mu.Lock()
		child.parent = dst
		child.mu.Unlock()
	}
}
