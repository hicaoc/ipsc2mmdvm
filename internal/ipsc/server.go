package ipsc

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hicaoc/ipsc2mmdvm/internal/config"
	"github.com/hicaoc/ipsc2mmdvm/internal/metrics"
)

type IPSCServer struct {
	cfg     *config.Config
	metrics *metrics.Metrics
	udp     *net.UDPConn
	mu      sync.RWMutex

	localID   uint32
	authKey   []byte // 20-byte HMAC key decoded from hex
	peers     map[uint32]*Peer
	lastSend  map[uint32]time.Time
	peerLocks map[string]*sync.Mutex

	burstHandler       func(packetType byte, data []byte, addr *net.UDPAddr)
	peerHandler        func(peer Peer)
	peerOfflineHandler func(sourceKey string)
	sendFilter         func(sourceKey string) bool

	wg       sync.WaitGroup
	stopped  atomic.Bool
	stopOnce sync.Once
	done     chan struct{}
}

type Packet struct {
	data []byte
}

type Peer struct {
	ID                 uint32
	Addr               *net.UDPAddr
	Mode               byte
	Flags              [4]byte
	LastSeen           time.Time
	KeepAliveReceived  uint64
	RegistrationStatus bool
}

type PacketType byte

const (
	PacketType_GroupVoice            PacketType = 0x80
	PacketType_PrivateVoice          PacketType = 0x81
	PacketType_GroupData             PacketType = 0x83
	PacketType_PrivateData           PacketType = 0x84
	PacketType_RepeaterWakeUp        PacketType = 0x85
	PacketType_MasterRegisterRequest PacketType = 0x90
	PacketType_MasterRegisterReply   PacketType = 0x91
	PacketType_PeerListRequest       PacketType = 0x92
	PacketType_PeerListReply         PacketType = 0x93
	PacketType_MasterAliveRequest    PacketType = 0x96
	PacketType_MasterAliveReply      PacketType = 0x97
	PacketType_ControlF0             PacketType = 0xF0
)

var (
	//nolint:gochecknoglobals
	ipscVersion = []byte{0x04, 0x02, 0x04, 0x01}
)

var ErrPacketIgnored = errors.New("packet ignored")

func NewIPSCServer(cfg *config.Config, m *metrics.Metrics) *IPSCServer {
	// Decode the auth key from hex string to raw bytes.
	// DMRlink left-pads the hex key to 40 characters (20 bytes) with zeros.
	var authKey []byte
	if cfg.IPSC.Auth.Enabled && cfg.IPSC.Auth.Key != "" {
		hexKey := cfg.IPSC.Auth.Key
		// Left-pad with zeros to 40 hex characters (20 bytes)
		for len(hexKey) < 40 {
			hexKey = "0" + hexKey
		}
		var err error
		authKey, err = hex.DecodeString(hexKey)
		if err != nil {
			slog.Error("failed to decode IPSC auth key as hex, using raw string", "error", err)
			authKey = []byte(cfg.IPSC.Auth.Key)
		}
	}

	// Use the first configured MMDVM network's ID as the local peer identity.
	localID := cfg.BridgeID()

	return &IPSCServer{
		cfg:       cfg,
		metrics:   m,
		localID:   localID,
		authKey:   authKey,
		peers:     map[uint32]*Peer{},
		lastSend:  map[uint32]time.Time{},
		peerLocks: map[string]*sync.Mutex{},
		done:      make(chan struct{}),
	}
}

func (s *IPSCServer) Start() error {
	var err error
	s.udp, err = net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.IPv4zero,
		Port: int(s.cfg.IPSC.Port),
	})

	if err != nil {
		return fmt.Errorf("error starting UDP listener: %w", err)
	}

	s.wg.Add(1)
	go s.handler()
	s.wg.Add(1)
	go s.peerExpiryLoop()

	return nil
}

func (s *IPSCServer) Stop() {
	s.stopOnce.Do(func() {
		slog.Info("Stopping IPSC server")
		s.stopped.Store(true)
		close(s.done)
		if s.udp != nil {
			if err := s.udp.Close(); err != nil {
				slog.Error("error closing UDP listener", "error", err)
			}
		}
	})
	s.wg.Wait()
}

