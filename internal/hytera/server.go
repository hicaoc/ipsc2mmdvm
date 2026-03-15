package hytera

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/USA-RedDragon/dmrgo/dmr/enums"
	"github.com/USA-RedDragon/dmrgo/dmr/layer2"
	"github.com/USA-RedDragon/dmrgo/dmr/layer2/elements"
	"github.com/USA-RedDragon/dmrgo/dmr/layer2/pdu"
	"github.com/hicaoc/ipsc2mmdvm/internal/config"
	"github.com/hicaoc/ipsc2mmdvm/internal/dmr/bptc"
	"github.com/hicaoc/ipsc2mmdvm/internal/metrics"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

const (
	hyteraPrefixA = 0x5A

	packetTypeA          = 0x41
	packetTypeTerminator = 0x43

	slotTypePrivacyIndicator = 0x0000
	slotTypeVoiceLCHeader    = 0x1111
	slotTypeTerminator       = 0x2222
	slotTypeCSBK             = 0x3333
	slotTypeDataHeader       = 0x4444
	slotTypeRate12Data       = 0x5555
	slotTypeRate34Data       = 0x6666
	slotTypeDataC            = 0x7777
	slotTypeDataD            = 0x8888
	slotTypeDataE            = 0x9999
	slotTypeDataF            = 0xAAAA
	slotTypeDataAOrPrivacy   = 0xBBBB
	slotTypeDataB            = 0xCCCC
	slotTypeWakeupRequest    = 0xDDDD
	slotTypeSync             = 0xEEEE

	timeslotRaw1 = 0x1111
	timeslotRaw2 = 0x2222

	hyteraGroupCallHangTime   = 6 * time.Second
	hyteraPrivateCallHangTime = 4 * time.Second
	hyteraSyncWakeupTime      = 300 * time.Millisecond
	hyteraGatewayWakeupTime   = 150 * time.Millisecond
	hyteraWakeupRetries       = 3
	hyteraGatewayWakeRetries  = 2
	motoHeaderRepeats         = 3
	hyteraBurstInterval       = 20 * time.Millisecond
	hyteraSyncHeaderInterval  = 5 * time.Millisecond

	// ETSI TS 102 361-1 Annex B.3.2:
	// Full LC parity bytes are RS(12,9) encoded then XOR-masked by data type.
	fullLCParityMaskVoiceHeader byte = 0x96
	fullLCParityMaskTerminator  byte = 0x99
)

var errIgnoredPacket = errors.New("packet ignored")

var (
	rdacStep0Request  = []byte{0x7E, 0x04, 0x00, 0xFE, 0x20, 0x10, 0x00, 0x00, 0x00, 0x0C, 0x60, 0xE1}
	rdacStep0Response = []byte{0x7E, 0x04, 0x00, 0xFD}
	rdacStep1Request  = []byte{
		0x7E, 0x04, 0x00, 0x00, 0x20, 0x10, 0x00, 0x01,
		0x00, 0x18, 0x9B, 0x60, 0x02, 0x04, 0x00, 0x05,
		0x00, 0x64, 0x00, 0x00, 0x00, 0x01, 0xC4, 0x03,
	}
	rdacStep1Response = []byte{0x7E, 0x04, 0x00, 0x10}
	rdacStep2Response = []byte{0x7E, 0x04, 0x00, 0x00}
)

type slotState struct {
	inLastSeqSet bool
	inLastSeq    byte
	mmdvmSeq     uint8
	outSeq       uint8
	outStreamID  uint
	lastSent     time.Time
	streamID     uint32
	active       bool
	inSrc        uint
	inDst        uint
	inGroupCall  bool
	lastInbound  time.Time

	lastMotoWakeup time.Time
	lastMotoHeader time.Time
	motoSO         uint8
	motoSOSet      bool
}

type rdacSession struct {
	step           int
	lastRequestAt  time.Time
	lastDMRID      uint32
	identCompleted bool
	lastP2PAt      time.Time
}

type Server struct {
	cfg     *config.Config
	metrics *metrics.Metrics

	p2pConn  *net.UDPConn
	dmrConn  *net.UDPConn
	rdacConn *net.UDPConn

	stopped  atomic.Bool
	stopOnce sync.Once
	wg       sync.WaitGroup
	done     chan struct{}

	mu               sync.RWMutex
	repeaterP2PAddr  map[string]*net.UDPAddr // 记录中继 P2P NAT 地址（从 0x10 注册包和 ping 获取）
	repeaterDMRAddr  map[string]*net.UDPAddr
	repeaterRDACAddr map[string]*net.UDPAddr
	peerLastSeen     map[string]time.Time
	registered       bool
	nextStreamID     uint32
	slots            map[bool]*slotState
	routeSlots       map[string]map[bool]*slotState
	routeSendLocks   map[string]*sync.Mutex
	startupTemplates map[string][]byte
	lastSend         map[string]time.Time
	rdacSessions     map[string]*rdacSession

	packetHandler      func(packet proto.Packet, addr *net.UDPAddr)
	peerHandler        func(addr *net.UDPAddr, p2pPort, dmrPort, rdacPort int, dmrid uint32)
	peerOfflineHandler func(sourceKey string)
	sendFilter         func(sourceKey string) bool
	nrlResolver        func(sourceKey string) (NRLPeerConfig, bool)
	nrlCallHandler     func(call NRLCallEvent)
	analogAudioHandler func(event AnalogAudioEvent)
	nrlBridge          *nrlBridge
}

func NewServer(cfg *config.Config, m *metrics.Metrics) *Server {
	return &Server{
		cfg:              cfg,
		metrics:          m,
		done:             make(chan struct{}),
		nextStreamID:     1,
		repeaterP2PAddr:  map[string]*net.UDPAddr{},
		repeaterDMRAddr:  map[string]*net.UDPAddr{},
		repeaterRDACAddr: map[string]*net.UDPAddr{},
		peerLastSeen:     map[string]time.Time{},
		slots: map[bool]*slotState{
			false: {}, // TS1
			true:  {}, // TS2
		},
		routeSlots:       map[string]map[bool]*slotState{},
		routeSendLocks:   map[string]*sync.Mutex{},
		startupTemplates: map[string][]byte{},
		lastSend:         map[string]time.Time{},
		rdacSessions:     map[string]*rdacSession{},
	}
}

func (s *Server) SetPacketHandler(handler func(packet proto.Packet, addr *net.UDPAddr)) {
	s.packetHandler = handler
}

func (s *Server) SetPeerUpdateHandler(handler func(addr *net.UDPAddr, p2pPort, dmrPort, rdacPort int, dmrid uint32)) {
	s.peerHandler = handler
}

func (s *Server) SetPeerOfflineHandler(handler func(sourceKey string)) {
	s.peerOfflineHandler = handler
}

func (s *Server) SetSendFilter(filter func(sourceKey string) bool) {
	s.sendFilter = filter
}

func (s *Server) SetNRLConfigResolver(resolver func(sourceKey string) (NRLPeerConfig, bool)) {
	s.nrlResolver = resolver
}

func (s *Server) SetNRLCallHandler(handler func(call NRLCallEvent)) {
	s.nrlCallHandler = handler
}

func (s *Server) SetAnalogAudioHandler(handler func(event AnalogAudioEvent)) {
	s.analogAudioHandler = handler
}

func (s *Server) Start() error {
	p2pAddr := &net.UDPAddr{IP: net.IPv4zero, Port: int(s.cfg.Hytera.P2PPort)}
	p2p, err := net.ListenUDP("udp", p2pAddr)
	if err != nil {
		return fmt.Errorf("error starting Hytera P2P listener: %w", err)
	}
	s.p2pConn = p2p

	dmrAddr := &net.UDPAddr{IP: net.IPv4zero, Port: int(s.cfg.Hytera.DMRPort)}
	dmr, err := net.ListenUDP("udp", dmrAddr)
	if err != nil {
		_ = s.p2pConn.Close()
		return fmt.Errorf("error starting Hytera DMR listener: %w", err)
	}
	s.dmrConn = dmr

	if s.cfg.Hytera.EnableRDAC {
		rdacAddr := &net.UDPAddr{IP: net.IPv4zero, Port: int(s.cfg.Hytera.RDACPort)}
		rdac, err := net.ListenUDP("udp", rdacAddr)
		if err != nil {
			_ = s.p2pConn.Close()
			_ = s.dmrConn.Close()
			return fmt.Errorf("error starting Hytera RDAC listener: %w", err)
		}
		s.rdacConn = rdac
	}

	s.wg.Add(2)
	go s.p2pLoop()
	go s.dmrLoop()

	if s.nrlResolver != nil {
		// Use Hytera voice socket for NRL bridge egress so local source port
		// matches voice service expectations.
		bridge, err := newNRLBridge(s.cfg, s.dmrConn, s.nrlResolver, s.nrlCallHandler, s.analogAudioHandler)
		if err != nil {
			return fmt.Errorf("error starting Hytera NRL bridge: %w", err)
		}
		s.nrlBridge = bridge
	}

	if s.rdacConn != nil {
		s.wg.Add(1)
		go s.rdacLoop()
	}

	s.wg.Add(1)
	go s.peerExpiryLoop()

	return nil
}

func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		s.stopped.Store(true)
		close(s.done)
		if s.nrlBridge != nil {
			s.nrlBridge.Stop()
		}
		if s.p2pConn != nil {
			_ = s.p2pConn.Close()
		}
		if s.dmrConn != nil {
			_ = s.dmrConn.Close()
		}
		if s.rdacConn != nil {
			_ = s.rdacConn.Close()
		}
	})
	s.wg.Wait()
}

const hyteraPeerTimeout = 30 * time.Second

