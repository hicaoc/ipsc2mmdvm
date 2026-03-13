package hytera

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hicaoc/ipsc2mmdvm/internal/config"
)

const (
	nrlHeaderLen             = 48
	hyteraAnalogPacketLen    = 512
	hyteraAnalogHeaderLen    = 32
	hyteraAnalogFrameSamples = 160
	hyteraAnalogFrameGroup   = hyteraAnalogFrameSamples * 3 // 480 bytes
	nrlVoiceFlushWatermark   = 500
	nrlVoiceSendInterval     = 20 * time.Millisecond
	nrlHeartbeatInterval     = 2 * time.Second
	nrlSessionTTL            = 6 * time.Second
	nrlCallIdleTimeout       = 1500 * time.Millisecond
)

var hyteraAnalogFallbackHeader = [hyteraAnalogHeaderLen]byte{
	0x5A, 0x5A, 0x5A, 0x5A, 0x00, 0x02, 0x00, 0x00,
	0x45, 0x00, 0x05, 0x01, 0x01, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0xC0, 0x03, 0xA0, 0xF6, 0x8B, 0x10,
}

type NRLPeerConfig struct {
	ServerAddr string
	ServerPort int
	SSID       uint8
	Callsign   string
	DMRID      uint32
	// HyteraVoicePort is the learned remote analog voice UDP port for this peer.
	HyteraVoicePort int
}

type nrlBridge struct {
	cfg         *config.Config
	hyteraConn  *net.UDPConn
	resolver    func(sourceKey string) (NRLPeerConfig, bool)
	callHandler func(call NRLCallEvent)

	mu       sync.Mutex
	sessions map[string]*nrlSession

	done chan struct{}
	wg   sync.WaitGroup
}

type nrlSession struct {
	bridge    *nrlBridge
	sourceKey string
	cfg       NRLPeerConfig

	conn       *net.UDPConn
	hyteraAddr *net.UDPAddr

	templateReady bool
	template      [hyteraAnalogHeaderLen]byte
	outSeq        byte

	nextNRLCount uint16
	ulawBuffer   []byte
	voiceQueue   chan []byte

	lastActivityAt  time.Time
	lastHeartbeatAt time.Time
	callActive      bool
	callStreamID    uint
	callLastVoiceAt time.Time
	callFromIP      string
	callFromPort    int
	callSourceName  string
	callSourceCS    string
	callSourceDMRID uint32

	mu     sync.Mutex
	closed bool
	done   chan struct{}
	wg     sync.WaitGroup
}

type nrlPacket struct {
	Type     byte
	DMRID    uint32
	Callsign string
	SSID     uint8
	Data     []byte
}

type NRLCallEvent struct {
	Ended           bool
	HyteraSourceKey string
	SourceKey       string
	SourceName      string
	SourceCallsign  string
	SourceDMRID     uint32
	StreamID        uint
	FromIP          string
	FromPort        int
}

func newNRLBridge(cfg *config.Config, hyteraConn *net.UDPConn, resolver func(sourceKey string) (NRLPeerConfig, bool), callHandler func(call NRLCallEvent)) (*nrlBridge, error) {
	if hyteraConn == nil {
		return nil, errors.New("hytera UDP connection is nil")
	}
	if resolver == nil {
		return nil, errors.New("nrl resolver is nil")
	}
	b := &nrlBridge{
		cfg:         cfg,
		hyteraConn:  hyteraConn,
		resolver:    resolver,
		callHandler: callHandler,
		sessions:    map[string]*nrlSession{},
		done:        make(chan struct{}),
	}
	b.wg.Add(1)
	go b.heartbeatLoop()
	return b, nil
}

func (b *nrlBridge) Stop() {
	close(b.done)
	b.mu.Lock()
	sessions := make([]*nrlSession, 0, len(b.sessions))
	for _, s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.sessions = map[string]*nrlSession{}
	b.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	b.wg.Wait()
}

