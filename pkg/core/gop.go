package core

import (
	"sync"
)

type GopCache struct {
	mu sync.RWMutex

	currentGOP        []*Packet
	previousGOP       []*Packet
	pendingRTPPackets []*Packet

	hasKeyframe      bool
	currentGOPFrames int
}

func NewGOPCache() *GopCache {
	return &GopCache{}
}

func (c *GopCache) Add(packet *Packet, isKeyframe bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if isKeyframe {
		if c.hasKeyframe && len(c.currentGOP) > 0 {
			c.previousGOP = make([]*Packet, len(c.currentGOP))
			copy(c.previousGOP, c.currentGOP)

			// 	len(c.previousGOP))
		}
		// else {
		// }

		c.currentGOP = c.currentGOP[:0]
		c.currentGOPFrames = 0
		c.hasKeyframe = true
	} else if !c.hasKeyframe {
		return // Ignore non-keyframes if no keyframe has been added yet
	}

	// 	packet.Header.SequenceNumber, packet.Header.Timestamp, len(packet.Payload))

	clone := &Packet{
		Header:  packet.Header,
		Payload: make([]byte, len(packet.Payload)),
	}
	copy(clone.Payload, packet.Payload)

	c.currentGOP = append(c.currentGOP, clone)
	c.currentGOPFrames++

	if len(c.pendingRTPPackets) > 0 {
		// 	len(c.pendingRTPPackets))
		c.pendingRTPPackets = c.pendingRTPPackets[:0]
	}
}

func (c *GopCache) AddRTPFragment(packet *Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if packet.Header.Version == 0 { // RTPPacketVersionAVC = AVCC
		return
	}

	// 	packet.Header.SequenceNumber, packet.Header.Timestamp, len(packet.Payload))

	clone := &Packet{
		Header:  packet.Header,
		Payload: make([]byte, len(packet.Payload)),
	}
	copy(clone.Payload, packet.Payload)

	c.pendingRTPPackets = append(c.pendingRTPPackets, clone)
}

func (c *GopCache) Get() []*Packet {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.hasKeyframe {
		return nil
	}

	totalPackets := len(c.previousGOP) + len(c.currentGOP) + len(c.pendingRTPPackets)
	if totalPackets == 0 {
		return nil
	}

	result := make([]*Packet, 0, totalPackets)
	result = append(result, c.previousGOP...)
	result = append(result, c.currentGOP...)
	result = append(result, c.pendingRTPPackets...)

	// 	len(c.previousGOP), len(c.currentGOP), len(c.pendingRTPPackets), len(result))

	return result
}

func (c *GopCache) HasContent() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// 	c.hasKeyframe, len(c.previousGOP), len(c.currentGOP), len(c.pendingRTPPackets))
	return c.hasKeyframe && (len(c.previousGOP) > 0 || len(c.currentGOP) > 0 || len(c.pendingRTPPackets) > 0)
}

func (c *GopCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.previousGOP = c.previousGOP[:0]
	c.currentGOP = c.currentGOP[:0]
	c.pendingRTPPackets = c.pendingRTPPackets[:0]
	c.hasKeyframe = false
	c.currentGOPFrames = 0
}
