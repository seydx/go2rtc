package core

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
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
	talkerTimeout        = 3 * time.Second        // a parent is mixed this long after its last packet
	realPacketTimeout    = 500 * time.Millisecond // time window after last real packet to forward FFmpeg output
	listenTimeout        = time.Second            // without output by then, ffmpeg gets the held talk anyway
	holdLimit            = 50                     // held packets per parent, 1 s of 20 ms frames
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
	ffmpegInputs []uint32 // parent IDs, in SDP order
	udpServer    *udp.UDPServer

	// talk that came before ffmpeg could take it, sent once ffmpeg listens:
	// its first output means its inputs are open
	held      map[uint32][]*Packet
	listening bool
	startedAt int64

	// last real packet per parent: the talkers, and keepalive for the inputs
	// that pause (prevents FFmpeg from waiting)
	lastPacketTime     map[uint32]int64
	lastRealPacketTime atomic.Int64 // tracks when last real (non-keepalive) packet arrived
	keepaliveDone      chan struct{}

	closing bool

	onPacket func()

	syncing   bool
	syncAgain bool

	mu sync.Mutex
	// runMu serializes ffmpeg starts and stops, taken before mu
	runMu sync.Mutex
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
		held:            make(map[uint32][]*Packet),
		sequencer:       rtp.NewRandomSequencer(),
		ffmpegBinary:    ffmpegBinary,
	}

	m.Node.SetOwner(m)
	atomic.StoreUint32(&m.timestamp, 0)

	return m
}

// AddParentWithCodec adds a parent that may talk. It starts nothing: ffmpeg
// runs once parents talk.
func (m *RTPMixer) AddParentWithCodec(parent *Node, codec *Codec) {
	m.mu.Lock()
	m.parents = append(m.parents, parent)
	m.parentCodecs[parent.id] = codec
	m.mu.Unlock()

	// Add mixer as child of parent (so it appears in consumer receiver's childs)
	parent.AppendChild(&m.Node)

	// Set Forward hook to route packets with parentID context
	parentID := parent.id
	parent.Forward = func(packet *Packet) {
		m.handlePacketFromParent(packet, parentID)
	}
}

// OnNextPacket runs f once, in its own goroutine, when a parent sends its
// next packet.
func (m *RTPMixer) OnNextPacket(f func()) {
	m.mu.Lock()
	m.onPacket = f
	m.mu.Unlock()
}

func (m *RTPMixer) RemoveParent(parent *Node) {
	m.mu.Lock()

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
	delete(m.held, parent.id)

	count := len(m.parents)
	m.mu.Unlock()

	if count == 0 {
		m.Close()
	}
	// a mixer closed this way still takes the next talker, and a second
	// Close returns early: the sync stops what that talker started
	m.syncTalkers()
}

func (m *RTPMixer) Close() {
	m.mu.Lock()

	if m.closing {
		m.mu.Unlock()
		return
	}

	m.closing = true
	// a parent left attached would restart ffmpeg on the closed mixer when it
	// leaves, and nothing would stop it again
	parents := m.parents
	m.parents = nil
	clear(m.parentCodecs)
	clear(m.held)
	m.mu.Unlock()

	for _, parent := range parents {
		parent.RemoveChild(&m.Node)
	}

	m.runMu.Lock()
	m.stopFFmpeg()
	m.runMu.Unlock()
	m.Node.Close()
}

func (m *RTPMixer) MarshalJSON() ([]byte, error) {
	m.mu.Lock()
	// Copy data while holding lock, then release before marshaling
	// to avoid nested lock acquisition
	parents := make([]*Node, len(m.parents))
	copy(parents, m.parents)
	id := m.id
	codec := m.Codec
	bytes := m.Bytes
	packets := m.Packets
	parentBytes := maps.Clone(m.parentBytes)
	m.mu.Unlock()

	// childs belong to the Node and its lock, not to m.mu
	childs := m.Node.children()

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

	m.mu.Lock()
	last, seen := m.lastPacketTime[parentID]
	m.lastPacketTime[parentID] = now
	codec, known := m.parentCodecs[parentID]
	talker := known && !isWildcard(codec)
	woke := talker && (!seen || now-last >= int64(talkerTimeout))
	if woke {
		delete(m.held, parentID)
	}

	var toFFmpeg []*Packet
	direct := false
	switch {
	case m.takesInput(parentID, now):
		toFFmpeg = append(m.held[parentID], packet)
		delete(m.held, parentID)
	case m.ffmpegCmd == nil && m.passthrough(parentID, now):
		direct = true
	case talker:
		// ffmpeg for this talker is about to start or still starting
		m.hold(parentID, packet)
	}

	onPacket := m.onPacket
	m.onPacket = nil
	m.mu.Unlock()

	if onPacket != nil {
		go onPacket()
	}
	if woke {
		m.syncTalkers()
	}

	if direct {
		m.forwardDirect(packet)
	}
	for _, p := range toFFmpeg {
		m.sendToFFmpeg(p, parentID)
	}

	m.mu.Lock()
	m.Bytes += len(packet.Payload)
	m.Packets++
	m.parentBytes[parentID] += len(packet.Payload)
	m.mu.Unlock()
}

