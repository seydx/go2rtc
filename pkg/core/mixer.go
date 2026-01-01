package core

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/ffmpeg"
	"github.com/AlexxIT/go2rtc/pkg/shell"
	"github.com/AlexxIT/go2rtc/pkg/udp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
)

const (
	keepaliveInterval    = 20 * time.Millisecond
	inactiveThreshold    = 100 * time.Millisecond
	realPacketTimeout    = 500 * time.Millisecond // time window after last real packet to forward FFmpeg output
	timestampDivisor20ms = 50
	restartDelay         = 3 * time.Second
	defaultMTU           = 1472
)

type RTPMixer struct {
	Node

	Media *Media
	Codec *Codec // Output codec (determined by first parent)

	parents      []*Node
	parentCodecs map[uint32]*Codec // Input codec per parent (for transcoding)
	parentPorts  map[uint32]int    // Map parent ID to its FFmpeg port

	// Per-parent RTP state for FFmpeg communication (each parent needs independent seq/ts)
	parentSequencer map[uint32]rtp.Sequencer
	parentTimestamp map[uint32]uint32
	parentBytes     map[uint32]int // Per-parent byte tracking for visualization

	// RTP packet normalization (for single-parent mode output)
	sequencer rtp.Sequencer
	timestamp uint32

	Bytes   int `json:"bytes,omitempty"`
	Packets int `json:"packets,omitempty"`

	ffmpegBinary string
	ffmpegCmd    *shell.Command
	udpServer    *udp.UDPServer

	// Keepalive for inactive parents (prevents FFmpeg from waiting)
	lastPacketTime     map[uint32]int64
	lastRealPacketTime atomic.Int64 // tracks when last real (non-keepalive) packet arrived
	keepaliveDone      chan struct{}

	closing            bool
	intentionalRestart bool

	mu sync.Mutex
}

func NewRTPMixer(ffmpegBinary string, media *Media, codec *Codec) *RTPMixer {
	m := &RTPMixer{
		Node:            Node{id: NewID(), Codec: codec},
		Media:           media,
		Codec:           codec,
		parentCodecs:    make(map[uint32]*Codec),
		parentPorts:     make(map[uint32]int),
		parentSequencer: make(map[uint32]rtp.Sequencer),
		parentTimestamp: make(map[uint32]uint32),
		parentBytes:     make(map[uint32]int),
		lastPacketTime:  make(map[uint32]int64),
		sequencer:       rtp.NewRandomSequencer(),
		ffmpegBinary:    ffmpegBinary,
	}

	m.Node.SetOwner(m)
	atomic.StoreUint32(&m.timestamp, 0)

	return m
}

func (m *RTPMixer) AddParentWithCodec(parent *Node, codec *Codec) {
	m.mu.Lock()
	oldCount := len(m.parents)
	m.parents = append(m.parents, parent)
	m.parentCodecs[parent.id] = codec
	newCount := len(m.parents)
	m.mu.Unlock()

	// Add mixer as child of parent (so it appears in consumer receiver's childs)
	parent.AppendChild(&m.Node)

	// Set Forward hook to route packets with parentID context
	parentID := parent.id
	parent.Forward = func(packet *Packet) {
		m.handlePacketFromParent(packet, parentID)
	}

	m.handleTopologyChange(oldCount, newCount)
}

func (m *RTPMixer) RemoveParent(parent *Node) {
	m.mu.Lock()
	oldCount := len(m.parents)

	// Remove parent from list
	for i, p := range m.parents {
		if p == parent {
			m.parents = append(m.parents[:i], m.parents[i+1:]...)
			break
		}
	}

	// Clear Forward hook to restore default forwarding behavior
	parent.Forward = nil

	// Remove mixer from parent's children
	parent.RemoveChild(&m.Node)

	// Clear parent state
	delete(m.parentPorts, parent.id)
	delete(m.parentCodecs, parent.id)
	delete(m.parentSequencer, parent.id)
	delete(m.parentTimestamp, parent.id)
	delete(m.parentBytes, parent.id)
	delete(m.lastPacketTime, parent.id)

	newCount := len(m.parents)
	m.mu.Unlock()

	// Handle topology change
	m.handleTopologyChange(oldCount, newCount)
}

