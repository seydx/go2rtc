package core

import (
	"sync"
	"time"

	"github.com/pion/rtp"
)

const (
	// MaxPrebufferDuration is the maximum allowed prebuffer duration in seconds
	MaxPrebufferDuration = 15
)

type CodecHandler interface {
	ProcessPacket(packet *Packet)
	SendCacheTo(s *Sender, playbackFPS int) (nextTimestamp uint32, lastSequence uint16)
	SendQueueTo(s *Sender, playbackFPS int, startTimestamp uint32, lastSequence uint16)
	SendPrebufferTo(s *Sender, offsetSec int, done <-chan struct{})
	SetupGOP()
	SetupPrebuffer(durationSec int)
	ClearCache()
}

type Payloader interface {
	Payload(mtu uint16, payload []byte) [][]byte
}

type BaseCodecHandler struct {
	codec          *Codec
	gopCache       *GopCache
	prebufferCache *PrebufferCache
	inputHandler   HandlerFunc
	mu             sync.RWMutex

	isKeyframeFunc       func([]byte) bool
	createRTPDepayFunc   func(*Codec, HandlerFunc) HandlerFunc
	createAVCCRepairFunc func(*Codec, HandlerFunc) HandlerFunc
	payloader            Payloader
	updateFmtpLineFunc   func(*Codec, []byte)
	fmtpLineUpdated      bool
}

func NewCodecHandler(
	codec *Codec,
	isKeyframeFunc func([]byte) bool,
	createRTPDepayFunc func(*Codec, HandlerFunc) HandlerFunc,
	createAVCCRepairFunc func(*Codec, HandlerFunc) HandlerFunc,
	payloader Payloader,
	updateFmtpLineFunc func(*Codec, []byte),
) CodecHandler {
	ch := &BaseCodecHandler{
		codec:                codec,
		isKeyframeFunc:       isKeyframeFunc,
		createRTPDepayFunc:   createRTPDepayFunc,
		createAVCCRepairFunc: createAVCCRepairFunc,
		payloader:            payloader,
		updateFmtpLineFunc:   updateFmtpLineFunc,
	}

	gopHandler := func(packet *Packet) {
		isKeyframe := ch.isKeyframeFunc(packet.Payload)

		// Update FmtpLine from first keyframe with parameter sets
		if isKeyframe && !ch.fmtpLineUpdated && ch.updateFmtpLineFunc != nil {
			ch.updateFmtpLineFunc(ch.codec, packet.Payload)
			ch.fmtpLineUpdated = true
		}

		// Add to GOP cache if enabled
		ch.mu.RLock()
		gop := ch.gopCache
		ch.mu.RUnlock()
		if gop != nil {
			gop.Add(packet, isKeyframe)
		}

		// Also add to prebuffer if enabled
		ch.mu.RLock()
		prebuffer := ch.prebufferCache
		ch.mu.RUnlock()
		if prebuffer != nil {
			prebuffer.Add(packet, isKeyframe)
		}
	}

	if ch.codec.IsRTP() {
		ch.inputHandler = ch.createRTPDepayFunc(ch.codec, gopHandler)
	} else {
		ch.inputHandler = gopHandler
	}

	return ch
}

func (ch *BaseCodecHandler) ProcessPacket(packet *Packet) {
	ch.mu.RLock()
	handler := ch.inputHandler
	cache := ch.gopCache
	ch.mu.RUnlock()

	if cache != nil {
		cache.AddRTPFragment(packet)
	}

	if handler != nil {
		handler(packet)
	}
}

