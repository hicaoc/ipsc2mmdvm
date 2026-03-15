package ipsc

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/USA-RedDragon/dmrgo/dmr/enums"
	"github.com/USA-RedDragon/dmrgo/dmr/layer2"
	"github.com/USA-RedDragon/dmrgo/dmr/layer2/elements"
	"github.com/USA-RedDragon/dmrgo/dmr/layer2/pdu"
	l3elements "github.com/USA-RedDragon/dmrgo/dmr/layer3/elements"
	intdmr "github.com/hicaoc/ipsc2mmdvm/internal/dmr"
	"github.com/hicaoc/ipsc2mmdvm/internal/dmr/bptc"
	"github.com/hicaoc/ipsc2mmdvm/internal/metrics"
	mmdvm "github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

// IPSCTranslator converts MMDVM DMRD packets into IPSC user packets.
// It maintains per-stream state (RTP sequence, timestamp, call control)
// and uses the dmrgo library to FEC-decode AMBE voice data from the
// 33-byte DMR burst into the 19-byte IPSC AMBE payload.
//
// It also converts IPSC user packets back into MMDVM DMRD packets for the
// reverse direction.
type IPSCTranslator struct {
	mu             sync.Mutex
	metrics        *metrics.Metrics
	peerID         uint32
	repeaterID     uint32
	colorCode      uint8
	streams        map[uint32]*streamState
	reverseStreams map[uint32]*reverseStreamState
	burst          layer2.Burst // reusable burst to reduce allocations

	nextCallControl uint32
	nextStreamID    uint32
}

// streamState tracks RTP sequencing and call framing for one voice stream.
type streamState struct {
	callControl  uint32 // random per-call
	rtpSeq       uint16
	rtpTimestamp uint32
	ipscSeq      uint8
	headersSent  int  // number of voice headers sent (3 required)
	burstIndex   int  // 0-5 → A-F
	firstPacket  bool // true for the very first packet
	src          uint
	dst          uint
	slot         bool
	groupCall    bool
	lastSeen     time.Time
}

// IPSC burst data type constants (byte 30 of IPSC voice packet)
const (
	ipscBurstVoiceHead byte = 0x01
	ipscBurstVoiceTerm byte = 0x02
	ipscBurstCSBK      byte = 0x03
	ipscBurstSlot1     byte = 0x0A
	ipscBurstSlot2     byte = 0x8A
)

// MMDVM FrameType values (bits 2-3 of DMRD byte 15)
const (
	mmdvmFrameTypeVoice     uint = 0 // Voice data
	mmdvmFrameTypeVoiceSync uint = 1 // Voice sync (marks A burst or data sync)
	mmdvmFrameTypeDataSync  uint = 2 // Data sync (header / terminator)
)

// RTP timestamp increment per burst (~60ms spacing in 16.16 format)
const rtpTimestampIncrement = 480
const ipscVoiceHeaderRepeats = 2
const forwardStreamReuseWindow = 8 * time.Second

func NewIPSCTranslator() (*IPSCTranslator, error) {
	return &IPSCTranslator{
		streams:        make(map[uint32]*streamState),
		reverseStreams: make(map[uint32]*reverseStreamState),
	}, nil
}

// SetMetrics configures the metrics collector for this translator.
func (t *IPSCTranslator) SetMetrics(m *metrics.Metrics) {
	t.metrics = m
}

// SetPeerID sets the local peer ID used in outgoing IPSC packets.
func (t *IPSCTranslator) SetPeerID(peerID uint32) {
	t.peerID = peerID
	t.repeaterID = peerID
}

// SetColorCode sets the DMR color code used when building DMR bursts.
func (t *IPSCTranslator) SetColorCode(cc uint8) {
	t.colorCode = cc & 0x0F
}