func (s *IPSCServer) handler() {
	defer s.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, addr, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if s.metrics != nil {
				s.metrics.IPSCUDPErrors.WithLabelValues("read").Inc()
			}
			slog.Warn("error reading from UDP", "error", err)
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])

		lock := s.peerPacketLock(addr)
		s.wg.Add(1)
		go func(packetData []byte, packetAddr *net.UDPAddr, peerLock *sync.Mutex) {
			defer s.wg.Done()
			peerLock.Lock()
			defer peerLock.Unlock()
			packet, err := s.handlePacket(packetData, packetAddr)
			if err != nil {
				if errors.Is(err, ErrPacketIgnored) {
					return
				}
				slog.Warn("error parsing packet", "peer", packetAddr, "error", err, "length", len(packetData), "packet", packetData)
				return
			}

			slog.Debug("received packet",
				"protocol", "ipsc",
				"channel", "udp",
				"peer", packetAddr,
				"length", len(packetData),
				"packet", packet)
		}(data, addr, lock)
	}
}

func (s *IPSCServer) peerPacketLock(addr *net.UDPAddr) *sync.Mutex {
	key := ""
	if addr != nil {
		key = addr.String()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock := s.peerLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		s.peerLocks[key] = lock
	}
	return lock
}

func (s *IPSCServer) handlePacket(data []byte, addr *net.UDPAddr) (*Packet, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("packet too short")
	}

	packetType := data[0]

	if s.cfg.IPSC.Auth.Enabled {
		if len(data) <= 10 {
			return nil, fmt.Errorf("packet too short for authentication")
		}
		if !s.auth(data) {
			if s.metrics != nil {
				s.metrics.IPSCAuthFailures.Inc()
			}
			return nil, fmt.Errorf("authentication failed")
		}
		data = data[:len(data)-10] // Remove the hash from the data
	}

	switch PacketType(packetType) {
	case PacketType_GroupVoice:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("group_voice").Inc()
		}
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_PrivateVoice:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("private_voice").Inc()
		}
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_GroupData:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("group_data").Inc()
		}
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_PrivateData:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("private_data").Inc()
		}
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_RepeaterWakeUp:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("wake_up").Inc()
		}
		if err := s.handleRepeaterWakeUp(data, addr); err != nil {
			return nil, err
		}
	case PacketType_MasterRegisterRequest:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("register").Inc()
		}
		if err := s.handleMasterRegisterRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_MasterAliveRequest:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("alive").Inc()
		}
		if err := s.handleMasterAliveRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_PeerListRequest:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("peer_list").Inc()
		}
		if err := s.handlePeerListRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_MasterRegisterReply, PacketType_PeerListReply, PacketType_MasterAliveReply, PacketType_ControlF0:
		// These are reply packets, we shouldn't receive them as a server, keeping quiet.
		return nil, ErrPacketIgnored
	default:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("other").Inc()
		}
		return nil, fmt.Errorf("unknown packet type: %d", packetType)
	}

	return &Packet{data: data}, nil
}

func (s *IPSCServer) handleMasterRegisterRequest(data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	mode := s.defaultModeByte()
	flags := s.defaultFlagsBytes()
	if len(data) >= 10 {
		mode = data[5]
		copy(flags[:], data[6:10])
	}

	s.upsertPeer(peerID, addr, mode, flags)

	packet := &Packet{data: s.buildMasterRegisterReply()}
	if err := s.sendPacket(packet, addr); err != nil {
		return fmt.Errorf("error sending master register reply: %w", err)
	}

	return nil
}

func (s *IPSCServer) handleMasterAliveRequest(data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	s.markPeerAlive(peerID, addr)

	packet := &Packet{data: s.buildMasterAliveReply()}
	if err := s.sendPacket(packet, addr); err != nil {
		return fmt.Errorf("error sending master alive reply: %w", err)
	}

	return nil
}