func (ch *BaseCodecHandler) SendCacheTo(s *Sender, playbackFPS int) (nextTimestamp uint32, lastSequence uint16) {
	// fmt.Printf("[HANDLER] Sending cache to sender %d for codec %s at %d FPS\n", s.id, ch.codec.Name, playbackFPS)

	ch.mu.RLock()
	cache := ch.gopCache
	ch.mu.RUnlock()

	if cache == nil || !cache.HasContent() {
		// fmt.Printf("[HANDLER] No content in cache for sender %d\n", s.id)
		return 0, 0
	}
	cachedPackets := cache.Get()
	if len(cachedPackets) == 0 {
		// fmt.Printf("[HANDLER] No cached packets to send for sender %d\n", s.id)
		return 0, 0
	}

	sleepDurationPerFrame := time.Second / time.Duration(playbackFPS)
	ticksPerPlaybackFrame := uint32(90000 / playbackFPS)
	lastOriginalTimestamp := cachedPackets[len(cachedPackets)-1].Header.Timestamp

	var avccFrames []*Packet
	var rtpFragments []*Packet
	for _, pkt := range cachedPackets {
		if pkt.Header.Version == 0 {
			avccFrames = append(avccFrames, pkt)
		} else {
			rtpFragments = append(rtpFragments, pkt)
		}
	}

	currentTimestamp := lastOriginalTimestamp - uint32(len(avccFrames)*int(ticksPerPlaybackFrame))

	// firstAVCCSequenceNumber := avccFrames[0].Header.SequenceNumber
	// lastAVCCSequenceNumber := firstAVCCSequenceNumber
	// if len(avccFrames) > 1 {
	// 	lastAVCCSequenceNumber = avccFrames[len(avccFrames)-1].Header.SequenceNumber
	// }

	// fmt.Printf("[HANDLER] Sender %d processing %d AVCC frames and %d RTP fragments\n",
	// 	s.id, len(avccFrames), len(rtpFragments))

	// fmt.Printf("[HANDLER] AVCC frames: first sequence=%d, last sequence=%d\n",
	// 	firstAVCCSequenceNumber, lastAVCCSequenceNumber)

	var lastSequenceNumber uint16

	for _, pkt := range avccFrames {
		clone := &rtp.Packet{Header: pkt.Header, Payload: pkt.Payload}
		clone.Header.Timestamp = currentTimestamp
		// fmt.Printf("[HANDLER] Sender %d processing cached avcc frame: sequence=%d, timestamp=%d, len=%d\n",
		// 	s.id, pkt.Header.SequenceNumber, pkt.Header.Timestamp, len(pkt.Payload))
		s.InputCache(clone)
		time.Sleep(sleepDurationPerFrame)
		currentTimestamp += ticksPerPlaybackFrame
		lastSequenceNumber = pkt.Header.SequenceNumber
	}

	if len(rtpFragments) > 0 {
		// firstRTPSequenceNumber := rtpFragments[0].Header.SequenceNumber
		// lastRTPSequenceNumber := firstRTPSequenceNumber
		// if len(rtpFragments) > 1 {
		// 	lastRTPSequenceNumber = rtpFragments[len(rtpFragments)-1].Header.SequenceNumber
		// }

		// fmt.Printf("[HANDLER] RTP fragments: first sequence=%d, last sequence=%d\n",
		// 	firstRTPSequenceNumber, lastRTPSequenceNumber)

		ticksPerFragment := ticksPerPlaybackFrame / uint32(len(rtpFragments))
		sleepDurationPerFragment := sleepDurationPerFrame / time.Duration(len(rtpFragments))

		for _, pkt := range rtpFragments {
			clone := &rtp.Packet{Header: pkt.Header, Payload: pkt.Payload}
			clone.Header.Timestamp = currentTimestamp
			// fmt.Printf("[HANDLER] Sender %d processing cached RTP fragment: sequence=%d, timestamp=%d, len=%d\n",
			// 	s.id, pkt.Header.SequenceNumber, pkt.Header.Timestamp, len(pkt.Payload))
			s.InputCache(clone)
			time.Sleep(sleepDurationPerFragment)
			currentTimestamp += ticksPerFragment
			lastSequenceNumber = pkt.Header.SequenceNumber
		}
	}

	// fmt.Printf("[HANDLER] COMPLETE: Sent %d AVCC frames and %d RTP fragments. Next timestamp: %d\n", len(avccFrames), len(rtpFragments), currentTimestamp)
	return currentTimestamp, lastSequenceNumber
}