// TranslateToIPSC converts an MMDVM DMRD Packet into one or more IPSC
// user packets ready to send to IPSC peers. It returns nil if the packet
// cannot be translated (e.g. non-voice data we don't handle yet).
func (t *IPSCTranslator) TranslateToIPSC(pkt mmdvm.Packet) [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()

	streamID := pkt.StreamID
	if streamID > math.MaxUint32 {
		return nil
	}

	// Get or create stream state
	ss, ok := t.streams[uint32(streamID)]
	if !ok {
		now := time.Now()
		if pkt.FrameType == mmdvmFrameTypeVoice || pkt.FrameType == mmdvmFrameTypeVoiceSync {
			if recent := t.findRecentForwardStream(pkt, now); recent != nil {
				ss = recent
				t.streams[uint32(streamID)] = ss
				ok = true
			}
		}
	}
	if !ok {
		t.nextCallControl++
		if t.nextCallControl == 0 {
			t.nextCallControl = 1
		}
		ss = &streamState{
			callControl: t.nextCallControl,
			firstPacket: true,
			src:         pkt.Src,
			dst:         pkt.Dst,
			slot:        pkt.Slot,
			groupCall:   pkt.GroupCall,
			lastSeen:    time.Now(),
		}
		t.streams[uint32(streamID)] = ss
		if t.metrics != nil {
			t.metrics.TranslatorActiveStreams.WithLabelValues("mmdvm_to_ipsc").Inc()
		}
	}
	ss.src = pkt.Src
	ss.dst = pkt.Dst
	ss.slot = pkt.Slot
	ss.groupCall = pkt.GroupCall
	ss.lastSeen = time.Now()

	frameType := pkt.FrameType
	dtypeOrVSeq := pkt.DTypeOrVSeq

	var results [][]byte

	switch frameType {
	case mmdvmFrameTypeDataSync:
		if dtypeOrVSeq > 255 {
			slog.Debug("IPSCTranslator: invalid dtype", "dtype", dtypeOrVSeq)
			return nil
		}
		// Voice LC Header, Terminator, or Data
		switch elements.DataType(dtypeOrVSeq) {
		case elements.DataTypeVoiceLCHeader:
			// Send a small number of startup headers for Moto/IPSC peers.
			for i := 0; i < ipscVoiceHeaderRepeats; i++ {
				data := t.buildVoiceHeader(pkt, ss, i == 0 && ss.firstPacket)
				results = append(results, data)
			}
			ss.headersSent = ipscVoiceHeaderRepeats
			ss.firstPacket = false
			ss.burstIndex = 0
		case elements.DataTypeTerminatorWithLC:
			data := t.buildVoiceTerminator(pkt, ss)
			results = append(results, data)
			// Clean up stream state
			t.removeForwardStreamAliases(ss)
			if t.metrics != nil {
				t.metrics.TranslatorActiveStreams.WithLabelValues("mmdvm_to_ipsc").Dec()
			}
		case elements.DataTypeCSBK, elements.DataTypePIHeader,
			elements.DataTypeDataHeader, elements.DataTypeRate12,
			elements.DataTypeRate34, elements.DataTypeRate1,
			elements.DataTypeMBCHeader, elements.DataTypeMBCContinuation:
			// Data packet — build IPSC data packet
			data := t.buildIPSCDataPacket(pkt, ss, elements.DataType(dtypeOrVSeq))
			results = append(results, data)
			ss.firstPacket = false
		case elements.DataTypeIdle, elements.DataTypeUnifiedSingleBlock, elements.DataTypeReserved:
			return nil
		default:
			slog.Debug("IPSCTranslator: unhandled data sync dtype", "dtype", dtypeOrVSeq)
			return nil
		}

	case mmdvmFrameTypeVoice, mmdvmFrameTypeVoiceSync:
		// Voice burst — decode DMR data and extract AMBE
		data := t.buildVoiceBurst(pkt, ss)
		if data != nil {
			results = append(results, data)
		}
		// Advance burst index (A=0 through F=5, then wrap)
		ss.burstIndex = (ss.burstIndex + 1) % 6

	default:
		slog.Debug("IPSCTranslator: unknown frame type", "frameType", frameType)
		return nil
	}

	if t.metrics != nil && len(results) > 0 {
		t.metrics.TranslatorPackets.WithLabelValues("mmdvm_to_ipsc").Add(float64(len(results)))
	}

	for _, data := range results {
		if len(data) < 31 {
			continue
		}
		callInfo := data[17]
		slot := "ts1"
		if callInfo&0x20 != 0 {
			slot = "ts2"
		}
		slotMarker := byte(0x00)
		if len(data) > 35 {
			slotMarker = data[35]
		}
		slog.Debug("IPSCTranslator: TranslateToIPSC",
			"slot", slot,
			"callInfo", fmt.Sprintf("0x%02X", callInfo),
			"burstType", fmt.Sprintf("0x%02X", data[30]),
			"slotMarker", fmt.Sprintf("0x%02X", slotMarker),
			"length", len(data),
			"src", pkt.Src,
			"dst", pkt.Dst,
			"streamID", pkt.StreamID)
	}

	return results
}

// CleanupStream removes state for a given stream (e.g. on timeout).
func (t *IPSCTranslator) CleanupStream(streamID uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.streams, streamID)
}

func (t *IPSCTranslator) findRecentForwardStream(pkt mmdvm.Packet, now time.Time) *streamState {
	for _, ss := range t.streams {
		if ss == nil {
			continue
		}
		if ss.src != pkt.Src || ss.dst != pkt.Dst || ss.groupCall != pkt.GroupCall || ss.slot != pkt.Slot {
			continue
		}
		if ss.lastSeen.IsZero() || now.Sub(ss.lastSeen) > forwardStreamReuseWindow {
			continue
		}
		return ss
	}
	return nil
}

func (t *IPSCTranslator) removeForwardStreamAliases(target *streamState) {
	if target == nil {
		return
	}
	for key, ss := range t.streams {
		if ss == target {
			delete(t.streams, key)
		}
	}
}

// buildIPSCHeader writes the common 18-byte IPSC header (bytes 0-17).
func (t *IPSCTranslator) buildIPSCHeader(buf []byte, pkt mmdvm.Packet, ss *streamState, isEnd bool, isData bool) {
	// Byte 0: Packet type
	if isData {
		if pkt.GroupCall {
			buf[0] = byte(0x83) // GROUP_DATA
		} else {
			buf[0] = byte(0x84) // PVT_DATA
		}
	} else {
		if pkt.GroupCall {
			buf[0] = byte(0x80) // GROUP_VOICE
		} else {
			buf[0] = byte(0x81) // PVT_VOICE
		}
	}

	// Bytes 1-4: Peer ID
	binary.BigEndian.PutUint32(buf[1:5], t.peerID)

	// Byte 5: IPSC sequence number
	buf[5] = ss.ipscSeq

	// Bytes 6-8: Source subscriber (24-bit)
	buf[6] = byte(pkt.Src >> 16)
	buf[7] = byte(pkt.Src >> 8)
	buf[8] = byte(pkt.Src)

	// Bytes 9-11: Destination (24-bit)
	buf[9] = byte(pkt.Dst >> 16)
	buf[10] = byte(pkt.Dst >> 8)
	buf[11] = byte(pkt.Dst)

	// Byte 12: Call type (0x02 = group call)
	if pkt.GroupCall {
		buf[12] = 0x02
	} else {
		buf[12] = 0x01
	}

	// Bytes 13-16: Call control (random per-call)
	binary.BigEndian.PutUint32(buf[13:17], ss.callControl)

	// Byte 17: Call info (timeslot + end flag)
	callInfo := byte(0x00)
	if pkt.Slot { // true = TS2
		callInfo |= 0x20
	}
	if isEnd {
		callInfo |= 0x40
	}
	buf[17] = callInfo
}

