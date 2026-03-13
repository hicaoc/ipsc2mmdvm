package ipsc

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/USA-RedDragon/dmrgo/dmr/enums"
	"github.com/USA-RedDragon/dmrgo/dmr/layer2"
	"github.com/USA-RedDragon/dmrgo/dmr/layer2/elements"
	"github.com/USA-RedDragon/dmrgo/dmr/layer2/pdu"
	internalbptc "github.com/hicaoc/ipsc2mmdvm/internal/dmr/bptc"
	mmdvm "github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

func newTestTranslator(t *testing.T) *IPSCTranslator {
	t.Helper()
	tr, err := NewIPSCTranslator()
	if err != nil {
		t.Fatalf("NewIPSCTranslator() error: %v", err)
	}
	tr.SetPeerID(12345)
	return tr
}

func TestNewIPSCTranslator(t *testing.T) {
	t.Parallel()
	tr, err := NewIPSCTranslator()
	if err != nil {
		t.Fatalf("NewIPSCTranslator() error: %v", err)
	}
	if tr == nil {
		t.Fatal("expected non-nil translator")
	}
	if tr.streams == nil {
		t.Fatal("expected non-nil streams map")
	}
	if tr.reverseStreams == nil {
		t.Fatal("expected non-nil reverseStreams map")
	}
}

func TestSetPeerID(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	if tr.peerID != 12345 {
		t.Fatalf("expected peerID 12345, got %d", tr.peerID)
	}
	if tr.repeaterID != 12345 {
		t.Fatalf("expected repeaterID 12345, got %d", tr.repeaterID)
	}
}

func TestCleanupStream(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Create some stream state by translating a voice header
	pkt := makeTestMMDVMPacket(true, true, mmdvmFrameTypeDataSync, 1) // VoiceLCHeader=1
	tr.TranslateToIPSC(pkt)

	streamID := uint32(pkt.StreamID) //nolint:gosec // test value is within uint32 range

	tr.mu.Lock()
	_, exists := tr.streams[streamID]
	tr.mu.Unlock()

	if !exists {
		t.Fatal("expected stream state to exist after translate")
	}

	tr.CleanupStream(streamID)

	tr.mu.Lock()
	_, exists = tr.streams[streamID]
	tr.mu.Unlock()

	if exists {
		t.Fatal("expected stream state to be removed after cleanup")
	}
}

func makeTestMMDVMPacket(groupCall, slot bool, frameType, dtypeOrVSeq uint) mmdvm.Packet {
	return mmdvm.Packet{
		Signature:   "DMRD",
		Seq:         0,
		Src:         100,
		Dst:         200,
		Repeater:    3001,
		Slot:        slot,
		GroupCall:   groupCall,
		FrameType:   frameType,
		DTypeOrVSeq: dtypeOrVSeq,
		StreamID:    0x1234,
		DMRData:     [33]byte{},
	}
}

func TestTranslateToIPSCNilOnUnknownFrameType(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, 3, 0) // frameType=3 is unknown
	result := tr.TranslateToIPSC(pkt)
	if result != nil {
		t.Fatalf("expected nil for unknown frame type, got %d packets", len(result))
	}
}

func TestTranslateToIPSCVoiceHeaderProducesStartupPackets(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	// DataTypeVoiceLCHeader = 1
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	result := tr.TranslateToIPSC(pkt)
	if len(result) != ipscVoiceHeaderRepeats {
		t.Fatalf("expected %d voice header packets, got %d", ipscVoiceHeaderRepeats, len(result))
	}
}

func TestTranslateToIPSCVoiceTerminator(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	// First send a header to establish stream
	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// DataTypeTerminatorWithLC = 2
	term := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 2)
	term.StreamID = header.StreamID
	result := tr.TranslateToIPSC(term)
	if len(result) != 1 {
		t.Fatalf("expected 1 terminator packet, got %d", len(result))
	}
}

func TestTranslateToIPSCReusesRecentForwardStreamForVoiceBurst(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	header.StreamID = 0x1001
	headerPackets := tr.TranslateToIPSC(header)
	if len(headerPackets) != ipscVoiceHeaderRepeats {
		t.Fatalf("expected %d header packets, got %d", ipscVoiceHeaderRepeats, len(headerPackets))
	}
	headerCallControl := binary.BigEndian.Uint32(headerPackets[0][13:17])

	voice := makeTestMMDVMPacket(true, false, mmdvmFrameTypeVoiceSync, 0)
	voice.StreamID = 0x1002
	voice.Src = header.Src
	voice.Dst = header.Dst
	voicePackets := tr.TranslateToIPSC(voice)
	if len(voicePackets) != 1 {
		t.Fatalf("expected 1 voice packet, got %d", len(voicePackets))
	}
	voiceCallControl := binary.BigEndian.Uint32(voicePackets[0][13:17])
	if voiceCallControl != headerCallControl {
		t.Fatalf("expected reused callControl %d, got %d", headerCallControl, voiceCallControl)
	}
}

func TestTranslateToIPSCGroupCallFlag(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Group call
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 1 {
		t.Fatal("expected at least 1 packet")
	}
	if result[0][0] != 0x80 {
		t.Fatalf("expected group voice type 0x80, got 0x%02X", result[0][0])
	}

	// Private call
	tr2 := newTestTranslator(t)
	pkt2 := makeTestMMDVMPacket(false, false, mmdvmFrameTypeDataSync, 1)
	pkt2.StreamID = 0x5678
	result2 := tr2.TranslateToIPSC(pkt2)
	if len(result2) < 1 {
		t.Fatal("expected at least 1 packet")
	}
	if result2[0][0] != 0x81 {
		t.Fatalf("expected private voice type 0x81, got 0x%02X", result2[0][0])
	}
}

func TestTranslateToIPSCPeerIDInHeader(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 1 {
		t.Fatal("expected at least 1 packet")
	}
	peerID := binary.BigEndian.Uint32(result[0][1:5])
	if peerID != 12345 {
		t.Fatalf("expected peer ID 12345 in header, got %d", peerID)
	}
}

