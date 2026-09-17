package core

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func testVideoReceiver() *Receiver {
	media := &Media{Kind: KindVideo, Direction: DirectionRecvonly}
	return NewReceiver(media, &Codec{Name: CodecH264, ClockRate: 90000})
}

// A consumer is judged by the receiver its sender ended up on: a reconnect
// retires the old receiver to its successor, and closes the old one later.
func TestReceiverGoneFollowsRetirement(t *testing.T) {
	live := testVideoReceiver()
	require.False(t, live.Gone())

	// same codec: retired to the new track, the old conn is closed afterwards
	old := testVideoReceiver()
	moved := testVideoReceiver()
	old.Retire(moved)
	old.Close()
	require.False(t, old.Gone(), "a retired receiver lives on in its successor")

	// parked, then dropped as stale by a later reconnect
	parked := testVideoReceiver()
	moved.Retire(parked)
	require.False(t, old.Gone())
	parked.Close()
	require.True(t, old.Gone(), "the end of the chain was dropped")

	dropped := testVideoReceiver()
	dropped.Close()
	require.True(t, dropped.Gone())
}

// A backchannel mixer fed by a receiver that gets dropped must lose that
// parent, and close when it was the last one: nothing may keep feeding it.
func TestDroppedReceiverDetachesMixer(t *testing.T) {
	audio := &Media{Kind: KindAudio, Direction: DirectionRecvonly}
	codec := &Codec{Name: CodecPCMA, ClockRate: 8000}
	receiver := NewReceiver(audio, codec)

	mixer := NewRTPMixer("ffmpeg", &Media{Kind: KindAudio, Direction: DirectionSendonly}, codec)
	mixer.AddParentWithCodec(&receiver.Node, codec)
	require.Len(t, receiver.children(), 1)

	receiver.Close()

	require.Empty(t, receiver.children(), "the mixer is detached from the dropped receiver")
	require.Nil(t, receiver.Forward, "the receiver no longer forwards into the mixer")
	mixer.mu.Lock()
	parents, closing := len(mixer.parents), mixer.closing
	mixer.mu.Unlock()
	require.Zero(t, parents)
	require.True(t, closing, "a mixer without parents closes")
}