// buildRTPHeader writes the 12-byte RTP header at buf[18:30].
func (t *IPSCTranslator) buildRTPHeader(buf []byte, ss *streamState, marker bool, payloadType byte) {
	// Byte 18: RTP version 2, no padding, no extension, 0 CSRCs
	buf[18] = 0x80

	// Byte 19: Marker + payload type
	pt := payloadType
	if marker {
		pt |= 0x80
	}
	buf[19] = pt

	// Bytes 20-21: RTP sequence number
	binary.BigEndian.PutUint16(buf[20:22], ss.rtpSeq)
	ss.rtpSeq++

	// Bytes 22-25: RTP timestamp
	binary.BigEndian.PutUint32(buf[22:26], ss.rtpTimestamp)
	ss.rtpTimestamp += rtpTimestampIncrement

	// Bytes 26-29: RTP SSRC (0)
	binary.BigEndian.PutUint32(buf[26:30], 0)
}

// buildVoiceHeader builds a 54-byte IPSC voice header packet.
// Voice headers embed the Full LC (link control) data.
func (t *IPSCTranslator) buildVoiceHeader(pkt mmdvm.Packet, ss *streamState, isFirst bool) []byte {
	buf := make([]byte, 54)

	t.buildIPSCHeader(buf, pkt, ss, false, false)

	// RTP header: marker on first header, payload type 0x5D
	t.buildRTPHeader(buf, ss, isFirst, 0x5D)

	// RTP Payload — voice header
	burstType := ipscBurstSlot2
	if !pkt.Slot {
		burstType = ipscBurstSlot1
	}
	_ = burstType

	buf[30] = ipscBurstVoiceHead                   // Burst type
	buf[31] = 0x80                                 // RSSI threshold / parity
	binary.BigEndian.PutUint16(buf[32:34], 0x000A) // Length to follow (10 words = 20 bytes)
	buf[34] = 0x80                                 // RSSI status
	if pkt.Slot {
		buf[35] = ipscBurstSlot2 // Slot type/sync
	} else {
		buf[35] = ipscBurstSlot1
	}
	binary.BigEndian.PutUint16(buf[36:38], 0x0060) // Data size (96 bits = 12 bytes)

	// Bytes 38-49: Full LC data (12 bytes)
	// Extract from the DMR burst data — the header burst carries a Voice LC Header
	// which contains FLCO, FID, ServiceOpt, Dst, Src, CRC
	flcBytes := extractFullLCBytes(pkt)
	copy(buf[38:50], flcBytes[:12])

	// Bytes 50-53: unknown trailing (zeros)
	return buf
}

// buildVoiceTerminator builds a 54-byte IPSC voice terminator packet.
func (t *IPSCTranslator) buildVoiceTerminator(pkt mmdvm.Packet, ss *streamState) []byte {
	buf := make([]byte, 54)

	t.buildIPSCHeader(buf, pkt, ss, true, false)

	// RTP header: no marker, payload type 0x5E for terminator
	t.buildRTPHeader(buf, ss, false, 0x5E)

	// RTP Payload — voice terminator (same structure as header)
	buf[30] = ipscBurstVoiceTerm
	buf[31] = 0x80
	binary.BigEndian.PutUint16(buf[32:34], 0x000A)
	buf[34] = 0x80
	if pkt.Slot {
		buf[35] = ipscBurstSlot2
	} else {
		buf[35] = ipscBurstSlot1
	}
	binary.BigEndian.PutUint16(buf[36:38], 0x0060)

	// Full LC data
	flcBytes := extractFullLCBytes(pkt)
	copy(buf[38:50], flcBytes[:12])

	ss.ipscSeq++
	return buf
}

// buildIPSCDataPacket builds a 54-byte IPSC data packet for CSBK, Data Header, etc.
// The structure is identical to voice header/terminator but with data packet types (0x83/0x84).
func (t *IPSCTranslator) buildIPSCDataPacket(pkt mmdvm.Packet, ss *streamState, dataType elements.DataType) []byte {
	buf := make([]byte, 54)

	t.buildIPSCHeader(buf, pkt, ss, false, true)

	// RTP header: no marker, payload type 0x5D
	t.buildRTPHeader(buf, ss, ss.firstPacket, 0x5D)

	// RTP Payload — data burst
	buf[30] = byte(dataType) // Burst type = DMR data type (e.g. 0x03 for CSBK)
	buf[31] = 0xC0           // RSSI threshold / parity
	binary.BigEndian.PutUint16(buf[32:34], 0x000A)
	buf[34] = 0x80 // RSSI status
	if pkt.Slot {
		buf[35] = ipscBurstSlot2 // Slot type/sync
	} else {
		buf[35] = ipscBurstSlot1
	}
	binary.BigEndian.PutUint16(buf[36:38], 0x0060) // Data size (96 bits = 12 bytes)

	// Bytes 38-49: Extract data from DMR burst via BPTC decode
	// Use extractFullLCBytes which constructs from packet fields
	flcBytes := extractFullLCBytes(pkt)
	copy(buf[38:50], flcBytes[:12])

	// Bytes 50-53: trailing (zeros)
	ss.ipscSeq++
	return buf
}