func (ch *BaseCodecHandler) SendQueueTo(s *Sender, playbackFPS int, startTimestamp uint32, lastSequence uint16) {
	ticksPerPlaybackFrame := uint32(90000 / playbackFPS)
	currentTimestamp := startTimestamp

	// fmt.Printf("[SENDER] Sender %d starting to process live queue at %d FPS\n", s.id, playbackFPS)

	for {
		select {
		case packet := <-s.liveQueue:
			if packet.Header.SequenceNumber > 0 && packet.Header.SequenceNumber <= lastSequence {
				// fmt.Printf("[SENDER] Sender %d skipping packet with sequence %d (already processed)\n",
				// 	s.id, packet.Header.SequenceNumber)
				continue
			}

			if currentTimestamp == 0 {
				currentTimestamp = packet.Header.Timestamp
			}

			packet.Header.Timestamp = currentTimestamp

			// fmt.Printf("[SENDER] Sender %d processing queued live packet: sequence=%d, timestamp=%d, len=%d\n",
			// 	s.id, packet.Header.SequenceNumber, packet.Header.Timestamp, len(packet.Payload))

			s.processPacket(packet)

			if packet.Marker {
				currentTimestamp += ticksPerPlaybackFrame
			}

		default:
			// fmt.Printf("[SENDER] COMPLETE: Sender %d finished processing live queue. Switching to live mode.\n", s.id)
			return
		}
	}
}

func (ch *BaseCodecHandler) SetupGOP() {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.gopCache = NewGOPCache()
}

func (ch *BaseCodecHandler) SetupPrebuffer(durationSec int) {
	ch.mu.Lock()
	defer ch.mu.Unlock()

	// Cap prebuffer duration to maximum allowed value
	if durationSec > MaxPrebufferDuration {
		durationSec = MaxPrebufferDuration
	}

	if ch.codec.ClockRate > 0 {
		ch.prebufferCache = NewPrebufferCache(durationSec, ch.codec.ClockRate)
	}
}