func (m *RTPMixer) Close() {
	m.mu.Lock()

	if m.closing {
		m.mu.Unlock()
		return
	}

	m.closing = true
	m.mu.Unlock()

	m.stopFFmpeg()
	m.Node.Close()
}

func (m *RTPMixer) MarshalJSON() ([]byte, error) {
	m.mu.Lock()
	// Copy data while holding lock, then release before marshaling
	// to avoid nested lock acquisition
	parents := make([]*Node, len(m.parents))
	copy(parents, m.parents)
	childs := make([]*Node, len(m.childs))
	copy(childs, m.childs)
	id := m.id
	codec := m.Codec
	bytes := m.Bytes
	packets := m.Packets
	parentBytes := maps.Clone(m.parentBytes)
	m.mu.Unlock()

	data := struct {
		ID          uint32         `json:"id"`
		Codec       *Codec         `json:"codec"`
		Parents     []uint32       `json:"parents,omitempty"`
		ParentBytes map[uint32]int `json:"parent_bytes,omitempty"`
		Childs      []uint32       `json:"childs,omitempty"`
		Bytes       int            `json:"bytes,omitempty"`
		Packets     int            `json:"packets,omitempty"`
	}{
		ID:          id,
		Codec:       codec,
		Parents:     nodeIDs(parents),
		ParentBytes: parentBytes,
		Childs:      nodeIDs(childs),
		Bytes:       bytes,
		Packets:     packets,
	}

	return json.Marshal(data)
}

func (m *RTPMixer) handlePacketFromParent(packet *Packet, parentID uint32) {
	now := time.Now().UnixNano()

	// Track that we received a real packet (not keepalive)
	m.lastRealPacketTime.Store(now)

	// Update last packet time for keepalive tracking
	m.mu.Lock()
	m.lastPacketTime[parentID] = now
	ffmpegRunning := m.ffmpegCmd != nil
	m.mu.Unlock()

	if ffmpegRunning {
		m.sendToFFmpeg(packet, parentID)
	} else {
		m.forwardDirect(packet)
	}

	m.mu.Lock()
	m.Bytes += len(packet.Payload)
	m.Packets++
	m.parentBytes[parentID] += len(packet.Payload)
	m.mu.Unlock()
}

func (m *RTPMixer) handleTopologyChange(oldCount, newCount int) {
	if newCount == 0 {
		m.Close()
		return
	}

	needsFFmpegNow := m.needsFFmpeg()

	m.mu.Lock()
	ffmpegRunning := m.ffmpegCmd != nil
	m.mu.Unlock()

	if needsFFmpegNow && !ffmpegRunning {
		if err := m.startFFmpeg(); err != nil {
			fmt.Fprintf(os.Stderr, "[mixer id=%d] Error starting FFmpeg: %v\n", m.id, err)
		} else {
			go m.monitorFFmpeg()
		}
	} else if !needsFFmpegNow && ffmpegRunning {
		m.stopFFmpeg()
	} else if needsFFmpegNow && ffmpegRunning && oldCount != newCount {
		// Topology changed while FFmpeg running - restart to reconfigure
		if err := m.restartFFmpeg(); err != nil {
			fmt.Fprintf(os.Stderr, "[mixer id=%d] Error restarting FFmpeg: %v\n", m.id, err)
		}
	}
}

func (m *RTPMixer) needsFFmpeg() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Count valid parents (excluding wildcard codecs like ANY/ALL)
	validParents := 0
	needsTranscode := false

	for _, parentCodec := range m.parentCodecs {
		// Skip wildcard codecs - they can't be decoded by FFmpeg
		if parentCodec != nil && (parentCodec.Name == CodecAny || parentCodec.Name == CodecAll) {
			continue
		}
		validParents++
		if m.needsTranscode(parentCodec) {
			needsTranscode = true
		}
	}

	// Need FFmpeg for mixing when 2+ valid parents
	if validParents >= 2 {
		return true
	}

	// Need FFmpeg for transcoding when any valid parent codec differs from output
	return needsTranscode
}