// buildVoiceBurst builds an IPSC voice burst packet.
// Burst A = 52 bytes, Bursts B-D,F = 57 bytes, Burst E = 66 bytes.
func (t *IPSCTranslator) buildVoiceBurst(pkt mmdvm.Packet, ss *streamState) []byte {
	// Decode the DMR burst to extract AMBE voice data
	t.burst.DecodeFromBytes(pkt.DMRData)

	if t.burst.IsData {
		// This is a data burst within a voice stream, skip it
		slog.Debug("IPSCTranslator: skipping data burst in voice stream")
		return nil
	}

	// Extract the 19-byte FEC-decoded AMBE payload from the 3 vocoder frames
	var frames [3][49]byte
	for i := range t.burst.VoiceData.Frames {
		frames[i] = t.burst.VoiceData.Frames[i].DecodedBits
	}
	ambeData := intdmr.PackAMBEVoiceFrames(frames)

	// Determine slot type byte
	slotBurst := ipscBurstSlot2
	if !pkt.Slot {
		slotBurst = ipscBurstSlot1
	}

	burstIdx := ss.burstIndex % 6

	var buf []byte
	switch burstIdx {
	case 0: // Burst A — sync burst, 52 bytes
		buf = make([]byte, 52)
		t.buildIPSCHeader(buf, pkt, ss, false, false)
		t.buildRTPHeader(buf, ss, false, 0x5D)

		buf[30] = slotBurst
		buf[31] = 0x14 // Length: 20 bytes follow
		buf[32] = 0x40 // Unknown field
		copy(buf[33:52], ambeData[:])

	case 4: // Burst E — extended with embedded LC, 66 bytes
		buf = make([]byte, 66)
		t.buildIPSCHeader(buf, pkt, ss, false, false)
		t.buildRTPHeader(buf, ss, false, 0x5D)

		buf[30] = slotBurst
		buf[31] = 0x22 // Length: 34 bytes follow
		buf[32] = 0x16 // Unknown field
		copy(buf[33:52], ambeData[:])

		// Bytes 52-58: Embedded LC data (7 bytes)
		// Extract from embedded signalling if available
		if t.burst.HasEmbeddedSignalling {
			embData := t.burst.PackEmbeddedSignallingData()
			copy(buf[52:56], embData[:4])
		}

		// Bytes 56-58 or 59-61: Destination repeated
		buf[59] = byte(pkt.Dst >> 16)
		buf[60] = byte(pkt.Dst >> 8)
		buf[61] = byte(pkt.Dst)
		// Bytes 62-64: Source repeated
		buf[62] = byte(pkt.Src >> 16)
		buf[63] = byte(pkt.Src >> 8)
		buf[64] = byte(pkt.Src)
		buf[65] = 0x14 // Unknown trailer

	default: // Bursts B, C, D, F — 57 bytes with embedded signalling
		buf = make([]byte, 57)
		t.buildIPSCHeader(buf, pkt, ss, false, false)
		t.buildRTPHeader(buf, ss, false, 0x5D)

		buf[30] = slotBurst
		buf[31] = 0x19 // Length: 25 bytes follow
		buf[32] = 0x06 // Unknown field
		copy(buf[33:52], ambeData[:])

		// Bytes 52-56: Embedded signalling data (5 bytes)
		if t.burst.HasEmbeddedSignalling {
			embData := t.burst.PackEmbeddedSignallingData()
			copy(buf[52:56], embData[:4])
		}
	}

	return buf
}

func buildStandardLCBytes(src, dst uint, groupCall bool) [12]byte {
	flco := enums.FLCOUnitToUnitVoiceChannelUser
	if dst > math.MaxInt || src > math.MaxInt {
		slog.Error("Full LC address out of range")
		return [12]byte{}
	}

	if groupCall {
		flco = enums.FLCOGroupVoiceChannelUser
	}

	flc := pdu.FullLinkControl{
		FLCO:         flco,
		FeatureSetID: enums.StandardizedFID,
		ServiceOptions: l3elements.ServiceOptions{
			Reserved: [2]byte{1, 0}, // Sets 0x20 (Default)
		},
		GroupAddress:  int(dst),
		TargetAddress: int(dst),
		SourceAddress: int(src),
	}

	encoded, err := flc.Encode()
	if err != nil {
		slog.Error("Failed to encode Full LC", "error", err)
		return [12]byte{}
	}

	var res [12]byte
	copy(res[:], encoded)
	return res
}