// callers may hold a producer lock, so the work runs in its own goroutine; a
// call during a run makes it run once more
func (m *RTPMixer) syncTalkers() {
	m.mu.Lock()
	if m.syncing {
		m.syncAgain = true
		m.mu.Unlock()
		return
	}
	m.syncing = true
	m.mu.Unlock()

	go func() {
		for {
			m.runMu.Lock()
			m.applyTalkers()
			m.runMu.Unlock()

			m.mu.Lock()
			again := m.syncAgain
			m.syncAgain = false
			m.syncing = again
			m.mu.Unlock()
			if !again {
				return
			}
		}
	}()
}

// callers hold runMu
func (m *RTPMixer) applyTalkers() {
	m.mu.Lock()
	inputs := m.talkers(time.Now().UnixNano())
	need := len(inputs) > 1 || len(inputs) == 1 && m.needsTranscode(m.parentCodecs[inputs[0]])
	running := m.ffmpegCmd != nil
	same := slices.Equal(inputs, m.ffmpegInputs)
	m.mu.Unlock()

	if running && need && same {
		return
	}
	if running {
		m.stopFFmpeg()
	}
	if need {
		if err := m.startFFmpeg(inputs); err != nil {
			fmt.Fprintf(os.Stderr, "[mixer id=%d] Error starting FFmpeg: %v\n", m.id, err)
			// a talker that keeps talking never wakes the mixer again
			time.AfterFunc(restartDelay, m.syncTalkers)
		}
	}
}

// callers hold mu
func (m *RTPMixer) talkers(now int64) []uint32 {
	var ids []uint32
	for _, parent := range m.parents {
		if m.talking(parent.id, now) {
			ids = append(ids, parent.id)
		}
	}
	return ids
}

func (m *RTPMixer) talking(id uint32, now int64) bool {
	if isWildcard(m.parentCodecs[id]) {
		return false
	}
	last, ok := m.lastPacketTime[id]
	return ok && now-last < int64(talkerTimeout)
}

// callers hold mu
func (m *RTPMixer) takesInput(parentID uint32, now int64) bool {
	if m.ffmpegCmd == nil {
		return false
	}
	if _, ok := m.parentPorts[parentID]; !ok {
		return false
	}
	return m.listening || now-m.startedAt >= int64(listenTimeout)
}

func (m *RTPMixer) hold(parentID uint32, packet *Packet) {
	held := m.held[parentID]
	if len(held) >= holdLimit {
		held = held[1:]
	}
	clone := *packet
	clone.Payload = slices.Clone(packet.Payload)
	m.held[parentID] = append(held, &clone)
}

// callers hold mu
func (m *RTPMixer) passthrough(parentID uint32, now int64) bool {
	codec := m.parentCodecs[parentID]
	if isWildcard(codec) {
		return true
	}
	if m.needsTranscode(codec) {
		return false
	}
	for _, parent := range m.parents {
		if parent.id != parentID && m.talking(parent.id, now) {
			return false
		}
	}
	return true
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
	m.mu.Unlock()

	packet.Timestamp = atomic.AddUint32(&m.timestamp, m.frameSize())
	packet.Marker = true

	for _, child := range m.Node.children() {
		child.Input(packet)
	}
}

func (m *RTPMixer) startFFmpeg(inputs []uint32) error {
	sdpContent, numInputs, err := m.generateSDP(inputs)
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
		Input:         "-protocol_whitelist pipe,rtp,udp,file,crypto -analyzeduration 0 -probesize 32 -fflags nobuffer -flags low_delay -listen_timeout 1 -f sdp -i pipe:0",
		FilterComplex: fmt.Sprintf("amix=inputs=%d:duration=first:dropout_transition=0:normalize=0,alimiter", numInputs), // idle inputs carry silence, 1/inputs scaling only cuts the talker
		Output:        fmt.Sprintf("-flush_packets 1 -f rtp rtp://127.0.0.1:%d", udpServer.Port()),
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

	cmd := shell.NewCommand(args.String())
	cmd.Stdin = bytes.NewReader(sdpContent)
	// cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		udpServer.Close()
		return fmt.Errorf("failed to start FFmpeg: %w", err)
	}

	// log.Printf("[mixer] id=%d FFmpeg started, output port=%d", m.id, udpServer.Port())

	keepaliveDone := make(chan struct{})

	m.mu.Lock()
	m.udpServer = udpServer
	m.ffmpegCmd = cmd
	m.ffmpegInputs = inputs
	m.listening = false
	m.startedAt = time.Now().UnixNano()
	m.keepaliveDone = keepaliveDone
	m.mu.Unlock()

	go m.readFromFFmpeg()
	go m.monitorFFmpeg(cmd)
	go m.runKeepalive(keepaliveDone)

	return nil
}