func (m *RTPMixer) needsTranscode(inputCodec *Codec) bool {
	if inputCodec == nil || m.Codec == nil {
		return false
	}
	return !inputCodec.Match(m.Codec)
}

func (m *RTPMixer) forwardDirect(packet *Packet) {
	m.mu.Lock()
	packet.SequenceNumber = m.sequencer.NextSequenceNumber()
	children := m.childs
	m.mu.Unlock()

	packet.Timestamp = atomic.AddUint32(&m.timestamp, m.frameSize())
	packet.Marker = true

	for _, child := range children {
		child.Input(packet)
	}
}

func (m *RTPMixer) restartFFmpeg() error {
	// Mark as intentional restart so monitor doesn't try to restart too
	m.mu.Lock()
	m.intentionalRestart = true
	m.mu.Unlock()

	m.stopFFmpeg()

	// Check if FFmpeg is still needed
	if !m.needsFFmpeg() {
		return nil
	}

	return m.startFFmpeg()
}

func (m *RTPMixer) startFFmpeg() error {
	sdpContent, numInputs, err := m.generateSDP()
	if err != nil {
		return fmt.Errorf("failed to generate SDP: %w", err)
	}

	// If no valid inputs for FFmpeg (all parents have wildcard codecs), don't start
	if numInputs == 0 {
		// log.Printf("[mixer] id=%d no valid inputs for FFmpeg, skipping", m.id)
		return nil
	}

	// log.Printf("[mixer] id=%d SDP:\n%s", m.id, string(sdpContent))

	udpServer, err := udp.NewUDPServer()
	if err != nil {
		return fmt.Errorf("failed to create UDP server: %w", err)
	}
	m.udpServer = udpServer

	outputCodec := FFmpegCodecName(m.Codec.Name)
	var audioBitrate string
	switch m.Codec.Name {
	case CodecELD:
		outputCodec = "libfdk_aac"
	case CodecG722:
		outputCodec = "pcm_s16le"
	default:
		// G726 variants: G726-16, G726-24, G726-32, G726-40
		// Extract bitrate from codec name (e.g., "G726-40" → "40k")
		if after, ok := strings.CutPrefix(m.Codec.Name, CodecG726+"-"); ok {
			bitrate := after
			audioBitrate = bitrate + "k"
		}
	}

	sampleRate := m.Codec.ClockRate
	if sampleRate == 0 {
		sampleRate = 8000
	}

	args := &ffmpeg.Args{
		Bin:           m.ffmpegBinary,
		Global:        "-hide_banner",
		Input:         "-protocol_whitelist pipe,rtp,udp,file,crypto -listen_timeout 1 -f sdp -i pipe:0",
		FilterComplex: fmt.Sprintf("amix=inputs=%d:duration=first:dropout_transition=0", numInputs),
		Output:        fmt.Sprintf("-f rtp rtp://127.0.0.1:%d", udpServer.Port()),
	}

	codecArgs := fmt.Sprintf("-ar %d -c:a %s", sampleRate, outputCodec)
	// Set output channels if specified (e.g., mono for camera backchannel)
	if m.Codec.Channels > 0 {
		codecArgs += fmt.Sprintf(" -ac %d", m.Codec.Channels)
	}
	if audioBitrate != "" {
		codecArgs += " -b:a " + audioBitrate
	}
	// AAC requires global_header
	if m.Codec.Name == CodecAAC || m.Codec.Name == CodecELD {
		codecArgs += " -flags +global_header"
	}
	// G726 specific options
	if strings.HasPrefix(m.Codec.Name, CodecG726) {
		// code_size: 2=G726-16, 3=G726-24, 4=G726-32, 5=G726-40
		codeSize := "5" // default to G726-40
		if after, ok := strings.CutPrefix(m.Codec.Name, CodecG726+"-"); ok {
			bitrate := after
			switch bitrate {
			case "16":
				codeSize = "2"
			case "24":
				codeSize = "3"
			case "32":
				codeSize = "4"
			case "40":
				codeSize = "5"
			}
		}
		codecArgs += " -code_size " + codeSize
	}
	args.AddCodec(codecArgs)

	// log.Printf("[mixer] id=%d cmd: %s", m.id, args.String())

	m.ffmpegCmd = shell.NewCommand(args.String())
	m.ffmpegCmd.Stdin = bytes.NewReader(sdpContent)
	// m.ffmpegCmd.Stderr = os.Stderr

	if err := m.ffmpegCmd.Start(); err != nil {
		udpServer.Close()
		return fmt.Errorf("failed to start FFmpeg: %w", err)
	}

	// log.Printf("[mixer] id=%d FFmpeg started, output port=%d", m.id, udpServer.Port())

	// Start reader
	go m.readFromFFmpeg()

	// Start keepalive
	m.keepaliveDone = make(chan struct{})
	go m.runKeepalive()

	return nil
}