func recoverAddressesFromFullLC(ipscData []byte, burstType byte) (uint, uint, bool) {
	if len(ipscData) < 50 {
		return 0, 0, false
	}

	var dataType elements.DataType
	switch burstType {
	case ipscBurstVoiceHead:
		dataType = elements.DataTypeVoiceLCHeader
	case ipscBurstVoiceTerm:
		dataType = elements.DataTypeTerminatorWithLC
	default:
		return 0, 0, false
	}

	var infoBits [96]byte
	for idx, b := range ipscData[38:50] {
		for bit := 0; bit < 8; bit++ {
			infoBits[idx*8+bit] = (b >> (7 - bit)) & 1
		}
	}

	var flc pdu.FullLinkControl
	if !flc.DecodeFromBits(infoBits[:], dataType) {
		return 0, 0, false
	}
	if flc.SourceAddress <= 0 {
		return 0, 0, false
	}

	src := uint(flc.SourceAddress)
	dst := uint(0)
	switch flc.FLCO {
	case enums.FLCOGroupVoiceChannelUser:
		if flc.GroupAddress <= 0 {
			return 0, 0, false
		}
		dst = uint(flc.GroupAddress)
	case enums.FLCOUnitToUnitVoiceChannelUser:
		if flc.TargetAddress <= 0 {
			return 0, 0, false
		}
		dst = uint(flc.TargetAddress)
	default:
		return 0, 0, false
	}

	return src, dst, true
}

// extractFullLCBytes builds 12 bytes of Full Link Control data
// from the packet fields, using the dmrgo library's encoder.
func extractFullLCBytes(pkt mmdvm.Packet) [12]byte {
	return buildStandardLCBytes(pkt.Src, pkt.Dst, pkt.GroupCall)
}

// reverseStreamState tracks per-call state for IPSC→MMDVM translation.
type reverseStreamState struct {
	streamID          uint32
	seq               uint8
	burstIndex        int // 0-5 -> A-F within a superframe
	started           bool
	peerID            uint32
	src               uint
	dst               uint
	slot              bool
	groupCall         bool
	lastSeen          time.Time
	embeddedFragments [4][4]byte // pre-computed embedded LC fragments for bursts B-E
	hasEmbeddedLC     bool       // true if embeddedFragments is valid
}

const reverseStreamReuseWindow = 1500 * time.Millisecond

func (t *IPSCTranslator) findRecentReverseStream(src, dst uint, groupCall, slot bool, now time.Time) *reverseStreamState {
	for _, rss := range t.reverseStreams {
		if rss == nil {
			continue
		}
		if rss.src != src || rss.dst != dst || rss.groupCall != groupCall || rss.slot != slot {
			continue
		}
		if rss.lastSeen.IsZero() || now.Sub(rss.lastSeen) > reverseStreamReuseWindow {
			continue
		}
		return rss
	}
	return nil
}

func (t *IPSCTranslator) findRecentReverseStreamByPeer(peerID uint32, groupCall, slot bool, now time.Time) *reverseStreamState {
	for _, rss := range t.reverseStreams {
		if rss == nil {
			continue
		}
		if rss.peerID != peerID || rss.groupCall != groupCall || rss.slot != slot {
			continue
		}
		if rss.lastSeen.IsZero() || now.Sub(rss.lastSeen) > reverseStreamReuseWindow {
			continue
		}
		return rss
	}
	return nil
}

func (t *IPSCTranslator) removeReverseStreamAliases(target *reverseStreamState) {
	if target == nil {
		return
	}
	for key, rss := range t.reverseStreams {
		if rss == target {
			delete(t.reverseStreams, key)
		}
	}
	if t.metrics != nil {
		t.metrics.TranslatorActiveStreams.WithLabelValues("ipsc_to_mmdvm").Dec()
	}
}