func (s *Server) peerExpiryLoop() {
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

func (s *Server) expireStalePeers() {
	now := time.Now()
	s.mu.Lock()
	var expired []string
	for key, lastSeen := range s.peerLastSeen {
		if now.Sub(lastSeen) > hyteraPeerTimeout {
			expired = append(expired, key)
			delete(s.peerLastSeen, key)
			delete(s.repeaterDMRAddr, key)
			delete(s.repeaterP2PAddr, key)
			delete(s.repeaterRDACAddr, key)
		}
	}
	if s.metrics != nil && len(expired) > 0 {
		s.metrics.IPSCPeersRegistered.Set(float64(len(s.repeaterDMRAddr)))
	}
	s.mu.Unlock()

	for _, key := range expired {
		sourceKey := "hytera:" + key
		slog.Info("Hytera peer offline (timeout)", "sourceKey", sourceKey)
		if s.peerOfflineHandler != nil {
			s.peerOfflineHandler(sourceKey)
		}
	}
}

func (s *Server) SendPacket(packet proto.Packet) {
	s.sendPacket(packet, false)
}

func (s *Server) SendPacketTo(packet proto.Packet, sourceKey string) bool {
	return s.sendPacketTo(packet, false, sourceKey)
}

func (s *Server) SendPacketFromMoto(packet proto.Packet) {
	s.sendPacket(packet, true)
}

func (s *Server) SendPacketFromMotoTo(packet proto.Packet, sourceKey string) bool {
	return s.sendPacketTo(packet, true, sourceKey)
}

func (s *Server) SendPacketToMany(packet proto.Packet, fromMoto bool, sourceKeys []string) map[string]bool {
	results := make(map[string]bool, len(sourceKeys))
	if s.stopped.Load() {
		return results
	}

	seen := make(map[string]struct{}, len(sourceKeys))
	unique := make([]string, 0, len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		if sourceKey == "" {
			continue
		}
		if _, ok := seen[sourceKey]; ok {
			continue
		}
		seen[sourceKey] = struct{}{}
		unique = append(unique, sourceKey)
	}

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, sourceKey := range unique {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			sent := s.sendPacketTo(packet, fromMoto, target)
			mu.Lock()
			results[target] = sent
			mu.Unlock()
		}(sourceKey)
	}
	wg.Wait()

	return results
}

func (s *Server) PrimeTargets(packet proto.Packet, fromMoto bool, sourceKeys []string) time.Duration {
	if s.stopped.Load() {
		return 0
	}
	var maxPause time.Duration
	seen := make(map[string]struct{}, len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		if sourceKey == "" {
			continue
		}
		if _, ok := seen[sourceKey]; ok {
			continue
		}
		seen[sourceKey] = struct{}{}

		st := s.outboundSlotState(packet.Slot, sourceKey)
		if st == nil {
			continue
		}
		lastStreamID := st.outStreamID
		newStream := packet.StreamID != 0 && packet.StreamID != lastStreamID
		idle := st.lastSent.IsZero() || time.Since(st.lastSent) > hangTimeForPacket(packet)
		acceptNewStream := false
		if fromMoto {
			startLike := isVoiceCallStartOrVoice(packet)
			if st.outStreamID == 0 && packet.StreamID != 0 && startLike {
				acceptNewStream = true
			}
			if packet.FrameType == 2 && packet.DTypeOrVSeq == 1 && newStream {
				acceptNewStream = true
			}
			if idle && newStream && startLike {
				acceptNewStream = true
			}
		} else {
			startLike := isVoiceCallStartOrVoice(packet)
			if st.outStreamID == 0 && packet.StreamID != 0 {
				acceptNewStream = true
			}
			if packet.FrameType == 2 && packet.DTypeOrVSeq == 1 && newStream {
				acceptNewStream = true
			}
			if idle && newStream && startLike {
				acceptNewStream = true
			}
		}
		if acceptNewStream && packet.StreamID != 0 {
			if fromMoto {
				// BM-style sequence normalization: the first transmitted burst of a
				// new moto->hytera stream should start at sequence 0.
				st.outSeq = ^uint8(0)
			} else {
				st.outSeq = 0
			}
			st.outStreamID = packet.StreamID
		}
		if !shouldWakeup(packet, lastStreamID, idle, fromMoto) || !idle {
			continue
		}

		repeats := hyteraGatewayWakeRetries
		pause := hyteraGatewayWakeupTime
		if fromMoto {
			repeats = hyteraWakeupRetries
			pause = hyteraSyncWakeupTime
			st.lastMotoWakeup = time.Now()
			st.lastMotoHeader = time.Time{}
		}
		s.sendWakeups(packet, repeats, sourceKey, fromMoto)
		if pause > maxPause {
			maxPause = pause
		}
	}
	return maxPause
}

func (s *Server) sendPacket(packet proto.Packet, fromMoto bool) {
	s.sendPacketTo(packet, fromMoto, "")
}

func (s *Server) sendPacketTo(packet proto.Packet, fromMoto bool, sourceKey string) bool {
	if s.stopped.Load() {
		return false
	}
	if sourceKey != "" {
		lock := s.routeSendLock(sourceKey, packet.Slot)
		lock.Lock()
		defer lock.Unlock()
	}

	st := s.outboundSlotState(packet.Slot, sourceKey)
	lastStreamID := uint(0)
	newStream := false
	idle := true
	acceptNewStream := false
	acceptReason := ""
	callStart := false
	if st != nil {
		lastStreamID = st.outStreamID
		newStream = packet.StreamID != 0 && packet.StreamID != lastStreamID
		idle = st.lastSent.IsZero() || time.Since(st.lastSent) > hangTimeForPacket(packet)
		if fromMoto {
			startLike := isVoiceCallStartOrVoice(packet)
			if st.outStreamID == 0 && packet.StreamID != 0 && startLike {
				acceptNewStream = true
				acceptReason = "empty-slot"
			}
			if packet.FrameType == 2 && packet.DTypeOrVSeq == 1 && newStream {
				acceptNewStream = true
				acceptReason = "explicit-lc-header"
			}
			if idle && newStream && startLike {
				acceptNewStream = true
				acceptReason = "idle-new-stream"
			}
		} else {
			startLike := isVoiceCallStartOrVoice(packet)
			if st.outStreamID == 0 && packet.StreamID != 0 {
				acceptNewStream = true
				acceptReason = "empty-slot"
			}
			// Explicit LC header is always a trusted call boundary.
			if packet.FrameType == 2 && packet.DTypeOrVSeq == 1 && newStream {
				acceptNewStream = true
				acceptReason = "explicit-lc-header"
			}
			// Without LC header, only accept a stream switch when idle.
			if idle && newStream && startLike {
				acceptNewStream = true
				acceptReason = "idle-new-stream"
			}
		}
		if acceptNewStream && !idle {
			slog.Debug("hytera stream switch accepted while not idle",
				"fromMoto", fromMoto,
				"reason", acceptReason,
				"slot", packet.Slot,
				"src", packet.Src,
				"dst", packet.Dst,
				"groupCall", packet.GroupCall,
				"ft", packet.FrameType,
				"dt", packet.DTypeOrVSeq,
				"newStreamID", packet.StreamID,
				"prevStreamID", lastStreamID,
				"sinceLastSentMs", time.Since(st.lastSent).Milliseconds(),
				"sourceKey", sourceKey)
		}
		if acceptNewStream && packet.StreamID != 0 {
			st.outSeq = 0
			st.outStreamID = packet.StreamID
			if fromMoto {
				st.motoSO = 0
				st.motoSOSet = false
			}
		}

		callStart = shouldWakeup(packet, lastStreamID, idle, fromMoto)
		if callStart && !idle {
			slog.Debug("hytera wakeup trigger while not idle",
				"fromMoto", fromMoto,
				"slot", packet.Slot,
				"src", packet.Src,
				"dst", packet.Dst,
				"groupCall", packet.GroupCall,
				"ft", packet.FrameType,
				"dt", packet.DTypeOrVSeq,
				"stream", packet.StreamID,
				"prevStream", lastStreamID,
				"sinceLastSentMs", time.Since(st.lastSent).Milliseconds(),
				"sourceKey", sourceKey)
		}
		if callStart {
			if idle {
				repeats := hyteraGatewayWakeRetries
				pause := hyteraGatewayWakeupTime
				if fromMoto {
					repeats = hyteraWakeupRetries
					pause = hyteraSyncWakeupTime
				}
				s.sendWakeups(packet, repeats, sourceKey, fromMoto)
				if fromMoto {
					st.lastMotoWakeup = time.Now()
					st.lastMotoHeader = time.Time{}
					slog.Debug("moto->hytera wakeup sent",
						"src", packet.Src,
						"dst", packet.Dst,
						"slot", packet.Slot,
						"repeats", repeats,
						"pauseMs", pause.Milliseconds())
				} else {
					slog.Debug("gateway->hytera wakeup sent",
						"src", packet.Src,
						"dst", packet.Dst,
						"slot", packet.Slot,
						"repeats", repeats,
						"pauseMs", pause.Milliseconds())
				}
				time.Sleep(pause)
			} else if fromMoto && acceptNewStream && newStream {
				// Moto path can start a fresh call shortly after a previous one on
				// the same slot. A single wakeup plus the same short pause is more
				// reliable here than multiple wakeups on this repeater.
				s.sendWakeups(packet, 1, sourceKey, fromMoto)

				st.lastMotoWakeup = time.Now()
				st.lastMotoHeader = time.Time{}
				slog.Debug("moto->hytera wakeup sent",
					"src", packet.Src,
					"dst", packet.Dst,
					"slot", packet.Slot,
					"repeats", 1,
					"pauseMs", hyteraSyncWakeupTime.Milliseconds())

				time.Sleep(hyteraSyncWakeupTime)
			}
		}
	}

	if fromMoto {
		outSeq := uint8(0)
		if st != nil {
			outSeq = st.outSeq
		}
		slog.Debug("moto->hytera",
			"src", packet.Src,
			"dst", packet.Dst,
			"slot", packet.Slot,
			"ft", packet.FrameType,
			"dt", packet.DTypeOrVSeq,
			"stream", packet.StreamID,
			"prevStream", lastStreamID,
			"newStream", newStream,
			"accept", acceptNewStream,
			"idle", idle,
			"wake", callStart,
			"outSeq", outSeq,
			"dmr", dmrPrefix(packet.DMRData[:], 8))
	}

	repeats := 1
	if fromMoto && packet.FrameType == 2 && packet.DTypeOrVSeq == 1 {
		repeats = motoHeaderRepeats
	}
	motoCC := uint8(0)
	motoSO := uint8(0x20)
	motoSOSource := ""
	if fromMoto {
		cc, ccSource := packetColorCodeWithSource(packet, cfgColorCode(s.cfg))
		cc, ccSource = normalizeMotoColorCode(cc, ccSource, cfgColorCode(s.cfg))
		motoCC = cc
		motoSO, motoSOSource = resolveMotoServiceOptions(packet, st)
		slog.Debug("moto->hytera color code selected",
			"cc", cc,
			"source", ccSource,
			"src", packet.Src,
			"dst", packet.Dst,
			"slot", packet.Slot,
			"ft", packet.FrameType,
			"dt", packet.DTypeOrVSeq,
			"stream", packet.StreamID)
		slog.Debug("moto->hytera service options selected",
			"so", fmt.Sprintf("0x%02X", motoSO),
			"source", motoSOSource,
			"src", packet.Src,
			"dst", packet.Dst,
			"slot", packet.Slot,
			"ft", packet.FrameType,
			"dt", packet.DTypeOrVSeq,
			"stream", packet.StreamID)
	}
	outPacket := packet
	if fromMoto {
		outPacket = standardizeMotoLCPacketWithSO(packet, motoCC, motoSO)
		outPacket = patchMotoVoiceEmbeddedControl(outPacket, motoCC)
	}
	if fromMoto && acceptNewStream && packet.FrameType != 2 && isVoiceCallStartOrVoice(packet) {
		if !s.sendSynthesizedMotoHeader(packet, motoCC, motoSO, st, sourceKey) {
			return false
		}
	}
	for i := 0; i < repeats; i++ {
		if packet.FrameType == 2 && packet.DTypeOrVSeq == 1 {
			if syncPacket, ok := s.buildStartupPacketForRoute(slotTypeSync, packet, fromMoto, motoCC); ok {
				if !s.sendEncodedToHyteraWithInterval(syncPacket, st, sourceKey, hyteraBurstInterval) {
					return false
				}
			}
		}
		data, err := s.encodeFromMMDVMWithState(outPacket, st)
		if err != nil {
			slog.Debug("failed encoding Hytera packet", "error", err)
			return false
		}
		if fromMoto {
			setHyteraHeaderColorCode(data, motoCC)
		}
		interval := hyteraBurstInterval
		if packet.FrameType == 2 && packet.DTypeOrVSeq == 1 {
			interval = hyteraSyncHeaderInterval
		}
		if !s.sendEncodedToHyteraWithInterval(data, st, sourceKey, interval) {
			return false
		}
	}

	if fromMoto && packet.FrameType == 2 && packet.DTypeOrVSeq == 1 {
		if st != nil {
			st.lastMotoHeader = time.Now()
			wakeupGap := int64(-1)
			if !st.lastMotoWakeup.IsZero() {
				wakeupGap = st.lastMotoHeader.Sub(st.lastMotoWakeup).Milliseconds()
			}
			slog.Debug("moto->hytera header sent",
				"src", packet.Src,
				"dst", packet.Dst,
				"slot", packet.Slot,
				"stream", packet.StreamID,
				"afterWakeupMs", wakeupGap,
				"guardMs", int64(0))
		}
	} else if fromMoto && (packet.FrameType == 1 || packet.FrameType == 0) && st != nil {
		headerGap := int64(-1)
		if !st.lastMotoHeader.IsZero() {
			headerGap = time.Since(st.lastMotoHeader).Milliseconds()
		}
		slog.Debug("moto->hytera voice sent",
			"src", packet.Src,
			"dst", packet.Dst,
			"slot", packet.Slot,
			"stream", packet.StreamID,
			"frameType", packet.FrameType,
			"dtypeOrVSeq", packet.DTypeOrVSeq,
			"afterHeaderMs", headerGap)
	}
	return true
}

