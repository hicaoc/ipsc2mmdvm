package mmdvm

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hicaoc/ipsc2mmdvm/internal/config"
	"github.com/hicaoc/ipsc2mmdvm/internal/metrics"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/rewrite"
)

type serverState uint8

const (
	serverStateIdle serverState = iota
	serverStateSentChallenge
	serverStateAwaitConfig
	serverStateReady
)

type PeerInfo struct {
	Listener    string
	SourceKey   string
	DMRID       uint32
	Callsign    string
	Model       string
	Description string
	Location    string
	URL         string
	IP          string
	Port        int
	Online      bool
	LastSeenAt  time.Time
	RXFreq      uint
	TXFreq      uint
	TXPower     uint8
	ColorCode   uint8
	Latitude    float64
	Longitude   float64
	Height      uint16
	Slots       byte
}

type peerUpdateHandler func(peer PeerInfo)

type session struct {
	addr      *net.UDPAddr
	state     serverState
	challenge []byte
	info      PeerInfo
	lastPing  time.Time
}

type MMDVMServer struct {
	*MMDVMClient

	conn       *net.UDPConn
	listenAddr string

	sessionMu sync.RWMutex
	sessions  map[string]*session

	peerHandler         peerUpdateHandler
	packetSourceHandler func(sourceKey string, packet proto.Packet)
	sendFilter          func(sourceKey string) bool
}

func NewMMDVMServer(cfg *config.MMDVM, m *metrics.Metrics) *MMDVMServer {
	base := NewMMDVMClient(cfg, m)
	base.keepAlive = 15 * time.Second
	// Hotspots and MMDVM boxes often send keepalives less aggressively than
	// upstream masters, especially behind NAT or embedded firmware loops.
	// Give inbound peers a wider timeout window before declaring them offline.
	base.timeout = 180 * time.Second
	return &MMDVMServer{
		MMDVMClient: base,
		listenAddr:  cfg.Listen,
		sessions:    map[string]*session{},
	}
}

func (h *MMDVMServer) SetPeerUpdateHandler(handler func(peer PeerInfo)) {
	h.peerHandler = handler
}

func (h *MMDVMServer) SetPacketSourceHandler(handler func(sourceKey string, packet proto.Packet)) {
	h.packetSourceHandler = handler
}

func (h *MMDVMServer) SetSendFilter(filter func(sourceKey string) bool) {
	h.sendFilter = filter
}

func (h *MMDVMServer) Start() error {
	if h.listenAddr == "" {
		h.listenAddr = ":62031"
	}
	addr, err := net.ResolveUDPAddr("udp", h.listenAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	h.conn = conn
	h.started.Store(true)
	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(0)
	}

	slog.Info("Listening for MMDVM clients", "network", h.cfg.Name, "listen", conn.LocalAddr().String())

	h.wg.Add(3)
	go h.rxLoop()
	go h.txLoop()
	go h.sessionWatchdog()
	return nil
}

func (h *MMDVMServer) Stop() {
	h.stopOnce.Do(func() {
		slog.Info("Stopping MMDVM server", "network", h.cfg.Name)
		close(h.done)
		h.started.Store(false)
		if h.conn != nil {
			_ = h.conn.Close()
		}
	})
	h.wg.Wait()
}

func (h *MMDVMServer) HandleTranslatedPacket(pkt proto.Packet, _ *net.UDPAddr) bool {
	if !h.started.Load() || h.readySessionCount() == 0 {
		return false
	}
	return h.handleInboundPackets([]proto.Packet{pkt})
}

func (h *MMDVMServer) SendPacketToPeer(pkt proto.Packet, sourceKey string) bool {
	if !h.started.Load() || sourceKey == "" {
		return false
	}

	h.sessionMu.RLock()
	var target *session
	for _, sess := range h.sessions {
		if sess.state != serverStateReady {
			continue
		}
		if sess.info.SourceKey == sourceKey {
			copySess := *sess
			copySess.addr = cloneUDPAddr(sess.addr)
			target = &copySess
			break
		}
	}
	h.sessionMu.RUnlock()
	if target == nil || target.addr == nil {
		return false
	}

	data := pkt.Encode()
	if h.metrics != nil {
		h.metrics.MMDVMPacketsSent.WithLabelValues(h.cfg.Name).Inc()
	}
	if _, err := h.conn.WriteToUDP(data, target.addr); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Error("Error writing to targeted MMDVM client", "network", h.cfg.Name, "peer", target.addr.String(), "error", err)
		return false
	}
	return true
}