func (m *RTPMixer) monitorFFmpeg() {
	for {
		m.mu.Lock()
		cmd := m.ffmpegCmd
		m.mu.Unlock()

		if cmd == nil {
			return // FFmpeg was intentionally stopped
		}

		// Wait for FFmpeg to exit
		_ = cmd.Wait()

		m.mu.Lock()
		wasIntentional := m.intentionalRestart
		m.intentionalRestart = false // Reset flag
		closing := m.closing
		m.mu.Unlock()

		if closing || !m.needsFFmpeg() {
			return
		}

		if wasIntentional {
			continue // Don't restart, handleTopologyChange already did it
		}

		time.Sleep(restartDelay)

		// Attempt restart
		if err := m.restartFFmpeg(); err != nil {
			continue
		}
	}
}

func (m *RTPMixer) stopFFmpeg() {
	// log.Printf("[mixer] id=%d stopFFmpeg called", m.id)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.keepaliveDone != nil {
		close(m.keepaliveDone)
		m.keepaliveDone = nil
	}

	if m.ffmpegCmd != nil {
		if m.ffmpegCmd.Process != nil {
			m.ffmpegCmd.Process.Kill()
		}
		m.ffmpegCmd.Wait()
		m.ffmpegCmd = nil
	}

	if m.udpServer != nil {
		m.udpServer.Close()
		m.udpServer = nil
	}

	m.parentPorts = make(map[uint32]int)
	m.parentSequencer = make(map[uint32]rtp.Sequencer)

	// log.Printf("[mixer] id=%d FFmpeg stopped", m.id)
}