func standardizeMotoLCPacket(pkt proto.Packet, colorCode uint8) proto.Packet {
	return standardizeMotoLCPacketWithSO(pkt, colorCode, 0x20)
}

func standardizeMotoLCPacketWithSO(pkt proto.Packet, colorCode uint8, so uint8) proto.Packet {
	out := pkt
	cc := colorCode & 0x0F

	switch {
	case pkt.FrameType == 2 && pkt.DTypeOrVSeq == 1:
		lc := buildStandardLCBytesForDataType(pkt.Src, pkt.Dst, pkt.GroupCall, so, elements.DataTypeVoiceLCHeader)
		out.DMRData = bptc.BuildLCDataBurst(lc, uint8(elements.DataTypeVoiceLCHeader), cc)
	case pkt.FrameType == 2 && pkt.DTypeOrVSeq == 2:
		lc := buildStandardLCBytesForDataType(pkt.Src, pkt.Dst, pkt.GroupCall, so, elements.DataTypeTerminatorWithLC)
		out.DMRData = bptc.BuildLCDataBurst(lc, uint8(elements.DataTypeTerminatorWithLC), cc)
	}
	return out
}

func (s *Server) sendSynthesizedMotoHeader(pkt proto.Packet, motoCC uint8, so uint8, st *slotState, sourceKey string) bool {
	header := pkt
	header.FrameType = 2
	header.DTypeOrVSeq = 1
	header.DMRData = bptc.BuildLCDataBurst(
		buildStandardLCBytesForDataType(pkt.Src, pkt.Dst, pkt.GroupCall, so, elements.DataTypeVoiceLCHeader),
		uint8(elements.DataTypeVoiceLCHeader),
		motoCC&0x0F,
	)

	for i := 0; i < motoHeaderRepeats; i++ {
		if syncPacket, ok := s.buildStartupPacketForRoute(slotTypeSync, header, true, motoCC); ok {
			if !s.sendEncodedToHyteraWithInterval(syncPacket, st, sourceKey, hyteraBurstInterval) {
				return false
			}
		}
		data, err := s.encodeFromMMDVMWithState(header, st)
		if err != nil {
			slog.Debug("failed encoding synthesized Hytera header", "error", err)
			return false
		}
		setHyteraHeaderColorCode(data, motoCC)
		if !s.sendEncodedToHyteraWithInterval(data, st, sourceKey, hyteraSyncHeaderInterval) {
			return false
		}
	}

	if st != nil {
		st.lastMotoHeader = time.Now()
	}
	slog.Debug("moto->hytera synthesized lc header sent",
		"src", pkt.Src,
		"dst", pkt.Dst,
		"slot", pkt.Slot,
		"stream", pkt.StreamID,
		"so", fmt.Sprintf("0x%02X", so),
		"repeats", motoHeaderRepeats)
	return true
}

func buildStandardLCBytes(src, dst uint, groupCall bool) [12]byte {
	return buildStandardLCBytesForDataType(src, dst, groupCall, 0x20, elements.DataTypeVoiceLCHeader)
}

func buildStandardLCBytesWithSO(src, dst uint, groupCall bool, so uint8) [12]byte {
	return buildStandardLCBytesForDataType(src, dst, groupCall, so, elements.DataTypeVoiceLCHeader)
}

func buildStandardLCBytesForDataType(src, dst uint, groupCall bool, so uint8, dataType elements.DataType) [12]byte {
	var lc [12]byte
	flco := enums.FLCOUnitToUnitVoiceChannelUser
	if src > math.MaxInt || dst > math.MaxInt {
		slog.Error("Hytera LC address out of range", "src", src, "dst", dst)
		return lc
	}
	if groupCall {
		flco = enums.FLCOGroupVoiceChannelUser
	}
	lc[0] = byte(flco) & 0x3F // PF=0, R=0
	lc[1] = byte(enums.StandardizedFID)
	lc[2] = so
	lc[3] = byte(dst >> 16)
	lc[4] = byte(dst >> 8)
	lc[5] = byte(dst)
	lc[6] = byte(src >> 16)
	lc[7] = byte(src >> 8)
	lc[8] = byte(src)
	parity := reedSolomon129Parity(lc[:9])
	mask := fullLCParityMaskForDataType(dataType)
	lc[9] = parity[0] ^ mask
	lc[10] = parity[1] ^ mask
	lc[11] = parity[2] ^ mask
	return lc
}

func fullLCParityMaskForDataType(dataType elements.DataType) byte {
	switch dataType {
	case elements.DataTypeVoiceLCHeader:
		return fullLCParityMaskVoiceHeader
	case elements.DataTypeTerminatorWithLC:
		return fullLCParityMaskTerminator
	default:
		return 0x00
	}
}

// reedSolomon129Parity computes RS(12,9) parity bytes over 9 data bytes.
// Coefficients follow ETSI TS 102 361-1 Annex B.3.2 for Full LC.
func reedSolomon129Parity(data []byte) [3]byte {
	var parity [3]byte
	if len(data) != 9 {
		return parity
	}
	for i := 0; i < 9; i++ {
		feedback := data[i] ^ parity[0]
		parity[0] = parity[1] ^ gf256Mul(feedback, 0x0E)
		parity[1] = parity[2] ^ gf256Mul(feedback, 0x38)
		parity[2] = gf256Mul(feedback, 0x40)
	}
	return parity
}

func gf256Mul(a, b byte) byte {
	var p byte
	aa := a
	bb := b
	for i := 0; i < 8; i++ {
		if (bb & 1) != 0 {
			p ^= aa
		}
		hi := (aa & 0x80) != 0
		aa <<= 1
		if hi {
			aa ^= 0x1D
		}
		bb >>= 1
	}
	return p
}

func logMotoHyteraHeaderLC(src, dst uint, groupCall bool, lc [12]byte) {
	callType := "private"
	if groupCall {
		callType = "group"
	}
	slog.Debug("moto->hytera full lc",
		"callType", callType,
		"src", src,
		"dst", dst,
		"lc", fmt.Sprintf("% X", lc[:]))
}

func resolveMotoServiceOptions(pkt proto.Packet, st *slotState) (uint8, string) {
	// Prefer SO from inbound LC header/terminator, then keep per-slot cached value.
	if pkt.FrameType == 2 && (pkt.DTypeOrVSeq == 1 || pkt.DTypeOrVSeq == 2) {
		if lc, ok := bptc.DecodeLCFromBurst(pkt.DMRData); ok {
			so := lc[2]
			if st != nil {
				st.motoSO = so
				st.motoSOSet = true
			}
			return so, "packet-lc"
		}
	}
	if st != nil && st.motoSOSet {
		return st.motoSO, "slot-cache"
	}
	// BM-normalized baseline is usually 0x00.
	return 0x00, "bm-default"
}