func (ch *BaseCodecHandler) SendPrebufferTo(s *Sender, offsetSec int, done <-chan struct{}) {
	// fmt.Printf("[HANDLER] Sender %d: Prebuffer cache reader started with offset %ds\n", s.id, offsetSec)

	ch.mu.RLock()
	cache := ch.prebufferCache
	ch.mu.RUnlock()

	if cache == nil {
		// fmt.Printf("[HANDLER] Sender %d: No prebuffer cache available\n", s.id)
		return
	}

	// Wait for cache to have content
	for !cache.HasContent() {
		select {
		case <-done:
			// fmt.Printf("[HANDLER] Sender %d: Prebuffer cache reader stopped\n", s.id)
			return
		case <-time.After(100 * time.Millisecond):
			// Keep waiting
		}
	}

	// Calculate starting position in cache
	latestTS := cache.GetLatestTimestamp()
	requestedOffset := uint32(offsetSec) * ch.codec.ClockRate

	// Check actual available duration in cache
	ch.mu.RLock()
	oldestTS := uint32(0)
	if len(cache.packets) > 0 {
		oldestTS = cache.packets[0].Header.Timestamp
	}
	ch.mu.RUnlock()

	availableDuration := latestTS - oldestTS
	actualOffset := requestedOffset
	if requestedOffset > availableDuration {
		actualOffset = availableDuration
		// fmt.Printf("[HANDLER] Sender %d: Requested offset %ds exceeds available duration, using %d ticks instead\n",
		// 	s.id, offsetSec, actualOffset)
	}

	clientPosTS := latestTS - actualOffset

	// fmt.Printf("[HANDLER] Sender %d: Starting to read from cache at original TS=%d (latest=%d, oldest=%d, offset=%d ticks)\n",
	// 	s.id, clientPosTS, latestTS, oldestTS, actualOffset)

	currentOutputTS := uint32(0)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	// Track last successful packet read for timeout detection
	// This catches producer lags/stalls (not complete shutdown, that's handled by done channel)
	lastPacketTime := time.Now()
	noPacketTimeout := 3 * time.Second

	for {
		select {
		case <-ticker.C:
			// Get packets from cache starting at clientPosTS
			ch.mu.RLock()
			packets := cache.GetPacketsFromTimestamp(clientPosTS)
			ch.mu.RUnlock()

			if len(packets) == 0 {
				// No new packets yet, check timeout
				if time.Since(lastPacketTime) > noPacketTimeout {
					// fmt.Printf("[HANDLER] Sender %d: No new packets for %v, assuming producer died, stopping\n",
					// 	s.id, noPacketTimeout)
					return
				}
				continue
			}

			// Reset timeout on successful packet read
			lastPacketTime = time.Now()

			// Send first packet
			pkt := packets[0]

			// Calculate increment based on timestamp difference
			var tsIncrement uint32
			if len(packets) > 1 {
				nextTS := packets[1].Header.Timestamp
				if nextTS >= pkt.Header.Timestamp {
					tsIncrement = nextTS - pkt.Header.Timestamp
				} else {
					// Wraparound
					tsIncrement = (0xFFFFFFFF - pkt.Header.Timestamp) + nextTS
				}
			} else {
				// Use default increment
				if ch.codec.IsVideo() {
					tsIncrement = ch.codec.ClockRate / 30
				} else {
					tsIncrement = ch.codec.ClockRate / 50
				}
			}

			// Create packet with recalculated timestamp
			clone := &Packet{
				Header:  pkt.Header,
				Payload: pkt.Payload,
			}
			clone.Header.Timestamp = currentOutputTS

			// fmt.Printf("[HANDLER] Sender %d: Sending packet from cache: origTS=%d, newTS=%d, increment=%d\n",
			// 	s.id, pkt.Header.Timestamp, currentOutputTS, tsIncrement)

			s.processPacket(clone)

			// Advance position in cache
			clientPosTS = pkt.Header.Timestamp + 1
			currentOutputTS += tsIncrement

			// Sleep based on increment to maintain realtime playback (interruptible)
			if tsIncrement > 0 && ch.codec.ClockRate > 0 {
				sleepDuration := time.Duration(float64(tsIncrement) / float64(ch.codec.ClockRate) * float64(time.Second))
				if sleepDuration > 0 && sleepDuration < time.Second {
					select {
					case <-time.After(sleepDuration):
						// Normal sleep completed
					case <-done:
						// fmt.Printf("[HANDLER] Sender %d: Prebuffer cache reader stopped during sleep\n", s.id)
						return
					}
				}
			}

		case <-done:
			// fmt.Printf("[HANDLER] Sender %d: Prebuffer cache reader stopped\n", s.id)
			return
		}
	}
}

func (ch *BaseCodecHandler) ClearCache() {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.gopCache != nil {
		ch.gopCache.Clear()
	}
	if ch.prebufferCache != nil {
		ch.prebufferCache.Clear()
	}
}

var (
	codecHandlerFactories = make(map[string]func(*Codec) CodecHandler)
	managerMutex          sync.RWMutex
)

func RegisterCodecHandler(codecName string, factory func(*Codec) CodecHandler) {
	managerMutex.Lock()
	defer managerMutex.Unlock()
	codecHandlerFactories[codecName] = factory
}

func CreateCodecHandler(codec *Codec) CodecHandler {
	managerMutex.RLock()
	factory, exists := codecHandlerFactories[codec.Name]
	managerMutex.RUnlock()

	if !exists {
		return nil
	}

	return factory(codec)
}