func (h *MMDVMServer) rxLoop() {
	defer h.wg.Done()
	buf := make([]byte, 1024)
	for {
		n, addr, err := h.conn.ReadFromUDP(buf)
		if err != nil {
			if !h.started.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Error("Error reading from MMDVM client", "network", h.cfg.Name, "error", err)
			continue
		}
		h.handlePacket(append([]byte(nil), buf[:n]...), addr)
	}
}

func (h *MMDVMServer) txLoop() {
	defer h.wg.Done()
	for {
		select {
		case <-h.done:
			return
		case pkt := <-h.tx_chan:
			data := pkt.Encode()
			for _, sess := range h.readySessions() {
				if h.sendFilter != nil && !h.sendFilter(sess.info.SourceKey) {
					continue
				}
				if h.metrics != nil {
					h.metrics.MMDVMPacketsSent.WithLabelValues(h.cfg.Name).Inc()
				}
				if _, err := h.conn.WriteToUDP(data, sess.addr); err != nil && !errors.Is(err, net.ErrClosed) {
					slog.Error("Error writing to MMDVM client", "network", h.cfg.Name, "peer", sess.addr.String(), "error", err)
				}
			}
		}
	}
}

func (h *MMDVMServer) sessionWatchdog() {
	defer h.wg.Done()
	ticker := time.NewTicker(h.keepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			var stale []string
			h.sessionMu.RLock()
			for key, sess := range h.sessions {
				if sess.state == serverStateReady && !sess.lastPing.IsZero() && now.Sub(sess.lastPing) > h.timeout {
					slog.Debug("MMDVM peer timeout",
						"network", h.cfg.Name,
						"peer", key,
						"sourceKey", sess.info.SourceKey,
						"dmrid", sess.info.DMRID,
						"lastPing", sess.lastPing.UTC().Format(time.RFC3339Nano),
						"ageSeconds", now.Sub(sess.lastPing).Seconds(),
						"timeoutSeconds", h.timeout.Seconds())
					stale = append(stale, key)
				}
			}
			h.sessionMu.RUnlock()
			for _, key := range stale {
				h.setOffline(key, "timeout")
			}
		case <-h.done:
			return
		}
	}
}

func (h *MMDVMServer) handlePacket(data []byte, addr *net.UDPAddr) {
	if len(data) < 4 {
		return
	}
	if len(data) >= 5 && string(data[:5]) == "RPTCL" {
		h.handleDisconnect(data, addr)
		return
	}

	switch string(data[:4]) {
	case "RPTL":
		h.handleLogin(data, addr)
	case "RPTK":
		h.handleAuth(data, addr)
	case "RPTC":
		h.handleConfig(data, addr)
	case "RPTP":
		if len(data) >= 7 && string(data[:7]) == "RPTPING" {
			h.handlePing(data, addr)
		}
	case "DMRD":
		h.handleDMRD(data, addr)
	}
}

func (h *MMDVMServer) handleLogin(data []byte, addr *net.UDPAddr) {
	if !validID(data, 4) {
		slog.Debug("rejected MMDVM login with invalid id", "network", h.cfg.Name, "peer", addr.String())
		h.sendNAK(addr)
		return
	}
	dmrid := binary.BigEndian.Uint32(data[4:8])
	h.beginHandshake(addr, dmrid, "login")
}

func (h *MMDVMServer) handleAuth(data []byte, addr *net.UDPAddr) {
	if len(data) < 40 || !validID(data, 4) {
		slog.Debug("rejected MMDVM auth with invalid id", "network", h.cfg.Name, "peer", addr.String())
		h.sendNAK(addr)
		return
	}
	dmrid := binary.BigEndian.Uint32(data[4:8])
	h.sessionMu.RLock()
	sess := h.sessions[addr.String()]
	h.sessionMu.RUnlock()
	if sess == nil || sess.state != serverStateSentChallenge {
		slog.Debug("rejected MMDVM auth without challenge session",
			"network", h.cfg.Name,
			"peer", addr.String(),
			"dmrid", dmrid)
		h.sendNAK(addr)
		return
	}

	s256 := sha256.New()
	s256.Write(sess.challenge)
	s256.Write([]byte(h.cfg.Password))
	expected := s256.Sum(nil)
	if !bytes.Equal(data[8:40], expected) {
		if h.metrics != nil {
			h.metrics.MMDVMAuthFailures.WithLabelValues(h.cfg.Name).Inc()
		}
		slog.Debug("rejected MMDVM auth due to invalid token",
			"network", h.cfg.Name,
			"peer", addr.String(),
			"dmrid", dmrid)
		h.sendNAK(addr)
		return
	}

	h.sessionMu.Lock()
	sess = h.getOrCreateSessionLocked(addr)
	sess.state = serverStateAwaitConfig
	h.sessionMu.Unlock()
	slog.Debug("accepted MMDVM auth",
		"network", h.cfg.Name,
		"peer", addr.String(),
		"dmrid", dmrid)
	h.sendACK(addr)
}