func (s *Server) routeSendLock(sourceKey string, slot bool) *sync.Mutex {
	key := sourceKey
	if slot {
		key += ":ts2"
	} else {
		key += ":ts1"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock := s.routeSendLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		s.routeSendLocks[key] = lock
	}
	return lock
}

func hangTimeForPacket(pkt proto.Packet) time.Duration {
	if pkt.GroupCall {
		return hyteraGroupCallHangTime
	}
	return hyteraPrivateCallHangTime
}

func shouldWakeup(pkt proto.Packet, lastStreamID uint, idle bool, fromMoto bool) bool {
	_ = fromMoto
	newStream := pkt.StreamID != 0 && pkt.StreamID != lastStreamID
	// Standard start marker.
	if pkt.DTypeOrVSeq == 1 {
		return newStream
	}
	// Fallback for gateways that miss LC Header: only trigger when idle and
	// a new stream starts, to avoid disturbing in-flight calls.
	if idle && newStream && isVoiceCallStartOrVoice(pkt) {
		return true
	}
	return false
}

func isVoiceCallStartOrVoice(pkt proto.Packet) bool {
	if pkt.FrameType == 2 && pkt.DTypeOrVSeq == 1 {
		return true
	}
	return pkt.FrameType == 0 || pkt.FrameType == 1
}

func dmrPrefix(data []byte, n int) string {
	if n > len(data) {
		n = len(data)
	}
	return fmt.Sprintf("% X", data[:n])
}

func hyteraSlotTypeName(slotType uint16) string {
	switch slotType {
	case slotTypePrivacyIndicator:
		return "0000"
	case slotTypeVoiceLCHeader:
		return "1111"
	case slotTypeTerminator:
		return "2222"
	case slotTypeCSBK:
		return "3333"
	case slotTypeDataHeader:
		return "4444"
	case slotTypeRate12Data:
		return "5555"
	case slotTypeRate34Data:
		return "6666"
	case slotTypeDataC:
		return "7777"
	case slotTypeDataD:
		return "8888"
	case slotTypeDataE:
		return "9999"
	case slotTypeDataF:
		return "AAAA"
	case slotTypeDataAOrPrivacy:
		return "BBBB"
	case slotTypeDataB:
		return "CCCC"
	case slotTypeWakeupRequest:
		return "DDDD"
	case slotTypeSync:
		return "EEEE"
	default:
		return fmt.Sprintf("%04X", slotType)
	}
}

func logHyteraOutboundPacket(addr *net.UDPAddr, sourceKey string, data []byte) {
	if len(data) < 72 {
		slog.Debug("hytera outbound packet",
			"peer", addr,
			"sourceKey", sourceKey,
			"length", len(data))
		return
	}

	packetType := data[8]
	slotMarker := data[12]
	slotType := binary.BigEndian.Uint16(data[18:20])
	callType := data[62]
	dst := decodeID24LE(data[64:68])
	src := decodeID24LE(data[68:72])

	slog.Debug("hytera outbound packet",
		"peer", addr,
		"sourceKey", sourceKey,
		"packetType", fmt.Sprintf("0x%02X", packetType),
		"slotMarker", fmt.Sprintf("0x%02X", slotMarker),
		"slotType", hyteraSlotTypeName(slotType),
		"callType", fmt.Sprintf("0x%02X", callType),
		"src", src,
		"dst", dst,
		"payload", dmrPrefix(data[24:], 16))
}

func (s *Server) sendEncodedToHytera(data []byte, st *slotState, sourceKey string) bool {
	return s.sendEncodedToHyteraWithInterval(data, st, sourceKey, hyteraBurstInterval)
}

func (s *Server) sendEncodedToHyteraWithInterval(data []byte, st *slotState, sourceKey string, interval time.Duration) bool {
	addrs := s.targetDMRAddrs(sourceKey)
	if len(addrs) == 0 {
		return false
	}
	for _, addr := range addrs {
		if addr == nil {
			continue
		}
		s.paceAddr(addr, interval)
		if _, err := s.dmrConn.WriteToUDP(data, addr); err != nil {
			slog.Warn("failed sending Hytera packet", "peer", addr, "error", err)
			if s.metrics != nil {
				s.metrics.IPSCUDPErrors.WithLabelValues("write").Inc()
			}
			continue
		}
		logHyteraOutboundPacket(addr, sourceKey, data)
		if s.metrics != nil {
			s.metrics.IPSCPacketsSent.Inc()
		}
		if st != nil {
			st.lastSent = time.Now()
		}
	}
	return true
}

func (s *Server) sendWakeups(pkt proto.Packet, repeats int, sourceKey string, fromMoto bool) {
	if repeats < 1 {
		repeats = 1
	}
	addrs := s.targetDMRAddrs(sourceKey)
	if len(addrs) == 0 {
		return
	}

	for _, addr := range addrs {
		if addr == nil {
			continue
		}
		if s.sendFilter != nil && !s.sendFilter("hytera:"+addr.IP.String()) {
			continue
		}
		slots := []bool{pkt.Slot}
		if fromMoto {
			slots = []bool{false, true}
		}
		for i := 0; i < repeats; i++ {
			for _, slot := range slots {
				wakeupPkt := pkt
				wakeupPkt.Slot = slot
				motoCC := uint8(0)
				if fromMoto {
					motoCC = packetColorCode(wakeupPkt, cfgColorCode(s.cfg))
				}
				packet, ok := s.buildStartupPacketForRoute(slotTypeWakeupRequest, wakeupPkt, fromMoto, motoCC)
				if !ok {
					continue
				}
				s.paceAddr(addr, hyteraBurstInterval)
				if _, err := s.dmrConn.WriteToUDP(packet, addr); err == nil {
					logHyteraOutboundPacket(addr, sourceKey, packet)
				}
			}
		}
	}
}

func (s *Server) outboundSlotState(slot bool, sourceKey string) *slotState {
	if sourceKey == "" {
		return s.slots[slot]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	perRoute := s.routeSlots[sourceKey]
	if perRoute == nil {
		perRoute = map[bool]*slotState{
			false: {},
			true:  {},
		}
		s.routeSlots[sourceKey] = perRoute
	}
	st := perRoute[slot]
	if st == nil {
		st = &slotState{}
		perRoute[slot] = st
	}
	return st
}

func (s *Server) targetDMRAddrs(sourceKey string) []*net.UDPAddr {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if sourceKey != "" {
		key := strings.TrimPrefix(sourceKey, "hytera:")
		if addr := s.repeaterDMRAddr[key]; addr != nil {
			if s.sendFilter != nil && !s.sendFilter(sourceKey) {
				return nil
			}
			return []*net.UDPAddr{cloneUDPAddr(addr)}
		}
		return nil
	}

	addrs := make([]*net.UDPAddr, 0, len(s.repeaterDMRAddr))
	for key, a := range s.repeaterDMRAddr {
		if s.sendFilter != nil && !s.sendFilter("hytera:"+key) {
			continue
		}
		addrs = append(addrs, cloneUDPAddr(a))
	}
	return addrs
}

func (s *Server) p2pLoop() {
	defer s.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, addr, err := s.p2pConn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("error reading Hytera P2P packet", "error", err)
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		if s.nrlBridge != nil && s.nrlBridge.HandleHyteraPacket(addr, data) {
			continue
		}
		s.handleP2P(data, addr)
	}
}

func (s *Server) dmrLoop() {
	defer s.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, addr, err := s.dmrConn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("error reading Hytera DMR packet", "error", err)
			if s.metrics != nil {
				s.metrics.IPSCUDPErrors.WithLabelValues("read").Inc()
			}
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		if s.nrlBridge != nil && s.nrlBridge.HandleHyteraPacket(addr, data) {
			continue
		}

		key := addr.IP.String()
		validDMRHeader := len(data) >= 20 && hasHyteraDMRHeader(data)

		s.mu.Lock()
		if validDMRHeader {
			s.repeaterDMRAddr[key] = &net.UDPAddr{IP: append([]byte(nil), addr.IP...), Port: addr.Port}
		} else {
			slog.Debug("ignoring DMR address refresh from non-hytera packet",
				"peer", addr.String(),
				"length", n,
				"hex", shortHex(data, 32))
		}
		s.peerLastSeen[key] = time.Now()
		p2pPort := 0
		if p2pAddr := s.repeaterP2PAddr[key]; p2pAddr != nil {
			p2pPort = p2pAddr.Port
		}
		rdacPort := 0
		if rdacAddr := s.repeaterRDACAddr[key]; rdacAddr != nil {
			rdacPort = rdacAddr.Port
		}
		s.mu.Unlock()
		if s.peerHandler != nil {
			dmrid, ok := parseHyteraDMRAppDMRID(data)
			if ok {
				s.peerHandler(cloneUDPAddr(addr), p2pPort, addr.Port, rdacPort, dmrid)
			} else {
				s.peerHandler(cloneUDPAddr(addr), p2pPort, addr.Port, rdacPort, 0)
			}
		}

		pkt, err := s.decodeToMMDVM(data)
		if err != nil {
			if errors.Is(err, errIgnoredPacket) {
				continue
			}
			slog.Debug("ignoring Hytera DMR packet", "error", err, "length", n)
			continue
		}

		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("group_voice").Inc()
		}
		if s.packetHandler != nil {
			s.packetHandler(pkt, addr)
		}
	}
}

