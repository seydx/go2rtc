package core

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSenser(t *testing.T) {
	recv := make(chan *Packet) // blocking receiver

	sender := NewSender(nil, &Codec{})
	sender.Output = func(packet *Packet) {
		recv <- packet
	}
	require.Equal(t, "new", sender.State())

	sender.Start()
	require.Equal(t, "connected", sender.State())

	sender.Input(&Packet{})
	sender.Input(&Packet{})

	require.Equal(t, 2, sender.Packets)
	require.Equal(t, 0, sender.Drops)

	// important to read one before close
	// because goroutine in Start() can run with nil chan
	// it's OK in real life, but bad for test
	_, ok := <-recv
	require.True(t, ok)

	sender.Close()
	require.Equal(t, "closed", sender.State())

	sender.Input(&Packet{})

	require.Equal(t, 2, sender.Packets)
	require.Equal(t, 1, sender.Drops)

	// read 2nd
	_, ok = <-recv
	require.True(t, ok)

	// read 3rd
	select {
	case <-recv:
		ok = true
	default:
		ok = false
	}
	require.False(t, ok)
}

// A sender waiting for its peer (WebRTC before ICE completes) must not pile
// up video it would burst out on start: a decoder starts at the latest
// keyframe, everything before it is dead weight.
func TestSenderQueuesFromLatestKeyframeBeforeStart(t *testing.T) {
	t.Run("rtp", func(t *testing.T) {
		receiver := NewReceiver(nil, &Codec{Name: CodecH264, ClockRate: 90000, PayloadType: 96})
		recv := make(chan *Packet, 16)
		sender := NewSender(nil, receiver.Codec)
		sender.Output = func(p *Packet) { recv <- p }
		sender.WithParent(receiver)

		receiver.Input(rtpFragment("K1", 1000, 1, true))
		receiver.Input(rtpFragment("P1", 2000, 2, true))
		receiver.Input(rtpFragment("S", 3000, 3, false)) // parameter sets, same access unit
		receiver.Input(rtpFragment("K2", 3000, 4, true))
		receiver.Input(rtpFragment("P2", 4000, 5, true))

		sender.Start()
		require.Equal(t, []string{"S", "K2", "P2"}, payloads(collect(t, recv, 3)))
		bytes, packets, _ := sender.Stats()
		require.Equal(t, 3, packets)
		require.Equal(t, 5, bytes)

		sender.Close()
		sender.Wait()
	})

	t.Run("avcc", func(t *testing.T) {
		receiver := newVideoReceiver()
		recv := make(chan *Packet, 16)
		sender := NewSender(nil, receiver.Codec)
		sender.Output = func(p *Packet) { recv <- p }
		sender.WithParent(receiver)

		receiver.Input(avcFrame("K1", 1000))
		receiver.Input(avcFrame("P1", 2000))
		receiver.Input(avcFrame("K2", 3000))
		receiver.Input(avcFrame("P2", 4000))

		sender.Start()
		require.Equal(t, []string{"K2", "P2"}, payloads(collect(t, recv, 2)))

		sender.Close()
		sender.Wait()
	})
}
