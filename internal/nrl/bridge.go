package nrl

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/dmrgo/dmr/layer2"
	intdmr "github.com/hicaoc/ipsc2mmdvm/internal/dmr"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
	md380vocoder "github.com/hicaoc/md380_vocoder_cgo"
)

const (
	voicePayloadSize   = 500
	streamIdleTimeout  = 1500 * time.Millisecond
	heartbeatInterval  = 2 * time.Second
	packetSendInterval = 20 * time.Millisecond
)

type DeviceConfig struct {
	SourceKey    string
	Name         string
	Callsign     string
	DMRID        uint32
	ServerAddr   string
	ServerPort   int
	LocalUDPPort int
	SSID         uint8
	Slot         int
	TargetTG     uint32
	ColorCode    uint8
}

type Bridge struct {
	resolveConfig func(sourceKey string) (DeviceConfig, bool)
	handlePacket  func(sourceKey string, pkt proto.Packet)
	statusHandler func(sourceKey, status string, online bool)
	endpointFunc  func(sourceKey, ip string, port int)
	callsignFunc  func(dmrid uint32) string
	inboundIDFunc func(dmrid uint32, originalCallsign string) uint32

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	bridge *Bridge
	cfg    DeviceConfig

	mu       sync.Mutex
	conn     *net.UDPConn
	count    uint16
	decoders map[string]*dmrDecodeState
	encoders map[string]*nrlEncodeState
	closed   bool
}

type dmrDecodeState struct {
	vocoder  *md380vocoder.Vocoder
	lastSeen time.Time
	srcDMRID uint32
	buffer   []byte
}

type nrlEncodeState struct {
	vocoder    *md380vocoder.Vocoder
	streamID   uint32
	sequence   uint32
	voiceSeq   uint8
	pcmBuffer  []int16
	ambeFrames [][]byte
	lastSeen   time.Time
	headerSent bool
	srcID      uint32
}

func NewBridge(resolveConfig func(sourceKey string) (DeviceConfig, bool), handlePacket func(sourceKey string, pkt proto.Packet)) *Bridge {
	return &Bridge{
		resolveConfig: resolveConfig,
		handlePacket:  handlePacket,
		sessions:      map[string]*session{},
	}
}

func (b *Bridge) SetStatusHandler(handler func(sourceKey, status string, online bool)) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.statusHandler = handler
}

func (b *Bridge) SetCallsignResolver(fn func(dmrid uint32) string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.callsignFunc = fn
}

func (b *Bridge) SetEndpointHandler(fn func(sourceKey, ip string, port int)) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.endpointFunc = fn
}

func (b *Bridge) SetInboundSourceResolver(fn func(dmrid uint32, originalCallsign string) uint32) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inboundIDFunc = fn
}

func (b *Bridge) publishEndpoint(sourceKey string, addr net.Addr) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishEndpointLocked(sourceKey, addr)
}

func (b *Bridge) publishEndpointLocked(sourceKey string, addr net.Addr) {
	if addr == nil {
		return
	}
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok || udpAddr == nil {
		return
	}
	handler := b.endpointFunc
	if handler == nil {
		return
	}
	ip := ""
	if udpAddr.IP != nil {
		ip = udpAddr.IP.String()
	}
	go handler(sourceKey, ip, udpAddr.Port)
}

func (b *Bridge) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	sessions := make([]*session, 0, len(b.sessions))
	for key, sess := range b.sessions {
		sessions = append(sessions, sess)
		delete(b.sessions, key)
	}
	b.mu.Unlock()
	for _, sess := range sessions {
		sess.close()
	}
	return nil
}

func (b *Bridge) HandleDMRPacket(targetKey string, pkt proto.Packet) bool {
	if b == nil || targetKey == "" {
		return false
	}
	sess, err := b.ensureSession(targetKey)
	if err != nil {
		slog.Warn("nrl session unavailable", "targetKey", targetKey, "error", err)
		return false
	}
	return sess.handleDMRPacket(pkt)
}

func (b *Bridge) Activate(sourceKey string) error {
	if b == nil || sourceKey == "" {
		return nil
	}
	slog.Debug("nrl activate requested", "sourceKey", sourceKey)
	_, err := b.ensureSession(sourceKey)
	return err
}

func (b *Bridge) Deactivate(sourceKey string) {
	if b == nil || sourceKey == "" {
		return
	}
	b.mu.Lock()
	sess := b.sessions[sourceKey]
	if sess != nil {
		delete(b.sessions, sourceKey)
	}
	b.mu.Unlock()
	if sess != nil {
		slog.Warn("nrl session deactivated", "sourceKey", sourceKey)
		sess.close()
	}
}

