package core

import (
	"sync"
)

type PrebufferCache struct {
	mu sync.RWMutex

	packets        []*Packet
	maxDurationSec int
	clockRate      uint32
	hasContent     bool
}

func NewPrebufferCache(maxDurationSec int, clockRate uint32) *PrebufferCache {
	return &PrebufferCache{
		maxDurationSec: maxDurationSec,
		clockRate:      clockRate,
	}
}

func (c *PrebufferCache) Add(packet *Packet, isKeyframe bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.hasContent {
		c.hasContent = true
		// fmt.Printf("[PREBUFFER] Starting prebuffer cache (maxDuration=%ds, clockRate=%d)\n",
		// 	c.maxDurationSec, c.clockRate)
	}

	clone := &Packet{
		Header:  packet.Header,
		Payload: make([]byte, len(packet.Payload)),
	}
	copy(clone.Payload, packet.Payload)

	c.packets = append(c.packets, clone)

	// fmt.Printf("[PREBUFFER] Added packet: timestamp=%d, len=%d, total packets=%d\n",
	// 	packet.Header.Timestamp, len(packet.Payload), len(c.packets))

	// Remove old packets based on timestamp
	if len(c.packets) > 0 && c.clockRate > 0 {
		latestTimestamp := c.packets[len(c.packets)-1].Header.Timestamp
		maxAgeTicks := uint32(c.maxDurationSec) * c.clockRate

		// Find first packet to keep
		keepFrom := 0
		for i, pkt := range c.packets {
			age := latestTimestamp - pkt.Header.Timestamp
			if age <= maxAgeTicks {
				keepFrom = i
				break
			}
		}

		// Remove old packets
		if keepFrom > 0 {
			// removedCount := keepFrom
			c.packets = c.packets[keepFrom:]

			// fmt.Printf("[PREBUFFER] Removed %d old packets, keeping %d packets\n",
			// 	removedCount, len(c.packets))
		}
	}
}

func (c *PrebufferCache) HasContent() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hasContent && len(c.packets) > 0
}

func (c *PrebufferCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.packets = c.packets[:0]
	c.hasContent = false
}

func (c *PrebufferCache) GetLatestTimestamp() uint32 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.packets) == 0 {
		return 0
	}
	return c.packets[len(c.packets)-1].Header.Timestamp
}

func (c *PrebufferCache) GetPacketsFromTimestamp(startTS uint32) []*Packet {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.hasContent || len(c.packets) == 0 {
		return nil
	}

	// Find first packet at or after startTS
	startIdx := -1
	for i, pkt := range c.packets {
		if pkt.Header.Timestamp >= startTS {
			startIdx = i
			break
		}
	}

	if startIdx == -1 {
		return nil
	}

	// Return all packets from startIdx to end
	result := make([]*Packet, len(c.packets)-startIdx)
	copy(result, c.packets[startIdx:])
	return result
}