func (b *nrlBridge) HandleHyteraPacket(addr *net.UDPAddr, data []byte) bool {
	if addr == nil {
		return false
	}
	sourceKey := "hytera:" + addr.IP.String()
	cfg, ok := b.resolver(sourceKey)
	if !ok || strings.TrimSpace(cfg.ServerAddr) == "" || cfg.ServerPort == 0 {
		b.closeSession(sourceKey)
		return isHyteraAnalogVoicePacket(data)
	}

	session, err := b.ensureSession(sourceKey, cfg)
	if err != nil {
		slog.Warn("failed creating NRL session", "sourceKey", sourceKey, "error", err)
		return isHyteraAnalogVoicePacket(data)
	}
	if len(data) == 1 && data[0] == 0x00 {
		session.mu.Lock()
		session.lastActivityAt = time.Now()
		session.mu.Unlock()
		_ = session.sendHeartbeat()
	}
	if !isHyteraAnalogVoicePacket(data) {
		return false
	}
	session.handleHyteraVoice(addr, data)
	return true
}

func (b *nrlBridge) ensureSession(sourceKey string, cfg NRLPeerConfig) (*nrlSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if s, ok := b.sessions[sourceKey]; ok {
		if s.sameEndpoint(cfg) {
			s.updateConfig(cfg)
			return s, nil
		}
		slog.Info("NRL session restarting on endpoint change",
			"sourceKey", sourceKey,
			"oldServer", net.JoinHostPort(strings.TrimSpace(s.cfg.ServerAddr), fmt.Sprintf("%d", s.cfg.ServerPort)),
			"newServer", net.JoinHostPort(strings.TrimSpace(cfg.ServerAddr), fmt.Sprintf("%d", cfg.ServerPort)))
		s.close()
		delete(b.sessions, sourceKey)
	}

	remoteAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(strings.TrimSpace(cfg.ServerAddr), fmt.Sprintf("%d", cfg.ServerPort)))
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, remoteAddr)
	if err != nil {
		return nil, err
	}
	s := &nrlSession{
		bridge:      b,
		sourceKey:   sourceKey,
		cfg:         cfg,
		conn:        conn,
		ulawBuffer:  make([]byte, 0, hyteraAnalogFrameGroup*2),
		voiceQueue:  make(chan []byte, 256),
		lastActivityAt: time.Now(),
		done:        make(chan struct{}),
	}
	s.wg.Add(1)
	go s.readLoop()
	s.wg.Add(1)
	go s.writeLoop()
	b.sessions[sourceKey] = s
	slog.Info("NRL session started",
		"sourceKey", sourceKey,
		"server", remoteAddr.String(),
		"ssid", cfg.SSID)
	_ = s.sendHeartbeat()
	return s, nil
}

func (b *nrlBridge) closeSession(sourceKey string) {
	b.mu.Lock()
	s := b.sessions[sourceKey]
	if s != nil {
		delete(b.sessions, sourceKey)
	}
	b.mu.Unlock()
	if s != nil {
		s.close()
	}
}

func (b *nrlBridge) heartbeatLoop() {
	defer b.wg.Done()
	ticker := time.NewTicker(nrlHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			b.tickSessions()
		case <-b.done:
			return
		}
	}
}

func (b *nrlBridge) tickSessions() {
	b.mu.Lock()
	sessions := make([]*nrlSession, 0, len(b.sessions))
	for _, s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.mu.Unlock()

	now := time.Now()
	for _, s := range sessions {
		s.expireCallIfIdle(now)
		if s.isExpired(now) {
			b.closeSession(s.sourceKey)
			continue
		}
		_ = s.sendHeartbeat()
	}
}

func (s *nrlSession) sameConfig(cfg NRLPeerConfig) bool {
	return s.sameEndpoint(cfg) &&
		s.sameDynamicConfig(cfg)
}

func (s *nrlSession) sameEndpoint(cfg NRLPeerConfig) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.EqualFold(strings.TrimSpace(s.cfg.ServerAddr), strings.TrimSpace(cfg.ServerAddr)) &&
		s.cfg.ServerPort == cfg.ServerPort
}

func (s *nrlSession) sameDynamicConfig(cfg NRLPeerConfig) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.SSID == cfg.SSID &&
		normalizeNRLCallsign(s.cfg.Callsign) == normalizeNRLCallsign(cfg.Callsign) &&
		s.cfg.DMRID == cfg.DMRID &&
		s.cfg.HyteraVoicePort == cfg.HyteraVoicePort
}