func TestTranslateToIPSCSlotFlag(t *testing.T) {
	t.Parallel()

	// TS1 (Slot=false)
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 1 {
		t.Fatal("expected packets")
	}
	callInfo := result[0][17]
	if callInfo&0x20 != 0 {
		t.Fatalf("expected TS1 (slot bit clear), got callInfo %02X", callInfo)
	}

	// TS2 (Slot=true)
	tr2 := newTestTranslator(t)
	pkt2 := makeTestMMDVMPacket(true, true, mmdvmFrameTypeDataSync, 1)
	pkt2.StreamID = 0x9999
	result2 := tr2.TranslateToIPSC(pkt2)
	if len(result2) < 1 {
		t.Fatal("expected packets")
	}
	callInfo2 := result2[0][17]
	if callInfo2&0x20 == 0 {
		t.Fatalf("expected TS2 (slot bit set), got callInfo %02X", callInfo2)
	}
}

func TestTranslateToIPSCSrcDstInHeader(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	pkt.Src = 0x123456
	pkt.Dst = 0xABCDEF
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 1 {
		t.Fatal("expected packets")
	}
	src := uint(result[0][6])<<16 | uint(result[0][7])<<8 | uint(result[0][8])
	dst := uint(result[0][9])<<16 | uint(result[0][10])<<8 | uint(result[0][11])
	if src != 0x123456 {
		t.Fatalf("expected src 0x123456, got 0x%06X", src)
	}
	if dst != 0xABCDEF {
		t.Fatalf("expected dst 0xABCDEF, got 0x%06X", dst)
	}
}

func TestTranslateToMMDVMTooShort(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	result := tr.TranslateToMMDVM(0x80, make([]byte, 10))
	if result != nil {
		t.Fatal("expected nil for too-short IPSC packet")
	}
}

func TestTranslateToMMDVMUnsupportedType(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	result := tr.TranslateToMMDVM(0x99, make([]byte, 54))
	if result != nil {
		t.Fatal("expected nil for unsupported packet type")
	}
}

func makeTestIPSCPacket(packetType byte, burstType byte, groupCall, slot bool) []byte {
	buf := make([]byte, 54)
	buf[0] = packetType

	// Peer ID
	binary.BigEndian.PutUint32(buf[1:5], 99999)

	// Src (bytes 6-8) — hardcoded test source ID
	const src uint = 100
	const dst uint = 200
	buf[6] = byte(src >> 16)
	buf[7] = byte(src >> 8)
	buf[8] = byte(src)

	// Dst (bytes 9-11)
	buf[9] = byte(dst >> 16)
	buf[10] = byte(dst >> 8)
	buf[11] = byte(dst)

	// Call type
	if groupCall {
		buf[12] = 0x02
	} else {
		buf[12] = 0x01
	}

	// Call control (bytes 13-16) - unique per call
	binary.BigEndian.PutUint32(buf[13:17], 0xAAAA)

	// Call info (byte 17)
	callInfo := byte(0x00)
	if slot {
		callInfo |= 0x20
	}
	buf[17] = callInfo

	// RTP header stub (bytes 18-29)
	buf[18] = 0x80

	// Burst type (byte 30)
	buf[30] = burstType

	return buf
}

func TestTranslateToMMDVMVoiceHeader(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	result := tr.TranslateToMMDVM(0x80, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for voice header, got %d", len(result))
	}
	pkt := result[0]
	if pkt.Signature != "DMRD" {
		t.Fatalf("expected DMRD signature, got %q", pkt.Signature)
	}
	if pkt.FrameType != mmdvmFrameTypeDataSync {
		t.Fatalf("expected frame type %d (data sync), got %d", mmdvmFrameTypeDataSync, pkt.FrameType)
	}
	if pkt.Src != 100 {
		t.Fatalf("expected src 100, got %d", pkt.Src)
	}
	if pkt.Dst != 200 {
		t.Fatalf("expected dst 200, got %d", pkt.Dst)
	}
}

func TestTranslateToMMDVMVoiceHeaderPrefersRawIPSCLCBytes(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	data[38] = 0xFF
	data[39] = 0xEE
	data[40] = 0xDD
	data[41] = 0xCC
	data[42] = 0xBB
	data[43] = 0xAA
	data[44] = 0x99
	data[45] = 0x88
	data[46] = 0x77
	data[47] = 0x66
	data[48] = 0x55
	data[49] = 0x44

	result := tr.TranslateToMMDVM(0x80, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for voice header, got %d", len(result))
	}

	var rawLC [12]byte
	copy(rawLC[:], data[38:50])
	expected := internalbptc.BuildLCDataBurst(rawLC, uint8(elements.DataTypeVoiceLCHeader), 0)
	if result[0].DMRData != expected {
		t.Fatal("expected voice header LC to preserve raw IPSC payload bytes")
	}
}

func TestTranslateToMMDVMVoiceHeaderPrefersFullLCAddresses(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)

	// Corrupt IPSC transport header src/dst.
	data[6] = 0x40
	data[7] = 0xFF
	data[8] = 0x14
	data[9] = 0x00
	data[10] = 0x42
	data[11] = 0x00

	// Keep the Full LC payload correct.
	lc := buildStandardLCBytes(4604111, 46025, true)
	copy(data[38:50], lc[:])

	result := tr.TranslateToMMDVM(0x80, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for voice header, got %d", len(result))
	}
	if result[0].Src != 4604111 {
		t.Fatalf("expected src from Full LC 4604111, got %d", result[0].Src)
	}
	if result[0].Dst != 46025 {
		t.Fatalf("expected dst from Full LC 46025, got %d", result[0].Dst)
	}
}

func TestTranslateToMMDVMVoiceBurstReusesRecentPeerStreamWhenHeaderBytesDrift(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	headerLC := buildStandardLCBytes(4604111, 46025, true)
	copy(header[38:50], headerLC[:])
	if result := tr.TranslateToMMDVM(0x80, header); len(result) != 1 {
		t.Fatalf("expected 1 packet for header, got %d", len(result))
	}

	voice := makeTestIPSCPacket(0x80, ipscBurstSlot1, true, false)
	binary.BigEndian.PutUint32(voice[13:17], 0xBBBB)
	voice[6] = 0x40
	voice[7] = 0xFF
	voice[8] = 0x14
	voice[9] = 0x00
	voice[10] = 0x42
	voice[11] = 0x00

	result := tr.TranslateToMMDVM(0x80, voice)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for voice burst, got %d", len(result))
	}
	if result[0].Src != 4604111 {
		t.Fatalf("expected src to reuse recent stream 4604111, got %d", result[0].Src)
	}
	if result[0].Dst != 46025 {
		t.Fatalf("expected dst to reuse recent stream 46025, got %d", result[0].Dst)
	}
}