func (s *IPSCServer) handlePeerListRequest(data []byte, addr *net.UDPAddr) error {
	if _, err := parsePeerID(data); err != nil {
		return err
	}

	packet := &Packet{data: s.buildPeerListReply()}
	if err := s.sendPacket(packet, addr); err != nil {
		return fmt.Errorf("error sending peer list reply: %w", err)
	}

	return nil
}

func (s *IPSCServer) handleRepeaterWakeUp(data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	s.markPeerAlive(peerID, addr)
	slog.Debug("repeater wake-up packet received", "peer", addr, "peerID", peerID, "length", len(data))
	return nil
}

func (s *IPSCServer) handleUserPacket(packetType PacketType, data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	s.markPeerAlive(peerID, addr)
	slog.Debug("IPSC burst received", "peer", addr, "peerID", peerID, "packetType", byte(packetType), "length", len(data))
	if s.burstHandler != nil {
		packetCopy := make([]byte, len(data))
		copy(packetCopy, data)
		go s.burstHandler(byte(packetType), packetCopy, addr)
	}
	return nil
}

func (s *IPSCServer) SetBurstHandler(handler func(packetType byte, data []byte, addr *net.UDPAddr)) {
	s.burstHandler = handler
}

func (s *IPSCServer) SetPeerUpdateHandler(handler func(peer Peer)) {
	s.peerHandler = handler
}

func (s *IPSCServer) SetPeerOfflineHandler(handler func(sourceKey string)) {
	s.peerOfflineHandler = handler
}

func (s *IPSCServer) upsertPeer(peerID uint32, addr *net.UDPAddr, mode byte, flags [4]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	peer, ok := s.peers[peerID]
	if !ok {
		peer = &Peer{ID: peerID}
		s.peers[peerID] = peer
	}
	peer.Addr = cloneUDPAddr(addr)
	peer.Mode = mode
	peer.Flags = flags
	peer.LastSeen = time.Now()
	peer.RegistrationStatus = true

	if s.metrics != nil {
		s.metrics.IPSCPeersRegistered.Set(float64(len(s.peers)))
	}
	if s.peerHandler != nil {
		s.peerHandler(*peer)
	}
}

func (s *IPSCServer) markPeerAlive(peerID uint32, addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()

	peer, ok := s.peers[peerID]
	if !ok {
		peer = &Peer{ID: peerID}
		s.peers[peerID] = peer
	}
	peer.Addr = cloneUDPAddr(addr)
	peer.LastSeen = time.Now()
	peer.KeepAliveReceived++
	if s.peerHandler != nil {
		s.peerHandler(*peer)
	}
}

const ipscPeerTimeout = 30 * time.Second

func (s *IPSCServer) peerExpiryLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if s.stopped.Load() {
				return
			}
			s.expireStalePeers()
		}
	}
}

func (s *IPSCServer) expireStalePeers() {
	now := time.Now()
	s.mu.Lock()
	var expired []Peer
	for id, peer := range s.peers {
		if now.Sub(peer.LastSeen) > ipscPeerTimeout {
			expired = append(expired, *peer)
			delete(s.peers, id)
		}
	}
	if s.metrics != nil && len(expired) > 0 {
		s.metrics.IPSCPeersRegistered.Set(float64(len(s.peers)))
	}
	s.mu.Unlock()

	for _, peer := range expired {
		sourceKey := fmt.Sprintf("moto:%d", peer.ID)
		slog.Info("IPSC peer offline (timeout)", "sourceKey", sourceKey, "peerID", peer.ID)
		if s.peerOfflineHandler != nil {
			s.peerOfflineHandler(sourceKey)
		}
	}
}

func (s *IPSCServer) buildMasterRegisterReply() []byte {
	packet := make([]byte, 0, 1+4+5+2+4)
	packet = append(packet, byte(PacketType_MasterRegisterReply))
	packet = append(packet, s.localIDBytes()...)
	packet = append(packet, s.defaultModeByte())
	flags := s.defaultFlagsBytes()
	packet = append(packet, flags[:]...)

	numPeers := s.peerCount()
	if numPeers > math.MaxUint16 {
		numPeers = math.MaxUint16
	}
	packet = append(packet, uint16ToBytes(uint16(numPeers))...) //nolint:gosec // Bounds checked
	packet = append(packet, ipscVersion...)
	return packet
}