func (s *Server) rdacLoop() {
	defer s.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, addr, err := s.rdacConn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Debug("error reading Hytera RDAC packet", "protocol", "hytera", "channel", "rdac", "error", err)
			continue
		}
		if n == 0 {
			continue
		}

		// 从接收到的RDAC包中提取对方的源端口
		s.mu.Lock()
		key := addr.IP.String()
		s.repeaterRDACAddr[key] = &net.UDPAddr{IP: append([]byte(nil), addr.IP...), Port: addr.Port}
		p2pPort := 0
		if p2pAddr := s.repeaterP2PAddr[key]; p2pAddr != nil {
			p2pPort = p2pAddr.Port
		}
		dmrPort := 0
		if dmrAddr := s.repeaterDMRAddr[key]; dmrAddr != nil {
			dmrPort = dmrAddr.Port
		}
		s.mu.Unlock()

		s.handleRDACHandshake(addr, buf[:n])

		dmrid, ok, source, reason := parseRDACDMRIDDetailed(buf[:n])
		slog.Debug("received Hytera RDAC packet",
			"protocol", "hytera",
			"channel", "rdac",
			"peer", addr.String(),
			"kind", classifyRDACPacket(buf[:n]),
			"hex", shortHex(buf[:n], 96),
			"length", n,
			"dmrid", dmrid,
			"dmridSource", source,
			"dmridReason", reason)
		if ok {
			slog.Info("Hytera RDAC DMRID parsed",
				"peer", addr.String(),
				"dmrid", dmrid,
				"source", source,
				"length", n)
		}

		if s.peerHandler != nil {
			if ok {
				s.peerHandler(cloneUDPAddr(addr), p2pPort, dmrPort, addr.Port, dmrid)
			} else {
				s.peerHandler(cloneUDPAddr(addr), p2pPort, dmrPort, addr.Port, 0)
			}
		}

		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("data").Inc()
		}
	}
}

func (s *Server) handleRDACHandshake(addr *net.UDPAddr, data []byte) {
	if addr == nil || s.rdacConn == nil {
		return
	}
	key := addr.IP.String()

	// Repeater sends 0x00 when no data is available. Treat that as readiness and
	// start the RDAC identification sequence against the learned UDP source port.
	if len(data) == 1 && data[0] == 0x00 {
		s.startRDACIdentification(addr, "rdac-heartbeat")
		return
	}

	session := s.rdacSession(key)
	switch session.step {
	case 1:
		if hasPrefix(data, rdacStep0Response) {
			session.step = 2
			session.lastRequestAt = time.Now()
			if _, err := s.rdacConn.WriteToUDP(rdacStep1Request, addr); err != nil {
				slog.Warn("failed sending Hytera RDAC step1 request", "peer", addr.String(), "error", err)
				return
			}
			slog.Debug("Hytera RDAC step0 response accepted", "peer", addr.String(), "rdacPort", addr.Port)
		}
	case 2:
		if hasPrefix(data, rdacStep1Response) {
			session.step = 3
			slog.Debug("Hytera RDAC step1 response accepted", "peer", addr.String(), "rdacPort", addr.Port)
		}
	case 3:
		if hasPrefix(data, rdacStep2Response) {
			if dmrid, ok, _, _ := parseRDACDMRIDDetailed(data); ok {
				session.lastDMRID = dmrid
				session.identCompleted = true
				slog.Info("Hytera RDAC identification completed", "peer", addr.String(), "rdacPort", addr.Port, "dmrid", dmrid)
				s.emitHyteraPeerUpdate(addr, dmrid)
			} else {
				slog.Debug("Hytera RDAC step2 response received without DMRID", "peer", addr.String(), "rdacPort", addr.Port, "hex", shortHex(data, 96))
			}
			session.step = 0
		}
	}
}

func (s *Server) startRDACIdentification(addr *net.UDPAddr, trigger string) {
	if addr == nil || s.rdacConn == nil {
		return
	}
	session := s.rdacSession(addr.IP.String())
	if session.lastP2PAt.IsZero() || time.Since(session.lastP2PAt) > 15*time.Second {
		slog.Debug("skipping Hytera RDAC identification without recent P2P association",
			"peer", addr.String(),
			"trigger", trigger,
			"lastP2PAt", session.lastP2PAt)
		return
	}
	if session.identCompleted && session.lastDMRID != 0 {
		return
	}
	if time.Since(session.lastRequestAt) < 2*time.Second {
		return
	}
	session.step = 1
	session.lastRequestAt = time.Now()
	if _, err := s.rdacConn.WriteToUDP(rdacStep0Request, addr); err != nil {
		slog.Warn("failed sending Hytera RDAC step0 request", "peer", addr.String(), "trigger", trigger, "error", err)
		return
	}
	slog.Info("Hytera RDAC identification started", "peer", addr.String(), "rdacPort", addr.Port, "trigger", trigger)
}

func (s *Server) rdacSession(key string) *rdacSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.rdacSessions[key]
	if session == nil {
		session = &rdacSession{}
		s.rdacSessions[key] = session
	}
	return session
}

func (s *Server) emitHyteraPeerUpdate(addr *net.UDPAddr, dmrid uint32) {
	if addr == nil || s.peerHandler == nil {
		return
	}
	key := addr.IP.String()

	s.mu.RLock()
	p2pPort := 0
	if p2pAddr := s.repeaterP2PAddr[key]; p2pAddr != nil {
		p2pPort = p2pAddr.Port
	}
	dmrPort := 0
	if dmrAddr := s.repeaterDMRAddr[key]; dmrAddr != nil {
		dmrPort = dmrAddr.Port
	}
	rdacPort := addr.Port
	if rdacAddr := s.repeaterRDACAddr[key]; rdacAddr != nil && rdacAddr.Port != 0 {
		rdacPort = rdacAddr.Port
	}
	s.mu.RUnlock()

	s.peerHandler(cloneUDPAddr(addr), p2pPort, dmrPort, rdacPort, dmrid)
}

func (s *Server) markP2PAssociation(addr *net.UDPAddr) {
	if addr == nil {
		return
	}
	key := addr.IP.String()
	s.mu.Lock()
	session := s.rdacSessions[key]
	if session == nil {
		session = &rdacSession{}
		s.rdacSessions[key] = session
	}
	session.lastP2PAt = time.Now()
	s.mu.Unlock()
}

func (s *Server) handleP2P(data []byte, addr *net.UDPAddr) {
	// ping 包不需要 21 字节，先检查（Python: data[4:9] == PING_PREFIX）
	if len(data) >= 15 && bytesEqual(data[4:9], []byte{0x0A, 0x00, 0x00, 0x00, 0x14}) {
		// 记录中继 P2P 的 NAT 地址
		s.mu.Lock()
		key := addr.IP.String()
		s.repeaterP2PAddr[key] = cloneUDPAddr(addr)
		s.peerLastSeen[key] = time.Now()
		s.mu.Unlock()
		s.markP2PAssociation(addr)
		resp := append([]byte(nil), data...)
		resp[12] = 0xFF
		resp[14] = 0x01
		_, _ = s.p2pConn.WriteToUDP(resp, addr)
		return
	}

	if len(data) < 21 {
		return
	}

	if bytesEqual(data[:3], []byte{0x50, 0x32, 0x50}) {
		s.markP2PAssociation(addr)
		s.handleP2PCommand(data, addr)
	}
}

func (s *Server) handleP2PCommand(data []byte, addr *net.UDPAddr) {
	packetType := data[20]
	switch packetType {
	case 0x10: // registration
		resp := append([]byte(nil), data...)
		resp[3] = 0x50
		resp[4]++
		resp[13] = 0x01
		resp[14] = 0x01
		resp[15] = 0x5A
		resp = append(resp, 0x01)
		_, _ = s.p2pConn.WriteToUDP(resp, addr)

		s.mu.Lock()
		s.registered = true
		// 0x10 从 P2P 端口发出，记录 NAT 后的 P2P 地址
		key := addr.IP.String()
		s.repeaterP2PAddr[key] = cloneUDPAddr(addr)
		s.repeaterDMRAddr[key] = &net.UDPAddr{IP: append([]byte(nil), addr.IP...), Port: addr.Port}
		s.peerLastSeen[key] = time.Now()
		session := s.rdacSessions[key]
		if session == nil {
			session = &rdacSession{}
			s.rdacSessions[key] = session
		}
		session.lastP2PAt = time.Now()
		s.mu.Unlock()
		if s.peerHandler != nil {
			s.peerHandler(cloneUDPAddr(addr), addr.Port, addr.Port, 0, 0)
		}
		if s.metrics != nil {
			s.metrics.IPSCPeersRegistered.Set(float64(len(s.repeaterDMRAddr)))
		}
	case 0x11, 0x12: // DMR or RDAC startup
		resp := append([]byte(nil), data...)
		resp[4]++
		resp[13] = 0x01
		resp = append(resp, 0x01)
		// 响应发回中继的 P2P 地址（NAT 场景用 0x10/ping 记录的地址，否则用 IP+P2PPort）
		s.mu.RLock()
		p2pAddr := s.repeaterP2PAddr[addr.IP.String()]
		s.mu.RUnlock()
		responseAddr := cloneUDPAddr(p2pAddr)
		if responseAddr == nil {
			responseAddr = &net.UDPAddr{IP: append([]byte(nil), addr.IP...), Port: int(s.cfg.Hytera.P2PPort)}
		}

		// 如果还没注册过这个对方，现在注册
		s.mu.Lock()
		key := addr.IP.String()
		session := s.rdacSessions[key]
		if session == nil {
			session = &rdacSession{}
			s.rdacSessions[key] = session
		}
		session.lastP2PAt = time.Now()
		s.peerLastSeen[key] = time.Now()
		if packetType == 0x11 {
			s.repeaterDMRAddr[key] = &net.UDPAddr{IP: append([]byte(nil), addr.IP...), Port: addr.Port}
		}
		if packetType == 0x12 {
			s.repeaterRDACAddr[key] = &net.UDPAddr{IP: append([]byte(nil), addr.IP...), Port: addr.Port}
		}
		if _, exists := s.repeaterDMRAddr[key]; !exists {
			s.repeaterDMRAddr[key] = &net.UDPAddr{IP: append([]byte(nil), addr.IP...), Port: int(s.cfg.Hytera.DMRPort)}
		}
		rdacPort := 0
		if rdacAddr := s.repeaterRDACAddr[key]; rdacAddr != nil {
			rdacPort = rdacAddr.Port
		}
		s.mu.Unlock()
		slog.Info("Hytera startup port discovered",
			"peer", addr.String(),
			"packetType", fmt.Sprintf("0x%02X", packetType),
			"p2pPort", responseAddr.Port,
			"dmrPort", func() int {
				if packetType == 0x11 {
					return addr.Port
				}
				return int(s.cfg.Hytera.DMRPort)
			}(),
			"rdacPort", func() int {
				if packetType == 0x12 {
					return addr.Port
				}
				return rdacPort
			}())
		if s.peerHandler != nil {
			peerDMRPort := int(s.cfg.Hytera.DMRPort)
			if packetType == 0x11 {
				peerDMRPort = addr.Port
			}
			peerRDACPort := rdacPort
			if packetType == 0x12 {
				peerRDACPort = addr.Port
			}
			s.peerHandler(cloneUDPAddr(addr), responseAddr.Port, peerDMRPort, peerRDACPort, 0)
		}

		// Python 参考实现：先发 ACK，再发 redirect
		_, _ = s.p2pConn.WriteToUDP(resp, responseAddr)
		redirect := getRedirectPacket(resp, s.cfg.Hytera.DMRPort)
		if packetType == 0x12 {
			redirect = getRedirectPacket(resp, s.cfg.Hytera.RDACPort)
		}
		_, _ = s.p2pConn.WriteToUDP(redirect, responseAddr)
		if packetType == 0x12 && s.rdacConn != nil {
			rdacAddr := cloneUDPAddr(addr)
			go func() {
				time.Sleep(150 * time.Millisecond)
				s.startRDACIdentification(rdacAddr, "p2p-rdac-startup")
			}()
		}
	}
}