func TestTranslateToMMDVMDuplicateHeaderSkipped(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)

	// First header should produce a packet
	result := tr.TranslateToMMDVM(0x80, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for first header, got %d", len(result))
	}

	// Second header with same call control should be skipped
	result = tr.TranslateToMMDVM(0x80, data)
	if len(result) != 0 {
		t.Fatalf("expected 0 packets for duplicate header, got %d", len(result))
	}
}

func TestTranslateToMMDVMHeaderWithChangedCallControlReused(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	first := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(first[13:17], 0x1111)
	result := tr.TranslateToMMDVM(0x80, first)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for first header, got %d", len(result))
	}

	second := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(second[13:17], 0x2222)
	result = tr.TranslateToMMDVM(0x80, second)
	if len(result) != 0 {
		t.Fatalf("expected changed-call-control duplicate header to be skipped, got %d packets", len(result))
	}
}

func TestTranslateToMMDVMHeaderWithNewFullLCDoesNotReusePreviousPeerStream(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	first := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(first[13:17], 0x1111)
	firstLC := buildStandardLCBytes(4604111, 46025, true)
	copy(first[38:50], firstLC[:])
	result := tr.TranslateToMMDVM(0x80, first)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for first header, got %d", len(result))
	}
	if result[0].Src != 4604111 || result[0].Dst != 46025 {
		t.Fatalf("expected first header addresses 4604111/46025, got %d/%d", result[0].Src, result[0].Dst)
	}

	second := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(second[13:17], 0x2222)
	secondLC := buildStandardLCBytes(4601816, 46026, true)
	copy(second[38:50], secondLC[:])
	result = tr.TranslateToMMDVM(0x80, second)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for new header, got %d", len(result))
	}
	if result[0].Src != 4601816 || result[0].Dst != 46026 {
		t.Fatalf("expected new header addresses 4601816/46026, got %d/%d", result[0].Src, result[0].Dst)
	}
}

func TestTranslateToMMDVMVoiceTerminator(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Send header first to establish stream
	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	tr.TranslateToMMDVM(0x80, header)

	// Send terminator
	term := makeTestIPSCPacket(0x80, ipscBurstVoiceTerm, true, false)
	term[17] |= 0x40 // end flag
	result := tr.TranslateToMMDVM(0x80, term)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for terminator, got %d", len(result))
	}
	if result[0].DTypeOrVSeq != 2 { // DataTypeTerminatorWithLC = 2
		t.Fatalf("expected dtype 2 (terminator), got %d", result[0].DTypeOrVSeq)
	}
}

func TestTranslateToMMDVMVoiceTerminatorPrefersRawIPSCLCBytes(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(header[13:17], 0xABCD)
	tr.TranslateToMMDVM(0x80, header)

	term := makeTestIPSCPacket(0x80, ipscBurstVoiceTerm, true, false)
	binary.BigEndian.PutUint32(term[13:17], 0xABCD)
	term[17] |= 0x40
	for i := 38; i < 50; i++ {
		term[i] = 0xFF
	}

	result := tr.TranslateToMMDVM(0x80, term)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for terminator, got %d", len(result))
	}

	var rawLC [12]byte
	copy(rawLC[:], term[38:50])
	expected := internalbptc.BuildLCDataBurst(rawLC, uint8(elements.DataTypeTerminatorWithLC), 0)
	if result[0].DMRData != expected {
		t.Fatal("expected terminator LC to preserve raw IPSC payload bytes")
	}
}

func TestTranslateToMMDVMVoiceTerminatorWithoutEndIgnored(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Send header first to establish stream
	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(header[13:17], 0xCCDD)
	tr.TranslateToMMDVM(0x80, header)

	// Send terminator-like burst without end flag; should be ignored.
	term := makeTestIPSCPacket(0x80, ipscBurstVoiceTerm, true, false)
	binary.BigEndian.PutUint32(term[13:17], 0xCCDD)
	result := tr.TranslateToMMDVM(0x80, term)
	if len(result) != 0 {
		t.Fatalf("expected 0 packets, got %d", len(result))
	}
}

func TestTranslateToMMDVMPrivateCall(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := makeTestIPSCPacket(0x81, ipscBurstVoiceHead, false, false)
	result := tr.TranslateToMMDVM(0x81, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(result))
	}
	if result[0].GroupCall {
		t.Fatal("expected GroupCall=false for private call")
	}
}

func TestTranslateToMMDVMSlotTS2(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, true)
	// Use a different call control to avoid collision
	binary.BigEndian.PutUint32(data[13:17], 0xBBBB)
	result := tr.TranslateToMMDVM(0x80, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(result))
	}
	if !result[0].Slot {
		t.Fatal("expected Slot=true for TS2")
	}
}

func TestTranslateToMMDVMEndFlagDoesNotCleanupWithoutTerminator(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Send header
	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(header[13:17], 0xCCCC)
	tr.TranslateToMMDVM(0x80, header)

	// Send another packet with end flag set (but not a terminator burst type)
	endPkt := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(endPkt[13:17], 0xCCCC)
	endPkt[17] |= 0x40 // set end flag
	tr.TranslateToMMDVM(0x80, endPkt)

	// Verify the stream was NOT cleaned up just by end flag.
	tr.mu.Lock()
	_, exists := tr.reverseStreams[0xCCCC]
	tr.mu.Unlock()
	if !exists {
		t.Fatal("expected reverse stream to remain active without explicit terminator")
	}
}

func TestTranslateToMMDVMCSBK(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	data := makeTestIPSCPacket(0x83, ipscBurstCSBK, true, false)
	binary.BigEndian.PutUint32(data[13:17], 0xDDDD)
	result := tr.TranslateToMMDVM(0x83, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for CSBK, got %d", len(result))
	}
	if result[0].DTypeOrVSeq != 3 { // DataTypeCSBK = 3
		t.Fatalf("expected dtype 3 (CSBK), got %d", result[0].DTypeOrVSeq)
	}
}

