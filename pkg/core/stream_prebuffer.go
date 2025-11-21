package core

import (
	"sync"
	"time"
)

// StreamPrebuffer is a time-based rolling buffer that stores packets as they arrive
// It maintains original timing and order for all tracks (audio + video)
type StreamPrebuffer struct {
	packets       []*TimestampedPacket
	mu            sync.RWMutex
	maxDuration   time.Duration
	oldestAllowed time.Time
}

// TimestampedPacket stores a packet with its arrival time
type TimestampedPacket struct {
	Packet      *Packet
	ArrivalTime time.Time
	TrackID     byte // Track identifier (for routing to correct receiver)
}

// NewStreamPrebuffer creates a new stream-level prebuffer
func NewStreamPrebuffer(durationSec int) *StreamPrebuffer {
	if durationSec > MaxPrebufferDuration {
		// fmt.Printf("[PREBUFFER] Duration %ds exceeds max %ds, capping\n", durationSec, MaxPrebufferDuration)
		durationSec = MaxPrebufferDuration
	}

	// fmt.Printf("[PREBUFFER] Created stream prebuffer with %ds duration\n", durationSec)
	return &StreamPrebuffer{
		packets:     make([]*TimestampedPacket, 0, 1000),
		maxDuration: time.Duration(durationSec) * time.Second,
	}
}

// Add adds a packet to the prebuffer with current timestamp
func (pb *StreamPrebuffer) Add(packet *Packet, trackID byte) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	now := time.Now()

	// Add packet
	pb.packets = append(pb.packets, &TimestampedPacket{
		Packet:      packet,
		ArrivalTime: now,
		TrackID:     trackID,
	})

	// Update oldest allowed time
	pb.oldestAllowed = now.Add(-pb.maxDuration)

	// Remove old packets
	cutoff := 0
	for i, p := range pb.packets {
		if p.ArrivalTime.After(pb.oldestAllowed) {
			cutoff = i
			break
		}
	}

	if cutoff > 0 {
		// fmt.Printf("[PREBUFFER] Removed %d old packets (buffer full)\n", cutoff)
		pb.packets = pb.packets[cutoff:]
	}

	// Calculate duration without calling GetLatestTimestamp() to avoid deadlock
	// var duration float64
	// if len(pb.packets) > 1 {
	// 	duration = pb.packets[len(pb.packets)-1].ArrivalTime.Sub(pb.packets[0].ArrivalTime).Seconds()
	// }

	// fmt.Printf("[PREBUFFER] Added packet trackID=%d, total=%d packets, duration=%.1fs\n",
	// 	trackID, len(pb.packets), duration)
}

// GetLatestTimestamp returns the timestamp of the most recent packet
func (pb *StreamPrebuffer) GetLatestTimestamp() time.Time {
	pb.mu.RLock()
	defer pb.mu.RUnlock()

	if len(pb.packets) == 0 {
		return time.Time{}
	}

	return pb.packets[len(pb.packets)-1].ArrivalTime
}

// HasContent returns true if buffer has packets
func (pb *StreamPrebuffer) HasContent() bool {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	return len(pb.packets) > 0
}

// GetAvailableDuration returns the actual duration of buffered data in seconds
func (pb *StreamPrebuffer) GetAvailableDuration() int {
	pb.mu.RLock()
	defer pb.mu.RUnlock()

	if len(pb.packets) < 2 {
		return 0
	}

	duration := pb.packets[len(pb.packets)-1].ArrivalTime.Sub(pb.packets[0].ArrivalTime)
	return int(duration.Seconds())
}

// GetPacketsFrom returns packets starting from the given offset (in seconds from latest)
// Returns the starting position and an iterator
func (pb *StreamPrebuffer) GetPacketsFrom(offsetSec int) (startTime time.Time, packets []*TimestampedPacket) {
	pb.mu.RLock()
	defer pb.mu.RUnlock()

	if len(pb.packets) == 0 {
		return time.Time{}, nil
	}

	latestTime := pb.packets[len(pb.packets)-1].ArrivalTime
	targetTime := latestTime.Add(-time.Duration(offsetSec) * time.Second)

	// fmt.Printf("[PREBUFFER] GetPacketsFrom: offset=%ds, latestTime=%v, targetTime=%v\n",
	// 	offsetSec, latestTime, targetTime)

	// Find first packet at or after target time
	startIdx := 0
	for i, p := range pb.packets {
		if !p.ArrivalTime.Before(targetTime) {
			startIdx = i
			break
		}
	}

	// Return copy of packets from start position
	result := make([]*TimestampedPacket, len(pb.packets)-startIdx)
	copy(result, pb.packets[startIdx:])

	if len(result) > 0 {
		// fmt.Printf("[PREBUFFER] GetPacketsFrom: found %d packets, actualOffset=%.1fs (requested %ds)\n",
		// 	len(result), latestTime.Sub(result[0].ArrivalTime).Seconds(), offsetSec)
		return result[0].ArrivalTime, result
	}

	return time.Time{}, nil
}

// GetNextPacketAt returns the next packet at or after the given position
// that is not beyond maxAllowedTime
func (pb *StreamPrebuffer) GetNextPacketAt(currentPos time.Time, maxAllowedTime time.Time) *TimestampedPacket {
	pb.mu.RLock()
	defer pb.mu.RUnlock()

	for _, p := range pb.packets {
		if p.ArrivalTime.After(currentPos) && !p.ArrivalTime.After(maxAllowedTime) {
			return p
		}
	}

	return nil
}

// Clear removes all packets from the buffer
func (pb *StreamPrebuffer) Clear() {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.packets = pb.packets[:0]
}