// TranslateToMMDVM converts raw IPSC user packet data into MMDVM DMRD Packets.
// Returns nil if the packet cannot be translated.
func (t *IPSCTranslator) TranslateToMMDVM(packetType byte, data []byte) []mmdvm.Packet {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(data) < 30 {
		slog.Debug("IPSCTranslator: IPSC packet too short", "length", len(data))
		return nil
	}

	// Handle voice (0x80/0x81) and data (0x83/0x84) packet types
	switch packetType {
	case 0x80, 0x81, 0x83, 0x84:
		// OK — supported packet types
	default:
		slog.Debug("IPSCTranslator: ignoring unsupported IPSC packet", "type", packetType)
		return nil
	}

	// Parse the IPSC header
	peerID := binary.BigEndian.Uint32(data[1:5])
	src := uint(data[6])<<16 | uint(data[7])<<8 | uint(data[8])
	dst := uint(data[9])<<16 | uint(data[10])<<8 | uint(data[11])
	groupCall := packetType == 0x80 || packetType == 0x83
	callInfo := data[17]
	slot := (callInfo & 0x20) != 0 // true = TS2
	isEnd := (callInfo & 0x40) != 0

	// Determine what kind of IPSC burst this is from byte 30
	burstType := data[30]
	hasTrustedLCAddresses := false
	if lcSrc, lcDst, ok := recoverAddressesFromFullLC(data, burstType); ok {
		src = lcSrc
		dst = lcDst
		hasTrustedLCAddresses = true
	}

	slog.Debug("IPSCTranslator: TranslateToMMDVM",
		"packetType", fmt.Sprintf("0x%02X", packetType),
		"src", src, "dst", dst, "groupCall", groupCall,
		"slot", slot, "isEnd", isEnd)

	// Use call control bytes as stream identifier
	callControl := binary.BigEndian.Uint32(data[13:17])
	now := time.Now()

	// Get or create reverse stream state
	rss, ok := t.reverseStreams[callControl]
	if !ok {
		// Moto repeaters can drift callControl across duplicate headers and early
		// voice bursts. Reuse a very recent stream with the same routing tuple.
		rss = t.findRecentReverseStream(src, dst, groupCall, slot, now)
		if rss == nil && !hasTrustedLCAddresses && burstType != ipscBurstVoiceHead {
			// Some repeaters also drift src/dst bytes inside the transport header
			// while the actual call on the same peer/slot is still in progress.
			// Do not apply this fallback to a fresh voice header that already
			// carries a decodable Full LC, otherwise a new call can inherit the
			// previous call's src/dst for a short reuse window.
			rss = t.findRecentReverseStreamByPeer(peerID, groupCall, slot, now)
		}
		if rss != nil {
			t.reverseStreams[callControl] = rss
		}
	}
	if rss == nil {
		t.nextStreamID++
		if t.nextStreamID == 0 {
			t.nextStreamID = 1
		}
		rss = &reverseStreamState{
			streamID:  t.nextStreamID,
			peerID:    peerID,
			src:       src,
			dst:       dst,
			slot:      slot,
			groupCall: groupCall,
			lastSeen:  now,
		}
		t.reverseStreams[callControl] = rss
		if t.metrics != nil {
			t.metrics.TranslatorActiveStreams.WithLabelValues("ipsc_to_mmdvm").Inc()
		}
	}
	if rss.src != 0 {
		src = rss.src
	}
	if rss.dst != 0 {
		dst = rss.dst
	}
	rss.lastSeen = now

	var results []mmdvm.Packet

	switch burstType {
	case ipscBurstVoiceHead:
		// Voice LC Header — only process the first one (IPSC sends 3)
		if !rss.started {
			pkt := t.buildMMDVMDataPacket(src, dst, groupCall, slot, rss,
				elements.DataTypeVoiceLCHeader, data)
			results = append(results, pkt)
			rss.started = true
			rss.burstIndex = 0
			// Pre-compute embedded LC fragments from the Full LC so that
			// voice bursts B-E carry consistent embedded signalling even
			// when the IPSC packet lacks trailing embedded data.
			rss.embeddedFragments = encodeEmbeddedLC(src, dst, groupCall)
			rss.hasEmbeddedLC = true
		}
		// Skip duplicate headers

	case ipscBurstVoiceTerm:
		// Some repeater variants can emit burst type 0x02 without indicating
		// a real call end in callInfo. Only treat it as terminator when end
		// flag is present; otherwise keep the stream alive.
		if !isEnd {
			break
		}
		// Voice Terminator
		pkt := t.buildMMDVMDataPacket(src, dst, groupCall, slot, rss,
			elements.DataTypeTerminatorWithLC, data)
		results = append(results, pkt)
		// Clean up
		t.removeReverseStreamAliases(rss)

	case ipscBurstSlot1, ipscBurstSlot2:
		// Voice burst — extract AMBE, FEC-encode, build DMR burst
		if len(data) < 52 {
			slog.Debug("IPSCTranslator: voice burst too short", "length", len(data))
			return nil
		}

		pkts := t.buildMMDVMVoiceBurst(src, dst, groupCall, slot, rss, data)
		results = append(results, pkts...)

	case ipscBurstCSBK:
		// CSBK or data burst — same 54-byte structure as voice header
		pkt := t.buildMMDVMDataPacket(src, dst, groupCall, slot, rss,
			elements.DataTypeCSBK, data)
		results = append(results, pkt)

	default:
		// Treat any other burst type as a generic data packet if it has
		// the same structure as a voice header (54 bytes with LC data).
		// The burst type byte maps directly to the DMR data type.
		if len(data) >= 50 && burstType <= 10 {
			pkt := t.buildMMDVMDataPacket(src, dst, groupCall, slot, rss,
				elements.DataType(burstType), data)
			results = append(results, pkt)
		} else {
			slog.Debug("IPSCTranslator: unknown IPSC burst type", "burstType", burstType)
			return nil
		}
	}

	// Some Moto implementations may set callInfo bits differently from the
	// upstream bridge assumptions. Only explicit terminator bursts should end
	// a reverse stream; otherwise we can split one call into many streams.

	if t.metrics != nil && len(results) > 0 {
		t.metrics.TranslatorPackets.WithLabelValues("ipsc_to_mmdvm").Add(float64(len(results)))
	}

	return results
}

// buildMMDVMDataPacket builds an MMDVM DMRD packet for a voice LC header, terminator,
// or data burst (CSBK, Data Header, etc.).
// It constructs the 33-byte DMR burst from the IPSC payload data using BPTC encoding.
func (t *IPSCTranslator) buildMMDVMDataPacket(
	src, dst uint, groupCall, slot bool,
	rss *reverseStreamState,
	dataType elements.DataType,
	ipscData []byte,
) mmdvm.Packet {
	pkt := mmdvm.Packet{
		Signature:   "DMRD",
		Seq:         uint(rss.seq),
		Src:         src,
		Dst:         dst,
		Repeater:    uint(t.repeaterID),
		Slot:        slot,
		GroupCall:   groupCall,
		FrameType:   mmdvmFrameTypeDataSync,
		DTypeOrVSeq: uint(dataType),
		StreamID:    uint(rss.streamID),
	}
	rss.seq++

	var lcBytes [12]byte
	if len(ipscData) >= 50 {
		// Prefer the Moto/IPSC payload as-is when building the outbound MMDVM
		// data burst. Some BM paths appear to depend on the original radio LC
		// bytes rather than a reconstructed standards-compliant variant.
		copy(lcBytes[:], ipscData[38:50])
	} else {
		lcBytes = buildStandardLCBytes(src, dst, groupCall)
	}

	// When we had to synthesize the LC bytes, ensure the call type matches
	// the routing tuple.
	if len(ipscData) < 50 && (dataType == elements.DataTypeVoiceLCHeader || dataType == elements.DataTypeTerminatorWithLC) {
		if groupCall {
			lcBytes[0] = byte(enums.FLCOGroupVoiceChannelUser)
		} else {
			lcBytes[0] = byte(enums.FLCOUnitToUnitVoiceChannelUser)
		}
	}

	// Build the 33-byte DMR data burst using correct BPTC(196,96).
	pkt.DMRData = bptc.BuildLCDataBurst(lcBytes, uint8(dataType), t.colorCode)

	return pkt
}