func TestTranslateToMMDVMHeaderUsesConfiguredColorCode(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	tr.SetColorCode(9)

	data := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	result := tr.TranslateToMMDVM(0x80, data)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for voice header, got %d", len(result))
	}

	var burst layer2.Burst
	burst.DecodeFromBytes(result[0].DMRData)
	if !burst.HasSlotType {
		t.Fatal("expected slot type on data sync burst")
	}
	if burst.SlotType.ColorCode != 9 {
		t.Fatalf("expected slot type color code 9, got %d", burst.SlotType.ColorCode)
	}
}

func TestTranslateToMMDVMVoiceEmbeddedUsesConfiguredColorCode(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	tr.SetColorCode(7)

	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(header[13:17], 0xCAFE)
	tr.TranslateToMMDVM(0x80, header)

	// First voice burst (A) to advance burst index.
	burstA := make([]byte, 52)
	copy(burstA[:18], header[:18])
	binary.BigEndian.PutUint32(burstA[13:17], 0xCAFE)
	burstA[30] = ipscBurstSlot1
	burstA[31] = 0x14
	burstA[32] = 0x40
	tr.TranslateToMMDVM(0x80, burstA)

	// Second voice burst (B) carries embedded signalling.
	burstB := make([]byte, 57)
	copy(burstB[:18], header[:18])
	binary.BigEndian.PutUint32(burstB[13:17], 0xCAFE)
	burstB[30] = ipscBurstSlot1
	burstB[31] = 0x19
	burstB[32] = 0x06
	result := tr.TranslateToMMDVM(0x80, burstB)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for voice burst B, got %d", len(result))
	}

	var burst layer2.Burst
	burst.DecodeFromBytes(result[0].DMRData)
	if !burst.HasEmbeddedSignalling {
		t.Fatal("expected embedded signalling on voice burst B")
	}
	if burst.EmbeddedSignalling.ColorCode != 7 {
		t.Fatalf("expected embedded signaling color code 7, got %d", burst.EmbeddedSignalling.ColorCode)
	}
}

func TestTranslateToMMDVMVoiceEmbeddedPrefersRawBytesOverHeaderDerivedLC(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(header[13:17], 0xCAFE)
	tr.TranslateToMMDVM(0x80, header)

	burstA := make([]byte, 52)
	copy(burstA[:18], header[:18])
	binary.BigEndian.PutUint32(burstA[13:17], 0xCAFE)
	burstA[30] = ipscBurstSlot1
	burstA[31] = 0x14
	burstA[32] = 0x40
	tr.TranslateToMMDVM(0x80, burstA)

	burstB := make([]byte, 57)
	copy(burstB[:18], header[:18])
	binary.BigEndian.PutUint32(burstB[13:17], 0xCAFE)
	burstB[30] = ipscBurstSlot1
	burstB[31] = 0x19
	burstB[32] = 0x06
	copy(burstB[52:57], []byte{0xFF, 0xEE, 0xDD, 0xCC, 0xBB})

	result := tr.TranslateToMMDVM(0x80, burstB)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet for voice burst B, got %d", len(result))
	}

	var burst layer2.Burst
	burst.DecodeFromBytes(result[0].DMRData)
	if !burst.HasEmbeddedSignalling {
		t.Fatal("expected embedded signalling on voice burst B")
	}

	got := burst.PackEmbeddedSignallingData()
	want := []byte{0xFF, 0xEE, 0xDD, 0xCC}
	if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("expected raw embedded LC % X, got % X", want, got[:4])
	}
}

func TestExtractFullLCBytesGroupCall(t *testing.T) {
	t.Parallel()
	pkt := mmdvm.Packet{
		GroupCall: true,
		Src:       100,
		Dst:       200,
	}
	lc := extractFullLCBytes(pkt)
	// First byte should be FLCO for group call (0x00)
	if lc[0] != 0x00 {
		t.Fatalf("expected FLCO 0x00 (group), got 0x%02X", lc[0])
	}
}

func TestExtractFullLCBytesPrivateCall(t *testing.T) {
	t.Parallel()
	pkt := mmdvm.Packet{
		GroupCall: false,
		Src:       100,
		Dst:       200,
	}
	lc := extractFullLCBytes(pkt)
	// First byte should be FLCO for unit-to-unit (0x03)
	if lc[0] != 0x03 {
		t.Fatalf("expected FLCO 0x03 (unit-to-unit), got 0x%02X", lc[0])
	}
}

func TestBuildIPSCHeaderDataPacket(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 3) // CSBK
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 1 {
		t.Fatal("expected at least 1 data packet")
	}
	// Data packet should use type 0x83 (group data)
	if result[0][0] != 0x83 {
		t.Fatalf("expected data packet type 0x83, got 0x%02X", result[0][0])
	}
}

func TestBuildIPSCHeaderEndFlag(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	// First send a header
	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// Then send terminator (end flag should be set)
	term := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 2)
	term.StreamID = header.StreamID
	result := tr.TranslateToIPSC(term)
	if len(result) != 1 {
		t.Fatalf("expected 1 terminator packet, got %d", len(result))
	}
	callInfo := result[0][17]
	if callInfo&0x40 == 0 {
		t.Fatalf("expected end flag set in terminator, got callInfo %02X", callInfo)
	}
}

func TestBuildRTPHeader(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	result := tr.TranslateToIPSC(pkt)
	if len(result) < 1 {
		t.Fatal("expected at least 1 packet")
	}
	// RTP version should be 2 (0x80 = version 2, no padding, no ext, 0 CSRCs)
	if result[0][18] != 0x80 {
		t.Fatalf("expected RTP version byte 0x80, got 0x%02X", result[0][18])
	}
}

func TestBuildRTPHeaderNoMarker(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	result := tr.TranslateToIPSC(pkt)
	if len(result) < ipscVoiceHeaderRepeats {
		t.Fatalf("expected %d header packets", ipscVoiceHeaderRepeats)
	}
	// Only the first startup header should carry the marker bit.
	pt := result[ipscVoiceHeaderRepeats-1][19]
	if pt&0x80 != 0 {
		t.Fatalf("expected no marker on final startup header, got PT byte 0x%02X", pt)
	}
}