func (s *IPSCServer) buildMasterAliveReply() []byte {
	packet := make([]byte, 0, 1+4+5+4)
	packet = append(packet, byte(PacketType_MasterAliveReply))
	packet = append(packet, s.localIDBytes()...)
	packet = append(packet, s.defaultModeByte())
	flags := s.defaultFlagsBytes()
	packet = append(packet, flags[:]...)
	packet = append(packet, ipscVersion...)
	return packet
}

func (s *IPSCServer) buildPeerListReply() []byte {
	peerList := s.buildPeerList()
	packet := make([]byte, 0, 1+4+2+len(peerList))
	packet = append(packet, byte(PacketType_PeerListReply))
	packet = append(packet, s.localIDBytes()...)
	if len(peerList) > math.MaxUint16 {
		packet = append(packet, uint16ToBytes(math.MaxUint16)...)
	} else {
		packet = append(packet, uint16ToBytes(uint16(len(peerList)))...) //nolint:gosec // Bounds checked
	}
	packet = append(packet, peerList...)
	return packet
}

func (s *IPSCServer) buildPeerList() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.peers) == 0 {
		return nil
	}

	peerList := make([]byte, 0, len(s.peers)*11)
	for _, peer := range s.peers {
		if peer.Addr == nil || peer.Addr.IP == nil {
			continue
		} //nolint:gosec
		peerList = append(peerList, uint32ToBytes(peer.ID)...)
		peerList = append(peerList, peer.Addr.IP.To4()...)
		peerPort := peer.Addr.Port
		if peerPort < 0 || peerPort > 65535 {
			peerPort = 0
		}
		peerList = append(peerList, uint16ToBytes(uint16(peerPort))...) //nolint:gosec // Bounds checked
		peerList = append(peerList, peer.Mode)
	}

	return peerList
}

func (s *IPSCServer) peerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.peers)
}

func (s *IPSCServer) localIDBytes() []byte {
	return uint32ToBytes(s.localID)
}

func (s *IPSCServer) defaultModeByte() byte {
	const (
		peerOperational = 0b01000000
		peerDigital     = 0b00100000
		ts1On           = 0b00001000
		ts2On           = 0b00000010
	)
	return peerOperational | peerDigital | ts1On | ts2On
}

func (s *IPSCServer) defaultFlagsBytes() [4]byte {
	flags := [4]byte{}
	flags[2] = 0x00
	flags[3] = 0x0D
	if s.cfg.IPSC.Auth.Enabled {
		flags[3] |= 0x10
	}
	return flags
}

func parsePeerID(data []byte) (uint32, error) {
	if len(data) < 5 {
		return 0, fmt.Errorf("packet too short for peer ID")
	}
	return binary.BigEndian.Uint32(data[1:5]), nil
}

func uint16ToBytes(value uint16) []byte {
	buf := make([]byte, 2)
	binary.BigEndian.PutUint16(buf, value)
	return buf
}

func uint32ToBytes(value uint32) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, value)
	return buf
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}
	cloned := &net.UDPAddr{Port: addr.Port, Zone: addr.Zone}
	if addr.IP != nil {
		cloned.IP = append([]byte(nil), addr.IP...)
	}
	return cloned
}

func (s *IPSCServer) auth(data []byte) bool {
	// Last 10 bytes are the sha hash
	payload := data[:len(data)-10]
	hash := data[len(data)-10:]
	expectedHash := hmac.New(sha1.New, s.authKey)
	expectedHash.Write(payload)
	expectedHashSum := expectedHash.Sum(nil)[:10]

	return hmac.Equal(hash, expectedHashSum)
}