func (m *RTPMixer) generateSDP() ([]byte, int, error) {
	m.mu.Lock()
	parents := m.parents
	parentCodecs := maps.Clone(m.parentCodecs)
	m.mu.Unlock()

	if len(parents) == 0 {
		return nil, 0, fmt.Errorf("no parents")
	}

	sd := &sdp.SessionDescription{
		Origin: sdp.Origin{
			Username: "-", SessionID: 1, SessionVersion: 1,
			NetworkType: "IN", AddressType: "IP4", UnicastAddress: "0.0.0.0",
		},
		SessionName: "go2rtc-mixer",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN", AddressType: "IP4",
			Address: &sdp.Address{Address: "127.0.0.1"},
		},
		TimeDescriptions: []sdp.TimeDescription{{Timing: sdp.Timing{}}},
	}

	numInputs := 0
	for _, parent := range parents {
		// Get the codec for this specific parent (for transcoding support)
		codec := parentCodecs[parent.id]
		if codec == nil {
			codec = m.Codec // Fallback to output codec
		}

		// Skip parents with wildcard codecs (ANY/ALL) - FFmpeg can't decode these
		// These are placeholders that match anything but don't represent real audio
		if codec.Name == CodecAny || codec.Name == CodecAll {
			// log.Printf("[mixer] id=%d skipping parent %d with wildcard codec %s", m.id, parent.id, codec.Name)
			continue
		}

		port, err := udp.GetFreePort()
		if err != nil {
			return nil, 0, fmt.Errorf("failed to allocate port: %w", err)
		}

		m.mu.Lock()
		m.parentPorts[parent.id] = port
		m.mu.Unlock()

		numInputs++

		// Codec name for SDP
		codecName := codec.Name
		switch codecName {
		case CodecELD:
			codecName = CodecAAC
		case CodecPCML:
			codecName = CodecPCM
		}

		md := &sdp.MediaDescription{
			MediaName: sdp.MediaName{
				Media:  KindAudio,
				Port:   sdp.RangedPort{Value: port},
				Protos: []string{"RTP", "AVP"},
			},
		}

		// Each port is a separate RTP session, so same payload types are fine
		md.WithCodec(codec.PayloadType, codecName, codec.ClockRate, uint16(codec.Channels), codec.FmtpLine)
		md.WithPropertyAttribute(DirectionRecvonly)

		sd.MediaDescriptions = append(sd.MediaDescriptions, md)
	}

	sdpBytes, err := sd.Marshal()
	return sdpBytes, numInputs, err
}

func (m *RTPMixer) readFromFFmpeg() {
	buf := make([]byte, defaultMTU)

	// Check if output codec is AAC and expects raw (not RTP) format
	// FFmpeg outputs RTP AAC with AU headers, but if codec expects raw AAC we need to strip them
	stripAUHeaders := m.Codec != nil && m.Codec.Name == CodecAAC && !m.Codec.IsRTP()
	// log.Printf("[mixer] id=%d readFromFFmpeg: stripAUHeaders=%v codec=%s isRTP=%v",
	// 	m.id, stripAUHeaders, m.Codec.Name, m.Codec.IsRTP())

	// log.Printf("[mixer] id=%d readFromFFmpeg started", m.id)

	for {
		m.mu.Lock()
		server := m.udpServer
		m.mu.Unlock()

		if server == nil {
			return
		}

		n, _, err := server.ReadFrom(buf)
		if err != nil {
			return
		}

		packet := &Packet{}
		if err := packet.Unmarshal(buf[:n]); err != nil {
			continue
		}

		// Only forward if we received real packets from parents recently
		// This filters out FFmpeg output that's based purely on keepalive silence
		lastReal := m.lastRealPacketTime.Load()
		if time.Now().UnixNano()-lastReal > int64(realPacketTimeout) {
			continue
		}

		// Strip AU headers from AAC RTP output (RFC 3640) if codec expects raw AAC
		// Forward each AU as separate packet (cameras expect 1 frame per packet)
		if stripAUHeaders {
			m.forwardAACFrames(packet)
		} else {
			m.forwardDirect(packet)
		}
	}
}

// forwardAACFrames parses RFC 3640 AU headers and forwards each AAC frame as separate packet
// This is necessary because cameras expect 1 frame per packet, but FFmpeg may bundle multiple
func (m *RTPMixer) forwardAACFrames(packet *Packet) {
	payload := packet.Payload
	if len(payload) < 4 {
		m.forwardDirect(packet)
		return
	}

	// First 2 bytes: AU headers length in bits
	headerSizeBytes := binary.BigEndian.Uint16(payload) >> 3

	// Sanity check
	if headerSizeBytes == 0 || len(payload) < int(2+headerSizeBytes) {
		m.forwardDirect(packet)
		return
	}

	headers := payload[2 : 2+headerSizeBytes]
	units := payload[2+headerSizeBytes:]
	// numAUs := int(headerSizeBytes / 2)

	// log.Printf("[mixer] id=%d AAC packet: %d AUs in %d bytes", m.id, numAUs, len(payload))

	// Forward each AU as separate packet
	for len(headers) >= 2 {
		unitSize := binary.BigEndian.Uint16(headers) >> 3
		if int(unitSize) > len(units) {
			break
		}

		// Create new packet for this AU
		auPacket := &Packet{}
		auPacket.Header = packet.Header
		auPacket.Version = 0 // RTPPacketVersionAAC
		auPacket.Payload = units[:unitSize]

		// log.Printf("[mixer] id=%d forwarding AU: %d bytes", m.id, len(auPacket.Payload))
		m.forwardDirect(auPacket)

		headers = headers[2:]
		units = units[unitSize:]
	}
}