func (m *RTPMixer) monitorFFmpeg(cmd *shell.Command) {
	_ = cmd.Wait() // blocks until this FFmpeg instance exits

	m.mu.Lock()
	crashed := m.ffmpegCmd == cmd // not stopped and not replaced
	m.mu.Unlock()
	if !crashed {
		return
	}

	// Unexpected exit (crash) — back off and restart.
	time.Sleep(restartDelay)

	m.runMu.Lock()
	defer m.runMu.Unlock()

	// a sync or Close during the backoff took over
	m.mu.Lock()
	current := m.ffmpegCmd == cmd
	m.mu.Unlock()
	if current {
		m.stopFFmpeg()
		m.applyTalkers()
	}
}

// kills and waits outside mu, packets keep flowing meanwhile
func (m *RTPMixer) stopFFmpeg() {
	m.mu.Lock()
	keepaliveDone := m.keepaliveDone
	cmd := m.ffmpegCmd
	server := m.udpServer
	m.keepaliveDone = nil
	m.ffmpegCmd = nil
	m.ffmpegInputs = nil
	m.udpServer = nil
	m.parentPorts = make(map[uint32]int)
	m.parentSequencer = make(map[uint32]rtp.Sequencer)
	m.mu.Unlock()

	if keepaliveDone != nil {
		close(keepaliveDone)
	}

	if cmd != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}

	if server != nil {
		_ = server.Close()
	}
}

func (m *RTPMixer) generateSDP(inputs []uint32) ([]byte, int, error) {
	m.mu.Lock()
	parentCodecs := maps.Clone(m.parentCodecs)
	// a failed start leaves ports nothing listens on
	m.parentPorts = make(map[uint32]int)
	m.mu.Unlock()

	if len(inputs) == 0 {
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
	for _, id := range inputs {
		// Get the codec for this specific parent (for transcoding support)
		codec := parentCodecs[id]
		if codec == nil {
			codec = m.Codec // Fallback to output codec
		}

		port, err := udp.GetFreePort()
		if err != nil {
			return nil, 0, fmt.Errorf("failed to allocate port: %w", err)
		}

		m.mu.Lock()
		m.parentPorts[id] = port
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

	var heard *udp.UDPServer
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

		if heard != server {
			heard = server
			m.mu.Lock()
			if m.udpServer == server {
				m.listening = true
			}
			m.mu.Unlock()
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

func (m *RTPMixer) runKeepalive(done chan struct{}) {
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			m.sendKeepalive()
		}
	}
}

func (m *RTPMixer) sendKeepalive() {
	now := time.Now().UnixNano()

	m.mu.Lock()
	inputs := m.ffmpegInputs
	// held talk doesn't reach ffmpeg: silence on every input brings out its
	// first output
	probing := !m.listening && now-m.startedAt < int64(listenTimeout)
	lastTimes := maps.Clone(m.lastPacketTime)
	m.mu.Unlock()

	quiet := false
	for _, id := range inputs {
		lastTime, exists := lastTimes[id]
		if probing || !exists || (now-lastTime) > int64(inactiveThreshold) {
			m.sendSilence(id)
		}
		if !exists || now-lastTime >= int64(talkerTimeout) {
			quiet = true
		}
	}
	if quiet {
		m.syncTalkers()
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
	packet.Payload = silenceFrame(codec)

	return m.sendToFFmpeg(packet, parentID)
}

// silenceFrame is 20 ms of silence in codec. Zero bytes are close to full
// scale negative in G.711 and no valid Opus frame.
func silenceFrame(codec *Codec) []byte {
	samples := int(frameSizeForCodec(codec))
	if codec.Channels > 1 {
		samples *= int(codec.Channels)
	}

	switch codec.Name {
	case CodecPCMU:
		return bytes.Repeat([]byte{0xFF}, samples)
	case CodecPCMA:
		return bytes.Repeat([]byte{0xD5}, samples)
	case CodecPCM, CodecPCML:
		return make([]byte, 2*samples)
	case CodecOpus:
		return []byte{0xF8, 0xFF, 0xFE} // CELT fullband 20 ms, silence flag
	}
	return make([]byte, frameSizeForCodec(codec))
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

// ANY and ALL are placeholders, ffmpeg can't decode them
func isWildcard(codec *Codec) bool {
	return codec != nil && (codec.Name == CodecAny || codec.Name == CodecAll)
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