func TestMultipleStreamsConcurrent(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Start two separate streams
	pkt1 := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	pkt1.StreamID = 0xAAAA
	pkt2 := makeTestMMDVMPacket(true, true, mmdvmFrameTypeDataSync, 1)
	pkt2.StreamID = 0xBBBB

	result1 := tr.TranslateToIPSC(pkt1)
	result2 := tr.TranslateToIPSC(pkt2)

	if len(result1) != ipscVoiceHeaderRepeats {
		t.Fatalf("stream 1: expected %d packets, got %d", ipscVoiceHeaderRepeats, len(result1))
	}
	if len(result2) != ipscVoiceHeaderRepeats {
		t.Fatalf("stream 2: expected %d packets, got %d", ipscVoiceHeaderRepeats, len(result2))
	}

	// Each stream should have its own call control
	cc1 := binary.BigEndian.Uint32(result1[0][13:17])
	cc2 := binary.BigEndian.Uint32(result2[0][13:17])
	if cc1 == cc2 {
		t.Fatal("expected different call control values for different streams")
	}
}

// makeVoiceDMRData builds a 33-byte DMR voice burst (with sync pattern) that
// round-trips through layer2.Burst Decode/Encode. This allows buildVoiceBurst
// and buildMMDVMVoiceBurst to work on realistic data.
func makeVoiceDMRData(syncBurst bool) [33]byte {
	var burst layer2.Burst
	// Set up minimal voice frames (silence-ish)
	burst.VoiceData = pdu.Vocoder{}
	if syncBurst {
		burst.SyncPattern = enums.MsSourcedVoice
		burst.VoiceBurst = enums.VoiceBurstA
		burst.HasEmbeddedSignalling = false
	} else {
		burst.SyncPattern = enums.EmbeddedSignallingPattern
		burst.VoiceBurst = enums.VoiceBurstB
		burst.HasEmbeddedSignalling = true
		burst.EmbeddedSignalling = pdu.EmbeddedSignalling{
			ColorCode:                          0,
			PreemptionAndPowerControlIndicator: false,
			LCSS:                               enums.FirstFragmentLC,
			ParityOK:                           true,
		}
	}
	return burst.Encode()
}

func TestBuildVoiceBurstA(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Send a header first to establish stream
	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// Build a voice sync burst (burst A, index 0)
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeVoiceSync, 0)
	pkt.StreamID = header.StreamID
	pkt.DMRData = makeVoiceDMRData(true)

	result := tr.TranslateToIPSC(pkt)
	if len(result) != 1 {
		t.Fatalf("expected 1 voice burst packet, got %d", len(result))
	}

	// Burst A should be 52 bytes
	if len(result[0]) != 52 {
		t.Fatalf("expected burst A to be 52 bytes, got %d", len(result[0]))
	}

	// Check the slot type byte
	slotByte := result[0][30]
	if slotByte != ipscBurstSlot1 {
		t.Fatalf("expected slot1 burst type 0x%02X, got 0x%02X", ipscBurstSlot1, slotByte)
	}

	// Check length byte
	if result[0][31] != 0x14 {
		t.Fatalf("expected length byte 0x14, got 0x%02X", result[0][31])
	}

	// Unknown field byte
	if result[0][32] != 0x40 {
		t.Fatalf("expected unknown field 0x40, got 0x%02X", result[0][32])
	}
}

func TestBuildVoiceBurstBCDF(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Send a header to establish stream state
	header := makeTestMMDVMPacket(true, true, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// Send burst A to advance burstIndex to 1
	burstA := makeTestMMDVMPacket(true, true, mmdvmFrameTypeVoiceSync, 0)
	burstA.StreamID = header.StreamID
	burstA.DMRData = makeVoiceDMRData(true)
	tr.TranslateToIPSC(burstA)

	// Now send burst B (burstIndex=1) — should produce 57-byte packet
	burstB := makeTestMMDVMPacket(true, true, mmdvmFrameTypeVoice, 1)
	burstB.StreamID = header.StreamID
	burstB.DMRData = makeVoiceDMRData(false)

	result := tr.TranslateToIPSC(burstB)
	if len(result) != 1 {
		t.Fatalf("expected 1 voice burst packet, got %d", len(result))
	}

	// Bursts B,C,D,F should be 57 bytes
	if len(result[0]) != 57 {
		t.Fatalf("expected burst B to be 57 bytes, got %d", len(result[0]))
	}

	// Check slot type byte — TS2
	slotByte := result[0][30]
	if slotByte != ipscBurstSlot2 {
		t.Fatalf("expected slot2 burst type 0x%02X, got 0x%02X", ipscBurstSlot2, slotByte)
	}

	// Check length byte
	if result[0][31] != 0x19 {
		t.Fatalf("expected length byte 0x19, got 0x%02X", result[0][31])
	}

	// Unknown field
	if result[0][32] != 0x06 {
		t.Fatalf("expected unknown field 0x06, got 0x%02X", result[0][32])
	}
}

func TestBuildVoiceBurstE(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Establish stream
	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// Send bursts A-D to advance burstIndex to 4
	for i := 0; i < 4; i++ {
		ft := mmdvmFrameTypeVoice
		if i == 0 {
			ft = mmdvmFrameTypeVoiceSync
		}
		pkt := makeTestMMDVMPacket(true, false, ft, uint(i)) //nolint:gosec // G115: i is in [0,3]
		pkt.StreamID = header.StreamID
		pkt.DMRData = makeVoiceDMRData(i == 0)
		tr.TranslateToIPSC(pkt)
	}

	// Now send burst E (burstIndex=4) — should produce 66-byte packet
	burstE := makeTestMMDVMPacket(true, false, mmdvmFrameTypeVoice, 4)
	burstE.StreamID = header.StreamID
	burstE.DMRData = makeVoiceDMRData(false)
	burstE.Src = 0x112233
	burstE.Dst = 0x445566

	result := tr.TranslateToIPSC(burstE)
	if len(result) != 1 {
		t.Fatalf("expected 1 voice burst packet, got %d", len(result))
	}

	// Burst E should be 66 bytes
	if len(result[0]) != 66 {
		t.Fatalf("expected burst E to be 66 bytes, got %d", len(result[0]))
	}

	// Check length byte
	if result[0][31] != 0x22 {
		t.Fatalf("expected length byte 0x22 for burst E, got 0x%02X", result[0][31])
	}

	// Unknown field
	if result[0][32] != 0x16 {
		t.Fatalf("expected unknown field 0x16 for burst E, got 0x%02X", result[0][32])
	}

	// Check that dst is repeated at bytes 59-61
	dstRepeated := uint(result[0][59])<<16 | uint(result[0][60])<<8 | uint(result[0][61])
	if dstRepeated != 0x445566 {
		t.Fatalf("expected dst 0x445566 at bytes 59-61, got 0x%06X", dstRepeated)
	}

	// Check that src is repeated at bytes 62-64
	srcRepeated := uint(result[0][62])<<16 | uint(result[0][63])<<8 | uint(result[0][64])
	if srcRepeated != 0x112233 {
		t.Fatalf("expected src 0x112233 at bytes 62-64, got 0x%06X", srcRepeated)
	}

	// Trailer byte
	if result[0][65] != 0x14 {
		t.Fatalf("expected trailer byte 0x14, got 0x%02X", result[0][65])
	}
}

func TestBuildVoiceBurstSkipsDataBurst(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Establish stream
	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// Build a DMR data burst (not voice) — the burst decodes as IsData=true
	dataDMR := layer2.BuildLCDataBurst([12]byte{}, elements.DataTypeVoiceLCHeader, 0)

	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeVoice, 0)
	pkt.StreamID = header.StreamID
	pkt.DMRData = dataDMR

	result := tr.TranslateToIPSC(pkt)
	if result != nil {
		t.Fatalf("expected nil for data burst in voice stream, got %d packets", len(result))
	}
}