func (h *MMDVMServer) handleConfig(data []byte, addr *net.UDPAddr) {
	if len(data) < 8 || !validID(data, 4) {
		slog.Debug("rejected MMDVM config with invalid id", "network", h.cfg.Name, "peer", addr.String())
		h.sendNAK(addr)
		return
	}
	info := parseRPTCConfig(data)
	info.Listener = h.cfg.Name
	info.IP = addr.IP.String()
	info.Port = addr.Port
	info.Online = true
	info.LastSeenAt = time.Now().UTC()
	info.SourceKey = fmt.Sprintf("mmdvm:%s:%d", h.cfg.Name, info.DMRID)
	if info.DMRID == 0 {
		info.SourceKey = fmt.Sprintf("mmdvm:%s:%s", h.cfg.Name, addr.String())
	}

	h.sessionMu.Lock()
	sess := h.getOrCreateSessionLocked(addr)
	sess.state = serverStateReady
	sess.lastPing = time.Now()
	sess.info = info
	h.sessionMu.Unlock()

	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(float64(h.readySessionCount()))
	}
	slog.Debug("accepted MMDVM config",
		"network", h.cfg.Name,
		"peer", addr.String(),
		"sourceKey", info.SourceKey,
		"dmrid", info.DMRID,
		"callsign", info.Callsign)
	h.emitPeer(info)
	h.sendACK(addr)
}

func (h *MMDVMServer) handlePing(data []byte, addr *net.UDPAddr) {
	if len(data) < 11 || !validID(data, 7) {
		return
	}
	dmrid := binary.BigEndian.Uint32(data[7:11])
	h.sessionMu.Lock()
	sess := h.sessions[addr.String()]
	if (sess == nil || sess.state != serverStateReady) && dmrid != 0 {
		if rebound, oldKey := h.findReadySessionByDMRIDLocked(dmrid); rebound != nil {
			h.rebindSessionLocked(oldKey, addr, rebound)
			sess = rebound
		}
	}
	if sess != nil && sess.state == serverStateReady {
		sess.lastPing = time.Now()
		sess.addr = cloneUDPAddr(addr)
		sess.info.IP = addr.IP.String()
		sess.info.Port = addr.Port
		sess.info.LastSeenAt = sess.lastPing.UTC()
		sess.info.Online = true
		info := sess.info
		h.sessionMu.Unlock()
		slog.Debug("received MMDVM ping",
			"network", h.cfg.Name,
			"peer", addr.String(),
			"sourceKey", info.SourceKey,
			"dmrid", info.DMRID,
			"callsign", info.Callsign)
		h.emitPeer(info)
	} else {
		ready := h.readySessionKeysLocked()
		h.sessionMu.Unlock()
		slog.Debug("received MMDVM ping without ready session",
			"network", h.cfg.Name,
			"peer", addr.String(),
			"dmrid", dmrid,
			"readySessions", strings.Join(ready, ","))
		h.beginHandshake(addr, dmrid, "ping")
		return
	}

	resp := make([]byte, 11)
	copy(resp, "MSTPONG")
	binary.BigEndian.PutUint32(resp[7:], h.cfg.ID)
	h.sendRaw(addr, resp)
}

func (h *MMDVMServer) handleDMRD(data []byte, addr *net.UDPAddr) {
	packet, ok := proto.Decode(data)
	if !ok {
		return
	}
	h.sessionMu.Lock()
	sess := h.sessions[addr.String()]
	if (sess == nil || sess.state != serverStateReady) && packet.Repeater != 0 {
		if rebound, oldKey := h.findReadySessionByDMRIDLocked(uint32(packet.Repeater)); rebound != nil {
			h.rebindSessionLocked(oldKey, addr, rebound)
			sess = rebound
		}
	}
	sourceKey := ""
	if sess != nil && sess.state == serverStateReady {
		sess.lastPing = time.Now()
		sess.addr = cloneUDPAddr(addr)
		sess.info.IP = addr.IP.String()
		sess.info.Port = addr.Port
		sess.info.LastSeenAt = sess.lastPing.UTC()
		sourceKey = sess.info.SourceKey
		info := sess.info
		h.sessionMu.Unlock()
		slog.Debug("received MMDVM DMRD",
			"network", h.cfg.Name,
			"peer", addr.String(),
			"sourceKey", sourceKey,
			"dmrid", info.DMRID,
			"callsign", info.Callsign)
		h.emitPeer(info)
	} else {
		h.sessionMu.Unlock()
		slog.Debug("received MMDVM DMRD without ready session",
			"network", h.cfg.Name,
			"peer", addr.String())
		return
	}
	if h.metrics != nil {
		h.metrics.MMDVMPacketsReceived.WithLabelValues(h.cfg.Name).Inc()
	}
	h.handleOutboundPacketFrom(sourceKey, packet)
}

