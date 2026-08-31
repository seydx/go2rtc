package core

import (
	"testing"

	"github.com/pion/rtp"
)

func moveTestReceiver() *Receiver {
	return NewReceiver(nil, &Codec{Name: CodecH264, ClockRate: 90000})
}

func countingSender() (*Sender, *int) {
	count := 0
	s := NewSender(nil, &Codec{Name: CodecH264, ClockRate: 90000})
	s.Output = func(*Packet) { count++ }
	s.Input = func(*Packet) { count++ }
	return s, &count
}

func TestMoveNodeForwardsLateAttaches(t *testing.T) {
	oldRecv := moveTestReceiver()
	newRecv := moveTestReceiver()

	oldRecv.Replace(newRecv)

	// the consumer picked oldRecv up before the swap and attaches after it
	late, hits := countingSender()
	late.WithParent(oldRecv)

	newRecv.Input(&rtp.Packet{})
	if *hits == 0 {
		t.Fatal("a sender attached to a replaced receiver gets nothing")
	}
	if !late.Node.Attached() {
		t.Fatal("the late sender is not attached to the successor")
	}
}

func TestMoveNodeKeepsTheTargetsOwnChildren(t *testing.T) {
	oldRecv := moveTestReceiver()
	newRecv := moveTestReceiver()

	early, earlyHits := countingSender()
	early.WithParent(newRecv)

	moved, movedHits := countingSender()
	moved.WithParent(oldRecv)

	oldRecv.Replace(newRecv)

	newRecv.Input(&rtp.Packet{})
	if *earlyHits == 0 {
		t.Fatal("the target's own child was dropped by the move")
	}
	if *movedHits == 0 {
		t.Fatal("the moved child gets nothing")
	}
}