func (b *Bridge) ensureSession(sourceKey string) (*session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
		if sess := b.sessions[sourceKey]; sess != nil {
			if cfg, ok := b.resolveConfig(sourceKey); ok {
				sess.cfg = cfg
			}
			slog.Debug("nrl session already active", "sourceKey", sourceKey, "local", sess.conn.LocalAddr().String())
			return sess, nil
		}
	cfg, ok := b.resolveConfig(sourceKey)
	if !ok {
		return nil, fmt.Errorf("nrl config missing for %s", sourceKey)
	}
	localAddr := &net.UDPAddr{}
	if cfg.LocalUDPPort > 0 {
		localAddr.Port = cfg.LocalUDPPort
	}
	remoteAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(strings.TrimSpace(cfg.ServerAddr), fmt.Sprintf("%d", cfg.ServerPort)))
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", localAddr, remoteAddr)
	if err != nil {
		return nil, err
	}
	sess := &session{
		bridge:   b,
		cfg:      cfg,
		conn:     conn,
		decoders: map[string]*dmrDecodeState{},
		encoders: map[string]*nrlEncodeState{},
	}
	b.sessions[sourceKey] = sess
	slog.Info("nrl session activated",
		"sourceKey", sourceKey,
		"server", remoteAddr.String(),
		"local", conn.LocalAddr().String(),
		"slot", cfg.Slot,
		"targetTG", cfg.TargetTG,
		"ssid", cfg.SSID)
	b.publishStatusLocked(sourceKey, "connecting", false)
	b.publishEndpointLocked(sourceKey, conn.LocalAddr())
	go sess.readLoop()
	go sess.heartbeatLoop()
	return sess, nil
}

func (b *Bridge) publishStatusLocked(sourceKey, status string, online bool) {
	handler := b.statusHandler
	if handler == nil {
		return
	}
	go handler(sourceKey, status, online)
}

func (b *Bridge) publishStatus(sourceKey, status string, online bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishStatusLocked(sourceKey, status, online)
}

func (b *Bridge) resolveCallsign(dmrid uint32) string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.callsignFunc == nil {
		return ""
	}
	return b.callsignFunc(dmrid)
}

func (b *Bridge) resolveInboundDMRID(dmrid uint32, originalCallsign string) uint32 {
	if b == nil {
		return dmrid
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inboundIDFunc == nil {
		return dmrid
	}
	return b.inboundIDFunc(dmrid, originalCallsign)
}

func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.conn != nil {
		_ = s.conn.Close()
	}
	for _, st := range s.decoders {
		_ = st.vocoder.Close()
	}
	for _, st := range s.encoders {
		_ = st.vocoder.Close()
	}
	if s.bridge != nil {
		s.bridge.publishStatus(s.cfg.SourceKey, "offline", false)
	}
}

func (s *session) heartbeatLoop() {
	slog.Debug("nrl heartbeat loop started", "sourceKey", s.cfg.SourceKey)
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	sendHeartbeat := func() bool {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return false
		}
		if strings.TrimSpace(s.cfg.Callsign) == "" {
			s.mu.Unlock()
			slog.Warn("skipping NRL heartbeat because device callsign is empty", "sourceKey", s.cfg.SourceKey)
			return true
		}
		packet := encodeHeartbeatPacket(s.cfg, s.count)
		s.count++
		conn := s.conn
		count := s.count - 1
		sourceKey := s.cfg.SourceKey
		s.mu.Unlock()
			slog.Debug("nrl heartbeat sending", "sourceKey", sourceKey, "count", count, "remote", conn.RemoteAddr().String())
			if _, err := conn.Write(packet); err != nil {
				slog.Warn("nrl heartbeat send failed", "sourceKey", sourceKey, "error", err)
			if s.bridge != nil {
				s.bridge.publishStatus(s.cfg.SourceKey, "offline", false)
			}
			return false
		}
			slog.Debug("nrl heartbeat sent", "sourceKey", sourceKey, "count", count)
			return true
		}
	if !sendHeartbeat() {
		return
	}
	for range ticker.C {
		if !sendHeartbeat() {
			return
		}
	}
}

func (s *session) readLoop() {
	slog.Debug("nrl read loop started", "sourceKey", s.cfg.SourceKey)
	buf := make([]byte, 2048)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			slog.Warn("nrl read loop exited", "sourceKey", s.cfg.SourceKey, "error", err)
			if s.bridge != nil {
				s.bridge.publishStatus(s.cfg.SourceKey, "offline", false)
			}
			return
		}
		pkt, err := decodePacket(buf[:n])
		if err != nil {
			continue
		}
		if s.bridge != nil {
			s.bridge.publishStatus(s.cfg.SourceKey, "online", true)
		}
		if pkt.Type != 1 && pkt.Type != 9 {
			continue
		}
		s.handleNRLPacket(pkt)
	}
}

func (b *Bridge) IsActive(sourceKey string) bool {
	if b == nil || sourceKey == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.sessions[sourceKey]
	return ok
}