func TestBuildVoiceBurstWrapsAfterF(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Establish stream
	header := makeTestMMDVMPacket(true, false, mmdvmFrameTypeDataSync, 1)
	tr.TranslateToIPSC(header)

	// Send 6 bursts (A-F) to complete one superframe
	for i := 0; i < 6; i++ {
		ft := mmdvmFrameTypeVoice
		if i == 0 {
			ft = mmdvmFrameTypeVoiceSync
		}
		pkt := makeTestMMDVMPacket(true, false, ft, uint(i)) //nolint:gosec // G115: i is in [0,5]
		pkt.StreamID = header.StreamID
		pkt.DMRData = makeVoiceDMRData(i == 0)
		tr.TranslateToIPSC(pkt)
	}

	// The 7th burst should wrap to index 0 (burst A again) → 52 bytes
	pkt := makeTestMMDVMPacket(true, false, mmdvmFrameTypeVoiceSync, 0)
	pkt.StreamID = header.StreamID
	pkt.DMRData = makeVoiceDMRData(true)

	result := tr.TranslateToIPSC(pkt)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(result))
	}
	if len(result[0]) != 52 {
		t.Fatalf("expected 52 bytes (burst A after wrap), got %d", len(result[0]))
	}
}

func TestBuildMMDVMVoiceBurstFromSlot1(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Send header to establish reverse stream
	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	tr.TranslateToMMDVM(0x80, header)

	// Build an IPSC voice burst (slot1 = burst A, 52 bytes)
	burstData := make([]byte, 52)
	copy(burstData[:18], header[:18]) // reuse IPSC header
	burstData[30] = ipscBurstSlot1
	burstData[31] = 0x14
	burstData[32] = 0x40
	// AMBE data at bytes 33-51 (19 bytes, zeros = silence)

	result := tr.TranslateToMMDVM(0x80, burstData)
	if len(result) != 1 {
		t.Fatalf("expected 1 MMDVM packet for voice burst, got %d", len(result))
	}

	pkt := result[0]
	if pkt.Signature != "DMRD" {
		t.Fatalf("expected DMRD signature, got %q", pkt.Signature)
	}
	if pkt.Src != 100 {
		t.Fatalf("expected src 100, got %d", pkt.Src)
	}
	if pkt.Dst != 200 {
		t.Fatalf("expected dst 200, got %d", pkt.Dst)
	}
	if !pkt.GroupCall {
		t.Fatal("expected GroupCall=true")
	}
	if pkt.Slot {
		t.Fatal("expected Slot=false for TS1")
	}
	// First voice burst (burstIndex=0) should be voice sync
	if pkt.FrameType != mmdvmFrameTypeVoiceSync {
		t.Fatalf("expected frame type %d (voice sync), got %d", mmdvmFrameTypeVoiceSync, pkt.FrameType)
	}
	if pkt.DTypeOrVSeq != 0 {
		t.Fatalf("expected DTypeOrVSeq 0 (burst A), got %d", pkt.DTypeOrVSeq)
	}
}

func TestBuildMMDVMVoiceBurstSequencing(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Establish reverse stream with a header
	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(header[13:17], 0xEEEE)
	tr.TranslateToMMDVM(0x80, header)

	// Send 3 voice bursts and verify sequencing
	for i := 0; i < ipscVoiceHeaderRepeats; i++ {
		burstData := make([]byte, 52)
		copy(burstData[:18], header[:18])
		binary.BigEndian.PutUint32(burstData[13:17], 0xEEEE)
		burstData[30] = ipscBurstSlot1
		burstData[31] = 0x14
		burstData[32] = 0x40

		result := tr.TranslateToMMDVM(0x80, burstData)
		if len(result) != 1 {
			t.Fatalf("burst %d: expected 1 packet, got %d", i, len(result))
		}

		pkt := result[0]
		if pkt.DTypeOrVSeq != uint(i) {
			t.Fatalf("burst %d: expected DTypeOrVSeq %d, got %d", i, i, pkt.DTypeOrVSeq)
		}
		// Burst 0 = voice sync, rest = voice
		if i == 0 {
			if pkt.FrameType != mmdvmFrameTypeVoiceSync {
				t.Fatalf("burst 0: expected voice sync frame type, got %d", pkt.FrameType)
			}
		} else {
			if pkt.FrameType != mmdvmFrameTypeVoice {
				t.Fatalf("burst %d: expected voice frame type, got %d", i, pkt.FrameType)
			}
		}
		if pkt.Seq != uint(i+1) { //nolint:gosec // G115: i is in [0,2]; seq starts at 1 after header consumed seq=0
			t.Fatalf("burst %d: expected seq %d, got %d", i, i+1, pkt.Seq)
		}
	}
}