// buildMMDVMVoiceBurst builds MMDVM DMRD packets from an IPSC voice burst.
// It extracts the 19-byte AMBE payload, FEC-encodes back to DMR format,
// and reconstructs the full 33-byte DMR burst with proper sync/EMB.
func (t *IPSCTranslator) buildMMDVMVoiceBurst(
	src, dst uint, groupCall, slot bool,
	rss *reverseStreamState,
	ipscData []byte,
) []mmdvm.Packet {
	// Extract the 19-byte AMBE data from IPSC packet (bytes 33-51)
	var ambeBytes [19]byte
	copy(ambeBytes[:], ipscData[33:52])

	// Unpack into 3 VocoderFrames (49 bits each)
	frames := intdmr.UnpackAMBEVoiceFrames(ambeBytes)

	// Build a vocoder PDU
	var vc pdu.Vocoder
	for i := range frames {
		vc.Frames[i].DecodedBits = frames[i]
	}

	// The vocoder Encode() FEC-encodes the 3×49 bit frames back to 3×72 = 216 bits
	voiceBits := vc.Encode()

	// Determine if this is a sync burst (A) or embedded signalling burst (B-F)
	burstIdx := rss.burstIndex % 6

	var burst layer2.Burst
	burst.VoiceData = vc

	if burstIdx == 0 {
		// Burst A — voice sync burst
		burst.SyncPattern = enums.MsSourcedVoice
		burst.VoiceBurst = enums.VoiceBurstA
		burst.HasEmbeddedSignalling = false
	} else {
		// Bursts B-F — embedded signalling
		burst.SyncPattern = enums.EmbeddedSignallingPattern
		burst.HasEmbeddedSignalling = true

		switch burstIdx {
		case 1:
			burst.VoiceBurst = enums.VoiceBurstB
		case 2:
			burst.VoiceBurst = enums.VoiceBurstC
		case 3:
			burst.VoiceBurst = enums.VoiceBurstD
		case 4:
			burst.VoiceBurst = enums.VoiceBurstE
		case 5:
			burst.VoiceBurst = enums.VoiceBurstF
		}

		// Extract embedded signalling from the IPSC packet if available,
		// falling back to pre-computed fragments from the Full LC header.
		t.populateEmbeddedSignalling(&burst, burstIdx, ipscData, rss)
	}

	_ = voiceBits // voiceBits used internally by burst.Encode() via vc

	// Encode the burst to 33 bytes
	dmrData := burst.Encode()

	// Determine frame type
	if burstIdx < 0 {
		burstIdx = 0
	}

	frameType := mmdvmFrameTypeVoice
	if burstIdx == 0 {
		frameType = mmdvmFrameTypeVoiceSync
	}

	pkt := mmdvm.Packet{
		Signature:   "DMRD",
		Seq:         uint(rss.seq),
		Src:         src,
		Dst:         dst,
		Repeater:    uint(t.repeaterID),
		Slot:        slot,
		GroupCall:   groupCall,
		FrameType:   frameType,
		DTypeOrVSeq: uint(burstIdx), //nolint:gosec // Bounds checked
		StreamID:    uint(rss.streamID),
		DMRData:     dmrData,
	}
	rss.seq++
	rss.burstIndex = (rss.burstIndex + 1) % 6

	return []mmdvm.Packet{pkt}
}

// populateEmbeddedSignalling fills in the embedded signalling fields
// for voice bursts B-F. Prefer the raw Moto/IPSC embedded bytes first and
// only fall back to header-derived fragments when the burst does not carry
// usable trailing embedded signalling bytes.
func (t *IPSCTranslator) populateEmbeddedSignalling(burst *layer2.Burst, burstIdx int, ipscData []byte, rss *reverseStreamState) {
	burst.EmbeddedSignalling = pdu.EmbeddedSignalling{
		ColorCode:                          int(t.colorCode),
		PreemptionAndPowerControlIndicator: false,
		LCSS:                               enums.ContinuationFragmentLCorCSBK,
		ParityOK:                           true,
	}

	// Set LCSS based on burst position
	switch burstIdx {
	case 1: // Burst B — first fragment
		burst.EmbeddedSignalling.LCSS = enums.FirstFragmentLC
	case 4: // Burst E — last fragment
		burst.EmbeddedSignalling.LCSS = enums.LastFragmentLCorCSBK
	default: // Bursts C, D, F — continuation
		burst.EmbeddedSignalling.LCSS = enums.ContinuationFragmentLCorCSBK
	}

	// Prefer raw IPSC trailing bytes first.
	var embBytes []byte
	switch len(ipscData) {
	case 57: // Bursts B, C, D, F — 5 bytes of embedded data at [52:57]
		embBytes = ipscData[52:57]
	case 66: // Burst E — embedded data at [52:59]
		embBytes = ipscData[52:59]
	}

	if len(embBytes) >= 4 {
		candidate := embBytes[:4]
		nonZero := false
		for _, b := range candidate {
			if b != 0x00 {
				nonZero = true
				break
			}
		}
		if nonZero {
			burst.UnpackEmbeddedSignallingData(candidate)
			return
		}
		streamID := uint32(0)
		src := uint(0)
		dst := uint(0)
		if rss != nil {
			streamID = rss.streamID
			src = rss.src
			dst = rss.dst
		}
		slog.Debug("IPSCTranslator: zero trailing embedded bytes, falling back to reconstructed embedded LC",
			"burstIdx", burstIdx,
			"streamID", streamID,
			"src", src,
			"dst", dst,
			"packetLen", len(ipscData))
	}

	// Fall back to pre-computed embedded LC fragments from the header when
	// the repeater does not provide usable trailing embedded bytes.
	if rss != nil && rss.hasEmbeddedLC {
		fragIdx := burstIdx - 1
		if fragIdx < 0 || fragIdx > 3 {
			fragIdx = 0
		}
		burst.UnpackEmbeddedSignallingData(rss.embeddedFragments[fragIdx][:])
	}
}