func (s *nrlSession) updateConfig(cfg NRLPeerConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	oldVoicePort := s.cfg.HyteraVoicePort
	s.cfg = cfg
	// If fallback target was prepared before first inbound Hytera analog packet,
	// keep it aligned with the latest learned voice port without rebuilding socket.
	if s.hyteraAddr != nil && oldVoicePort != cfg.HyteraVoicePort && cfg.HyteraVoicePort > 0 {
		s.hyteraAddr.Port = cfg.HyteraVoicePort
	}
}

func (s *nrlSession) isExpired(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Sub(s.lastActivityAt) > nrlSessionTTL
}

func (s *nrlSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.callActive {
		s.emitCallEventLocked(true)
		s.callActive = false
	}
	s.closed = true
	close(s.done)
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	s.wg.Wait()
}

func (s *nrlSession) handleHyteraVoice(addr *net.UDPAddr, data []byte) {
	s.mu.Lock()
	s.hyteraAddr = cloneUDPAddr(addr)
	if len(data) >= hyteraAnalogHeaderLen {
		copy(s.template[:], data[:hyteraAnalogHeaderLen])
		s.templateReady = true
		s.outSeq = data[4]
	}
	s.lastActivityAt = time.Now()
	s.mu.Unlock()

	if len(data) <= hyteraAnalogHeaderLen {
		return
	}
	voice := data[hyteraAnalogHeaderLen:]
	for start := 0; start+hyteraAnalogFrameSamples <= len(voice); start += hyteraAnalogFrameSamples {
		alaw := ulawChunkToAlaw(voice[start : start+hyteraAnalogFrameSamples])
		_ = s.sendNRLVoice(alaw)
	}
}

func (s *nrlSession) sendNRLVoice(alaw []byte) error {
	chunk := append([]byte(nil), alaw...)
	select {
	case <-s.done:
		return errors.New("session closed")
	case s.voiceQueue <- chunk:
		return nil
	}
}

func (s *nrlSession) sendHeartbeat() error {
	now := time.Now()
	s.mu.Lock()
	if s.closed || s.conn == nil {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	if !s.lastHeartbeatAt.IsZero() && now.Sub(s.lastHeartbeatAt) < nrlHeartbeatInterval {
		s.mu.Unlock()
		return nil
	}
	count := s.nextNRLCount
	s.nextNRLCount++
	s.lastHeartbeatAt = now
	cfg := s.cfg
	conn := s.conn
	s.mu.Unlock()

	packet := encodeNRL21Packet(cfg, 2, nil, count)
	_, err := conn.Write(packet)
	return err
}

func (s *nrlSession) readLoop() {
	defer s.wg.Done()
	buf := make([]byte, 2048)
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := s.conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				select {
				case <-s.done:
					return
				default:
				}
				continue
			}
			slog.Debug("NRL read failed", "sourceKey", s.sourceKey, "error", err)
			return
		}
		pkt, err := decodeNRL21Packet(buf[:n])
		if err != nil {
			continue
		}
		s.handleNRLPacket(pkt)
	}
}

func (s *nrlSession) writeLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case payload := <-s.voiceQueue:
			if err := s.writeNRLPacket(1, payload); err != nil {
				return
			}
			select {
			case <-s.done:
				return
			case <-time.After(nrlVoiceSendInterval):
			}
		}
	}
}