func classifyRDACPacket(data []byte) string {
	if len(data) == 0 {
		return "empty"
	}
	if len(data) >= 21 && bytesEqual(data[:4], []byte("P2P1")) {
		return fmt.Sprintf("p2p1-0x%02x", data[20])
	}
	if len(data) >= 3 && bytesEqual(data[:3], []byte("P32")) {
		return "p32"
	}
	if len(data) >= 4 && bytesEqual(data[:4], []byte{0x5A, 0x5A, 0x5A, 0x5A}) {
		return "keepalive"
	}
	return "raw"
}

func shortHex(data []byte, limit int) string {
	if len(data) == 0 {
		return ""
	}
	if limit <= 0 || len(data) <= limit {
		return hex.EncodeToString(data)
	}
	return hex.EncodeToString(data[:limit]) + "..."
}

func parseRDACDMRID(data []byte) (uint32, bool) {
	dmrid, ok, _, _ := parseRDACDMRIDDetailed(data)
	return dmrid, ok
}

func parseRDACDMRIDDetailed(data []byte) (uint32, bool, string, string) {
	// Observed Hytera short RDAC identity packet:
	// 96 00 <dmrid:3B BE> ...
	if len(data) == 14 && data[0] == 0x96 && data[1] == 0x00 {
		dmrid := uint32(data[2])<<16 | uint32(data[3])<<8 | uint32(data[4])
		if dmrid == 0 {
			return 0, false, "short-identity", "zero id"
		}
		return dmrid, true, "short-identity", "matched 14-byte 0x96 packet"
	}

	// RDAC HRNP step2 response used by Hytera_Homebrew_Bridge:
	// the repeater ID is exposed at bytes [18:21] in little-endian order.
	if len(data) >= 21 && data[0] == 0x7E && data[3] == 0x00 {
		dmrid := uint32(data[18]) | uint32(data[19])<<8 | uint32(data[20])<<16
		if dmrid == 0 {
			return 0, false, "rdac-hrnp-step2", "zero id at bytes 18:21"
		}
		return dmrid, true, "rdac-hrnp-step2", "matched HRNP data packet bytes 18:21"
	}

	// Some repeaters expose the ID as a native Hytera/IPSC 24-bit little-endian
	// source field near the tail of the packet.
	if len(data) >= 72 && hasHyteraDMRHeader(data) {
		dmrid := uint32(decodeID24LE(data[len(data)-4 : len(data)-1]))
		if dmrid == 0 {
			return 0, false, "hytera-tail-src", "zero id in packet tail"
		}
		return dmrid, true, "hytera-tail-src", "matched Hytera tail source field"
	}

	// Avoid guessing on short HRNP control/ack packets. They frequently contain
	// small counters/checksum bytes that look like valid 24-bit IDs but are not.
	if len(data) >= 32 {
		dmrid, ok := scan24BitDMRID(data)
		if ok {
			if dmrid == 0 {
				return 0, false, "scan24", "zero id after scan"
			}
			return dmrid, true, "scan24", "matched plausible 24-bit candidate while scanning packet"
		}
	}
	return 0, false, "", "no supported rdac dmrid pattern matched"
}

func scan24BitDMRID(data []byte) (uint32, bool) {
	for i := 0; i+3 <= len(data); i++ {
		candidate := uint32(data[i]) | uint32(data[i+1])<<8 | uint32(data[i+2])<<16
		if candidate == 0 {
			continue
		}
		if plausibleDMRID(candidate) {
			return candidate, true
		}
	}
	return 0, false
}

func plausibleDMRID(id uint32) bool {
	// Avoid obvious protocol constants and tiny counters while still accepting
	// common repeater/radio ID ranges.
	return id >= 100000
}

func parseHyteraDMRAppDMRID(data []byte) (uint32, bool) {
	// HRNP data packet carrying HDAP Radio Registration Service:
	// 7E .... opcode=0x00 ... hdap(message_type=0x11) opcode=0x00 rrs_type len radio_ip ...
	if len(data) < 21 {
		return 0, false
	}
	if data[0] != 0x7E || data[3] != 0x00 {
		return 0, false
	}

	hrnpLen := int(binary.BigEndian.Uint16(data[8:10]))
	if hrnpLen > 0 && hrnpLen <= len(data) {
		data = data[:hrnpLen]
	}
	if len(data) < 21 {
		return 0, false
	}

	hdap := data[12:]
	// The low 5 bits carry the HDAP message type; the high bits are flags.
	messageType := hdap[0] & 0x1F
	if messageType != 0x11 {
		return 0, false
	}
	if hdap[1] != 0x00 {
		return 0, false
	}
	rrsType := hdap[2]
	switch rrsType {
	case 0x01, 0x02, 0x03, 0x80, 0x82:
	default:
		return 0, false
	}
	if binary.LittleEndian.Uint16(hdap[3:5]) < 4 {
		return 0, false
	}

	return decodeHyteraRadioIPDMRID(hdap[5:9])
}

func decodeHyteraRadioIPDMRID(data []byte) (uint32, bool) {
	// Hytera radio_ip is encoded as subnet.byte1.byte2.byte3 and the bridge
	// project derives the radio ID by concatenating the last three decimal bytes.
	if len(data) < 4 {
		return 0, false
	}
	dmrid, err := strconv.ParseUint(
		strconv.Itoa(int(data[1]))+strconv.Itoa(int(data[2]))+strconv.Itoa(int(data[3])),
		10,
		32,
	)
	if err != nil || dmrid == 0 {
		return 0, false
	}
	return uint32(dmrid), true
}

func (s *Server) decodeToMMDVM(data []byte) (proto.Packet, error) {
	if len(data) < 38 {
		return proto.Packet{}, fmt.Errorf("short packet")
	}
	if !hasHyteraDMRHeader(data) {
		return proto.Packet{}, errIgnoredPacket
	}

	slot := data[12] != 0x01 // TS2=true
	slotType := binary.BigEndian.Uint16(data[18:20])
	if slotType == slotTypeSync || slotType == slotTypeWakeupRequest || slotType == slotTypeVoiceLCHeader {
		s.cacheStartupTemplate(slot, slotType, data)
	}
	if slotType == slotTypeSync || slotType == slotTypeWakeupRequest {
		return proto.Packet{}, errIgnoredPacket
	}

	frameType, dtype, ok := decodeSlotType(slotType)
	if !ok {
		return proto.Packet{}, errIgnoredPacket
	}

	tailStart := len(data) - 12
	if tailStart < 26 {
		return proto.Packet{}, fmt.Errorf("invalid packet tail")
	}

	callType := data[tailStart+2]
	groupCall := callType == 0x01
	dst := decodeID24LE(data[tailStart+4 : tailStart+8])
	src := decodeID24LE(data[tailStart+8 : tailStart+12])

	payload := data[26:tailStart]
	dmrDataBytes := swapDMRPayload(payload)
	if len(dmrDataBytes) != 33 {
		return proto.Packet{}, fmt.Errorf("unexpected DMR payload length: %d", len(dmrDataBytes))
	}
	var dmrData [33]byte
	copy(dmrData[:], dmrDataBytes)

	st := s.slots[slot]
	if st == nil {
		st = &slotState{}
		s.slots[slot] = st
	}
	// Some Hytera repeaters send the same 72-byte DMR bursts with a compact
	// wrapper instead of the 0x5A-prefixed header. In that variant byte 4 is
	// not a monotonic sequence number, so duplicate suppression would drop
	// valid frames. Keep de-dup only for the native 0x5A header.
	if hasNativeHyteraPrefix(data) {
		seqIn := data[4]
		if st.inLastSeqSet && st.inLastSeq == seqIn {
			return proto.Packet{}, errIgnoredPacket
		}
		st.inLastSeq = seqIn
		st.inLastSeqSet = true
	}

	now := time.Now()
	if !st.active || shouldStartNewInboundStream(st, slotType, src, dst, groupCall, now) {
		st.streamID = s.nextStreamID
		s.nextStreamID++
		if s.nextStreamID == 0 {
			s.nextStreamID = 1
		}
		st.active = true
	}
	if slotType == slotTypeTerminator {
		defer func() { st.active = false }()
	}
	st.inSrc = src
	st.inDst = dst
	st.inGroupCall = groupCall
	st.lastInbound = now

	repeaterID := s.cfg.BridgeID()

	pkt := proto.Packet{
		Signature:   "DMRD",
		Seq:         uint(st.mmdvmSeq),
		Src:         src,
		Dst:         dst,
		Repeater:    uint(repeaterID),
		Slot:        slot,
		GroupCall:   groupCall,
		FrameType:   frameType,
		DTypeOrVSeq: dtype,
		StreamID:    uint(st.streamID),
		DMRData:     dmrData,
	}
	st.mmdvmSeq++
	return pkt, nil
}