// encodeEmbeddedLC computes the 4 embedded LC fragments (each 4 bytes / 32 bits)
// from the call addressing fields.  The encoding follows ETSI TS 102 361-1
// Annex B.2 (variable-length BPTC for embedded signalling):
//
//  1. Build 72-bit LC: FLCO(8) + FID(8) + SO(8) + Dst(24) + Src(24)
//  2. Compute 5-bit CRC → 77 info bits
//  3. Place into 8×16 BPTC matrix (7 data rows + 1 column-parity row,
//     each row protected by Hamming(16,11,4))
//  4. Read out column-wise into 4 fragments of 32 bits
func encodeEmbeddedLC(src, dst uint, groupCall bool) [4][4]byte {
	// --- Step 1: build 9-byte (72-bit) LC ---
	var lc [9]byte
	if groupCall {
		lc[0] = 0x00 // FLCO = Group Voice Channel User
	} else {
		lc[0] = 0x03 // FLCO = Unit to Unit Voice Channel User
	}
	lc[1] = 0x00 // FID = Standard
	lc[2] = 0x20 // Service Options (default)
	lc[3] = byte(dst >> 16)
	lc[4] = byte(dst >> 8)
	lc[5] = byte(dst)
	lc[6] = byte(src >> 16)
	lc[7] = byte(src >> 8)
	lc[8] = byte(src)

	// --- Step 2: convert to 72 bits and compute 5-bit CRC ---
	var bits [77]byte
	for i := range 9 {
		for j := range 8 {
			bits[i*8+j] = (lc[i] >> (7 - j)) & 1
		}
	}
	crc := embeddedLCCRC5(bits[:72])
	for i := range 5 {
		bits[72+i] = (crc >> (4 - i)) & 1
	}

	// --- Step 3: build 8×16 BPTC matrix ---
	var matrix [8][16]byte

	// Rows 0-6: 11 data bits each (7 × 11 = 77)
	idx := 0
	for r := range 7 {
		for c := range 11 {
			matrix[r][c] = bits[idx]
			idx++
		}
	}

	// Row 7: column parity (even parity over rows 0-6)
	for c := range 11 {
		p := byte(0)
		for r := range 7 {
			p ^= matrix[r][c]
		}
		matrix[7][c] = p
	}

	// Hamming(16,11,4) parity for each row — uses the same generator
	// as BPTC(196,96) row parity plus an overall parity bit.
	for r := range 8 {
		d := matrix[r][:11]
		matrix[r][11] = d[0] ^ d[1] ^ d[2] ^ d[3] ^ d[5] ^ d[7] ^ d[8]
		matrix[r][12] = d[1] ^ d[2] ^ d[3] ^ d[4] ^ d[6] ^ d[8] ^ d[9]
		matrix[r][13] = d[2] ^ d[3] ^ d[4] ^ d[5] ^ d[7] ^ d[9] ^ d[10]
		matrix[r][14] = d[0] ^ d[1] ^ d[2] ^ d[4] ^ d[6] ^ d[7] ^ d[10]
		// Column 15: overall row parity (Hamming SEC-DED extension)
		p := byte(0)
		for c := range 15 {
			p ^= matrix[r][c]
		}
		matrix[r][15] = p
	}

	// --- Step 4: read column-wise into 4 packed fragments ---
	// Fragment f covers columns [f*4 .. f*4+3], each column has 8 rows.
	var fragments [4][4]byte
	for f := range 4 {
		bitIdx := 0
		for c := 0; c < 4; c++ {
			for r := 0; r < 8; r++ {
				if matrix[r][f*4+c] == 1 {
					fragments[f][bitIdx/8] |= 1 << (7 - (bitIdx % 8))
				}
				bitIdx++
			}
		}
	}
	return fragments
}

// embeddedLCCRC5 computes the 5-bit CRC for the embedded LC per
// ETSI TS 102 361-1 B.3.11.  Generator polynomial:
// G(x) = x^5 + x^4 + x^2 + 1  (0x15 with MSB implicit).
func embeddedLCCRC5(bits []byte) byte {
	var reg byte
	for _, bit := range bits {
		msb := (reg >> 4) & 1
		reg = ((reg << 1) & 0x1F) | (bit & 1)
		if msb == 1 {
			reg ^= 0x15
		}
	}
	// ETSI TS 102 361-1 B.3.11: final inversion (XOR with all ones)
	return (reg & 0x1F) ^ 0x1F
}