func TestBuildMMDVMVoiceBurstWrapsAt6(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(header[13:17], 0xFFFF)
	tr.TranslateToMMDVM(0x80, header)

	// Send 7 voice bursts — the 7th should wrap to burstIndex 0 (voice sync again)
	for i := 0; i < 7; i++ {
		burstData := make([]byte, 52)
		copy(burstData[:18], header[:18])
		binary.BigEndian.PutUint32(burstData[13:17], 0xFFFF)
		burstData[30] = ipscBurstSlot1
		burstData[31] = 0x14
		burstData[32] = 0x40

		result := tr.TranslateToMMDVM(0x80, burstData)
		if len(result) != 1 {
			t.Fatalf("burst %d: expected 1 packet, got %d", i, len(result))
		}

		pkt := result[0]
		expectedIdx := i % 6
		//nolint:gosec // G115: i is in [0,6], expectedIdx fits in uint
		if pkt.DTypeOrVSeq != uint(expectedIdx) {
			t.Fatalf("burst %d: expected DTypeOrVSeq %d, got %d", i, expectedIdx, pkt.DTypeOrVSeq)
		}
	}
}

func TestBuildMMDVMVoiceBurstSlot2(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, true)
	binary.BigEndian.PutUint32(header[13:17], 0x1111)
	tr.TranslateToMMDVM(0x80, header)

	burstData := make([]byte, 52)
	copy(burstData[:18], header[:18])
	binary.BigEndian.PutUint32(burstData[13:17], 0x1111)
	burstData[30] = ipscBurstSlot2
	burstData[31] = 0x14
	burstData[32] = 0x40

	result := tr.TranslateToMMDVM(0x80, burstData)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(result))
	}
	if !result[0].Slot {
		t.Fatal("expected Slot=true for TS2 voice burst")
	}
}

func TestBuildMMDVMVoiceBurstTooShort(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	// Establish reverse stream
	header := makeTestIPSCPacket(0x80, ipscBurstVoiceHead, true, false)
	binary.BigEndian.PutUint32(header[13:17], 0x2222)
	tr.TranslateToMMDVM(0x80, header)

	// Send a voice burst packet that is too short (< 52 bytes)
	burstData := make([]byte, 40)
	copy(burstData[:18], header[:18])
	binary.BigEndian.PutUint32(burstData[13:17], 0x2222)
	burstData[30] = ipscBurstSlot1

	result := tr.TranslateToMMDVM(0x80, burstData)
	if result != nil {
		t.Fatalf("expected nil for too-short voice burst, got %d packets", len(result))
	}
}

func TestBuildMMDVMVoiceBurstPrivateCall(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	header := makeTestIPSCPacket(0x81, ipscBurstVoiceHead, false, false)
	binary.BigEndian.PutUint32(header[13:17], 0x3333)
	tr.TranslateToMMDVM(0x81, header)

	burstData := make([]byte, 52)
	copy(burstData[:18], header[:18])
	binary.BigEndian.PutUint32(burstData[13:17], 0x3333)
	burstData[30] = ipscBurstSlot1
	burstData[31] = 0x14
	burstData[32] = 0x40

	result := tr.TranslateToMMDVM(0x81, burstData)
	if len(result) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(result))
	}
	if result[0].GroupCall {
		t.Fatal("expected GroupCall=false for private call voice burst")
	}
}

func TestPopulateEmbeddedSignallingBurstB(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	var burst layer2.Burst
	burst.HasEmbeddedSignalling = true

	// Burst B with 57-byte packet (5 bytes embedded data at [52:57])
	ipscData := make([]byte, 57)
	ipscData[52] = 0xAB
	ipscData[53] = 0xCD
	ipscData[54] = 0xEF
	ipscData[55] = 0x12

	tr.populateEmbeddedSignalling(&burst, 1, ipscData, nil)

	// Burst B (index 1) should have LCSS = FirstFragmentLC
	if burst.EmbeddedSignalling.LCSS != enums.FirstFragmentLC {
		t.Fatalf("expected LCSS FirstFragmentLC, got %d", burst.EmbeddedSignalling.LCSS)
	}

	// Check that embedded data was unpacked
	packed := burst.PackEmbeddedSignallingData()
	//nolint:gosec // packed is [4]byte, index is always in range
	if packed[0] != 0xAB || packed[1] != 0xCD || packed[2] != 0xEF || packed[3] != 0x12 {
		t.Fatalf("expected embedded data [AB CD EF 12], got [%02X %02X %02X %02X]",
			packed[0], packed[1], packed[2], packed[3])
	}
}

func TestPopulateEmbeddedSignallingBurstE(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	var burst layer2.Burst
	burst.HasEmbeddedSignalling = true

	// Burst E with 66-byte packet (7 bytes embedded data at [52:59])
	ipscData := make([]byte, 66)
	ipscData[52] = 0x11
	ipscData[53] = 0x22
	ipscData[54] = 0x33
	ipscData[55] = 0x44

	tr.populateEmbeddedSignalling(&burst, 4, ipscData, nil)

	// Burst E (index 4) should have LCSS = LastFragmentLCorCSBK
	if burst.EmbeddedSignalling.LCSS != enums.LastFragmentLCorCSBK {
		t.Fatalf("expected LCSS LastFragmentLCorCSBK, got %d", burst.EmbeddedSignalling.LCSS)
	}

	packed := burst.PackEmbeddedSignallingData()
	//nolint:gosec // packed is [4]byte, index is always in range
	if packed[0] != 0x11 || packed[1] != 0x22 || packed[2] != 0x33 || packed[3] != 0x44 {
		t.Fatalf("expected embedded data [11 22 33 44], got [%02X %02X %02X %02X]",
			packed[0], packed[1], packed[2], packed[3])
	}
}

func TestPopulateEmbeddedSignallingContinuation(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	var burst layer2.Burst
	burst.HasEmbeddedSignalling = true

	// Bursts C, D, F should all get ContinuationFragmentLCorCSBK
	for _, idx := range []int{2, 3, 5} {
		ipscData := make([]byte, 57)
		tr.populateEmbeddedSignalling(&burst, idx, ipscData, nil)
		if burst.EmbeddedSignalling.LCSS != enums.ContinuationFragmentLCorCSBK {
			t.Fatalf("burst index %d: expected LCSS ContinuationFragmentLCorCSBK, got %d",
				idx, burst.EmbeddedSignalling.LCSS)
		}
	}
}