func hasNativeHyteraPrefix(data []byte) bool {
	return len(data) >= 4 &&
		data[0] == hyteraPrefixA &&
		data[1] == hyteraPrefixA &&
		data[2] == hyteraPrefixA &&
		data[3] == hyteraPrefixA
}

func shouldStartNewInboundStream(st *slotState, slotType uint16, src, dst uint, groupCall bool, now time.Time) bool {
	if st == nil {
		return true
	}
	if !st.active {
		return true
	}
	if slotType != slotTypeVoiceLCHeader {
		return false
	}
	if st.inSrc != src || st.inDst != dst || st.inGroupCall != groupCall {
		return true
	}
	hangTime := hyteraPrivateCallHangTime
	if groupCall {
		hangTime = hyteraGroupCallHangTime
	}
	if st.lastInbound.IsZero() || now.Sub(st.lastInbound) > hangTime {
		return true
	}
	return false
}

func hasHyteraDMRHeader(data []byte) bool {
	if hasNativeHyteraPrefix(data) {
		return true
	}
	if len(data) < 20 {
		return false
	}
	if data[9] != 0x00 || data[10] != 0x05 || data[11] != 0x01 {
		return false
	}
	if data[12] != 0x01 && data[12] != 0x02 {
		return false
	}
	slotRaw := binary.BigEndian.Uint16(data[16:18])
	switch slotRaw {
	case timeslotRaw1, timeslotRaw2:
		return true
	default:
		return false
	}
}

func (s *Server) encodeFromMMDVM(pkt proto.Packet) ([]byte, error) {
	st := s.slots[pkt.Slot]
	if st == nil {
		st = &slotState{}
		s.slots[pkt.Slot] = st
	}
	return s.encodeFromMMDVMWithState(pkt, st)
}

func (s *Server) encodeFromMMDVMWithState(pkt proto.Packet, st *slotState) ([]byte, error) {
	if st == nil {
		st = &slotState{}
	}
	prevSeq := st.outSeq
	st.outSeq++
	if prevSeq == 0xFF {
		slog.Debug("hytera out sequence wrapped",
			"slot", pkt.Slot,
			"src", pkt.Src,
			"dst", pkt.Dst,
			"groupCall", pkt.GroupCall,
			"ft", pkt.FrameType,
			"dt", pkt.DTypeOrVSeq,
			"stream", pkt.StreamID,
			"prevSeq", 0xFF,
			"nextSeq", st.outSeq)
	}

	slotType, packetType, err := encodeSlotType(pkt)
	if err != nil {
		return nil, err
	}

	payload := swapDMRPayload(pkt.DMRData[:])
	if len(payload) != 34 {
		return nil, fmt.Errorf("unexpected Hytera payload length: %d", len(payload))
	}

	buf := make([]byte, 26+len(payload)+12)
	buf[0], buf[1], buf[2], buf[3] = hyteraPrefixA, hyteraPrefixA, hyteraPrefixA, hyteraPrefixA
	buf[4] = st.outSeq
	buf[5], buf[6], buf[7] = 0xE0, 0x00, 0x00
	buf[8] = packetType
	buf[9], buf[10], buf[11] = 0x00, 0x05, 0x01
	if pkt.Slot {
		buf[12] = 0x02
		binary.BigEndian.PutUint16(buf[16:18], timeslotRaw2)
	} else {
		buf[12] = 0x01
		binary.BigEndian.PutUint16(buf[16:18], timeslotRaw1)
	}
	binary.BigEndian.PutUint16(buf[18:20], slotType)

	cc := cfgColorCode(s.cfg)
	ccByte := cc | (cc << 4)
	buf[20] = ccByte
	buf[21] = ccByte
	binary.BigEndian.PutUint16(buf[22:24], 0x0000)
	buf[24], buf[25] = 0x40, 0x5C
	copy(buf[26:], payload)

	tailStart := 26 + len(payload)
	buf[tailStart], buf[tailStart+1] = 0x63, 0x02
	if pkt.GroupCall {
		buf[tailStart+2] = 0x01
	} else {
		buf[tailStart+2] = 0x00
	}
	encodeID24LE(buf[tailStart+4:tailStart+8], pkt.Dst)
	encodeID24LE(buf[tailStart+8:tailStart+12], pkt.Src)
	return buf, nil
}

func decodeSlotType(slotType uint16) (frameType, dtype uint, ok bool) {
	switch slotType {
	case slotTypeVoiceLCHeader:
		return 2, 1, true
	case slotTypeTerminator:
		return 2, 2, true
	case slotTypeCSBK:
		return 2, 3, true
	case slotTypeDataHeader:
		return 2, 6, true
	case slotTypeRate12Data:
		return 2, 7, true
	case slotTypeRate34Data:
		return 2, 8, true
	case slotTypeDataAOrPrivacy:
		return 1, 0, true
	case slotTypeDataB:
		return 0, 1, true
	case slotTypeDataC:
		return 0, 2, true
	case slotTypeDataD:
		return 0, 3, true
	case slotTypeDataE:
		return 0, 4, true
	case slotTypeDataF:
		return 0, 5, true
	case slotTypePrivacyIndicator:
		return 2, 0, true
	default:
		return 0, 0, false
	}
}

func encodeSlotType(pkt proto.Packet) (slotType uint16, packetType byte, err error) {
	packetType = packetTypeA
	if pkt.FrameType == 2 {
		switch pkt.DTypeOrVSeq {
		case 0:
			return slotTypePrivacyIndicator, packetType, nil
		case 1:
			return slotTypeVoiceLCHeader, packetType, nil
		case 2:
			return slotTypeTerminator, packetTypeTerminator, nil
		case 3:
			return slotTypeCSBK, packetType, nil
		case 6:
			return slotTypeDataHeader, packetType, nil
		case 7:
			return slotTypeRate12Data, packetType, nil
		case 8:
			return slotTypeRate34Data, packetType, nil
		default:
			return 0, 0, fmt.Errorf("unsupported data type: %d", pkt.DTypeOrVSeq)
		}
	}

	switch pkt.DTypeOrVSeq {
	case 0:
		return slotTypeDataAOrPrivacy, packetType, nil
	case 1:
		return slotTypeDataB, packetType, nil
	case 2:
		return slotTypeDataC, packetType, nil
	case 3:
		return slotTypeDataD, packetType, nil
	case 4:
		return slotTypeDataE, packetType, nil
	case 5:
		return slotTypeDataF, packetType, nil
	default:
		return 0, 0, fmt.Errorf("unsupported voice seq: %d", pkt.DTypeOrVSeq)
	}
}

func getRedirectPacket(data []byte, targetPort uint16) []byte {
	if len(data) < 16 {
		return data
	}
	out := append([]byte(nil), data[:len(data)-1]...)
	out[4] = 0x0B
	out[12] = 0xFF
	out[13] = 0xFF
	out[14] = 0x01
	out[15] = 0x00
	out = append(out, 0xFF, 0x01)
	port := make([]byte, 2)
	binary.LittleEndian.PutUint16(port, targetPort)
	out = append(out, port...)
	return out
}

func buildWakeupPacket(slot bool, sourceID, targetID uint, _ bool, colorCode uint8) []byte {
	data := make([]byte, 0, 72)
	data = append(data,
		0x5A, 0x5A, 0x5A, 0x5A,
		0x00, 0x00, 0x00, 0x00,
		0x42,
		0x00, 0x05, 0x42,
	)
	if slot {
		data = append(data, 0x02)
	} else {
		data = append(data, 0x01)
	}
	data = append(data, 0x00, 0x00, 0x00)
	if slot {
		data = append(data, 0x22, 0x22)
	} else {
		data = append(data, 0x11, 0x11)
	}
	data = append(data,
		0xDD, 0xDD,
		(colorCode&0x0F)|((colorCode&0x0F)<<4), (colorCode&0x0F)|((colorCode&0x0F)<<4),
		0x00, 0x00,
		0x40,
	)
	data = append(data, make([]byte, 11)...)
	data = append(data, 0x01, 0x00, 0x02, 0x00, 0x02, 0x00, 0x01)
	data = append(data, make([]byte, 13)...)
	data = append(data, 0xFF, 0xFF, 0xEF, 0x08, 0x2A, 0x00, 0x07, 0x00)
	data = append(data, byte(targetID), byte(targetID>>8), byte(targetID>>16), 0x00)
	data = append(data, byte(sourceID), byte(sourceID>>8), byte(sourceID>>16), 0x00)
	return data
}

