package core

import (
	"sync"
	"sync/atomic"

	"github.com/pion/rtp"
)

// RTPMixer normalizes RTP packets from multiple sources (consumers)
// by maintaining unified sequence numbers and timestamps.
// This allows multiple consumers to send backchannel audio to a single producer.
type RTPMixer struct {
	Node

	Media *Media
	Codec *Codec

	// RTP packet normalization
	seqNum    uint32 // atomic counter for sequence numbers
	timestamp uint32 // atomic counter for timestamps

	mu sync.Mutex
}

// NewRTPMixer creates a new RTP mixer for the given codec
func NewRTPMixer(media *Media, codec *Codec) *RTPMixer {
	m := &RTPMixer{
		Node:      Node{id: NewID(), Codec: codec},
		Media:     media,
		Codec:     codec,
	}

	// Initialize counters
	atomic.StoreUint32(&m.seqNum, 0)
	atomic.StoreUint32(&m.timestamp, 0)

	// Input function: normalizes packets from multiple sources and forwards to children
	m.Input = func(packet *Packet) {
		// Create a new packet with normalized sequence number and timestamp
		normalized := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Padding:        packet.Padding,
				Extension:      packet.Extension,
				Marker:         packet.Marker,
				PayloadType:    packet.PayloadType,
				SequenceNumber: uint16(atomic.AddUint32(&m.seqNum, 1) - 1),
				Timestamp:      atomic.AddUint32(&m.timestamp, m.calculateTimestampIncrement()),
				SSRC:           packet.SSRC,
				CSRC:           packet.CSRC,
			},
			Payload: packet.Payload,
		}

		// Forward normalized packet to all children (like a normal Receiver)
		m.mu.Lock()
		children := m.childs
		m.mu.Unlock()

		for _, child := range children {
			child.Input(normalized)
		}
	}

	return m
}

// calculateTimestampIncrement calculates the timestamp increment based on the packet
// For now, we use a simple heuristic based on common audio packet durations
func (m *RTPMixer) calculateTimestampIncrement() uint32 {
	// Most audio codecs use 20ms packets
	// timestamp_increment = sample_rate * packet_duration
	// For 20ms: increment = sample_rate * 0.02

	switch m.Codec.Name {
	case CodecPCMA, CodecPCMU, CodecPCML, CodecPCM, CodecFLAC:
		// 8000 Hz * 0.02s = 160 samples per 20ms
		return 160
	case CodecOpus:
		// 48000 Hz * 0.02s = 960 samples per 20ms
		return 960
	case CodecAAC, CodecELD:
		// Typically 1024 samples per frame at various sample rates
		// For 48kHz: 1024 samples
		return 1024
	case CodecG722:
		// 16000 Hz * 0.02s = 320 samples per 20ms
		return 320
	default:
		// Default: assume 20ms at codec clock rate
		if m.Codec.ClockRate > 0 {
			return m.Codec.ClockRate / 50 // 20ms = 1/50 second
		}
		return 160 // fallback
	}
}