func (s *session) handleDMRPacket(pkt proto.Packet) bool {
	now := time.Now().UTC()
	key := fmt.Sprintf("%d:%t:%d", pkt.Src, pkt.Slot, pkt.StreamID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if pkt.FrameType == 2 && pkt.DTypeOrVSeq == 2 {
		s.flushDecoderLocked(key, true)
		return true
	}
	if pkt.FrameType != 0 && pkt.FrameType != 1 {
		s.expireLocked(now)
		return false
	}
	var burst layer2.Burst
	burst.DecodeFromBytes(pkt.DMRData)
	if burst.IsData {
		s.expireLocked(now)
		return false
	}
	state := s.decoders[key]
	if state == nil {
		v, err := md380vocoder.NewVocoder()
		if err != nil {
			return false
		}
		state = &dmrDecodeState{vocoder: v}
		s.decoders[key] = state
	}
	state.lastSeen = now
	state.srcDMRID = uint32(pkt.Src)
	for _, frame := range burst.VoiceData.Frames {
		pcm, err := state.vocoder.Decode(ambeFrameBytes(frame.DecodedBits))
		if err != nil {
			continue
		}
		for _, sample := range pcm {
			state.buffer = append(state.buffer, LinearToAlaw(sample))
		}
	}
	for len(state.buffer) >= voicePayloadSize {
		payload := append([]byte(nil), state.buffer[:voicePayloadSize]...)
		state.buffer = state.buffer[voicePayloadSize:]
		if err := s.writeVoicePacketLocked(uint32(pkt.Src), payload); err != nil {
			return false
		}
	}
	s.expireLocked(now)
	return true
}

func (s *session) flushDecoderLocked(key string, remove bool) {
	state := s.decoders[key]
	if state == nil {
		return
	}
	if len(state.buffer) > 0 {
		padding := make([]byte, voicePayloadSize-len(state.buffer))
		for i := range padding {
			padding[i] = 0xD5
		}
		state.buffer = append(state.buffer, padding...)
		_ = s.writeVoicePacketLocked(state.srcDMRID, state.buffer[:voicePayloadSize])
		state.buffer = state.buffer[:0]
	}
	if remove {
		_ = state.vocoder.Close()
		delete(s.decoders, key)
	}
}

func (s *session) expireLocked(now time.Time) {
	for key, state := range s.decoders {
		if now.Sub(state.lastSeen) > streamIdleTimeout {
			s.flushDecoderLocked(key, true)
		}
	}
	for key, state := range s.encoders {
		if now.Sub(state.lastSeen) <= streamIdleTimeout {
			continue
		}
		s.emitTerminatorLocked(state)
		_ = state.vocoder.Close()
		delete(s.encoders, key)
	}
}

func (s *session) writeVoicePacketLocked(srcDMRID uint32, payload []byte) error {
	if s.conn == nil {
		return fmt.Errorf("nrl session closed")
	}
	deviceCallsign := strings.TrimSpace(s.cfg.Callsign)
	if deviceCallsign == "" {
		return fmt.Errorf("nrl device callsign is empty")
	}
	if srcDMRID == 0 {
		return fmt.Errorf("source dmrid is empty")
	}
	localIP := net.IPv4zero
	if addr, _ := s.conn.LocalAddr().(*net.UDPAddr); addr != nil && addr.IP != nil {
		localIP = addr.IP.To4()
	}
	originalCallsign := ""
	if srcDMRID != 0 && s.bridge != nil {
		originalCallsign = s.bridge.resolveCallsign(srcDMRID)
	}
	if strings.TrimSpace(originalCallsign) == "" {
		return fmt.Errorf("source callsign is empty for dmrid %d", srcDMRID)
	}
	packet := encodeVoicePacket(s.cfg, s.count, srcDMRID, originalCallsign, payload, localIP)
	s.count++
	_, err := s.conn.Write(packet)
	if err == nil {
		time.Sleep(packetSendInterval)
	}
	return err
}

func (s *session) handleNRLPacket(pkt packet) {
	if s.bridge == nil || s.bridge.handlePacket == nil {
		return
	}
	now := time.Now().UTC()

	srcID := s.bridge.resolveInboundDMRID(pkt.DMRID, pkt.OriginalCallsign)
	if srcID == 0 {
		srcID = pkt.DMRID
	}
	if srcID == 0 {
		slog.Warn("dropping NRL voice without resolvable source DMRID",
			"sourceKey", s.cfg.SourceKey,
			"deviceCallsign", strings.TrimSpace(pkt.Callsign),
			"originalCallsign", strings.TrimSpace(pkt.OriginalCallsign),
			"ssid", pkt.SSID,
			"type", pkt.Type,
			"count", pkt.Count,
			"payloadLen", len(pkt.Data))
		return
	}
	if s.cfg.TargetTG == 0 {
		slog.Warn("dropping NRL voice without bound target TG",
			"sourceKey", s.cfg.SourceKey,
			"srcDMRID", srcID,
			"slot", s.cfg.Slot,
			"deviceCallsign", strings.TrimSpace(pkt.Callsign),
			"originalCallsign", strings.TrimSpace(pkt.OriginalCallsign),
			"payloadLen", len(pkt.Data))
		return
	}

	identity := strings.TrimSpace(pkt.OriginalCallsign)
	if identity == "" {
		identity = strings.TrimSpace(pkt.Callsign)
	}
	key := fmt.Sprintf("%d:%s:%d", srcID, strings.ToUpper(identity), pkt.SSID)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	// Purge idle encoder/decoder states before selecting stream state so a new
	// PTT after silence starts a fresh DMR stream (header + vseq=0).
	s.expireLocked(now)
	state := s.encoders[key]
	if state == nil {
		v, err := md380vocoder.NewVocoder()
		if err != nil {
			slog.Warn("failed to create NRL inbound vocoder", "sourceKey", s.cfg.SourceKey, "error", err)
			return
		}
		state = &nrlEncodeState{
			vocoder:    v,
			streamID:   randomStreamID(),
			ambeFrames: make([][]byte, 0, 3),
			srcID:      srcID,
		}
		s.encoders[key] = state
	}
	// A long gap on the same identity indicates a new NRL talkspurt.
	if now.Sub(state.lastSeen) > streamIdleTimeout {
		state.streamID = randomStreamID()
		state.sequence = 0
		state.voiceSeq = 0
		state.pcmBuffer = state.pcmBuffer[:0]
		state.ambeFrames = state.ambeFrames[:0]
		state.headerSent = false
	}
	state.lastSeen = now
	for _, sample := range pkt.Data {
		state.pcmBuffer = append(state.pcmBuffer, AlawToLinear(sample))
	}
	if !state.headerSent {
		s.bridge.handlePacket(s.cfg.SourceKey, newHeaderPacket(state.srcID, s.cfg.TargetTG, s.cfg.Slot, s.cfg.ColorCode, state.streamID, state.sequence))
		slog.Debug("nrl->dmr stream header sent",
			"sourceKey", s.cfg.SourceKey,
			"srcDMRID", state.srcID,
			"targetTG", s.cfg.TargetTG,
			"slot", s.cfg.Slot,
			"streamID", state.streamID,
			"seq", state.sequence)
		state.sequence++
		state.headerSent = true
	}
	for len(state.pcmBuffer) >= md380vocoder.PCMFrameSize {
		framePCM := append([]int16(nil), state.pcmBuffer[:md380vocoder.PCMFrameSize]...)
		state.pcmBuffer = state.pcmBuffer[md380vocoder.PCMFrameSize:]
		ambe, err := state.vocoder.Encode(framePCM)
		if err != nil {
			slog.Warn("failed to encode NRL voice to AMBE",
				"sourceKey", s.cfg.SourceKey,
				"srcDMRID", state.srcID,
				"streamID", state.streamID,
				"error", err)
			continue
		}
		state.ambeFrames = append(state.ambeFrames, ambe)
		if len(state.ambeFrames) < 3 {
			continue
		}
		frames := [3][]byte{state.ambeFrames[0], state.ambeFrames[1], state.ambeFrames[2]}
		s.bridge.handlePacket(s.cfg.SourceKey, newVoicePacket(state.srcID, s.cfg.TargetTG, s.cfg.Slot, s.cfg.ColorCode, state.streamID, state.sequence, state.voiceSeq, frames))
		if state.voiceSeq == 0 {
			slog.Debug("nrl->dmr voice sync sent",
				"sourceKey", s.cfg.SourceKey,
				"srcDMRID", state.srcID,
				"streamID", state.streamID,
				"seq", state.sequence)
		}
		state.sequence++
		state.voiceSeq = (state.voiceSeq + 1) % 6
		state.ambeFrames = state.ambeFrames[:0]
	}
}

func (s *session) emitTerminatorLocked(state *nrlEncodeState) {
	if !state.headerSent || s.cfg.TargetTG == 0 {
		return
	}
	s.bridge.handlePacket(s.cfg.SourceKey, newTerminatorPacket(state.srcID, s.cfg.TargetTG, s.cfg.Slot, s.cfg.ColorCode, state.streamID, state.sequence))
}

func ambeFrameBytes(decodedBits [49]byte) []byte {
	bits := intdmr.EncodeAMBEFrame(decodedBits)
	out := make([]byte, md380vocoder.AMBEFrameSize)
	for i, bit := range bits {
		if bit == 1 {
			out[i/8] |= 1 << (7 - (i % 8))
		}
	}
	return out
}