func (s *nrlSession) writeNRLPacket(packetType byte, payload []byte) error {
	s.mu.Lock()
	if s.closed || s.conn == nil {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	count := s.nextNRLCount
	s.nextNRLCount++
	cfg := s.cfg
	conn := s.conn
	s.mu.Unlock()

	packet := encodeNRL21Packet(cfg, packetType, payload, count)
	_, err := conn.Write(packet)
	if err != nil {
		slog.Debug("NRL write failed", "sourceKey", s.sourceKey, "packetType", packetType, "error", err)
		return err
	}
	return nil
}

func (s *nrlSession) handleNRLPacket(pkt nrlPacket) {
	if pkt.Type != 1 || len(pkt.Data) == 0 {
		return
	}
	s.mu.Lock()
	now := time.Now()
	s.lastActivityAt = now
	if !s.callActive || now.Sub(s.callLastVoiceAt) > nrlCallIdleTimeout || s.callSourceCS != s.callSourceCallsign(pkt.Callsign, pkt.SSID) {
		if s.callActive {
			s.emitCallEventLocked(true)
		}
		s.callActive = true
		s.callStreamID++
		s.callSourceCS = s.callSourceCallsign(pkt.Callsign, pkt.SSID)
		s.callSourceName = fmt.Sprintf("NRL %s via %s", s.callSourceCS, s.sourceKey)
		s.callSourceDMRID = pkt.DMRID
		if remote, ok := s.conn.RemoteAddr().(*net.UDPAddr); ok && remote != nil {
			s.callFromIP = remote.IP.String()
			s.callFromPort = remote.Port
		}
		s.emitCallEventLocked(false)
	}
	s.callLastVoiceAt = now
	for start := 0; start+hyteraAnalogFrameSamples <= len(pkt.Data); start += hyteraAnalogFrameSamples {
		ulaw := alawChunkToUlaw(pkt.Data[start : start+hyteraAnalogFrameSamples])
		s.ulawBuffer = append(s.ulawBuffer, ulaw...)
	}
	if len(s.ulawBuffer) < nrlVoiceFlushWatermark {
		s.mu.Unlock()
		return
	}
	for len(s.ulawBuffer) >= hyteraAnalogFrameGroup {
		chunk := append([]byte(nil), s.ulawBuffer[:hyteraAnalogFrameGroup]...)
		s.ulawBuffer = s.ulawBuffer[hyteraAnalogFrameGroup:]
		packet, addr, ok := s.buildHyteraAnalogPacket(chunk)
		if !ok {
			continue
		}
		if _, err := s.bridge.hyteraConn.WriteToUDP(packet, addr); err != nil {
			slog.Debug("NRL->Hytera write failed", "sourceKey", s.sourceKey, "peer", addr.String(), "error", err)
		}
	}
	s.mu.Unlock()
}

func (s *nrlSession) callSourceCallsign(callsign string, ssid uint8) string {
	cs := strings.TrimSpace(callsign)
	if cs == "" {
		cs = strings.TrimSpace(normalizeNRLCallsign(s.cfg.Callsign))
	}
	return fmt.Sprintf("%s-%d", cs, ssid)
}

func (s *nrlSession) expireCallIfIdle(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.callActive {
		return
	}
	if now.Sub(s.callLastVoiceAt) <= nrlCallIdleTimeout {
		return
	}
	s.emitCallEventLocked(true)
	s.callActive = false
}

func (s *nrlSession) emitCallEventLocked(ended bool) {
	if s.bridge.callHandler == nil || s.callStreamID == 0 {
		return
	}
	event := NRLCallEvent{
		Ended:           ended,
		HyteraSourceKey: s.sourceKey,
		// Keep source key stable per Hytera-side analog channel so small caller
		// identity fluctuations (callsign/SSID) don't create duplicate live calls.
		SourceKey:      fmt.Sprintf("nrl:%s", s.sourceKey),
		SourceName:     s.callSourceName,
		SourceCallsign: s.callSourceCS,
		SourceDMRID:    s.callSourceDMRID,
		StreamID:       s.callStreamID,
		FromIP:         s.callFromIP,
		FromPort:       s.callFromPort,
	}
	// Keep start/end ordering deterministic. Async dispatch can reorder events
	// under load and make the registry insert duplicated NRL call rows.
	s.bridge.callHandler(event)
}

func (s *nrlSession) buildHyteraAnalogPacket(voice []byte) ([]byte, *net.UDPAddr, bool) {
	s.bootstrapHyteraFallbackLocked()
	if !s.templateReady || s.hyteraAddr == nil || len(voice) < hyteraAnalogFrameGroup {
		return nil, nil, false
	}
	packet := make([]byte, hyteraAnalogPacketLen)
	copy(packet[:hyteraAnalogHeaderLen], s.template[:])
	s.outSeq++
	packet[4] = s.outSeq
	packet[8] = 0x45
	copy(packet[hyteraAnalogHeaderLen:], voice[:hyteraAnalogFrameGroup])
	return packet, cloneUDPAddr(s.hyteraAddr), true
}

func (s *nrlSession) bootstrapHyteraFallbackLocked() {
	if !s.templateReady {
		s.template = hyteraAnalogFallbackHeader
		s.templateReady = true
		if s.outSeq == 0 {
			s.outSeq = s.template[4]
		}
	}
	if s.hyteraAddr != nil {
		return
	}
	ipText := strings.TrimSpace(strings.TrimPrefix(s.sourceKey, "hytera:"))
	ip := net.ParseIP(ipText)
	if ip == nil {
		return
	}
	// Prefer peer-specific learned voice port (from P2P/startup discovery).
	port := s.cfg.HyteraVoicePort
	if port <= 0 {
		port = int(s.bridge.cfg.Hytera.DMRPort)
	}
	if port <= 0 {
		port = 50001
	}
	s.hyteraAddr = &net.UDPAddr{IP: ip, Port: port}
	slog.Info("NRL->Hytera fallback target prepared", "sourceKey", s.sourceKey, "peer", s.hyteraAddr.String(), "port", port)
}

func isHyteraAnalogVoicePacket(data []byte) bool {
	return len(data) == hyteraAnalogPacketLen &&
		len(data) >= hyteraAnalogHeaderLen &&
		hasNativeHyteraPrefix(data) &&
		data[8] == 0x45
}

func decodeNRL21Packet(data []byte) (nrlPacket, error) {
	if len(data) < nrlHeaderLen {
		return nrlPacket{}, errors.New("short nrl packet")
	}
	if string(data[:4]) != "NRL2" {
		return nrlPacket{}, errors.New("invalid nrl signature")
	}
	total := min(int(binary.BigEndian.Uint16(data[4:6])), len(data))
	p := nrlPacket{
		Type:     data[20],
		DMRID:    bytesToUint24(data[6:9]),
		Callsign: strings.TrimRight(string(data[24:30]), "\x00\r "),
		SSID:     data[30],
	}
	if total > nrlHeaderLen {
		p.Data = append([]byte(nil), data[nrlHeaderLen:total]...)
	}
	return p, nil
}

func encodeNRL21Packet(cfg NRLPeerConfig, packetType byte, payload []byte, count uint16) []byte {
	total := nrlHeaderLen + len(payload)
	packet := make([]byte, total)
	copy(packet[:4], []byte("NRL2"))
	binary.BigEndian.PutUint16(packet[4:6], uint16(total))
	packet[6] = byte(cfg.DMRID >> 16)
	packet[7] = byte(cfg.DMRID >> 8)
	packet[8] = byte(cfg.DMRID)
	packet[20] = packetType
	packet[21] = 0x01
	binary.BigEndian.PutUint16(packet[22:24], count)
	copy(packet[24:30], []byte(normalizeNRLCallsign(cfg.Callsign)))
	ssid := cfg.SSID
	if ssid == 0 {
		ssid = 50
	}
	packet[30] = ssid
	packet[31] = 0x32 // dev_type = 50
	copy(packet[nrlHeaderLen:], payload)
	return packet
}

func normalizeNRLCallsign(callsign string) string {
	cs := strings.ToUpper(strings.TrimSpace(callsign))
	if cs == "" {
		cs = "HYTNRL"
	}
	if len(cs) > 6 {
		cs = cs[:6]
	}
	if len(cs) < 6 {
		cs += strings.Repeat(" ", 6-len(cs))
	}
	return cs
}

func ulawChunkToAlaw(in []byte) []byte {
	out := make([]byte, len(in))
	for i := range in {
		out[i] = Linear2Alaw(ulaw2linear(in[i]))
	}
	return out
}

func alawChunkToUlaw(in []byte) []byte {
	out := make([]byte, len(in))
	for i := range in {
		out[i] = Linear2Ulaw(alaw2linear(in[i]))
	}
	return out
}

func bytesToUint24(b []byte) uint32 {
	if len(b) < 3 {
		return 0
	}
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}