func buildSyncPacket(slot bool, sourceID, targetID uint, groupCall bool, colorCode uint8) []byte {
	ccByte := (colorCode & 0x0F) | ((colorCode & 0x0F) << 4)
	data := make([]byte, 0, 72)
	data = append(data,
		0x5A, 0x5A, 0x5A, 0x5A,
		0x00, 0x00, 0x00, 0x00,
		0x42, 0x00, 0x05, 0x01,
	)
	if slot {
		data = append(data, 0x02)
	} else {
		data = append(data, 0x01)
	}
	data = append(data, 0x00, 0x00, 0x00)
	if slot {
		data = append(data, 0x22, 0x22)
	} else {
		data = append(data, 0x11, 0x11)
	}
	data = append(data, 0xEE, 0xEE, ccByte, ccByte, 0x11, 0x11, 0x40, 0x2F)
	data = append(data, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	data = append(data,
		byte(targetID>>16), 0x00,
		byte(targetID>>8), 0x00,
		byte(targetID), 0x00,
		byte(sourceID>>16), 0x00,
		byte(sourceID>>8), 0x00,
		byte(sourceID), 0x00,
	)
	data = append(data, make([]byte, 14)...)
	data = append(data, make([]byte, 4)...)
	if groupCall {
		data = append(data, 0x01, 0x00)
	} else {
		data = append(data, 0x00, 0x00)
	}
	data = append(data, byte(targetID), byte(targetID>>8), byte(targetID>>16), 0x00)
	data = append(data, byte(sourceID), byte(sourceID>>8), byte(sourceID>>16), 0x00)
	slog.Debug("moto->hytera sync template",
		"src", sourceID,
		"dst", targetID,
		"slot", slot,
		"groupCall", groupCall,
		"visible", fmt.Sprintf("% X", data[33:45]),
		"tailDst", fmt.Sprintf("% X", data[65:69]),
		"tailSrc", fmt.Sprintf("% X", data[69:73]))
	return data
}

func encodeID24LE(dst []byte, id uint) {
	if len(dst) < 4 {
		return
	}
	dst[0] = byte(id)
	dst[1] = byte(id >> 8)
	dst[2] = byte(id >> 16)
	dst[3] = 0x00
}

func decodeID24LE(src []byte) uint {
	if len(src) < 3 {
		return 0
	}
	return uint(src[0]) | uint(src[1])<<8 | uint(src[2])<<16
}

func swapDMRPayload(data []byte) []byte {
	out := append([]byte(nil), data...)
	odd := len(out)%2 != 0
	if odd {
		out = append(out, 0x00)
	}
	for i := 0; i+1 < len(out); i += 2 {
		out[i], out[i+1] = out[i+1], out[i]
	}
	if !odd {
		out = out[:len(out)-1]
	}
	return out
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

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasPrefix(data, prefix []byte) bool {
	if len(data) < len(prefix) {
		return false
	}
	return bytesEqual(data[:len(prefix)], prefix)
}

func (s *Server) paceAddr(addr *net.UDPAddr, interval time.Duration) {
	if addr == nil {
		return
	}
	if interval <= 0 {
		interval = hyteraBurstInterval
	}
	key := addr.String()
	s.mu.Lock()
	last := s.lastSend[key]
	now := time.Now()
	if !last.IsZero() {
		elapsed := now.Sub(last)
		if elapsed < interval {
			s.mu.Unlock()
			time.Sleep(interval - elapsed)
			s.mu.Lock()
		}
	}
	s.lastSend[key] = time.Now()
	s.mu.Unlock()
}

func startupTemplateKey(slot bool, slotType uint16) string {
	slotKey := "ts1"
	if slot {
		slotKey = "ts2"
	}
	return fmt.Sprintf("%s:%04x", slotKey, slotType)
}

func (s *Server) cacheStartupTemplate(slot bool, slotType uint16, data []byte) {
	if len(data) < 72 {
		return
	}
	key := startupTemplateKey(slot, slotType)
	template := append([]byte(nil), data...)
	s.mu.Lock()
	s.startupTemplates[key] = template
	s.mu.Unlock()
}

func (s *Server) startupTemplate(slot bool, slotType uint16) []byte {
	s.mu.RLock()
	template := s.startupTemplates[startupTemplateKey(slot, slotType)]
	if template == nil {
		template = s.startupTemplates[startupTemplateKey(!slot, slotType)]
	}
	s.mu.RUnlock()
	if template == nil {
		return nil
	}
	return append([]byte(nil), template...)
}

func (s *Server) buildStartupPacket(slotType uint16, pkt proto.Packet) ([]byte, bool) {
	template := s.startupTemplate(pkt.Slot, slotType)
	if template == nil {
		return buildFixedStartupPacket(slotType, pkt, cfgColorCode(s.cfg))
	}
	tailStart := len(template) - 12
	if tailStart < 26 {
		return nil, false
	}
	if pkt.Slot {
		template[12] = 0x02
		binary.BigEndian.PutUint16(template[16:18], timeslotRaw2)
	} else {
		template[12] = 0x01
		binary.BigEndian.PutUint16(template[16:18], timeslotRaw1)
	}
	binary.BigEndian.PutUint16(template[18:20], slotType)
	if pkt.GroupCall {
		template[tailStart+2] = 0x01
	} else {
		template[tailStart+2] = 0x00
	}
	encodeID24LE(template[tailStart+4:tailStart+8], pkt.Dst)
	encodeID24LE(template[tailStart+8:tailStart+12], pkt.Src)
	return template, true
}

func buildFixedStartupPacket(slotType uint16, pkt proto.Packet, colorCode uint8) ([]byte, bool) {
	cc := colorCode & 0x0F
	if slotType == slotTypeWakeupRequest {
		return buildWakeupPacket(pkt.Slot, pkt.Src, pkt.Dst, !pkt.GroupCall, cc), true
	}
	if slotType == slotTypeSync {
		return buildSyncPacket(pkt.Slot, pkt.Src, pkt.Dst, pkt.GroupCall, cc), true
	}
	return nil, false
}

func cfgColorCode(cfg *config.Config) uint8 {
	if cfg == nil {
		return 0
	}
	return cfg.Local.ColorCode & 0x0F
}

func packetColorCode(pkt proto.Packet, fallback uint8) uint8 {
	cc, _ := packetColorCodeWithSource(pkt, fallback)
	return cc
}

func packetColorCodeWithSource(pkt proto.Packet, fallback uint8) (uint8, string) {
	cc := fallback & 0x0F
	if slotCC, ok := packetSlotTypeColorCode(pkt.DMRData); ok {
		return slotCC, "slotType"
	}
	if embeddedCC, ok := packetEmbeddedColorCode(pkt.DMRData); ok {
		return embeddedCC, "embedded"
	}
	return cc, "fallback"
}

func packetSlotTypeColorCode(data [33]byte) (uint8, bool) {
	bits := packetBits(data)
	if !packetHasDataSync(bits) {
		return 0, false
	}
	var slotBits [20]byte
	for i := 0; i < 10; i++ {
		if bits[98+i] {
			slotBits[i] = 1
		}
	}
	for i := 0; i < 10; i++ {
		if bits[156+i] {
			slotBits[10+i] = 1
		}
	}
	slotType := pdu.NewSlotTypeFromBits(slotBits)
	return uint8(slotType.ColorCode) & 0x0F, true
}

func packetEmbeddedColorCode(data [33]byte) (uint8, bool) {
	bits := packetBits(data)
	if !packetHasEmbeddedSignalling(bits) {
		return 0, false
	}
	var embeddedBits [16]byte
	for i := 0; i < 8; i++ {
		if bits[108+i] {
			embeddedBits[i] = 1
		}
	}
	for i := 0; i < 8; i++ {
		if bits[148+i] {
			embeddedBits[8+i] = 1
		}
	}
	embedded := pdu.NewEmbeddedSignallingFromBits(embeddedBits)
	return uint8(embedded.ColorCode) & 0x0F, true
}

func packetBits(data [33]byte) [264]bool {
	var bits [264]bool
	for i := 0; i < 264; i++ {
		bits[i] = (data[i/8] & (1 << (7 - (i % 8)))) != 0
	}
	return bits
}

func packetHasDataSync(bits [264]bool) bool {
	sync := packetSyncPattern(bits)
	return sync == enums.Tdma1Data || sync == enums.Tdma2Data || sync == enums.MsSourcedData || sync == enums.BsSourcedData
}

func packetHasEmbeddedSignalling(bits [264]bool) bool {
	return packetSyncPattern(bits) == enums.EmbeddedSignallingPattern
}

func packetSyncPattern(bits [264]bool) enums.SyncPattern {
	var syncBytes [6]byte
	for i := 0; i < 6; i++ {
		for j := 0; j < 8; j++ {
			if bits[108+(i*8)+j] {
				syncBytes[i] |= 1 << (7 - j)
			}
		}
	}
	return enums.SyncPatternFromBytes(syncBytes)
}

func normalizeMotoColorCode(cc uint8, source string, fallback uint8) (uint8, string) {
	cc &= 0x0F
	fb := fallback & 0x0F
	// Some Moto-origin packets carry CC=0 in embedded/signalling fields even when
	// the target system uses a configured CC. Treat zero as unknown and fall back.
	if cc == 0 && fb != 0 {
		return fb, source + "-zero->fallback"
	}
	return cc, source
}

func patchMotoVoiceEmbeddedControl(pkt proto.Packet, colorCode uint8) proto.Packet {
	cc := colorCode & 0x0F
	if pkt.FrameType != 0 {
		return pkt
	}
	var burst layer2.Burst
	burst.DecodeFromBytes(pkt.DMRData)
	if !burst.HasEmbeddedSignalling {
		return pkt
	}
	changed := false
	if cc != 0 && burst.EmbeddedSignalling.ColorCode == 0 {
		burst.EmbeddedSignalling.ColorCode = int(cc)
		changed = true
	}

	// Keep lightweight path aligned with BM-style voice burst control signalling.
	switch pkt.DTypeOrVSeq {
	case 1:
		if burst.EmbeddedSignalling.LCSS != enums.FirstFragmentLC {
			burst.EmbeddedSignalling.LCSS = enums.FirstFragmentLC
			changed = true
		}
	case 2, 3:
		if burst.EmbeddedSignalling.LCSS != enums.ContinuationFragmentLCorCSBK {
			burst.EmbeddedSignalling.LCSS = enums.ContinuationFragmentLCorCSBK
			changed = true
		}
	case 4:
		if burst.EmbeddedSignalling.LCSS != enums.LastFragmentLCorCSBK {
			burst.EmbeddedSignalling.LCSS = enums.LastFragmentLCorCSBK
			changed = true
		}
	case 5:
		// BM captures show voice-F uses single-fragment state; normalizing this
		// avoids stream drop after a few seconds on some Hytera repeaters.
		if burst.EmbeddedSignalling.LCSS != enums.SingleFragmentLCorCSBK {
			burst.EmbeddedSignalling.LCSS = enums.SingleFragmentLCorCSBK
			changed = true
		}
		// Keep payload nibbles neutral on F bursts.
		burst.UnpackEmbeddedSignallingData([]byte{0x00, 0x00, 0x00, 0x00})
		if cc != 0 {
			burst.EmbeddedSignalling.ColorCode = int(cc)
		}
		changed = true
	}

	if !changed {
		return pkt
	}
	out := pkt
	out.DMRData = burst.Encode()
	return out
}

func setHyteraHeaderColorCode(data []byte, colorCode uint8) {
	if len(data) < 22 {
		return
	}
	ccByte := (colorCode & 0x0F) | ((colorCode & 0x0F) << 4)
	data[20] = ccByte
	data[21] = ccByte
}

func (s *Server) buildStartupPacketForRoute(slotType uint16, pkt proto.Packet, fromMoto bool, motoColorCode uint8) ([]byte, bool) {
	if fromMoto {
		return buildFixedStartupPacket(slotType, pkt, motoColorCode)
	}
	return s.buildStartupPacket(slotType, pkt)
}