func TestPopulateEmbeddedSignallingNoEmbeddedData(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	var burst layer2.Burst
	burst.HasEmbeddedSignalling = true

	// A 52-byte packet doesn't match 57 or 66, so no embedded data is extracted
	ipscData := make([]byte, 52)
	tr.populateEmbeddedSignalling(&burst, 1, ipscData, nil)

	// LCSS should still be set
	if burst.EmbeddedSignalling.LCSS != enums.FirstFragmentLC {
		t.Fatalf("expected LCSS FirstFragmentLC, got %d", burst.EmbeddedSignalling.LCSS)
	}

	// Embedded data should remain empty (all zeros)
	packed := burst.PackEmbeddedSignallingData()
	for i, b := range packed {
		if b != 0 {
			t.Fatalf("expected zero embedded data byte %d, got 0x%02X", i, b)
		}
	}
}

func TestPopulateEmbeddedSignallingPrefersRawTrailingBytes(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	var burst layer2.Burst
	burst.HasEmbeddedSignalling = true

	ipscData := make([]byte, 57)
	ipscData[52] = 0xAA
	ipscData[53] = 0xBB
	ipscData[54] = 0xCC
	ipscData[55] = 0xDD

	rss := &reverseStreamState{
		hasEmbeddedLC:     true,
		embeddedFragments: encodeEmbeddedLC(100, 200, true),
		src:               100,
		dst:               200,
		streamID:          7,
	}

	tr.populateEmbeddedSignalling(&burst, 1, ipscData, rss)

	expected := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	packed := burst.PackEmbeddedSignallingData()
	if packed != expected {
		t.Fatalf("expected raw embedded LC % X, got % X", expected, packed)
	}
}

func TestPopulateEmbeddedSignallingFallsBackWhenRawTrailingBytesAreZero(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator(t)

	var burst layer2.Burst
	burst.HasEmbeddedSignalling = true

	// Some Moto variants carry 5 trailing bytes for B/C/D/F bursts where the
	// first 4 bytes are all zero. In that case we should fall back to the
	// header-derived embedded LC fragments instead of propagating zeros.
	ipscData := make([]byte, 57)
	ipscData[52] = 0x00
	ipscData[53] = 0x00
	ipscData[54] = 0x00
	ipscData[55] = 0x00

	rss := &reverseStreamState{
		hasEmbeddedLC:     true,
		embeddedFragments: encodeEmbeddedLC(4604111, 46025, true),
	}

	tr.populateEmbeddedSignalling(&burst, 1, ipscData, rss)

	expected := rss.embeddedFragments[0]
	packed := burst.PackEmbeddedSignallingData()
	if packed != expected {
		t.Fatalf("expected fallback embedded LC % X, got % X", expected, packed)
	}
}

func TestDumpMotoTxtIPSCSequence(t *testing.T) {
	if os.Getenv("DUMP_MOTO_TX_IPSC") != "1" {
		t.Skip("set DUMP_MOTO_TX_IPSC=1 to dump IPSC->MMDVM sequence from moto.txt")
	}
	path := resolveCapturePathForIPSC(t, "moto.txt")
	pkts := parseTCPDumpPacketsIPSC(t, path)
	if len(pkts) == 0 {
		t.Fatalf("no packets parsed from %s", path)
	}

	tr := newTestTranslator(t)
	inCount := 0
	outCount := 0
	typeSig := make([]string, 0, 128)
	mmdvmSeq := make([]string, 0, 256)
	inVoiceBursts := 0
	for _, p := range pkts {
		if p.direction == "In" {
			inCount++
		} else {
			outCount++
		}
		if len(p.payload) == 0 {
			continue
		}
		pt := p.payload[0]
		typeSig = append(typeSig, "0x"+strings.ToUpper(strconv.FormatInt(int64(pt), 16)))
		if pt == 0x80 || pt == 0x81 || pt == 0x83 || pt == 0x84 {
			inVoiceBursts++
			out := tr.TranslateToMMDVM(pt, p.payload)
			for _, d := range out {
				mmdvmSeq = append(mmdvmSeq, strconv.Itoa(int(d.FrameType))+"/"+strconv.Itoa(int(d.DTypeOrVSeq)))
			}
		}
	}
	t.Logf("packets total=%d in=%d out=%d", len(pkts), inCount, outCount)
	t.Logf("ipsc packet-type sequence=%s", strings.Join(typeSig, ","))
	t.Logf("voice-capable ipsc packets=%d mmdvm decoded=%d", inVoiceBursts, len(mmdvmSeq))
	t.Logf("mmdvm ft/dt sequence=%s", strings.Join(mmdvmSeq, ","))
}

type tcpdumpPacketIPSC struct {
	direction string
	payload   []byte // UDP payload only
}

func resolveCapturePathForIPSC(t *testing.T, captureName string) string {
	t.Helper()
	candidates := []string{
		captureName,
		filepath.Join("..", "..", captureName),
		filepath.Join("..", captureName),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("capture file not found: %s", captureName)
	return ""
}

func parseTCPDumpPacketsIPSC(t *testing.T, path string) []tcpdumpPacketIPSC {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	type raw struct {
		direction string
		data      []byte
	}
	var raws []raw
	var cur *raw

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, " UDP, length ") && (strings.Contains(line, " In  IP ") || strings.Contains(line, " Out IP ")) {
			if cur != nil && len(cur.data) > 0 {
				raws = append(raws, *cur)
			}
			dir := "In"
			if strings.Contains(line, " Out IP ") {
				dir = "Out"
			}
			cur = &raw{direction: dir}
			continue
		}
		if cur == nil {
			continue
		}
		if !strings.Contains(line, "0x") || !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		fields := strings.Fields(parts[1])
		for _, tok := range fields {
			if len(tok) != 4 {
				break
			}
			b, err := hex.DecodeString(tok)
			if err != nil || len(b) != 2 {
				break
			}
			cur.data = append(cur.data, b[0], b[1])
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	if cur != nil && len(cur.data) > 0 {
		raws = append(raws, *cur)
	}

	out := make([]tcpdumpPacketIPSC, 0, len(raws))
	for _, r := range raws {
		if len(r.data) < 28 {
			continue
		}
		ihl := int(r.data[0]&0x0F) * 4
		if ihl < 20 || len(r.data) < ihl+8 {
			continue
		}
		udpPayload := r.data[ihl+8:]
		out = append(out, tcpdumpPacketIPSC{
			direction: r.direction,
			payload:   append([]byte(nil), udpPayload...),
		})
	}
	return out
}