func (h *MMDVMServer) handleDisconnect(data []byte, addr *net.UDPAddr) {
	if len(data) < 9 || !validID(data, 5) {
		return
	}
	h.setOffline(addr.String(), "disconnected")
}

func (h *MMDVMServer) setOffline(key, status string) {
	h.sessionMu.Lock()
	sess := h.sessions[key]
	if sess == nil {
		h.sessionMu.Unlock()
		return
	}
	info := sess.info
	info.Online = false
	info.StatusString(status)
	delete(h.sessions, key)
	count := h.readySessionCountLocked()
	h.sessionMu.Unlock()

	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(float64(count))
	}
	h.emitPeer(info)
}

func (h *MMDVMServer) emitPeer(info PeerInfo) {
	slog.Debug("emitting MMDVM peer update",
		"network", h.cfg.Name,
		"sourceKey", info.SourceKey,
		"dmrid", info.DMRID,
		"callsign", info.Callsign,
		"online", info.Online,
		"peer", fmt.Sprintf("%s:%d", info.IP, info.Port))
	if h.peerHandler != nil {
		h.peerHandler(info)
	}
}

func (h *MMDVMServer) handleOutboundPacketFrom(sourceKey string, packet proto.Packet) {
	if !rewrite.Apply(h.netRewrites, &packet) {
		slog.Debug("MMDVM DMRD dropped (no rewrite rule matched)", "network", h.cfg.Name)
		if h.metrics != nil {
			h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "no_rewrite").Inc()
		}
		return
	}

	slog.Debug("MMDVM DMRD after rewrite", "network", h.cfg.Name, "packet", packet, "sourceKey", sourceKey)

	isTerminator := packet.FrameType == frameTypeDataSync && packet.DTypeOrVSeq == dtypeTerminatorWithLC
	if h.outboundTSMgr != nil {
		if !h.outboundTSMgr.Submit(packet.Slot, packet.StreamID, h.cfg.Name, packet) {
			slog.Debug("MMDVM DMRD buffered (timeslot busy)",
				"network", h.cfg.Name, "slot", packet.Slot, "streamID", packet.StreamID)
			if h.metrics != nil {
				h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "timeslot_busy").Inc()
			}
			return
		}
	}

	if h.packetSourceHandler != nil {
		h.packetSourceHandler(sourceKey, packet)
	} else {
		h.translateAndForwardToIPSC(packet)
	}

	if isTerminator && h.outboundTSMgr != nil {
		h.drainPendingOutbound(packet.Slot, packet.StreamID)
	}
}

func (h *MMDVMServer) getOrCreateSessionLocked(addr *net.UDPAddr) *session {
	key := addr.String()
	if sess, ok := h.sessions[key]; ok {
		sess.addr = cloneUDPAddr(addr)
		return sess
	}
	sess := &session{addr: cloneUDPAddr(addr)}
	h.sessions[key] = sess
	return sess
}

func (h *MMDVMServer) findReadySessionByDMRIDLocked(dmrid uint32) (*session, string) {
	for key, sess := range h.sessions {
		if sess != nil && sess.state == serverStateReady && sess.info.DMRID == dmrid {
			return sess, key
		}
	}
	return nil, ""
}

func (h *MMDVMServer) rebindSessionLocked(oldKey string, addr *net.UDPAddr, sess *session) {
	newKey := addr.String()
	if sess == nil || newKey == "" {
		return
	}
	if oldKey != "" && oldKey != newKey {
		delete(h.sessions, oldKey)
	}
	sess.addr = cloneUDPAddr(addr)
	h.sessions[newKey] = sess
}

func (h *MMDVMServer) readySessions() []*session {
	h.sessionMu.RLock()
	defer h.sessionMu.RUnlock()
	out := make([]*session, 0, len(h.sessions))
	for _, sess := range h.sessions {
		if sess.state == serverStateReady && sess.addr != nil {
			copySess := *sess
			copySess.addr = cloneUDPAddr(sess.addr)
			out = append(out, &copySess)
		}
	}
	return out
}