func (s *IPSCServer) sendPacket(packet *Packet, addr *net.UDPAddr) error {
	if s.cfg.IPSC.Auth.Enabled {
		hash := hmac.New(sha1.New, s.authKey)
		hash.Write(packet.data)
		hashSum := hash.Sum(nil)[:10]
		packet.data = append(packet.data, hashSum...)
	}

	n, err := s.udp.WriteToUDP(packet.data, addr)
	if err != nil {
		if s.metrics != nil {
			s.metrics.IPSCUDPErrors.WithLabelValues("write").Inc()
		}
		return fmt.Errorf("error sending packet: %w", err)
	}
	if n != len(packet.data) {
		return fmt.Errorf("error sending packet: only sent %d of %d bytes", n, len(packet.data))
	}
	return nil
}

func (s *IPSCServer) SendUserPacket(data []byte) {
	if s.stopped.Load() {
		return
	}
	s.mu.RLock()
	peers := make([]*Peer, 0, len(s.peers))
	for _, peer := range s.peers {
		if peer.Addr != nil {
			peers = append(peers, peer)
		}
	}
	s.mu.RUnlock()

	for _, peer := range peers {
		if s.sendFilter != nil && !s.sendFilter(fmt.Sprintf("moto:%d", peer.ID)) {
			continue
		}
		s.pacePeer(peer.ID)
		packetData := make([]byte, len(data))
		copy(packetData, data)
		packet := &Packet{data: packetData}
		callInfo := byte(0x00)
		burstType := byte(0x00)
		slotMarker := byte(0x00)
		slot := "n/a"
		if len(packet.data) > 17 {
			callInfo = packet.data[17]
			slot = "ts1"
			if callInfo&0x20 != 0 {
				slot = "ts2"
			}
		}
		if len(packet.data) > 30 {
			burstType = packet.data[30]
		}
		if len(packet.data) > 35 {
			slotMarker = packet.data[35]
		}
		slog.Debug("IPSC burst sending",
			"peer", peer.Addr,
			"length", len(packet.data),
			"slot", slot,
			"callInfo", fmt.Sprintf("0x%02X", callInfo),
			"burstType", fmt.Sprintf("0x%02X", burstType),
			"slotMarker", fmt.Sprintf("0x%02X", slotMarker))
		if err := s.sendPacket(packet, peer.Addr); err != nil {
			slog.Warn("failed sending IPSC user packet", "peer", peer.Addr, "error", err)
		} else if s.metrics != nil {
			s.metrics.IPSCPacketsSent.Inc()
		}
	}
}

func (s *IPSCServer) SendUserPacketToPeer(peerID uint32, data []byte) bool {
	if s.stopped.Load() || peerID == 0 {
		return false
	}
	if s.sendFilter != nil && !s.sendFilter(fmt.Sprintf("moto:%d", peerID)) {
		return false
	}

	s.mu.RLock()
	peer := s.peers[peerID]
	var addr *net.UDPAddr
	if peer != nil && peer.Addr != nil {
		addr = cloneUDPAddr(peer.Addr)
	}
	s.mu.RUnlock()
	if addr == nil {
		return false
	}

	s.pacePeer(peerID)
	packetData := make([]byte, len(data))
	copy(packetData, data)
	packet := &Packet{data: packetData}
	if err := s.sendPacket(packet, addr); err != nil {
		slog.Warn("failed sending IPSC user packet to peer", "peerID", peerID, "peer", addr, "error", err)
		return false
	}
	if s.metrics != nil {
		s.metrics.IPSCPacketsSent.Inc()
	}
	return true
}

func (s *IPSCServer) SetSendFilter(filter func(sourceKey string) bool) {
	s.sendFilter = filter
}

func (s *IPSCServer) pacePeer(peerID uint32) {
	const burstInterval = 30 * time.Millisecond

	s.mu.Lock()
	last := s.lastSend[peerID]
	now := time.Now()
	if !last.IsZero() {
		elapsed := now.Sub(last)
		if elapsed < burstInterval {
			s.mu.Unlock()
			time.Sleep(burstInterval - elapsed)
			s.mu.Lock()
		}
	}
	s.lastSend[peerID] = time.Now()
	s.mu.Unlock()
}