func (m *RTPMixer) sendToFFmpeg(packet *Packet, parentID uint32) error {
	m.mu.Lock()
	port, exists := m.parentPorts[parentID]
	server := m.udpServer
	codec := m.parentCodecs[parentID]

	sequencer, hasSequencer := m.parentSequencer[parentID]
	if !hasSequencer {
		sequencer = rtp.NewRandomSequencer()
		m.parentSequencer[parentID] = sequencer
	}

	packet.SequenceNumber = sequencer.NextSequenceNumber()

	// Use parent's codec for frame size calculation
	frameSize := frameSizeForCodec(codec)
	if codec == nil {
		frameSize = m.frameSize()
	}

	timestamp := m.parentTimestamp[parentID]
	m.parentTimestamp[parentID] = timestamp + frameSize
	packet.Timestamp = timestamp

	m.mu.Unlock()

	if !exists {
		return fmt.Errorf("no port for parent %d", parentID)
	}

	if server == nil {
		return fmt.Errorf("no UDP server")
	}

	data, err := packet.Marshal()
	if err != nil {
		return err
	}

	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	_, err = server.WriteTo(data, addr)
	return err
}

func (m *RTPMixer) runKeepalive() {
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.keepaliveDone:
			return
		case <-ticker.C:
			m.sendKeepalive()
		}
	}
}

func (m *RTPMixer) sendKeepalive() {
	now := time.Now().UnixNano()

	m.mu.Lock()
	parents := m.parents
	lastTimes := maps.Clone(m.lastPacketTime)
	m.mu.Unlock()

	for _, parent := range parents {
		lastTime, exists := lastTimes[parent.id]
		if !exists || (now-lastTime) > int64(inactiveThreshold) {
			m.sendSilence(parent.id)
		}
	}
}

func (m *RTPMixer) sendSilence(parentID uint32) error {
	m.mu.Lock()
	codec := m.parentCodecs[parentID]
	m.mu.Unlock()

	if codec == nil {
		codec = m.Codec
	}

	packet := &Packet{}
	packet.Version = 2
	packet.PayloadType = codec.PayloadType
	packet.SSRC = 0
	packet.Payload = make([]byte, frameSizeForCodec(codec))

	return m.sendToFFmpeg(packet, parentID)
}

func (m *RTPMixer) frameSize() uint32 {
	return frameSizeForCodec(m.Codec)
}

// frameSizeForCodec calculates the frame size (in samples) for 20ms of audio
func frameSizeForCodec(codec *Codec) uint32 {
	if codec == nil {
		return 160 // Default 8kHz
	}

	switch codec.Name {
	case CodecOpus:
		return 960 // Opus @ 48kHz: 20ms = 960 samples
	case CodecAAC, CodecELD:
		return 1024 // AAC always uses 1024 samples per frame
	}

	// For all other codecs, calculate based on ClockRate
	// 20ms = ClockRate / 50
	if codec.ClockRate > 0 {
		return codec.ClockRate / timestampDivisor20ms
	}

	// Fallback to 8kHz default (160 samples)
	return 160
}

func nodeIDs(nodes []*Node) []uint32 {
	if len(nodes) == 0 {
		return nil
	}
	ids := make([]uint32, len(nodes))
	for i, node := range nodes {
		ids[i] = node.id
	}
	return ids
}