func (h *MMDVMServer) readySessionCount() int {
	h.sessionMu.RLock()
	defer h.sessionMu.RUnlock()
	return h.readySessionCountLocked()
}

func (h *MMDVMServer) readySessionCountLocked() int {
	count := 0
	for _, sess := range h.sessions {
		if sess.state == serverStateReady {
			count++
		}
	}
	return count
}

func (h *MMDVMServer) readySessionKeysLocked() []string {
	out := make([]string, 0, len(h.sessions))
	for key, sess := range h.sessions {
		if sess.state != serverStateReady {
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d@%s", sess.info.SourceKey, sess.info.DMRID, key))
	}
	return out
}

func (h *MMDVMServer) sendACK(addr *net.UDPAddr) {
	h.sendRaw(addr, []byte(rptAck))
}

func (h *MMDVMServer) sendNAK(addr *net.UDPAddr) {
	h.sendRaw(addr, []byte("RPTNAK"))
}

func (h *MMDVMServer) beginHandshake(addr *net.UDPAddr, dmrid uint32, reason string) {
	challenge := make([]byte, 4)
	if _, err := rand.Read(challenge); err != nil {
		h.sendNAK(addr)
		return
	}
	h.sessionMu.Lock()
	sess := h.getOrCreateSessionLocked(addr)
	sess.state = serverStateSentChallenge
	sess.challenge = challenge
	h.sessionMu.Unlock()

	resp := make([]byte, 10)
	copy(resp, rptAck)
	copy(resp[6:], challenge)
	slog.Debug("accepted MMDVM login",
		"network", h.cfg.Name,
		"peer", addr.String(),
		"dmrid", dmrid,
		"reason", reason)
	h.sendRaw(addr, resp)
}

func (h *MMDVMServer) sendRaw(addr *net.UDPAddr, data []byte) {
	if addr == nil {
		return
	}
	if _, err := h.conn.WriteToUDP(data, addr); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Error("Error sending to MMDVM client", "network", h.cfg.Name, "peer", addr.String(), "error", err)
	}
}

func validID(data []byte, offset int) bool {
	if len(data) < offset+4 {
		return false
	}
	return binary.BigEndian.Uint32(data[offset:offset+4]) != 0
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}
	clone := *addr
	if addr.IP != nil {
		clone.IP = append([]byte(nil), addr.IP...)
	}
	return &clone
}

func parseRPTCConfig(data []byte) PeerInfo {
	info := PeerInfo{}
	if len(data) >= 8 {
		info.DMRID = binary.BigEndian.Uint32(data[4:8])
	}
	if len(data) < 98 {
		return info
	}
	info.Callsign = trimASCII(data[8:16])
	info.RXFreq = parseUintField(data[16:25])
	info.TXFreq = parseUintField(data[25:34])
	info.TXPower = uint8(parseUintField(data[34:36]))
	info.ColorCode = uint8(parseUintField(data[36:38]))
	info.Latitude = parseFloatField(data[38:46])
	info.Longitude = parseFloatField(data[46:55])
	info.Height = uint16(parseUintField(data[55:58]))
	info.Location = trimASCII(data[58:78])
	info.Description = trimASCII(data[78:97])
	info.Slots = byte(parseUintField(data[97:98]))
	if len(data) >= 222 {
		info.URL = trimASCII(data[98:222])
	}
	if len(data) >= 302 {
		info.Model = trimASCII(data[262:302])
	}
	if info.Model == "" {
		info.Model = "MMDVM Client"
	}
	return info
}

func trimASCII(data []byte) string {
	return strings.TrimSpace(string(bytes.Trim(data, "\x00")))
}

func parseUintField(data []byte) uint {
	var out uint
	for _, b := range data {
		if b < '0' || b > '9' {
			continue
		}
		out = out*10 + uint(b-'0')
	}
	return out
}

func parseFloatField(data []byte) float64 {
	var sign float64 = 1
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return 0
	}
	if raw[0] == '-' {
		sign = -1
		raw = raw[1:]
	} else if raw[0] == '+' {
		raw = raw[1:]
	}
	parts := strings.SplitN(raw, ".", 2)
	whole := parseUintField([]byte(parts[0]))
	if len(parts) == 1 {
		return sign * float64(whole)
	}
	fractionRaw := parts[1]
	fraction := parseUintField([]byte(fractionRaw))
	denom := 1.0
	for range fractionRaw {
		denom *= 10
	}
	return sign * (float64(whole) + float64(fraction)/denom)
}

func (p *PeerInfo) StatusString(status string) {
	p.Online = false
	if p.Model == "" {
		p.Model = "MMDVM Client"
	}
}
