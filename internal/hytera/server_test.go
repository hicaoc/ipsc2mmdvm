package hytera

import (
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/dmr/enums"
	"github.com/USA-RedDragon/dmrgo/dmr/fec/golay"
	"github.com/USA-RedDragon/dmrgo/dmr/layer2"
	"github.com/USA-RedDragon/dmrgo/dmr/layer2/elements"
	"github.com/USA-RedDragon/dmrgo/dmr/layer2/pdu"
	l3elements "github.com/USA-RedDragon/dmrgo/dmr/layer3/elements"
	"github.com/hicaoc/ipsc2mmdvm/internal/config"
	intdmr "github.com/hicaoc/ipsc2mmdvm/internal/dmr"
	internalbptc "github.com/hicaoc/ipsc2mmdvm/internal/dmr/bptc"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

type tcpdumpPacket struct {
	srcIP   string
	dstIP   string
	payload []byte
}

func TestHandleP2PCommandDiscoversRDACPortFromStartupRequest(t *testing.T) {
	s := NewServer(&config.Config{Hytera: config.Hytera{P2PPort: 50001, DMRPort: 30001, RDACPort: 30002}}, nil)
	var gotRDACPort int
	s.peerHandler = func(addr *net.UDPAddr, p2pPort, dmrPort, rdacPort int, dmrid uint32) {
		gotRDACPort = rdacPort
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer conn.Close()
	s.p2pConn = conn

	addr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 45678}
	data := make([]byte, 21)
	copy(data[:3], []byte{0x50, 0x32, 0x50})
	data[20] = 0x12

	s.handleP2PCommand(data, addr)

	stored := s.repeaterRDACAddr["1.2.3.4"]
	if stored == nil || stored.Port != 45678 {
		t.Fatalf("expected repeater rdac addr port 45678, got %#v", stored)
	}
	if gotRDACPort != 45678 {
		t.Fatalf("expected peer handler rdac port 45678, got %d", gotRDACPort)
	}
}

func TestHandleP2PCommandStartsRDACIdentificationAfterStartupRequest(t *testing.T) {
	s := NewServer(&config.Config{Hytera: config.Hytera{P2PPort: 50001, DMRPort: 30001, RDACPort: 30002}}, nil)

	p2pConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen p2p: %v", err)
	}
	defer p2pConn.Close()
	s.p2pConn = p2pConn

	rdacServer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen rdac server: %v", err)
	}
	defer rdacServer.Close()
	s.rdacConn = rdacServer

	peerRDAC, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen rdac peer: %v", err)
	}
	defer peerRDAC.Close()

	addr := peerRDAC.LocalAddr().(*net.UDPAddr)
	data := make([]byte, 21)
	copy(data[:3], []byte{0x50, 0x32, 0x50})
	data[20] = 0x12

	s.handleP2PCommand(data, addr)

	_ = peerRDAC.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 64)
	n, _, err := peerRDAC.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("expected rdac step0 request after p2p startup: %v", err)
	}
	if got, want := buf[:n], rdacStep0Request; !bytesEqual(got, want) {
		t.Fatalf("unexpected step0 request: got % X want % X", got, want)
	}
}

func TestRDACHeartbeatTriggersStep0Request(t *testing.T) {
	s := NewServer(&config.Config{}, nil)

	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen server: %v", err)
	}
	defer serverConn.Close()
	s.rdacConn = serverConn

	peerConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen peer: %v", err)
	}
	defer peerConn.Close()

	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)
	s.rdacSession(peerAddr.IP.String()).lastP2PAt = time.Now()
	s.handleRDACHandshake(peerAddr, []byte{0x00})

	_ = peerConn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 64)
	n, _, err := peerConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("expected step0 request: %v", err)
	}
	if got, want := buf[:n], rdacStep0Request; !bytesEqual(got, want) {
		t.Fatalf("unexpected step0 request: got % X want % X", got, want)
	}
}

func TestRDACHeartbeatWithoutP2PAssociationDoesNotTriggerRequest(t *testing.T) {
	s := NewServer(&config.Config{}, nil)

	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen server: %v", err)
	}
	defer serverConn.Close()
	s.rdacConn = serverConn

	peerConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen peer: %v", err)
	}
	defer peerConn.Close()

	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)
	s.handleRDACHandshake(peerAddr, []byte{0x00})

	_ = peerConn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 64)
	if _, _, err := peerConn.ReadFromUDP(buf); err == nil {
		t.Fatal("expected no rdac step0 request without p2p association")
	}
}

func TestP2PPingMarksAssociationForRDAC(t *testing.T) {
	s := NewServer(&config.Config{}, nil)
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen p2p: %v", err)
	}
	defer conn.Close()
	s.p2pConn = conn

	addr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 45678}
	data := []byte{
		0x50, 0x32, 0x50, 0x31,
		0x0A, 0x00, 0x00, 0x00, 0x14,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	s.handleP2P(data, addr)

	session := s.rdacSession(addr.IP.String())
	if session.lastP2PAt.IsZero() {
		t.Fatal("expected p2p ping to mark rdac association")
	}
}

func TestRDACStep2ResponseCompletesDMRIDRead(t *testing.T) {
	s := NewServer(&config.Config{}, nil)
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen server: %v", err)
	}
	defer conn.Close()
	s.rdacConn = conn
	var gotDMRID uint32
	s.peerHandler = func(addr *net.UDPAddr, p2pPort, dmrPort, rdacPort int, dmrid uint32) {
		gotDMRID = dmrid
	}

	peerAddr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 50002}

	session := s.rdacSession(peerAddr.IP.String())
	session.step = 3

	data := []byte{
		0x7E, 0x04, 0x00, 0x00,
		0x20, 0x10, 0x00, 0x01,
		0x00, 0x1A, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00,
		0xD8, 0x37, 0x46,
		0x00, 0x00, 0x00, 0x00, 0x00,
	}

	s.handleRDACHandshake(peerAddr, data)

	if !session.identCompleted {
		t.Fatalf("expected rdac identification to complete, step=%d dmrid=%d", session.step, session.lastDMRID)
	}
	if session.lastDMRID != 0x4637D8 {
		t.Fatalf("unexpected dmrid: got %d want %d", session.lastDMRID, uint32(0x4637D8))
	}
	if gotDMRID != 0x4637D8 {
		t.Fatalf("expected peer update dmrid %d, got %d", uint32(0x4637D8), gotDMRID)
	}
}

func TestSwapDMRPayloadRoundTrip(t *testing.T) {
	in := make([]byte, 33)
	for i := range in {
		in[i] = byte(i + 1)
	}

	swapped := swapDMRPayload(in)
	if len(swapped) != 34 {
		t.Fatalf("expected len 34, got %d", len(swapped))
	}
	out := swapDMRPayload(swapped)
	if len(out) != 33 {
		t.Fatalf("expected len 33, got %d", len(out))
	}
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("round-trip mismatch at %d", i)
		}
	}
}

func TestClassifyRDACPacket(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "p2p1 control",
			data: append([]byte("P2P1"), append(make([]byte, 16), 0x12)...),
			want: "p2p1-0x12",
		},
		{
			name: "keepalive",
			data: []byte{0x5A, 0x5A, 0x5A, 0x5A, 0x0A, 0x00},
			want: "keepalive",
		},
		{
			name: "raw",
			data: []byte{0x01, 0x02, 0x03},
			want: "raw",
		},
	}

	for _, tt := range tests {
		if got := classifyRDACPacket(tt.data); got != tt.want {
			t.Fatalf("%s: got %q want %q", tt.name, got, tt.want)
		}
	}
}

func TestParseRDACDMRID(t *testing.T) {
	data := []byte{0x96, 0x00, 0x46, 0x37, 0xD8, 0x6A, 0x24, 0x00, 0x80, 0x4C, 0x04, 0x02, 0x04, 0x01}
	got, ok := parseRDACDMRID(data)
	if !ok {
		t.Fatal("expected parser to extract dmrid")
	}
	if got != 0x4637D8 {
		t.Fatalf("unexpected dmrid: got %d want %d", got, uint32(0x4637D8))
	}
}

func TestParseRDACDMRIDFromHRNPStep2Response(t *testing.T) {
	data := []byte{
		0x7E, 0x04, 0x00, 0x00,
		0x20, 0x10, 0x00, 0x01,
		0x00, 0x1A, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00,
		0xD8, 0x37, 0x46,
		0x00, 0x00, 0x00, 0x00, 0x00,
	}

	got, ok := parseRDACDMRID(data)
	if !ok {
		t.Fatal("expected parser to extract dmrid from rdac hrnp packet")
	}
	if got != 0x4637D8 {
		t.Fatalf("unexpected dmrid: got %d want %d", got, uint32(0x4637D8))
	}
}

func TestDecodeHyteraRadioIPDMRID(t *testing.T) {
	got, ok := decodeHyteraRadioIPDMRID([]byte{10, 33, 16, 221})
	if !ok {
		t.Fatal("expected parser to extract dmrid")
	}
	if got != 3316221 {
		t.Fatalf("unexpected dmrid: got %d want %d", got, uint32(3316221))
	}
}

func TestParseHyteraDMRAppDMRID(t *testing.T) {
	data := []byte{
		0x7E, 0x04, 0x00, 0x00,
		0x20, 0x10, 0x00, 0x01,
		0x00, 0x15, 0x00, 0x00,
		0x11,
		0x00, 0x03, 0x04, 0x00,
		0x0A, 0x21, 0x10, 0xDD,
		0x00, 0x03,
	}

	got, ok := parseHyteraDMRAppDMRID(data)
	if !ok {
		t.Fatal("expected parser to extract dmrid")
	}
	if got != 3316221 {
		t.Fatalf("unexpected dmrid: got %d want %d", got, uint32(3316221))
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	s := NewServer(&config.Config{MMDVMClients: []config.MMDVM{{ID: 12345, ColorCode: 7}}}, nil)
	pkt := proto.Packet{
		Signature:   "DMRD",
		Seq:         1,
		Src:         0x010203,
		Dst:         0xA1B2C3,
		Repeater:    12345,
		Slot:        false,
		GroupCall:   true,
		FrameType:   2,
		DTypeOrVSeq: 1,
		StreamID:    10,
	}
	for i := range pkt.DMRData {
		pkt.DMRData[i] = byte(i)
	}

	data, err := s.encodeFromMMDVM(pkt)
	if err != nil {
		t.Fatalf("encodeFromMMDVM: %v", err)
	}
	if got, want := data[64:68], []byte{0xC3, 0xB2, 0xA1, 0x00}; !bytesEqual(got, want) {
		t.Fatalf("encoded dst mismatch: got %v want %v", got, want)
	}
	if got, want := data[68:72], []byte{0x03, 0x02, 0x01, 0x00}; !bytesEqual(got, want) {
		t.Fatalf("encoded src mismatch: got %v want %v", got, want)
	}
	decoded, err := s.decodeToMMDVM(data)
	if err != nil {
		t.Fatalf("decodeToMMDVM: %v", err)
	}

	if decoded.Src != pkt.Src || decoded.Dst != pkt.Dst {
		t.Fatalf("src/dst mismatch: got %d/%d want %d/%d", decoded.Src, decoded.Dst, pkt.Src, pkt.Dst)
	}
	if decoded.FrameType != pkt.FrameType || decoded.DTypeOrVSeq != pkt.DTypeOrVSeq {
		t.Fatalf("frame mismatch: got (%d,%d) want (%d,%d)", decoded.FrameType, decoded.DTypeOrVSeq, pkt.FrameType, pkt.DTypeOrVSeq)
	}
	if len(decoded.DMRData) != len(pkt.DMRData) {
		t.Fatalf("dmr length mismatch: got %d want %d", len(decoded.DMRData), len(pkt.DMRData))
	}
}

func TestDecodeCompactHyteraVoiceHeader(t *testing.T) {
	s := NewServer(&config.Config{MMDVMClients: []config.MMDVM{{ID: 12345, ColorCode: 7}}}, nil)
	data := []byte{
		0xC3, 0x51, 0x00, 0x50, 0x2A, 0x02, 0x00, 0x00,
		0x01, 0x00, 0x05, 0x01, 0x02, 0x00, 0x00, 0x00,
		0x22, 0x22, 0x11, 0x11, 0x99, 0x99, 0x00, 0x00,
		0x40, 0x01, 0xD5, 0x03, 0xE4, 0x1F, 0x1C, 0x20,
		0x38, 0x00, 0x40, 0xF0, 0xA1, 0xE7, 0x6D, 0xE4,
		0x57, 0xFF, 0x5D, 0xD7, 0xD1, 0xF5, 0x00, 0x59,
		0x88, 0x03, 0xC0, 0x5E, 0x41, 0xFA, 0xC3, 0x19,
		0x80, 0x78, 0x00, 0x9E, 0x2A, 0x03, 0x01, 0x00,
		0xC9, 0xB3, 0x00, 0x00, 0xD8, 0x37, 0x46, 0x00,
	}

	pkt, err := s.decodeToMMDVM(data)
	if err != nil {
		t.Fatalf("decodeToMMDVM: %v", err)
	}
	if !pkt.Slot {
		t.Fatal("expected timeslot 2")
	}
	if pkt.FrameType != 2 || pkt.DTypeOrVSeq != 1 {
		t.Fatalf("unexpected frame type/dtype: got (%d,%d)", pkt.FrameType, pkt.DTypeOrVSeq)
	}
	if !pkt.GroupCall {
		t.Fatal("expected group call")
	}
	if pkt.Dst != 0x00B3C9 {
		t.Fatalf("unexpected dst: got %d", pkt.Dst)
	}
	if pkt.Src != 0x4637D8 {
		t.Fatalf("unexpected src: got %d", pkt.Src)
	}
}

func TestShouldStartNewInboundStreamKeepsSameCallOnRepeatedLCHeader(t *testing.T) {
	now := time.Now()
	st := &slotState{
		active:      true,
		inSrc:       1001,
		inDst:       91,
		inGroupCall: true,
		lastInbound: now,
	}

	if shouldStartNewInboundStream(st, slotTypeVoiceLCHeader, 1001, 91, true, now.Add(500*time.Millisecond)) {
		t.Fatal("expected repeated LC header for same active call to keep existing stream")
	}
}

func TestShouldStartNewInboundStreamStartsOnChangedRoute(t *testing.T) {
	now := time.Now()
	st := &slotState{
		active:      true,
		inSrc:       1001,
		inDst:       91,
		inGroupCall: true,
		lastInbound: now,
	}

	if !shouldStartNewInboundStream(st, slotTypeVoiceLCHeader, 1002, 91, true, now.Add(500*time.Millisecond)) {
		t.Fatal("expected changed source to start a new stream")
	}
}

func TestBuildWakeupPacketLen(t *testing.T) {
	data := buildWakeupPacket(true, 1001, 9, false, 0)
	if len(data) != 72 {
		t.Fatalf("expected wakeup len 72, got %d", len(data))
	}
}

func TestBuildWakeupPacketIDs(t *testing.T) {
	data := buildWakeupPacket(true, 0x010203, 0xA1B2C3, false, 0)
	if got, want := data[64:68], []byte{0xC3, 0xB2, 0xA1, 0x00}; !bytesEqual(got, want) {
		t.Fatalf("wakeup dst mismatch: got %v want %v", got, want)
	}
	if got, want := data[68:72], []byte{0x03, 0x02, 0x01, 0x00}; !bytesEqual(got, want) {
		t.Fatalf("wakeup src mismatch: got %v want %v", got, want)
	}
}

func TestBuildWakeupPacketTimeslot(t *testing.T) {
	ts1 := buildWakeupPacket(false, 1001, 9, false, 0)
	if ts1[12] != 0x01 {
		t.Fatalf("expected TS1 marker 0x01, got 0x%02X", ts1[12])
	}
	if ts1[16] != 0x11 || ts1[17] != 0x11 {
		t.Fatalf("expected TS1 raw marker 0x1111, got 0x%02X%02X", ts1[16], ts1[17])
	}

	ts2 := buildWakeupPacket(true, 1001, 9, false, 0)
	if ts2[12] != 0x02 {
		t.Fatalf("expected TS2 marker 0x02, got 0x%02X", ts2[12])
	}
	if ts2[16] != 0x22 || ts2[17] != 0x22 {
		t.Fatalf("expected TS2 raw marker 0x2222, got 0x%02X%02X", ts2[16], ts2[17])
	}
}

func TestBuildWakeupPacketMatchesReferenceTemplate(t *testing.T) {
	data := buildWakeupPacket(false, 0x4631BC, 0x00B3C9, false, 0)
	if got := data[8]; got != 0x42 {
		t.Fatalf("expected wakeup packet type 0x42, got 0x%02X", got)
	}
	if got, want := data[11], byte(0x42); got != want {
		t.Fatalf("expected wakeup reserved marker[11] 0x%02X, got 0x%02X", want, got)
	}
	if got, want := data[24], byte(0x40); got != want {
		t.Fatalf("expected wakeup marker[24] 0x%02X, got 0x%02X", want, got)
	}
	if got, want := data[25], byte(0x00); got != want {
		t.Fatalf("expected wakeup marker[25] 0x%02X, got 0x%02X", want, got)
	}
	if got, want := data[62:64], []byte{0x07, 0x00}; !bytesEqual(got, want) {
		t.Fatalf("expected wakeup call marker %v, got %v", want, got)
	}
	if got, want := data[20:22], []byte{0x00, 0x00}; !bytesEqual(got, want) {
		t.Fatalf("expected wakeup cc marker %v, got %v", want, got)
	}
}

func TestBuildSyncPacketMatchesReferenceTemplate(t *testing.T) {
	data := buildSyncPacket(false, 0x4631BC, 0x00B3C9, true, 0)
	if got := data[8]; got != 0x42 {
		t.Fatalf("expected sync packet type 0x42, got 0x%02X", got)
	}
	if got, want := data[18:20], []byte{0xEE, 0xEE}; !bytesEqual(got, want) {
		t.Fatalf("expected sync slot type %v, got %v", want, got)
	}
	if got, want := data[24:26], []byte{0x40, 0x2F}; !bytesEqual(got, want) {
		t.Fatalf("expected sync marker %v, got %v", want, got)
	}
	if got, want := data[63:65], []byte{0x01, 0x00}; !bytesEqual(got, want) {
		t.Fatalf("expected sync call marker %v, got %v", want, got)
	}
	if got, want := data[20:22], []byte{0x00, 0x00}; !bytesEqual(got, want) {
		t.Fatalf("expected sync cc marker %v, got %v", want, got)
	}
}

func TestEncodeFromMMDVMUsesConfigColorCode(t *testing.T) {
	s := NewServer(&config.Config{Local: config.Local{ColorCode: 7}}, nil)
	pkt := proto.Packet{
		Signature:   "DMRD",
		Src:         1001,
		Dst:         9,
		GroupCall:   true,
		FrameType:   2,
		DTypeOrVSeq: 1,
		Slot:        false,
		StreamID:    1,
	}
	pkt.DMRData = layer2.BuildLCDataBurst([12]byte{}, elements.DataTypeVoiceLCHeader, 5)
	data, err := s.encodeFromMMDVMWithState(pkt, &slotState{})
	if err != nil {
		t.Fatalf("encodeFromMMDVMWithState: %v", err)
	}
	if got, want := data[20:22], []byte{0x77, 0x77}; !bytesEqual(got, want) {
		t.Fatalf("expected encoded cc marker %v, got %v", want, got)
	}
}

func TestBuildSyncPacketVisibleIDs(t *testing.T) {
	data := buildSyncPacket(false, 0x010203, 0xA1B2C3, true, 0)
	if got, want := data[33:39], []byte{0xA1, 0x00, 0xB2, 0x00, 0xC3, 0x00}; !bytesEqual(got, want) {
		t.Fatalf("expected visible dst bytes %v, got %v", want, got)
	}
	if got, want := data[39:45], []byte{0x01, 0x00, 0x02, 0x00, 0x03, 0x00}; !bytesEqual(got, want) {
		t.Fatalf("expected visible src bytes %v, got %v", want, got)
	}
}

func TestBuildSyncPacketTimeslotMarkers(t *testing.T) {
	ts1 := buildSyncPacket(false, 1001, 9, true, 0)
	if got, want := ts1[16:18], []byte{0x11, 0x11}; !bytesEqual(got, want) {
		t.Fatalf("expected TS1 sync marker %v, got %v", want, got)
	}

	ts2 := buildSyncPacket(true, 1001, 9, true, 0)
	if got, want := ts2[16:18], []byte{0x22, 0x22}; !bytesEqual(got, want) {
		t.Fatalf("expected TS2 sync marker %v, got %v", want, got)
	}
}

func TestStandardizeMotoLCPacketUsesETSIHeaderAndTerminator(t *testing.T) {
	src := uint(4604111)
	dst := uint(46025)
	cc := uint8(1)
	wantHeaderLC := intdmr.BuildStandardLCBytesForDataType(src, dst, true, 0x20, intdmr.DataTypeVoiceLCHeader)
	wantTermLC := intdmr.BuildStandardLCBytesForDataType(src, dst, true, 0x20, intdmr.DataTypeTerminatorWithLC)

	header := proto.Packet{
		Signature:   "DMRD",
		Src:         src,
		Dst:         dst,
		GroupCall:   true,
		FrameType:   2,
		DTypeOrVSeq: 1,
	}
	term := header
	term.DTypeOrVSeq = 2

	gotHeader := standardizeMotoLCPacket(header, cc)
	gotTerm := standardizeMotoLCPacket(term, cc)

	// Decode using correct BPTC and verify LC addresses.
	headerLC, ok := internalbptc.DecodeLCFromBurst(gotHeader.DMRData)
	if !ok {
		t.Fatal("failed to decode header LC from burst")
	}
	headerSrc := int(headerLC[6])<<16 | int(headerLC[7])<<8 | int(headerLC[8])
	headerDst := int(headerLC[3])<<16 | int(headerLC[4])<<8 | int(headerLC[5])
	if headerSrc != int(src) || headerDst != int(dst) {
		t.Fatalf("header LC src=%d dst=%d, want src=%d dst=%d", headerSrc, headerDst, src, dst)
	}

	termLC, ok := internalbptc.DecodeLCFromBurst(gotTerm.DMRData)
	if !ok {
		t.Fatal("failed to decode terminator LC from burst")
	}
	termSrc := int(termLC[6])<<16 | int(termLC[7])<<8 | int(termLC[8])
	termDst := int(termLC[3])<<16 | int(termLC[4])<<8 | int(termLC[5])
	if termSrc != int(src) || termDst != int(dst) {
		t.Fatalf("terminator LC src=%d dst=%d, want src=%d dst=%d", termSrc, termDst, src, dst)
	}

	if gotHeader.DMRData != internalbptc.BuildLCDataBurst(wantHeaderLC, uint8(elements.DataTypeVoiceLCHeader), cc) {
		t.Fatal("standardized header does not match ETSI-encoded LC header burst")
	}
	if gotTerm.DMRData != internalbptc.BuildLCDataBurst(wantTermLC, uint8(elements.DataTypeTerminatorWithLC), cc) {
		t.Fatal("standardized terminator does not match ETSI-encoded LC terminator burst")
	}
}

func TestStandardizeMotoLCPacketWithSOUsesProvidedValue(t *testing.T) {
	src := uint(4604111)
	dst := uint(46025)
	cc := uint8(1)

	header := proto.Packet{
		Signature:   "DMRD",
		Src:         src,
		Dst:         dst,
		GroupCall:   true,
		FrameType:   2,
		DTypeOrVSeq: 1,
	}
	gotHeader := standardizeMotoLCPacketWithSO(header, cc, 0x00)

	headerLC, ok := internalbptc.DecodeLCFromBurst(gotHeader.DMRData)
	if !ok {
		t.Fatal("failed to decode header LC from burst")
	}
	if headerLC[2] != 0x00 {
		t.Fatalf("expected SO=0x00, got 0x%02X", headerLC[2])
	}
}

func TestResolveMotoServiceOptionsDefaultsAndCachesFromHeader(t *testing.T) {
	s := NewServer(&config.Config{Local: config.Local{ColorCode: 1}, Hytera: config.Hytera{}}, nil)
	st := s.outboundSlotState(false, "hytera:test")
	if st == nil {
		t.Fatal("expected slot state")
	}

	voice := proto.Packet{
		Signature:   "DMRD",
		FrameType:   0,
		DTypeOrVSeq: 3,
	}
	so, source := resolveMotoServiceOptions(voice, st)
	if so != 0x00 || source != "bm-default" {
		t.Fatalf("expected default SO 0x00 from bm-default, got so=0x%02X source=%s", so, source)
	}

	header := proto.Packet{
		Signature:   "DMRD",
		Src:         4604111,
		Dst:         46025,
		GroupCall:   true,
		FrameType:   2,
		DTypeOrVSeq: 1,
	}
	lc := intdmr.BuildStandardLCBytesWithSO(header.Src, header.Dst, header.GroupCall, 0x20)
	header.DMRData = internalbptc.BuildLCDataBurst(lc, uint8(elements.DataTypeVoiceLCHeader), 1)
	so, source = resolveMotoServiceOptions(header, st)
	if so != 0x20 || source != "packet-lc" {
		t.Fatalf("expected SO 0x20 from packet-lc, got so=0x%02X source=%s", so, source)
	}

	so, source = resolveMotoServiceOptions(voice, st)
	if so != 0x20 || source != "slot-cache" {
		t.Fatalf("expected cached SO 0x20 from slot-cache, got so=0x%02X source=%s", so, source)
	}
}

func TestLC12CurrentEncoderMatchesBMForHeaderAndTerminator(t *testing.T) {
	src := uint(4600252)
	dst := uint(46025)
	so := uint8(0x00)

	headerLC := intdmr.BuildStandardLCBytesForDataType(src, dst, true, so, intdmr.DataTypeVoiceLCHeader)
	bmHeader := [12]byte{0x00, 0x00, 0x00, 0x00, 0xB3, 0xC9, 0x46, 0x31, 0xBC, 0x08, 0x30, 0x88}
	if headerLC != bmHeader {
		t.Fatalf("header lc12 mismatch\n got=% X\nwant=% X", headerLC, bmHeader)
	}

	termLC := intdmr.BuildStandardLCBytesForDataType(src, dst, true, so, intdmr.DataTypeTerminatorWithLC)
	bmTerm := [12]byte{0x00, 0x00, 0x00, 0x00, 0xB3, 0xC9, 0x46, 0x31, 0xBC, 0x07, 0x3F, 0x87}
	if termLC != bmTerm {
		t.Fatalf("terminator lc12 mismatch\n got=% X\nwant=% X", termLC, bmTerm)
	}
}

func TestSendPacketFromMoto_DefaultLightweightNormalizesControlButKeepsIDs(t *testing.T) {
	s := NewServer(&config.Config{Local: config.Local{ColorCode: 1}, Hytera: config.Hytera{}}, nil)

	writer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen writer: %v", err)
	}
	defer writer.Close()
	s.dmrConn = writer

	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer target.Close()
	s.repeaterDMRAddr["10.0.0.1"] = target.LocalAddr().(*net.UDPAddr)

	pkt := proto.Packet{
		Signature:   "DMRD",
		Src:         0x010203,
		Dst:         0xA1B2C3,
		GroupCall:   true,
		FrameType:   0,
		DTypeOrVSeq: 3,
		StreamID:    77,
	}
	for i := range pkt.DMRData {
		pkt.DMRData[i] = byte(0x30 + i)
	}

	if ok := s.SendPacketFromMotoTo(pkt, "hytera:10.0.0.1"); !ok {
		t.Fatal("expected moto packet send to succeed")
	}

	var (
		decoded proto.Packet
		found   bool
	)
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = target.SetReadDeadline(time.Now().Add(120 * time.Millisecond))
		raw := make([]byte, 256)
		n, _, err := target.ReadFromUDP(raw)
		if err != nil {
			continue
		}
		p, err := s.decodeToMMDVM(raw[:n])
		if err != nil {
			continue
		}
		if p.FrameType == pkt.FrameType && p.DTypeOrVSeq == pkt.DTypeOrVSeq {
			decoded = p
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected to receive original voice burst after normalization packets")
	}
	if decoded.GroupCall != pkt.GroupCall {
		t.Fatalf("expected group call flag unchanged, got %v want %v", decoded.GroupCall, pkt.GroupCall)
	}
	if decoded.Src != pkt.Src || decoded.Dst != pkt.Dst {
		t.Fatalf("expected src/dst unchanged, got %d/%d want %d/%d", decoded.Src, decoded.Dst, pkt.Src, pkt.Dst)
	}
	if decoded.DMRData == pkt.DMRData {
		t.Fatalf("expected lightweight moto->hytera path to normalize control fields, got unchanged dmrdata % X", decoded.DMRData)
	}
	var b layer2.Burst
	b.DecodeFromBytes(decoded.DMRData)
	if !b.HasEmbeddedSignalling {
		t.Fatal("expected normalized burst to carry embedded signalling")
	}
	if b.EmbeddedSignalling.LCSS != enums.ContinuationFragmentLCorCSBK {
		t.Fatalf("expected dt=3 to be continuation fragment, got %d", b.EmbeddedSignalling.LCSS)
	}
}

func TestSendSynthesizedMotoHeader(t *testing.T) {
	s := NewServer(&config.Config{Local: config.Local{ColorCode: 1}, Hytera: config.Hytera{}}, nil)

	writer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen writer: %v", err)
	}
	defer writer.Close()
	s.dmrConn = writer

	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer target.Close()
	s.repeaterDMRAddr["10.0.0.1"] = target.LocalAddr().(*net.UDPAddr)

	pkt := proto.Packet{
		Signature:   "DMRD",
		Src:         0x4637D8,
		Dst:         0x00B3C9,
		GroupCall:   true,
		FrameType:   0,
		DTypeOrVSeq: 3,
		StreamID:    501,
	}
	for i := range pkt.DMRData {
		pkt.DMRData[i] = byte(0x20 + i)
	}

	st := s.outboundSlotState(pkt.Slot, "hytera:10.0.0.1")
	if st == nil {
		t.Fatal("expected outbound slot state")
	}
	if ok := s.sendSynthesizedMotoHeader(pkt, 1, 0x20, st, "hytera:10.0.0.1"); !ok {
		t.Fatal("expected synthesized header send to succeed")
	}

	decodedPackets := make([]proto.Packet, 0, 2)
	deadline := time.Now().Add(600 * time.Millisecond)
	for len(decodedPackets) < 1 && time.Now().Before(deadline) {
		_ = target.SetReadDeadline(time.Now().Add(120 * time.Millisecond))
		raw := make([]byte, 256)
		n, _, err := target.ReadFromUDP(raw)
		if err != nil {
			continue
		}
		decoded, err := s.decodeToMMDVM(raw[:n])
		if err != nil {
			continue
		}
		decodedPackets = append(decodedPackets, decoded)
	}

	if len(decodedPackets) < 1 {
		t.Fatalf("expected at least 1 decoded packet, got %d", len(decodedPackets))
	}
	if st.lastMotoHeader.IsZero() {
		t.Fatal("expected synthesized LC header to update lastMotoHeader timestamp")
	}
}

func TestMotoCaptureNormalizationMatchesBMStyleHeaderPrefix(t *testing.T) {
	s := NewServer(&config.Config{Local: config.Local{ColorCode: 1}, Hytera: config.Hytera{}}, nil)

	writer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen writer: %v", err)
	}
	defer writer.Close()
	s.dmrConn = writer

	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer target.Close()
	s.repeaterDMRAddr["10.0.0.1"] = target.LocalAddr().(*net.UDPAddr)

	// Replay the first part of the moto->bm capture (no initial LC header).
	rawDMRD := []string{
		"444d5244014640cf00b3c9000000001000000015b9e881526173002a6bb9e8815267f7d5dd57dfd173002a6bb9e881526173002a6b",
		"444d5244024640cf00b3c9000000000100000015b9e881526173002a6bb9e8815260211051e1273173002a6bb9e881526173002a6b",
		"444d5244034640cf00b3c9000000000200000015b9e881526173002a6bb9e88152606000c121b96173002a6bf90ba0106530f88337",
		"444d5244044640cf00b3c9000000000300000015e2e8e7a775d9704159dd2ea5567061212111d964999cedafea2ba3162014c9e023",
	}
	for _, hx := range rawDMRD {
		data, err := hex.DecodeString(hx)
		if err != nil {
			t.Fatalf("decode hex: %v", err)
		}
		pkt, ok := proto.Decode(data)
		if !ok {
			t.Fatalf("failed to decode replay packet: %s", hx)
		}
		if ok := s.SendPacketFromMotoTo(pkt, "hytera:10.0.0.1"); !ok {
			t.Fatal("expected replay packet send to succeed")
		}
	}

	decoded := make([]proto.Packet, 0, 8)
	deadline := time.Now().Add(900 * time.Millisecond)
	for len(decoded) < 6 && time.Now().Before(deadline) {
		_ = target.SetReadDeadline(time.Now().Add(120 * time.Millisecond))
		buf := make([]byte, 256)
		n, _, err := target.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		p, err := s.decodeToMMDVM(buf[:n])
		if err != nil {
			continue
		}
		decoded = append(decoded, p)
	}

	if len(decoded) < 4 {
		t.Fatalf("expected >=4 decoded packets, got %d", len(decoded))
	}
	// BM-returned stream starts with repeated LC headers; our normalization should
	// produce the same prefix before voice sync/data.
	for i := 0; i < 3; i++ {
		if decoded[i].FrameType != 2 || decoded[i].DTypeOrVSeq != 1 {
			t.Fatalf("packet %d expected LC header (2/1), got (%d/%d)", i, decoded[i].FrameType, decoded[i].DTypeOrVSeq)
		}
	}
	if decoded[3].FrameType != 1 || decoded[3].DTypeOrVSeq != 0 {
		t.Fatalf("packet 3 expected voice sync (1/0), got (%d/%d)", decoded[3].FrameType, decoded[3].DTypeOrVSeq)
	}
}

func TestReplayBMOutboundMatchesBMReturnedPrefix(t *testing.T) {
	if os.Getenv("STRICT_BM_REPLAY_COMPARE") != "1" {
		t.Skip("set STRICT_BM_REPLAY_COMPARE=1 to run strict BM replay comparison")
	}

	s := NewServer(&config.Config{Local: config.Local{ColorCode: 1}, Hytera: config.Hytera{}}, nil)

	path := resolveCapturePath(t, "bm-bm.txt")
	packets := parseTCPDumpPacketsFromFile(t, path)
	if len(packets) == 0 {
		t.Fatal("expected parsed UDP packets from bm-bm.txt")
	}

	bmOutbound, bmReturned, seenSrc := splitBMDMRDPackets(t, packets)
	if len(bmOutbound) == 0 {
		t.Fatalf("expected bm outbound DMRD packets from bm-bm.txt, seen src=%v", seenSrc)
	}
	if len(bmReturned) == 0 {
		t.Fatalf("expected bm returned DMRD packets from bm-bm.txt, seen src=%v", seenSrc)
	}

	actual, err := replayMotoToHyteraPure(s, bmOutbound)
	if err != nil {
		t.Fatalf("pure replay failed: %v", err)
	}
	if len(actual) < len(bmReturned) {
		t.Fatalf("expected at least %d output packets, got %d", len(bmReturned), len(actual))
	}

	actual = actual[:len(bmReturned)]
	for i := range bmReturned {
		want := bmReturned[i]
		got := actual[i]
		// Ignore transport-level sequencing/repeater id differences.
		if got.Src != want.Src || got.Dst != want.Dst || got.Slot != want.Slot ||
			got.GroupCall != want.GroupCall || got.FrameType != want.FrameType ||
			got.DTypeOrVSeq != want.DTypeOrVSeq || got.DMRData != want.DMRData {
			t.Fatalf(
				"packet %d mismatch: got src=%d dst=%d slot=%t gc=%t ft/dt=%d/%d dmr=% X; want src=%d dst=%d slot=%t gc=%t ft/dt=%d/%d dmr=% X",
				i,
				got.Src, got.Dst, got.Slot, got.GroupCall, got.FrameType, got.DTypeOrVSeq, got.DMRData,
				want.Src, want.Dst, want.Slot, want.GroupCall, want.FrameType, want.DTypeOrVSeq, want.DMRData,
			)
		}
	}
}

func replayMotoToHyteraPure(s *Server, in []proto.Packet) ([]proto.Packet, error) {
	sourceKey := "hytera:pure-test"
	out := make([]proto.Packet, 0, len(in)*2)

	now := time.Now()
	for _, packet := range in {
		st := s.outboundSlotState(packet.Slot, sourceKey)
		lastStreamID := uint(0)
		newStream := false
		idle := true
		acceptNewStream := false
		if st != nil {
			lastStreamID = st.outStreamID
			newStream = packet.StreamID != 0 && packet.StreamID != lastStreamID
			idle = st.lastSent.IsZero() || now.Sub(st.lastSent) > hangTimeForPacket(packet)

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
			if acceptNewStream && packet.StreamID != 0 {
				st.outSeq = 0
				st.outStreamID = packet.StreamID
			}
		}

		motoCC := packetColorCode(packet, cfgColorCode(s.cfg))
		outPacket := packet

		if acceptNewStream && packet.FrameType != 2 && isVoiceCallStartOrVoice(packet) {
			header := packet
			header.FrameType = 2
			header.DTypeOrVSeq = 1
			header.DMRData = layer2.BuildLCDataBurst(
				intdmr.BuildStandardLCBytes(packet.Src, packet.Dst, packet.GroupCall),
				elements.DataTypeVoiceLCHeader,
				motoCC&0x0F,
			)
			for i := 0; i < motoHeaderRepeats; i++ {
				if syncPacket, ok := s.buildStartupPacketForRoute(slotTypeSync, header, true, motoCC); ok {
					decoded, err := s.decodeToMMDVM(syncPacket)
					if err != nil {
						if err == errIgnoredPacket {
							decoded = proto.Packet{}
						} else {
							return nil, err
						}
					}
					if decoded.Signature == "DMRD" {
						out = append(out, decoded)
					}
					if st != nil {
						st.lastSent = now
					}
				}
				raw, err := s.encodeFromMMDVMWithState(header, st)
				if err != nil {
					return nil, err
				}
				setHyteraHeaderColorCode(raw, motoCC)
				decoded, err := s.decodeToMMDVM(raw)
				if err != nil {
					return nil, err
				}
				out = append(out, decoded)
				if st != nil {
					st.lastSent = now
					st.lastMotoHeader = now
				}
			}
		}

		repeats := 1
		if packet.FrameType == 2 && packet.DTypeOrVSeq == 1 {
			repeats = motoHeaderRepeats
		}
		for i := 0; i < repeats; i++ {
			if packet.FrameType == 2 && packet.DTypeOrVSeq == 1 {
				if syncPacket, ok := s.buildStartupPacketForRoute(slotTypeSync, packet, true, motoCC); ok {
					decoded, err := s.decodeToMMDVM(syncPacket)
					if err != nil {
						if err == errIgnoredPacket {
							decoded = proto.Packet{}
						} else {
							return nil, err
						}
					}
					if decoded.Signature == "DMRD" {
						out = append(out, decoded)
					}
					if st != nil {
						st.lastSent = now
					}
				}
			}
			raw, err := s.encodeFromMMDVMWithState(outPacket, st)
			if err != nil {
				return nil, err
			}
			setHyteraHeaderColorCode(raw, motoCC)
			decoded, err := s.decodeToMMDVM(raw)
			if err != nil {
				return nil, err
			}
			out = append(out, decoded)
			if st != nil {
				st.lastSent = now
				if packet.FrameType == 2 && packet.DTypeOrVSeq == 1 {
					st.lastMotoHeader = now
				}
			}
		}
		now = now.Add(60 * time.Millisecond)
	}

	return out, nil
}

func TestShouldWakeup(t *testing.T) {
	tests := []struct {
		name       string
		pkt        proto.Packet
		lastStream uint
		want       bool
		idle       bool
	}{
		{
			name: "voice lc header",
			pkt: proto.Packet{
				FrameType:   2,
				DTypeOrVSeq: 1,
				StreamID:    100,
			},
			lastStream: 90,
			want:       true,
			idle:       false,
		},
		{
			name: "voice non-header same stream",
			pkt: proto.Packet{
				FrameType:   2,
				DTypeOrVSeq: 3,
				StreamID:    100,
			},
			lastStream: 100,
			want:       false,
			idle:       true,
		},
		{
			name: "voice non-header new stream",
			pkt: proto.Packet{
				FrameType:   2,
				DTypeOrVSeq: 3,
				StreamID:    101,
			},
			lastStream: 100,
			want:       false,
			idle:       true,
		},
		{
			name: "data frame",
			pkt: proto.Packet{
				FrameType:   3,
				DTypeOrVSeq: 6,
				StreamID:    200,
			},
			lastStream: 100,
			want:       false,
			idle:       true,
		},
		{
			name: "voice sync new stream",
			pkt: proto.Packet{
				FrameType:   1,
				DTypeOrVSeq: 0,
				StreamID:    201,
			},
			lastStream: 100,
			want:       true,
			idle:       true,
		},
		{
			name: "new stream but not idle",
			pkt: proto.Packet{
				FrameType:   1,
				DTypeOrVSeq: 0,
				StreamID:    202,
			},
			lastStream: 100,
			want:       false,
			idle:       false,
		},
	}

	for _, tt := range tests {
		got := shouldWakeup(tt.pkt, tt.lastStream, tt.idle, false)
		if got != tt.want {
			t.Fatalf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
}

func TestShouldWakeupFromMotoOnVoiceWithoutLCHeaderWhenIdle(t *testing.T) {
	pkt := proto.Packet{
		FrameType:   0,
		DTypeOrVSeq: 3,
		StreamID:    201,
	}
	if !shouldWakeup(pkt, 100, true, true) {
		t.Fatal("expected wakeup for moto voice burst on new idle stream without LC header")
	}
	if shouldWakeup(pkt, 100, false, true) {
		t.Fatal("did not expect wakeup when stream is not idle")
	}
}

func TestSendPacketResetsSeqOnNewStreamWithoutWakeup(t *testing.T) {
	cfg := &config.Config{
		MMDVMClients: []config.MMDVM{{ID: 12345, ColorCode: 1}},
		Hytera:       config.Hytera{},
	}
	s := NewServer(cfg, nil)
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer conn.Close()
	s.dmrConn = conn
	s.repeaterDMRAddr["peer"] = &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: conn.LocalAddr().(*net.UDPAddr).Port}

	st := s.slots[false]
	st.outSeq = 77
	st.outStreamID = 100

	pkt := proto.Packet{
		Signature:   "DMRD",
		Src:         1001,
		Dst:         9,
		Slot:        false,
		GroupCall:   true,
		FrameType:   0, // not wakeup-triggering
		DTypeOrVSeq: 3,
		StreamID:    101,
	}
	for i := range pkt.DMRData {
		pkt.DMRData[i] = byte(i)
	}

	s.SendPacket(pkt)
	if got := s.slots[false].outSeq; got != 1 {
		t.Fatalf("expected outSeq reset then increment to 1, got %d", got)
	}
	if got := s.slots[false].outStreamID; got != 101 {
		t.Fatalf("expected outStreamID 101, got %d", got)
	}
}

func TestOutboundSlotStateIsolatedByTarget(t *testing.T) {
	s := NewServer(&config.Config{}, nil)

	first := s.outboundSlotState(false, "hytera:10.0.0.1")
	second := s.outboundSlotState(false, "hytera:10.0.0.2")
	global := s.outboundSlotState(false, "")

	if first == second {
		t.Fatal("expected different target routes to have isolated slot state")
	}
	if first == global || second == global {
		t.Fatal("expected targeted slot state to be isolated from global slot state")
	}

	first.outStreamID = 1001
	if got := second.outStreamID; got != 0 {
		t.Fatalf("expected second route stream id to remain isolated, got %d", got)
	}
	if got := global.outStreamID; got != 0 {
		t.Fatalf("expected global slot state to remain isolated, got %d", got)
	}
}

func TestPrimeTargetsPrimesAllTargetsOnce(t *testing.T) {
	cfg := &config.Config{
		MMDVMClients: []config.MMDVM{{ID: 12345, ColorCode: 1}},
		Hytera:       config.Hytera{},
	}
	s := NewServer(cfg, nil)
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer conn.Close()
	s.dmrConn = conn
	port := conn.LocalAddr().(*net.UDPAddr).Port
	s.repeaterDMRAddr["10.0.0.1"] = &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
	s.repeaterDMRAddr["10.0.0.2"] = &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}

	pkt := proto.Packet{
		Signature:   "DMRD",
		Src:         1001,
		Dst:         9,
		Slot:        false,
		GroupCall:   true,
		FrameType:   2,
		DTypeOrVSeq: 1,
		StreamID:    101,
	}

	pause := s.PrimeTargets(pkt, true, []string{"hytera:10.0.0.1", "hytera:10.0.0.2"})
	if pause != hyteraSyncWakeupTime {
		t.Fatalf("expected pause %v, got %v", hyteraSyncWakeupTime, pause)
	}
	if got := s.outboundSlotState(false, "hytera:10.0.0.1").outStreamID; got != 101 {
		t.Fatalf("expected first target primed stream 101, got %d", got)
	}
	if got := s.outboundSlotState(false, "hytera:10.0.0.2").outStreamID; got != 101 {
		t.Fatalf("expected second target primed stream 101, got %d", got)
	}
}

func TestSendWakeupsFromMotoActuallyWritesPackets(t *testing.T) {
	s := NewServer(&config.Config{}, nil)

	writer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen writer: %v", err)
	}
	defer writer.Close()
	s.dmrConn = writer

	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer target.Close()
	s.repeaterDMRAddr["10.0.0.1"] = target.LocalAddr().(*net.UDPAddr)

	pkt := proto.Packet{
		Src:       0x4631BC,
		Dst:       0x00B3C9,
		Slot:      false,
		GroupCall: true,
	}

	s.sendWakeups(pkt, 1, "hytera:10.0.0.1", true)

	_ = target.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 128)
	n, _, err := target.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("expected wakeup packet from moto path: %v", err)
	}
	if n != 72 {
		t.Fatalf("expected 72-byte wakeup packet, got %d", n)
	}
	if got := buf[8]; got != 0x42 {
		t.Fatalf("expected wakeup packet type 0x42, got 0x%02X", got)
	}
	second := make([]byte, 128)
	n, _, err = target.ReadFromUDP(second)
	if err != nil {
		t.Fatalf("expected second wakeup packet from moto path: %v", err)
	}
	if n != 72 {
		t.Fatalf("expected second 72-byte wakeup packet, got %d", n)
	}
	if got, want := second[12], byte(0x02); got != want {
		t.Fatalf("expected second wakeup on slot 2, got marker 0x%02X want 0x%02X", got, want)
	}
}

func TestMotoWakeupConfigMatchesReferenceBridge(t *testing.T) {
	if hyteraWakeupRetries != 3 {
		t.Fatalf("expected moto wakeup retries 3, got %d", hyteraWakeupRetries)
	}
	if hyteraSyncWakeupTime != 300*time.Millisecond {
		t.Fatalf("expected moto wakeup pause 300ms, got %v", hyteraSyncWakeupTime)
	}
}

func TestBuildSyncPacketVisibleIDsDoNotShiftSourceWords(t *testing.T) {
	data := buildSyncPacket(false, 0x4640CF, 0x00B3C9, true, 0)
	got := data[33:45]
	want := []byte{
		0x00, 0x00,
		0xB3, 0x00,
		0xC9, 0x00,
		0x46, 0x00,
		0x40, 0x00,
		0xCF, 0x00,
	}
	if !bytesEqual(got, want) {
		t.Fatalf("sync visible ids mismatch: got % X want % X", got, want)
	}
}

func TestDecodeCurrentMotoToHyteraHeaderIDs(t *testing.T) {
	s := NewServer(&config.Config{MMDVMClients: []config.MMDVM{{ID: 12345, ColorCode: 1}}}, nil)
	data := []byte{
		0x5A, 0x5A, 0x5A, 0x5A, 0x01, 0xE0, 0x00, 0x00,
		0x41, 0x00, 0x05, 0x01, 0x01, 0x00, 0x00, 0x00,
		0x11, 0x11, 0x11, 0x11, 0x00, 0x00, 0x00, 0x00,
		0x40, 0x5C, 0xB1, 0x0E, 0x26, 0x11, 0x78, 0x02,
		0x60, 0x7C, 0xB0, 0x6D, 0x00, 0xD5, 0x6D, 0x00,
		0x57, 0xFF, 0x5D, 0xD7, 0xD3, 0xF5, 0x30, 0xAE,
		0x49, 0x27, 0xD2, 0x62, 0x41, 0xA5, 0x00, 0xA9,
		0x91, 0x63, 0x00, 0x7C, 0x63, 0x02, 0x01, 0x00,
		0xC9, 0xB3, 0x00, 0x00, 0xCF, 0x40, 0x46, 0x00,
	}

	pkt, err := s.decodeToMMDVM(data)
	if err != nil {
		t.Fatalf("decodeToMMDVM: %v", err)
	}
	if pkt.Dst != 46025 {
		t.Fatalf("unexpected dst: got %d want %d", pkt.Dst, 46025)
	}
	if pkt.Src != 4604111 {
		t.Fatalf("unexpected src: got %d want %d", pkt.Src, 4604111)
	}
}

func TestDecodeCurrentMotoToHyteraVoiceBurstIDs(t *testing.T) {
	s := NewServer(&config.Config{MMDVMClients: []config.MMDVM{{ID: 12345, ColorCode: 1}}}, nil)
	data := []byte{
		0x5A, 0x5A, 0x5A, 0x5A, 0x04, 0xE0, 0x00, 0x00,
		0x41, 0x00, 0x05, 0x01, 0x01, 0x00, 0x00, 0x00,
		0x11, 0x11, 0xBB, 0xBB, 0x00, 0x00, 0x00, 0x00,
		0x40, 0x5C, 0xE8, 0xB9, 0x52, 0x81, 0x73, 0x61,
		0x2A, 0x00, 0xB9, 0x6B, 0x81, 0xE8, 0x67, 0x52,
		0xD5, 0xF7, 0x57, 0xDD, 0xD1, 0xDF, 0x00, 0x73,
		0x6B, 0x2A, 0xE8, 0xB9, 0x52, 0x81, 0x73, 0x61,
		0x2A, 0x00, 0x00, 0x6B, 0x63, 0x02, 0x01, 0x00,
		0xC9, 0xB3, 0x00, 0x00, 0xCF, 0x40, 0x46, 0x00,
	}

	pkt, err := s.decodeToMMDVM(data)
	if err != nil {
		t.Fatalf("decodeToMMDVM: %v", err)
	}
	if pkt.Dst != 46025 {
		t.Fatalf("unexpected dst: got %d want %d", pkt.Dst, 46025)
	}
	if pkt.Src != 4604111 {
		t.Fatalf("unexpected src: got %d want %d", pkt.Src, 4604111)
	}
}

func TestParsePacketMeta_Ens17OutLine(t *testing.T) {
	line := "21:17:02.358633 ens17 Out IP 110.42.107.105.36559 > 43.129.83.124.62031: UDP, length 53"
	meta, ok := parsePacketMeta(line)
	if !ok {
		t.Fatal("expected parsePacketMeta ok")
	}
	if meta.srcIP != "110.42.107.105" || meta.dstIP != "43.129.83.124" {
		t.Fatalf("unexpected meta src=%s dst=%s", meta.srcIP, meta.dstIP)
	}
}

func TestParseTCPDumpPacketsFromFile_BMBMHasMultipleSources(t *testing.T) {
	path := resolveCapturePath(t, "bm-bm.txt")
	packets := parseTCPDumpPacketsFromFile(t, path)
	if len(packets) == 0 {
		t.Fatal("expected parsed packets")
	}
	seen := map[string]int{}
	for _, p := range packets {
		seen[p.srcIP]++
	}
	if len(seen) < 2 {
		t.Fatalf("expected at least 2 source IPs, got %v", seen)
	}
}

func TestDecodeBMReturnedHeaderColorCodeFromCapture(t *testing.T) {
	path := resolveCapturePath(t, "bm-bm.txt")
	packets := parseTCPDumpPacketsFromFile(t, path)
	_, returned, _ := splitBMDMRDPackets(t, packets)
	if len(returned) == 0 {
		t.Fatal("no returned BM DMRD packet found")
	}
	firstReturned := returned[0]
	if firstReturned.FrameType != 2 || firstReturned.DTypeOrVSeq != 1 {
		t.Fatalf("expected first returned packet to be LC header, got ft/dt=%d/%d", firstReturned.FrameType, firstReturned.DTypeOrVSeq)
	}

	var burst layer2.Burst
	burst.DecodeFromBytes(firstReturned.DMRData)
	if !burst.HasSlotType {
		t.Fatal("expected slot type in returned BM header")
	}
	t.Logf("bm returned first header color code=%d dmr=% X", burst.SlotType.ColorCode, firstReturned.DMRData)
}

func TestDecodeBMOutboundBurstKindsFromCapture(t *testing.T) {
	path := resolveCapturePath(t, "bm-bm.txt")
	packets := parseTCPDumpPacketsFromFile(t, path)
	outbound := make([]proto.Packet, 0, 16)
	for _, p := range packets {
		if p.srcIP != "110.42.107.105" {
			continue
		}
		if len(p.payload) < 4 || string(p.payload[:4]) != "DMRD" {
			continue
		}
		decoded, ok := proto.Decode(p.payload)
		if !ok {
			continue
		}
		outbound = append(outbound, decoded)
	}
	if len(outbound) == 0 {
		t.Fatal("no outbound BM DMRD packet found")
	}

	for i, pkt := range outbound {
		var burst layer2.Burst
		burst.DecodeFromBytes(pkt.DMRData)
		t.Logf(
			"idx=%d ft/dt=%d/%d voiceBurst=%d hasSlotType=%t hasEmbedded=%t sync=%d dmr=% X",
			i, pkt.FrameType, pkt.DTypeOrVSeq, int(burst.VoiceBurst), burst.HasSlotType, burst.HasEmbeddedSignalling, int(burst.SyncPattern), pkt.DMRData,
		)
	}
}

func TestCompareGeneratedHeaderAgainstBMReturnedHeader(t *testing.T) {
	path := resolveCapturePath(t, "bm-bm.txt")
	packets := parseTCPDumpPacketsFromFile(t, path)
	_, returned, _ := splitBMDMRDPackets(t, packets)
	var bmHeader proto.Packet
	found := false
	for _, pkt := range returned {
		if pkt.FrameType == 2 && pkt.DTypeOrVSeq == 1 {
			bmHeader = pkt
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no BM returned voice LC header found")
	}

	lc := intdmr.BuildStandardLCBytes(bmHeader.Src, bmHeader.Dst, bmHeader.GroupCall)
	generated := layer2.BuildLCDataBurst(lc, elements.DataTypeVoiceLCHeader, 1)
	if generated == bmHeader.DMRData {
		t.Logf("generated header equals BM returned header")
		return
	}
	t.Logf("generated header differs from BM returned header")
	t.Logf("generated=% X", generated)
	t.Logf("bm      =% X", bmHeader.DMRData)

	var bmBurst layer2.Burst
	bmBurst.DecodeFromBytes(bmHeader.DMRData)
	if flc, ok := bmBurst.Data.(*pdu.FullLinkControl); ok {
		t.Logf("bm header decoded flco=%v src=%d dst(group)=%d target=%d fid=%v svc=%v",
			flc.FLCO, flc.SourceAddress, flc.GroupAddress, flc.TargetAddress, flc.FeatureSetID, flc.ServiceOptions)
	}

	var genBurst layer2.Burst
	genBurst.DecodeFromBytes(generated)
	if flc, ok := genBurst.Data.(*pdu.FullLinkControl); ok {
		t.Logf("gen header decoded flco=%v src=%d dst(group)=%d target=%d fid=%v svc=%v",
			flc.FLCO, flc.SourceAddress, flc.GroupAddress, flc.TargetAddress, flc.FeatureSetID, flc.ServiceOptions)
	}
}

func TestCompareGeneratedHeaderAgainstBMReturnedHeaderDistance(t *testing.T) {
	path := resolveCapturePath(t, "bm-bm.txt")
	packets := parseTCPDumpPacketsFromFile(t, path)
	_, returned, _ := splitBMDMRDPackets(t, packets)

	var bmHeader proto.Packet
	found := false
	for _, pkt := range returned {
		if pkt.FrameType == 2 && pkt.DTypeOrVSeq == 1 {
			bmHeader = pkt
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no BM returned voice LC header found")
	}

	lc := intdmr.BuildStandardLCBytes(bmHeader.Src, bmHeader.Dst, bmHeader.GroupCall)
	generated := layer2.BuildLCDataBurst(lc, elements.DataTypeVoiceLCHeader, 1)

	if len(generated) != len(bmHeader.DMRData) {
		t.Fatalf("length mismatch: generated=%d bm=%d", len(generated), len(bmHeader.DMRData))
	}

	diffBytes := 0
	diffBits := 0
	for i := 0; i < len(generated); i++ {
		x := generated[i] ^ bmHeader.DMRData[i]
		if x != 0 {
			diffBytes++
			t.Logf("byte[%02d]: gen=%02X bm=%02X xor=%02X", i, generated[i], bmHeader.DMRData[i], x)
		}
		for b := 0; b < 8; b++ {
			if (x & (1 << b)) != 0 {
				diffBits++
			}
		}
	}

	t.Logf("summary: totalBytes=%d diffBytes=%d diffBits=%d", len(generated), diffBytes, diffBits)
}

func TestDMRGoRoundTripBMReturnedHeader(t *testing.T) {
	path := resolveCapturePath(t, "bm-bm.txt")
	packets := parseTCPDumpPacketsFromFile(t, path)
	_, returned, _ := splitBMDMRDPackets(t, packets)

	var bmHeader proto.Packet
	found := false
	for _, pkt := range returned {
		if pkt.FrameType == 2 && pkt.DTypeOrVSeq == 1 {
			bmHeader = pkt
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no BM returned voice LC header found")
	}

	var burst layer2.Burst
	burst.DecodeFromBytes(bmHeader.DMRData)
	if !burst.HasSlotType {
		t.Fatal("expected slot type in bm returned header")
	}
	flc, ok := burst.Data.(*pdu.FullLinkControl)
	if !ok {
		t.Fatal("expected full link control in bm returned header")
	}

	enc, err := flc.Encode()
	if err != nil {
		t.Fatalf("flc encode failed: %v", err)
	}
	var lc [12]byte
	copy(lc[:], enc)
	regen := layer2.BuildLCDataBurst(lc, elements.DataTypeVoiceLCHeader, uint8(burst.SlotType.ColorCode))

	diffBytes := 0
	diffBits := 0
	for i := range regen {
		x := regen[i] ^ bmHeader.DMRData[i]
		if x != 0 {
			diffBytes++
		}
		for b := 0; b < 8; b++ {
			if (x & (1 << b)) != 0 {
				diffBits++
			}
		}
	}
	t.Logf(
		"roundtrip: src=%d dst=%d flco=%v fid=%v cc=%d diffBytes=%d diffBits=%d",
		flc.SourceAddress, flc.GroupAddress, flc.FLCO, flc.FeatureSetID, burst.SlotType.ColorCode, diffBytes, diffBits,
	)
}

func TestDumpBMHeaderHexAndFullLCDetail(t *testing.T) {
	if os.Getenv("DUMP_BM_HEADER") != "1" {
		t.Skip("set DUMP_BM_HEADER=1 to dump full byte-by-byte comparison")
	}

	path := resolveCapturePath(t, "bm-bm.txt")
	packets := parseTCPDumpPacketsFromFile(t, path)
	_, returned, _ := splitBMDMRDPackets(t, packets)

	var bmHeader proto.Packet
	found := false
	for _, pkt := range returned {
		if pkt.FrameType == 2 && pkt.DTypeOrVSeq == 1 {
			bmHeader = pkt
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no BM returned voice LC header found")
	}

	bmDataArr := bmHeader.DMRData
	bmData := bmDataArr[:]
	standardLC := intdmr.BuildStandardLCBytes(bmHeader.Src, bmHeader.Dst, bmHeader.GroupCall)
	stdData := layer2.BuildLCDataBurst(standardLC, elements.DataTypeVoiceLCHeader, 1)
	newData := buildLCDataBurstWithInternalBPTC(standardLC, elements.DataTypeVoiceLCHeader, 1)

	var bmBurst layer2.Burst
	bmBurst.DecodeFromBytes(bmDataArr)
	bmFLC, ok := bmBurst.Data.(*pdu.FullLinkControl)
	if !ok {
		t.Fatal("expected FullLinkControl from BM header")
	}
	bmLCEncoded, err := bmFLC.Encode()
	if err != nil {
		t.Fatalf("encode decoded BM FLC failed: %v", err)
	}
	var bmLC12 [12]byte
	copy(bmLC12[:], bmLCEncoded)
	regenFromDecoded := layer2.BuildLCDataBurst(bmLC12, elements.DataTypeVoiceLCHeader, uint8(bmBurst.SlotType.ColorCode))

	var stdBurst layer2.Burst
	stdBurst.DecodeFromBytes(stdData)
	stdFLC, _ := stdBurst.Data.(*pdu.FullLinkControl)

	t.Logf("BM packet outer fields: src=%d dst=%d slot=%t gc=%t ft/dt=%d/%d seq=%d stream=%d", bmHeader.Src, bmHeader.Dst, bmHeader.Slot, bmHeader.GroupCall, bmHeader.FrameType, bmHeader.DTypeOrVSeq, bmHeader.Seq, bmHeader.StreamID)
	t.Logf("BM DMRData (33B)        = % X", bmData)
	t.Logf("STD DMRData (33B)       = % X", stdData)
	t.Logf("NEW DMRData (33B)       = % X", newData)
	t.Logf("REGEN DMRData (33B)     = % X", regenFromDecoded)
	t.Logf("STD LC bytes (12B)      = % X", standardLC)
	t.Logf("BM decoded->LC (12B)    = % X", bmLC12)
	t.Logf("BM decoded FullLC       = flco=%v fid=%v src=%d dst(group)=%d target=%d svc=%v cc=%d", bmFLC.FLCO, bmFLC.FeatureSetID, bmFLC.SourceAddress, bmFLC.GroupAddress, bmFLC.TargetAddress, bmFLC.ServiceOptions, bmBurst.SlotType.ColorCode)
	if stdFLC != nil {
		t.Logf("STD decoded FullLC      = flco=%v fid=%v src=%d dst(group)=%d target=%d svc=%v cc=%d", stdFLC.FLCO, stdFLC.FeatureSetID, stdFLC.SourceAddress, stdFLC.GroupAddress, stdFLC.TargetAddress, stdFLC.ServiceOptions, stdBurst.SlotType.ColorCode)
	}

	logByteDiff(t, "BM vs STD DMRData", bmData, stdData[:])
	logByteDiff(t, "BM vs NEW DMRData", bmData, newData[:])
	logByteDiff(t, "STD vs NEW DMRData", stdData[:], newData[:])
	logByteDiff(t, "BM vs REGEN DMRData", bmData, regenFromDecoded[:])
	logByteDiff(t, "BM-LC12 vs STD-LC12", bmLC12[:], standardLC[:])
	logDiffSummary(t, "BM vs STD DMRData", bmData, stdData[:])
	logDiffSummary(t, "BM vs NEW DMRData", bmData, newData[:])
	logDiffSummary(t, "BM vs REGEN DMRData", bmData, regenFromDecoded[:])
	logDiffSummary(t, "STD vs NEW DMRData", stdData[:], newData[:])
}

func buildLCDataBurstWithInternalBPTC(lcBytes [12]byte, dataType elements.DataType, colorCode uint8) [33]byte {
	var infoBits [96]byte
	for i := 0; i < 12; i++ {
		for j := 0; j < 8; j++ {
			if (lcBytes[i]>>(7-j))&1 == 1 {
				infoBits[i*8+j] = 1
			}
		}
	}

	encoded := internalbptc.Encode(infoBits)

	var bitData [264]bool
	for i := 0; i < 98; i++ {
		bitData[i] = encoded[i] == 1
	}
	for i := 0; i < 98; i++ {
		bitData[166+i] = encoded[98+i] == 1
	}

	// Keep slot-type packing consistent with dmrgo BuildLCDataBurst.
	inputByte := colorCode&0xF<<4 | byte(dataType&0xF)
	slotTypeBits := golay.Encode(inputByte)
	for i := 0; i < 10; i++ {
		bitData[98+i] = slotTypeBits[i] == 1
	}
	for i := 0; i < 10; i++ {
		bitData[156+i] = slotTypeBits[10+i] == 1
	}

	syncVal := int64(enums.BsSourcedData)
	for i := 0; i < 48; i++ {
		bitData[108+i] = ((syncVal >> (47 - i)) & 1) == 1
	}

	var data [33]byte
	for i := 0; i < 264; i++ {
		if bitData[i] {
			data[i/8] |= 1 << (7 - (i % 8))
		}
	}
	return data
}

func logByteDiff(t *testing.T, title string, left, right []byte) {
	t.Helper()
	if len(left) != len(right) {
		t.Logf("%s length mismatch: left=%d right=%d", title, len(left), len(right))
		return
	}
	t.Logf("%s byte diff start (len=%d)", title, len(left))
	for i := 0; i < len(left); i++ {
		x := left[i] ^ right[i]
		t.Logf("%s [%02d] left=%02X right=%02X xor=%02X", title, i, left[i], right[i], x)
	}
	t.Logf("%s byte diff end", title)
}

func logDiffSummary(t *testing.T, title string, left, right []byte) {
	t.Helper()
	if len(left) != len(right) {
		t.Logf("%s summary: length mismatch left=%d right=%d", title, len(left), len(right))
		return
	}
	diffBytes := 0
	diffBits := 0
	for i := 0; i < len(left); i++ {
		x := left[i] ^ right[i]
		if x != 0 {
			diffBytes++
		}
		for b := 0; b < 8; b++ {
			if (x & (1 << b)) != 0 {
				diffBits++
			}
		}
	}
	t.Logf("%s summary: total=%d diffBytes=%d diffBits=%d", title, len(left), diffBytes, diffBits)
}

func TestBruteforceStandardLCCanMatchBMHeader(t *testing.T) {
	if os.Getenv("REVERSE_BM_ANALYZE") != "1" {
		t.Skip("set REVERSE_BM_ANALYZE=1 to run brute-force LC parameter analysis")
	}

	path := resolveCapturePath(t, "bm-bm.txt")
	packets := parseTCPDumpPacketsFromFile(t, path)
	_, returned, _ := splitBMDMRDPackets(t, packets)
	var bmHeader proto.Packet
	found := false
	for _, pkt := range returned {
		if pkt.FrameType == 2 && pkt.DTypeOrVSeq == 1 {
			bmHeader = pkt
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no BM returned LC header found in bm-bm.txt")
	}

	src := bmHeader.Src
	dst := bmHeader.Dst
	target := bmHeader.DMRData

	type optionCase struct {
		protect bool
		flco    enums.FLCO
		fid     enums.FeatureSetID
		so      byte
		cc      uint8
	}

	fids := []enums.FeatureSetID{
		enums.StandardizedFID,
		enums.MotorolaLtd,
		enums.HytScienceTech,
		enums.TaitElectronicsLtd,
		enums.KirisunCommunications,
	}
	flcos := []enums.FLCO{
		enums.FLCOGroupVoiceChannelUser,
		enums.FLCOUnitToUnitVoiceChannelUser,
	}

	var matches []optionCase
	for _, cc := range []uint8{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15} {
		for _, protect := range []bool{false, true} {
			for _, flco := range flcos {
				for _, fid := range fids {
					for so := 0; so <= 0xFF; so++ {
						flc := pdu.FullLinkControl{
							ProtectFlag:  protect,
							FLCO:         flco,
							FeatureSetID: fid,
							ServiceOptions: l3elements.ServiceOptions{
								Reserved: [2]byte{byte((so >> 1) & 1), byte(so & 1)},
							},
							GroupAddress:  int(dst),
							TargetAddress: int(dst),
							SourceAddress: int(src),
						}
						enc, err := flc.Encode()
						if err != nil {
							continue
						}
						var lc [12]byte
						copy(lc[:], enc)
						gen := layer2.BuildLCDataBurst(lc, elements.DataTypeVoiceLCHeader, cc)
						if gen == target {
							matches = append(matches, optionCase{
								protect: protect,
								flco:    flco,
								fid:     fid,
								so:      byte(so),
								cc:      cc,
							})
						}
					}
				}
			}
		}
	}

	t.Logf("bruteforce matches=%d", len(matches))
	for i, m := range matches {
		if i >= 10 {
			break
		}
		t.Logf("match[%d]: protect=%t flco=%v fid=%v so=0x%02X cc=%d", i, m.protect, m.flco, m.fid, m.so, m.cc)
	}
}

func TestVoiceLCHeaderInputsAffectDMRData(t *testing.T) {
	cc := uint8(1)
	base := proto.Packet{
		Signature:   "DMRD",
		Src:         4604111,
		Dst:         46025,
		Slot:        false,
		GroupCall:   true,
		FrameType:   2,
		DTypeOrVSeq: 1,
		StreamID:    123,
		Seq:         10,
	}

	baseHeader := standardizeMotoLCPacket(base, cc)
	baseDMR := baseHeader.DMRData

	changedID := base
	changedID.Src = 4601816
	idDMR := standardizeMotoLCPacket(changedID, cc).DMRData
	if idDMR == baseDMR {
		t.Fatal("expected src/dst id change to affect voice lc header dmrdata")
	}

	changedTS := base
	changedTS.Slot = true
	tsDMR := standardizeMotoLCPacket(changedTS, cc).DMRData
	if tsDMR != baseDMR {
		t.Fatalf("expected timeslot change not to affect voice lc header dmrdata, got base=% X ts=% X", baseDMR, tsDMR)
	}

	changedSeq := base
	changedSeq.Seq = 77
	seqDMR := standardizeMotoLCPacket(changedSeq, cc).DMRData
	if seqDMR != baseDMR {
		t.Fatalf("expected seq change not to affect voice lc header dmrdata, got base=% X seq=% X", baseDMR, seqDMR)
	}

	changedType := base
	changedType.DTypeOrVSeq = 2 // terminator with LC
	typeDMR := standardizeMotoLCPacket(changedType, cc).DMRData
	if typeDMR == baseDMR {
		t.Fatal("expected frame type/data type change to affect dmrdata")
	}
}

func TestNormalizeMotoColorCode_ZeroFallsBackToConfig(t *testing.T) {
	cc, source := normalizeMotoColorCode(0, "embedded", 1)
	if cc != 1 {
		t.Fatalf("expected fallback cc=1, got %d", cc)
	}
	if source != "embedded-zero->fallback" {
		t.Fatalf("unexpected source tag: %s", source)
	}

	cc, source = normalizeMotoColorCode(3, "embedded", 1)
	if cc != 3 || source != "embedded" {
		t.Fatalf("expected keep non-zero cc, got cc=%d source=%s", cc, source)
	}
}

func TestPatchMotoVoiceEmbeddedControl_UsesConfiguredCCWhenZero(t *testing.T) {
	pkt := proto.Packet{
		Signature:   "DMRD",
		FrameType:   0,
		DTypeOrVSeq: 1,
		Src:         4604111,
		Dst:         46025,
		GroupCall:   true,
	}
	// Real moto->hytera voice burst B sample with embedded CC=0.
	pkt.DMRData = [33]byte{
		0xB9, 0xE8, 0x81, 0x52, 0x61, 0x73, 0x00, 0x2A,
		0x6B, 0xB9, 0xE8, 0x81, 0x52, 0x60, 0x21, 0x10,
		0x51, 0xE1, 0x27, 0x31, 0x73, 0x00, 0x2A, 0x6B,
		0xB9, 0xE8, 0x81, 0x52, 0x61, 0x73, 0x00, 0x2A,
		0x6B,
	}

	before := pkt
	got := patchMotoVoiceEmbeddedControl(pkt, 1)
	if got.DMRData == before.DMRData {
		t.Fatal("expected voice burst DMRData to be rewritten when embedded CC is zero")
	}

	var b layer2.Burst
	b.DecodeFromBytes(got.DMRData)
	if !b.HasEmbeddedSignalling {
		t.Fatal("expected embedded signalling after patch")
	}
	if b.EmbeddedSignalling.ColorCode != 1 {
		t.Fatalf("expected embedded CC=1, got %d", b.EmbeddedSignalling.ColorCode)
	}

	// Non-zero embedded CC should be preserved.
	preserve := got
	preserved := patchMotoVoiceEmbeddedControl(preserve, 7)
	var b2 layer2.Burst
	b2.DecodeFromBytes(preserved.DMRData)
	if b2.EmbeddedSignalling.ColorCode != 1 {
		t.Fatalf("expected existing non-zero CC preserved, got %d", b2.EmbeddedSignalling.ColorCode)
	}
}

func TestPatchMotoVoiceEmbeddedControl_NormalizesVoiceFToSingleFragment(t *testing.T) {
	pkt := proto.Packet{
		Signature:   "DMRD",
		FrameType:   0,
		DTypeOrVSeq: 5,
		Src:         4604111,
		Dst:         46025,
		GroupCall:   true,
	}
	pkt.DMRData = [33]byte{
		0xB9, 0xE8, 0x81, 0x52, 0x61, 0x73, 0x00, 0x2A,
		0x6B, 0xB9, 0xE8, 0x81, 0x52, 0x61, 0x55, 0xFD,
		0x24, 0x1A, 0xF0, 0x71, 0x73, 0x00, 0x2A, 0x6B,
		0xB9, 0xE8, 0x81, 0x52, 0x61, 0x73, 0x00, 0x2A,
		0x6B,
	}
	got := patchMotoVoiceEmbeddedControl(pkt, 1)
	var b layer2.Burst
	b.DecodeFromBytes(got.DMRData)
	if !b.HasEmbeddedSignalling {
		t.Fatal("expected embedded signalling on voice F")
	}
	if b.EmbeddedSignalling.LCSS != enums.SingleFragmentLCorCSBK {
		t.Fatalf("expected LCSS single fragment(0), got %d", b.EmbeddedSignalling.LCSS)
	}
	if b.EmbeddedSignalling.ColorCode != 1 {
		t.Fatalf("expected embedded CC=1, got %d", b.EmbeddedSignalling.ColorCode)
	}
}

func TestDumpBMPacketLengthsAndCurrentOutput(t *testing.T) {
	if os.Getenv("DUMP_BM_PACKET_LENGTHS") != "1" {
		t.Skip("set DUMP_BM_PACKET_LENGTHS=1 to dump bm/current packet lengths and hex")
	}

	s := NewServer(&config.Config{Local: config.Local{ColorCode: 1}, Hytera: config.Hytera{}}, nil)
	path := resolveCapturePath(t, "bm-bm.txt")
	packets := parseTCPDumpPacketsFromFile(t, path)
	if len(packets) == 0 {
		t.Fatal("expected parsed packets from bm-bm.txt")
	}

	var (
		firstBMRaw     []byte
		firstBMDecoded proto.Packet
		foundBM        bool
		bmOutbound     []proto.Packet
		bmReturned     []proto.Packet
	)

	for _, p := range packets {
		if len(p.payload) < 4 || string(p.payload[:4]) != "DMRD" {
			continue
		}
		decoded, ok := proto.Decode(p.payload)
		if !ok {
			t.Fatalf("decode proto payload failed: len=%d", len(p.payload))
		}
		switch p.srcIP {
		case "110.42.107.105":
			bmOutbound = append(bmOutbound, decoded)
		case "8.218.137.199":
			bmReturned = append(bmReturned, decoded)
			if !foundBM {
				firstBMRaw = append([]byte(nil), p.payload...)
				firstBMDecoded = decoded
				foundBM = true
			}
		}
	}
	if !foundBM || len(bmOutbound) == 0 || len(bmReturned) == 0 {
		t.Fatalf("missing bm packets: foundBM=%v outbound=%d returned=%d", foundBM, len(bmOutbound), len(bmReturned))
	}

	actual, err := replayMotoToHyteraPure(s, bmOutbound)
	if err != nil {
		t.Fatalf("pure replay failed: %v", err)
	}
	if len(actual) == 0 {
		t.Fatal("expected replay output packets")
	}

	currentFirst := actual[0]
	currentRaw := currentFirst.Encode()
	bmDecodedRaw := firstBMDecoded.Encode()

	t.Logf("bm capture payload len=%d", len(firstBMRaw))
	t.Logf("bm decoded encode len=%d", len(bmDecodedRaw))
	t.Logf("current output encode len=%d", len(currentRaw))

	t.Logf("bm capture raw payload   = % X", firstBMRaw)
	t.Logf("bm decoded 53-byte frame = % X", bmDecodedRaw)
	t.Logf("current output 53-byte   = % X", currentRaw)
}

func TestDumpBMOutboundVsReturnedMMDVMVoiceNormalization(t *testing.T) {
	if os.Getenv("DUMP_BM_VOICE_NORMALIZE") != "1" {
		t.Skip("set DUMP_BM_VOICE_NORMALIZE=1 to dump outbound/returned voice normalization details")
	}

	path := resolveCapturePath(t, "bm-bm.txt")
	packets := parseTCPDumpPacketsFromFile(t, path)
	outbound, returned, _ := splitBMDMRDPackets(t, packets)
	if len(outbound) == 0 || len(returned) == 0 {
		t.Fatalf("missing packets: outbound=%d returned=%d", len(outbound), len(returned))
	}

	t.Logf("outbound count=%d returned count=%d", len(outbound), len(returned))
	t.Logf("---- outbound to BM ----")
	outAsm := newEmbeddedAssembler()
	for i, pkt := range outbound {
		dumpPacketWithEmbedded(t, "out", i, pkt, outAsm)
	}
	t.Logf("---- returned from BM ----")
	retAsm := newEmbeddedAssembler()
	for i, pkt := range returned {
		dumpPacketWithEmbedded(t, "ret", i, pkt, retAsm)
	}

	// Compare voice packets in order while skipping BM's extra LC headers.
	outVoice := make([]proto.Packet, 0, len(outbound))
	retVoice := make([]proto.Packet, 0, len(returned))
	for _, p := range outbound {
		if p.FrameType == 0 || p.FrameType == 1 {
			outVoice = append(outVoice, p)
		}
	}
	for _, p := range returned {
		if p.FrameType == 0 || p.FrameType == 1 {
			retVoice = append(retVoice, p)
		}
	}
	pairs := len(outVoice)
	if len(retVoice) < pairs {
		pairs = len(retVoice)
	}
	t.Logf("voice compare pairs=%d (out=%d ret=%d)", pairs, len(outVoice), len(retVoice))

	diffDMR := 0
	diffEmb := 0
	compact := os.Getenv("DUMP_BM_VOICE_COMPACT") == "1"
	for i := 0; i < pairs; i++ {
		o := outVoice[i]
		r := retVoice[i]
		if o.DMRData != r.DMRData {
			diffDMR++
		}
		oEmb, oHasEmb := packetEmbedded4(o)
		rEmb, rHasEmb := packetEmbedded4(r)
		if oHasEmb != rHasEmb || (oHasEmb && oEmb != rEmb) {
			diffEmb++
		}

		t.Logf(
			"pair[%d] out(ft/dt=%d/%d seq=%d stream=%d src=%d dst=%d) | ret(ft/dt=%d/%d seq=%d stream=%d src=%d dst=%d)",
			i, o.FrameType, o.DTypeOrVSeq, o.Seq, o.StreamID, o.Src, o.Dst,
			r.FrameType, r.DTypeOrVSeq, r.Seq, r.StreamID, r.Src, r.Dst,
		)
		t.Logf("pair[%d] out33=% X", i, o.DMRData)
		t.Logf("pair[%d] ret33=% X", i, r.DMRData)
		if compact {
			t.Logf("pair[%d] xor33=% X", i, xor33(o.DMRData, r.DMRData))
		} else {
			logByteDiff(t, "pair out33 vs ret33", o.DMRData[:], r.DMRData[:])
		}
		if oHasEmb {
			t.Logf("pair[%d] outEmb4=% X", i, oEmb)
		}
		if rHasEmb {
			t.Logf("pair[%d] retEmb4=% X", i, rEmb)
		}
		if oHasEmb && rHasEmb {
			t.Logf("pair[%d] xorEmb4=% X", i, [4]byte{oEmb[0] ^ rEmb[0], oEmb[1] ^ rEmb[1], oEmb[2] ^ rEmb[2], oEmb[3] ^ rEmb[3]})
		}
	}

	t.Logf("voice normalization summary: compared=%d dmrDataChanged=%d embeddedChanged=%d", pairs, diffDMR, diffEmb)
}

func TestDumpHytCaptureDecode(t *testing.T) {
	if os.Getenv("DUMP_HYT_ANALYZE") != "1" {
		t.Skip("set DUMP_HYT_ANALYZE=1 to dump hyt.txt decode analysis")
	}

	s := NewServer(&config.Config{
		Local:        config.Local{ColorCode: 1},
		MMDVMClients: []config.MMDVM{{ID: 12345, ColorCode: 1}},
	}, nil)

	path := resolveCapturePath(t, "hyt.txt")
	packets := parseTCPDumpPacketsFromFile(t, path)
	if len(packets) == 0 {
		t.Fatalf("no packets parsed from %s", path)
	}

	total := 0
	okCnt := 0
	failCnt := 0
	asm := newEmbeddedAssembler()

	for i, p := range packets {
		// Focus on outgoing hytera DMR channel packets in this capture.
		if p.srcIP != "110.42.107.105" || p.dstIP != "49.77.145.131" {
			continue
		}
		total++
		t.Logf("hyt[%d] rawLen=%d raw=% X", i, len(p.payload), p.payload)

		pkt, err := s.decodeToMMDVM(p.payload)
		if err != nil {
			failCnt++
			t.Logf("hyt[%d] decode err=%v", i, err)
			continue
		}
		okCnt++
		dumpPacketWithEmbedded(t, "hyt-dec", i, pkt, asm)
	}

	t.Logf("hyt decode summary: total=%d ok=%d fail=%d", total, okCnt, failCnt)
}

func TestCompareHytDecodedVsBMNormalizedVoice(t *testing.T) {
	if os.Getenv("DUMP_HYT_VS_BM") != "1" {
		t.Skip("set DUMP_HYT_VS_BM=1 to compare hyt decoded voice against bm outbound/returned")
	}

	// Load bm reference sets.
	bmPath := resolveCapturePath(t, "bm-bm.txt")
	bmPackets := parseTCPDumpPacketsFromFile(t, bmPath)
	bmOut, bmRet, _ := splitBMDMRDPackets(t, bmPackets)

	bmOutVoice := make([]proto.Packet, 0, len(bmOut))
	bmRetVoice := make([]proto.Packet, 0, len(bmRet))
	outSet := map[[33]byte]struct{}{}
	retSet := map[[33]byte]struct{}{}
	for _, p := range bmOut {
		if p.FrameType == 0 || p.FrameType == 1 {
			bmOutVoice = append(bmOutVoice, p)
			outSet[p.DMRData] = struct{}{}
		}
	}
	for _, p := range bmRet {
		if p.FrameType == 0 || p.FrameType == 1 {
			bmRetVoice = append(bmRetVoice, p)
			retSet[p.DMRData] = struct{}{}
		}
	}
	if len(bmOutVoice) == 0 || len(bmRetVoice) == 0 {
		t.Fatalf("bm voice reference missing: out=%d ret=%d", len(bmOutVoice), len(bmRetVoice))
	}

	// Decode hyt capture to MMDVM packets.
	s := NewServer(&config.Config{
		Local:        config.Local{ColorCode: 1},
		MMDVMClients: []config.MMDVM{{ID: 12345, ColorCode: 1}},
	}, nil)
	hytPath := resolveCapturePath(t, "hyt.txt")
	hytPackets := parseTCPDumpPacketsFromFile(t, hytPath)

	hytVoice := make([]proto.Packet, 0, 128)
	for _, p := range hytPackets {
		if p.srcIP != "110.42.107.105" {
			continue
		}
		pkt, err := s.decodeToMMDVM(p.payload)
		if err != nil {
			continue
		}
		if pkt.FrameType == 0 || pkt.FrameType == 1 {
			hytVoice = append(hytVoice, pkt)
		}
	}
	if len(hytVoice) == 0 {
		t.Fatal("no decodable hyt voice packet")
	}

	matchOut := 0
	matchRet := 0
	for i, p := range hytVoice {
		_, inOut := outSet[p.DMRData]
		_, inRet := retSet[p.DMRData]
		if inOut {
			matchOut++
		}
		if inRet {
			matchRet++
		}
		if i < 12 {
			t.Logf(
				"hyt[%d] ft/dt=%d/%d seq=%d src=%d dst=%d inBMOut=%t inBMRet=%t dmr33=% X",
				i, p.FrameType, p.DTypeOrVSeq, p.Seq, p.Src, p.Dst, inOut, inRet, p.DMRData,
			)
		}
	}

	t.Logf(
		"hyt-vs-bm summary: hytVoice=%d matchBMOut=%d matchBMRet=%d bmOutVoice=%d bmRetVoice=%d",
		len(hytVoice), matchOut, matchRet, len(bmOutVoice), len(bmRetVoice),
	)
}

func TestSessionStructureCompareHytAndMMDVM(t *testing.T) {
	if os.Getenv("DUMP_SESSION_STRUCT") != "1" {
		t.Skip("set DUMP_SESSION_STRUCT=1 to dump per-session structure metrics")
	}

	// hyt.txt: decode Hytera raw UDP payload to MMDVM packets.
	s := NewServer(&config.Config{
		Local:        config.Local{ColorCode: 1},
		MMDVMClients: []config.MMDVM{{ID: 12345, ColorCode: 1}},
	}, nil)
	hytPath := resolveCapturePath(t, "hyt.txt")
	hytPkts := parseTCPDumpPacketsFromFile(t, hytPath)
	hytByDst := map[string][]proto.Packet{}
	for _, p := range hytPkts {
		if p.srcIP != "110.42.107.105" {
			continue
		}
		dec, err := s.decodeToMMDVM(p.payload)
		if err != nil {
			continue
		}
		hytByDst[p.dstIP] = append(hytByDst[p.dstIP], dec)
	}

	// mmdvm.txt: decode DMRD payload directly.
	mmdvmPath := resolveCapturePath(t, "mmdvm.txt")
	mmdvmPkts := parseTCPDumpPacketsFromFile(t, mmdvmPath)
	mmdvmBySrc := map[string][]proto.Packet{}
	for _, p := range mmdvmPkts {
		if len(p.payload) < 4 || string(p.payload[:4]) != "DMRD" {
			continue
		}
		dec, ok := proto.Decode(p.payload)
		if !ok {
			continue
		}
		mmdvmBySrc[p.srcIP] = append(mmdvmBySrc[p.srcIP], dec)
	}

	t.Logf("=== HYT session structure ===")
	dsts := mapKeysString(hytByDst)
	for _, dst := range dsts {
		stats := analyzeSessionStructure(hytByDst[dst])
		t.Logf("hyt dst=%s packets=%d", dst, len(hytByDst[dst]))
		logSessionStats(t, stats)
	}

	t.Logf("=== MMDVM session structure ===")
	srcs := mapKeysString(mmdvmBySrc)
	for _, src := range srcs {
		stats := analyzeSessionStructure(mmdvmBySrc[src])
		t.Logf("mmdvm src=%s packets=%d", src, len(mmdvmBySrc[src]))
		logSessionStats(t, stats)
	}
}

func dumpPacketWithEmbedded(t *testing.T, prefix string, idx int, pkt proto.Packet, asm *embeddedAssembler) {
	t.Helper()

	t.Logf(
		"%s[%d] ft/dt=%d/%d seq=%d slot=%t gc=%t src=%d dst=%d stream=%d dmr33=% X",
		prefix, idx, pkt.FrameType, pkt.DTypeOrVSeq, pkt.Seq, pkt.Slot, pkt.GroupCall, pkt.Src, pkt.Dst, pkt.StreamID, pkt.DMRData,
	)

	var burst layer2.Burst
	burst.DecodeFromBytes(pkt.DMRData)
	t.Logf(
		"%s[%d] burst voice=%d hasSlotType=%t hasEmbedded=%t sync=%d",
		prefix, idx, int(burst.VoiceBurst), burst.HasSlotType, burst.HasEmbeddedSignalling, int(burst.SyncPattern),
	)

	if pkt.FrameType == 2 && (pkt.DTypeOrVSeq == 1 || pkt.DTypeOrVSeq == 2) {
		if lc, ok := internalbptc.DecodeLCFromBurst(pkt.DMRData); ok {
			pf := (lc[0] & 0x80) != 0
			flco := lc[0] & 0x3F
			fid := lc[1]
			so := lc[2]
			dst := int(lc[3])<<16 | int(lc[4])<<8 | int(lc[5])
			src := int(lc[6])<<16 | int(lc[7])<<8 | int(lc[8])
			t.Logf(
				"%s[%d] lc12=% X parsed(PF=%t FLCO=0x%02X FID=0x%02X SO=0x%02X dst=%d src=%d RS=%02X %02X %02X)",
				prefix, idx, lc, pf, flco, fid, so, dst, src, lc[9], lc[10], lc[11],
			)
		} else {
			t.Logf("%s[%d] lc12 decode failed", prefix, idx)
		}
	}

	if burst.HasEmbeddedSignalling {
		t.Logf(
			"%s[%d] emb4=% X lcss=%d cc=%d",
			prefix, idx, burst.PackEmbeddedSignallingData(), int(burst.EmbeddedSignalling.LCSS), burst.EmbeddedSignalling.ColorCode,
		)
		if asm != nil && pkt.FrameType == 0 {
			if result, ok := asm.push(pkt, burst.PackEmbeddedSignallingData()); ok {
				t.Logf(
					"%s[%d] embedded-lc9=% X parsed(flco=0x%02X fid=0x%02X so=0x%02X dst=%d src=%d crc=%02X/%02X)",
					prefix, idx, result.lc9, result.flco, result.fid, result.so, result.dst, result.src, result.rxCRC, result.calcCRC,
				)
			}
		}
	}
}

func packetEmbedded4(pkt proto.Packet) ([4]byte, bool) {
	var burst layer2.Burst
	burst.DecodeFromBytes(pkt.DMRData)
	if !burst.HasEmbeddedSignalling {
		return [4]byte{}, false
	}
	return burst.PackEmbeddedSignallingData(), true
}

func TestSessionStructureCompareMotoVsBMToHytera(t *testing.T) {
	if os.Getenv("DUMP_HYT_BMHYT_COMPARE") != "1" {
		t.Skip("set DUMP_HYT_BMHYT_COMPARE=1 to compare hyt.txt and bm-hyt.txt structures")
	}

	hytByDst := decodeHyteraCaptureByDst(t, "hyt.txt")
	bmHytByDst := decodeHyteraCaptureByDst(t, "bm-hyt.txt")

	t.Logf("=== MOTO->HYT (hyt.txt) ===")
	for _, dst := range mapKeysString(hytByDst) {
		pkts := hytByDst[dst]
		stats := analyzeSessionStructure(pkts)
		t.Logf("hyt dst=%s packets=%d seqSig=%s", dst, len(pkts), frameTypeSeqSignature(pkts, 30))
		logSessionStats(t, stats)
	}

	t.Logf("=== BM->HYT (bm-hyt.txt) ===")
	for _, dst := range mapKeysString(bmHytByDst) {
		pkts := bmHytByDst[dst]
		stats := analyzeSessionStructure(pkts)
		t.Logf("bm-hyt dst=%s packets=%d seqSig=%s", dst, len(pkts), frameTypeSeqSignature(pkts, 30))
		logSessionStats(t, stats)
	}
}

func TestSessionStructureCompareMotoHytVsBMHyt(t *testing.T) {
	if os.Getenv("DUMP_MOTOHYT_BMHYT_COMPARE") != "1" {
		t.Skip("set DUMP_MOTOHYT_BMHYT_COMPARE=1 to compare moto-hyt.txt and bm-hyt.txt structures")
	}

	motoByDst := decodeHyteraCaptureByDst(t, "moto-hyt.txt")
	bmHytByDst := decodeHyteraCaptureByDst(t, "bm-hyt.txt")

	t.Logf("=== MOTO->HYT (moto-hyt.txt) ===")
	for _, dst := range mapKeysString(motoByDst) {
		pkts := motoByDst[dst]
		stats := analyzeSessionStructure(pkts)
		t.Logf("moto-hyt dst=%s packets=%d seqSig=%s", dst, len(pkts), frameTypeSeqSignature(pkts, 30))
		logSessionStats(t, stats)
	}

	t.Logf("=== BM->HYT (bm-hyt.txt) ===")
	for _, dst := range mapKeysString(bmHytByDst) {
		pkts := bmHytByDst[dst]
		stats := analyzeSessionStructure(pkts)
		t.Logf("bm-hyt dst=%s packets=%d seqSig=%s", dst, len(pkts), frameTypeSeqSignature(pkts, 30))
		logSessionStats(t, stats)
	}
}

func TestDetailedDiffMotoHytVsBMHyt(t *testing.T) {
	if os.Getenv("DUMP_MOTOHYT_BMHYT_DIFF") != "1" {
		t.Skip("set DUMP_MOTOHYT_BMHYT_DIFF=1 to dump detailed diff between moto-hyt and bm-hyt")
	}

	motoByDst := decodeHyteraCaptureByDst(t, "moto-hyt.txt")
	bmByDst := decodeHyteraCaptureByDst(t, "bm-hyt.txt")
	moto := motoByDst["49.77.145.131"]
	bm := bmByDst["49.77.145.131"]
	if len(moto) == 0 || len(bm) == 0 {
		t.Fatalf("missing packets for dst 49.77.145.131: moto=%d bm=%d", len(moto), len(bm))
	}

	t.Logf("moto total=%d bm total=%d", len(moto), len(bm))

	type key struct {
		ft uint
		dt uint
	}
	motoGroups := map[key][]proto.Packet{}
	bmGroups := map[key][]proto.Packet{}
	for _, p := range moto {
		k := key{ft: p.FrameType, dt: p.DTypeOrVSeq}
		motoGroups[k] = append(motoGroups[k], p)
	}
	for _, p := range bm {
		k := key{ft: p.FrameType, dt: p.DTypeOrVSeq}
		bmGroups[k] = append(bmGroups[k], p)
	}

	keys := make([]key, 0, len(motoGroups)+len(bmGroups))
	seen := map[key]struct{}{}
	for k := range motoGroups {
		seen[k] = struct{}{}
	}
	for k := range bmGroups {
		seen[k] = struct{}{}
	}
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ft != keys[j].ft {
			return keys[i].ft < keys[j].ft
		}
		return keys[i].dt < keys[j].dt
	})

	for _, k := range keys {
		mg := motoGroups[k]
		bg := bmGroups[k]
		t.Logf("group ft/dt=%d/%d moto=%d bm=%d", k.ft, k.dt, len(mg), len(bg))
		limit := len(mg)
		if len(bg) < limit {
			limit = len(bg)
		}
		if limit == 0 {
			continue
		}
		for i := 0; i < limit; i++ {
			diffBytes, diffBits := byteDiffSummary(mg[i].DMRData[:], bg[i].DMRData[:])
			t.Logf(
				"  idx=%d moto(seq=%d stream=%d slot=%t) bm(seq=%d stream=%d slot=%t) diffBytes=%d diffBits=%d",
				i, mg[i].Seq, mg[i].StreamID, mg[i].Slot, bg[i].Seq, bg[i].StreamID, bg[i].Slot, diffBytes, diffBits,
			)
			if i == 0 {
				t.Logf("  moto33=% X", mg[i].DMRData)
				t.Logf("  bm33  =% X", bg[i].DMRData)
				t.Logf("  xor33 =% X", xor33(mg[i].DMRData, bg[i].DMRData))

				var mb, bb layer2.Burst
				mb.DecodeFromBytes(mg[i].DMRData)
				bb.DecodeFromBytes(bg[i].DMRData)
				if mb.HasEmbeddedSignalling || bb.HasEmbeddedSignalling {
					me, mok := packetEmbedded4(mg[i])
					be, bok := packetEmbedded4(bg[i])
					if mok {
						t.Logf("  motoEmb4=% X", me)
					}
					if bok {
						t.Logf("  bmEmb4  =% X", be)
					}
					if mok && bok {
						t.Logf("  xorEmb4 =% X", [4]byte{me[0] ^ be[0], me[1] ^ be[1], me[2] ^ be[2], me[3] ^ be[3]})
					}
				}
			}
		}
	}
}

func TestExplainFieldDiffMotoHytVsBMHyt(t *testing.T) {
	if os.Getenv("DUMP_MOTOHYT_BMHYT_FIELDS") != "1" {
		t.Skip("set DUMP_MOTOHYT_BMHYT_FIELDS=1 to dump parsed field-level differences")
	}

	motoByDst := decodeHyteraCaptureByDst(t, "moto-hyt.txt")
	bmByDst := decodeHyteraCaptureByDst(t, "bm-hyt.txt")
	moto := motoByDst["49.77.145.131"]
	bm := bmByDst["49.77.145.131"]
	if len(moto) == 0 || len(bm) == 0 {
		t.Fatalf("missing packets for dst 49.77.145.131: moto=%d bm=%d", len(moto), len(bm))
	}

	type key struct{ ft, dt uint }
	motoGroups := map[key][]proto.Packet{}
	bmGroups := map[key][]proto.Packet{}
	for _, p := range moto {
		motoGroups[key{p.FrameType, p.DTypeOrVSeq}] = append(motoGroups[key{p.FrameType, p.DTypeOrVSeq}], p)
	}
	for _, p := range bm {
		bmGroups[key{p.FrameType, p.DTypeOrVSeq}] = append(bmGroups[key{p.FrameType, p.DTypeOrVSeq}], p)
	}

	keys := make([]key, 0, len(motoGroups)+len(bmGroups))
	seen := map[key]struct{}{}
	for k := range motoGroups {
		seen[k] = struct{}{}
	}
	for k := range bmGroups {
		seen[k] = struct{}{}
	}
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ft != keys[j].ft {
			return keys[i].ft < keys[j].ft
		}
		return keys[i].dt < keys[j].dt
	})

	for _, k := range keys {
		mg := motoGroups[k]
		bg := bmGroups[k]
		limit := len(mg)
		if len(bg) < limit {
			limit = len(bg)
		}
		if limit == 0 {
			continue
		}
		t.Logf("group ft/dt=%d/%d compare=%d (moto=%d bm=%d)", k.ft, k.dt, limit, len(mg), len(bg))
		for i := 0; i < limit && i < 2; i++ {
			mp := mg[i]
			bp := bg[i]
			rawBytes, rawBits := byteDiffSummary(mp.DMRData[:], bp.DMRData[:])
			t.Logf("  idx=%d raw diffBytes=%d diffBits=%d", i, rawBytes, rawBits)
			t.Logf("  moto meta: seq=%d stream=%d slot=%t src=%d dst=%d", mp.Seq, mp.StreamID, mp.Slot, mp.Src, mp.Dst)
			t.Logf("  bm   meta: seq=%d stream=%d slot=%t src=%d dst=%d", bp.Seq, bp.StreamID, bp.Slot, bp.Src, bp.Dst)

			switch {
			case k.ft == 2 && (k.dt == 1 || k.dt == 2):
				mLC, mok := internalbptc.DecodeLCFromBurst(mp.DMRData)
				bLC, bok := internalbptc.DecodeLCFromBurst(bp.DMRData)
				if mok && bok {
					t.Logf("  moto lc12=% X", mLC)
					t.Logf("  bm   lc12=% X", bLC)
					t.Logf("  xor  lc12=% X", [12]byte{
						mLC[0] ^ bLC[0], mLC[1] ^ bLC[1], mLC[2] ^ bLC[2], mLC[3] ^ bLC[3],
						mLC[4] ^ bLC[4], mLC[5] ^ bLC[5], mLC[6] ^ bLC[6], mLC[7] ^ bLC[7],
						mLC[8] ^ bLC[8], mLC[9] ^ bLC[9], mLC[10] ^ bLC[10], mLC[11] ^ bLC[11],
					})
					logLCParse(t, "moto", mLC)
					logLCParse(t, "bm", bLC)
				} else {
					t.Logf("  lc decode failed: moto=%t bm=%t", mok, bok)
				}
			case k.ft == 0 || k.ft == 1:
				var mb, bb layer2.Burst
				mb.DecodeFromBytes(mp.DMRData)
				bb.DecodeFromBytes(bp.DMRData)
				t.Logf("  moto burst: voiceBurst=%d sync=%d hasSlotType=%t hasEmb=%t", int(mb.VoiceBurst), int(mb.SyncPattern), mb.HasSlotType, mb.HasEmbeddedSignalling)
				t.Logf("  bm   burst: voiceBurst=%d sync=%d hasSlotType=%t hasEmb=%t", int(bb.VoiceBurst), int(bb.SyncPattern), bb.HasSlotType, bb.HasEmbeddedSignalling)
				if mb.HasEmbeddedSignalling || bb.HasEmbeddedSignalling {
					me, mok := packetEmbedded4(mp)
					be, bok := packetEmbedded4(bp)
					if mok {
						t.Logf("  moto emb4=% X lcss=%d cc=%d", me, int(mb.EmbeddedSignalling.LCSS), mb.EmbeddedSignalling.ColorCode)
					}
					if bok {
						t.Logf("  bm   emb4=% X lcss=%d cc=%d", be, int(bb.EmbeddedSignalling.LCSS), bb.EmbeddedSignalling.ColorCode)
					}
				}

				// Isolate non-voice differences: force both bursts to same voice payload.
				bbSameVoice := bb
				bbSameVoice.VoiceData = mb.VoiceData
				mEnc := mb.Encode()
				bEnc := bbSameVoice.Encode()
				nonVoiceBytes, nonVoiceBits := byteDiffSummary(mEnc[:], bEnc[:])
				t.Logf("  non-voice-only diff after voice align: diffBytes=%d diffBits=%d", nonVoiceBytes, nonVoiceBits)
				if nonVoiceBytes > 0 {
					t.Logf("  non-voice byte-level diff: %s", formatByteDiffs(mEnc[:], bEnc[:], 33))
				}
			}
		}
	}
}

func TestDumpLCRepeatConsistencyFromMotoAndBMHyt(t *testing.T) {
	if os.Getenv("DUMP_LC_REPEAT_CONSISTENCY") != "1" {
		t.Skip("set DUMP_LC_REPEAT_CONSISTENCY=1 to dump 2/1 and 2/2 LC consistency")
	}

	check := func(name string) {
		byDst := decodeHyteraCaptureByDst(t, name)
		pkts := byDst["49.77.145.131"]
		if len(pkts) == 0 {
			t.Fatalf("%s missing packets for dst 49.77.145.131", name)
		}

		var headers [][12]byte
		var terms [][12]byte
		for _, p := range pkts {
			if p.FrameType != 2 {
				continue
			}
			if p.DTypeOrVSeq != 1 && p.DTypeOrVSeq != 2 {
				continue
			}
			lc, ok := internalbptc.DecodeLCFromBurst(p.DMRData)
			if !ok {
				t.Logf("%s ft/dt=%d/%d seq=%d decode failed", name, p.FrameType, p.DTypeOrVSeq, p.Seq)
				continue
			}
			if p.DTypeOrVSeq == 1 {
				headers = append(headers, lc)
			} else {
				terms = append(terms, lc)
			}
		}

		t.Logf("=== %s 2/1 headers (%d) ===", name, len(headers))
		for i, lc := range headers {
			t.Logf("  h[%d]=% X", i, lc)
		}
		t.Logf("=== %s 2/2 terms (%d) ===", name, len(terms))
		for i, lc := range terms {
			t.Logf("  t[%d]=% X", i, lc)
		}

		if len(headers) > 0 {
			base := headers[0]
			allSame := true
			for i := 1; i < len(headers); i++ {
				if headers[i] != base {
					allSame = false
					t.Logf("%s header mismatch h[0]^h[%d]=% X", name, i, xor12(base, headers[i]))
				}
			}
			t.Logf("%s header repeat consistent=%t", name, allSame)
		}
		if len(terms) > 0 {
			base := terms[0]
			allSame := true
			for i := 1; i < len(terms); i++ {
				if terms[i] != base {
					allSame = false
					t.Logf("%s term mismatch t[0]^t[%d]=% X", name, i, xor12(base, terms[i]))
				}
			}
			t.Logf("%s term repeat consistent=%t", name, allSame)
		}
	}

	check("bm-hyt.txt")
	check("moto-hyt.txt")
}

func xor33(a, b [33]byte) [33]byte {
	var out [33]byte
	for i := 0; i < 33; i++ {
		out[i] = a[i] ^ b[i]
	}
	return out
}

func byteDiffSummary(a, b []byte) (diffBytes int, diffBits int) {
	if len(a) != len(b) {
		n := len(a)
		if len(b) < n {
			n = len(b)
		}
		a = a[:n]
		b = b[:n]
	}
	for i := 0; i < len(a); i++ {
		x := a[i] ^ b[i]
		if x != 0 {
			diffBytes++
		}
		for bit := 0; bit < 8; bit++ {
			if (x & (1 << bit)) != 0 {
				diffBits++
			}
		}
	}
	return diffBytes, diffBits
}

func formatByteDiffs(a, b []byte, maxShown int) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	shown := 0
	var sb strings.Builder
	for i := 0; i < n; i++ {
		if a[i] == b[i] {
			continue
		}
		if shown > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(fmt.Sprintf("[%02d] %02X/%02X", i, a[i], b[i]))
		shown++
		if maxShown > 0 && shown >= maxShown {
			break
		}
	}
	if shown == 0 {
		return "none"
	}
	return sb.String()
}

func logLCParse(t *testing.T, name string, lc [12]byte) {
	t.Helper()
	pf := (lc[0] & 0x80) != 0
	flco := lc[0] & 0x3F
	fid := lc[1]
	so := lc[2]
	dst := int(lc[3])<<16 | int(lc[4])<<8 | int(lc[5])
	src := int(lc[6])<<16 | int(lc[7])<<8 | int(lc[8])
	t.Logf("  %s parsed: PF=%t FLCO=0x%02X FID=0x%02X SO=0x%02X dst=%d src=%d RS=%02X %02X %02X",
		name, pf, flco, fid, so, dst, src, lc[9], lc[10], lc[11])
}

func xor12(a, b [12]byte) [12]byte {
	var out [12]byte
	for i := 0; i < 12; i++ {
		out[i] = a[i] ^ b[i]
	}
	return out
}

type sessionStats struct {
	totalPackets int
	streamCount  int

	headers   int
	terms     int
	voiceSync int
	voiceData int
	seqBreaks int

	embeddedTotal    int
	embeddedComplete int
	embeddedCRCOk    int
	embeddedIDMatch  int
	embeddedIDMis    int

	uniqueSrcDst int
	ccDist       map[int]int
	lcssDist     map[int]int
}

type streamAnalyzeState struct {
	lastSeq   uint
	hasLast   bool
	emb       *embeddedAssembler
	lastSrc   uint
	lastDst   uint
	haveOuter bool
}

func analyzeSessionStructure(pkts []proto.Packet) sessionStats {
	stats := sessionStats{
		totalPackets: len(pkts),
		ccDist:       map[int]int{},
		lcssDist:     map[int]int{},
	}
	streams := map[uint]*streamAnalyzeState{}
	srcdst := map[string]struct{}{}

	for _, p := range pkts {
		key := fmt.Sprintf("%d/%d", p.Src, p.Dst)
		srcdst[key] = struct{}{}

		st := streams[p.StreamID]
		if st == nil {
			st = &streamAnalyzeState{emb: newEmbeddedAssembler()}
			streams[p.StreamID] = st
		}

		if st.hasLast {
			if p.Seq != (st.lastSeq+1)&0xFF {
				stats.seqBreaks++
			}
		}
		st.lastSeq = p.Seq
		st.hasLast = true

		switch {
		case p.FrameType == 2 && p.DTypeOrVSeq == 1:
			stats.headers++
			st.lastSrc, st.lastDst = p.Src, p.Dst
			st.haveOuter = true
		case p.FrameType == 2 && p.DTypeOrVSeq == 2:
			stats.terms++
		case p.FrameType == 1:
			stats.voiceSync++
		case p.FrameType == 0:
			stats.voiceData++
		}

		var b layer2.Burst
		b.DecodeFromBytes(p.DMRData)
		if b.HasEmbeddedSignalling {
			stats.embeddedTotal++
			stats.ccDist[b.EmbeddedSignalling.ColorCode]++
			stats.lcssDist[int(b.EmbeddedSignalling.LCSS)]++
			if p.FrameType == 0 {
				if decoded, ok := st.emb.push(p, b.PackEmbeddedSignallingData()); ok {
					stats.embeddedComplete++
					if decoded.rxCRC == decoded.calcCRC {
						stats.embeddedCRCOk++
					}
					expectSrc, expectDst := p.Src, p.Dst
					if st.haveOuter {
						expectSrc, expectDst = st.lastSrc, st.lastDst
					}
					if uint(decoded.src) == expectSrc && uint(decoded.dst) == expectDst {
						stats.embeddedIDMatch++
					} else {
						stats.embeddedIDMis++
					}
				}
			}
		}
	}

	stats.streamCount = len(streams)
	stats.uniqueSrcDst = len(srcdst)
	return stats
}

func logSessionStats(t *testing.T, s sessionStats) {
	t.Helper()
	t.Logf("  streams=%d uniqueSrcDst=%d seqBreaks=%d", s.streamCount, s.uniqueSrcDst, s.seqBreaks)
	t.Logf("  header=%d term=%d voiceSync=%d voiceData=%d", s.headers, s.terms, s.voiceSync, s.voiceData)
	t.Logf("  embedded total=%d complete(B-E)=%d crcOk=%d idMatch=%d idMismatch=%d",
		s.embeddedTotal, s.embeddedComplete, s.embeddedCRCOk, s.embeddedIDMatch, s.embeddedIDMis)
	t.Logf("  embedded ccDist=%s lcssDist=%s", formatIntMap(s.ccDist), formatIntMap(s.lcssDist))
}

func formatIntMap(m map[int]int) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d:%d", k, m[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func mapKeysString[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func decodeHyteraCaptureByDst(t *testing.T, captureName string) map[string][]proto.Packet {
	t.Helper()
	s := NewServer(&config.Config{
		Local:        config.Local{ColorCode: 1},
		MMDVMClients: []config.MMDVM{{ID: 12345, ColorCode: 1}},
	}, nil)

	path := resolveCapturePath(t, captureName)
	rawPkts := parseTCPDumpPacketsFromFile(t, path)
	byDst := map[string][]proto.Packet{}
	for _, p := range rawPkts {
		if p.srcIP != "110.42.107.105" {
			continue
		}
		dec, err := s.decodeToMMDVM(p.payload)
		if err != nil {
			continue
		}
		byDst[p.dstIP] = append(byDst[p.dstIP], dec)
	}
	return byDst
}

func frameTypeSeqSignature(pkts []proto.Packet, limit int) string {
	if len(pkts) == 0 {
		return "-"
	}
	if limit > len(pkts) {
		limit = len(pkts)
	}
	parts := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		parts = append(parts, fmt.Sprintf("%d/%d", pkts[i].FrameType, pkts[i].DTypeOrVSeq))
	}
	return strings.Join(parts, ",")
}

type embeddedAssembler struct {
	byStream map[uint]*embState
}

type embState struct {
	frags [4][4]byte
	have  [4]bool
}

type embDecodeResult struct {
	lc9     [9]byte
	flco    byte
	fid     byte
	so      byte
	dst     int
	src     int
	rxCRC   byte
	calcCRC byte
}

func newEmbeddedAssembler() *embeddedAssembler {
	return &embeddedAssembler{byStream: map[uint]*embState{}}
}

func (a *embeddedAssembler) push(pkt proto.Packet, emb4 [4]byte) (embDecodeResult, bool) {
	fragIdx, ok := embFragIndexForDType(pkt.DTypeOrVSeq)
	if !ok {
		return embDecodeResult{}, false
	}
	st := a.byStream[pkt.StreamID]
	if st == nil {
		st = &embState{}
		a.byStream[pkt.StreamID] = st
	}
	st.frags[fragIdx] = emb4
	st.have[fragIdx] = true
	if !(st.have[0] && st.have[1] && st.have[2] && st.have[3]) {
		return embDecodeResult{}, false
	}

	lc9, rxCRC, calcCRC := decodeEmbeddedLC9FromFragments(st.frags)
	// reset cycle for next B-E group in same stream
	st.have = [4]bool{}
	return embDecodeResult{
		lc9:     lc9,
		flco:    lc9[0] & 0x3F,
		fid:     lc9[1],
		so:      lc9[2],
		dst:     int(lc9[3])<<16 | int(lc9[4])<<8 | int(lc9[5]),
		src:     int(lc9[6])<<16 | int(lc9[7])<<8 | int(lc9[8]),
		rxCRC:   rxCRC,
		calcCRC: calcCRC,
	}, true
}

func embFragIndexForDType(dtype uint) (int, bool) {
	switch dtype {
	case 1:
		return 0, true
	case 2:
		return 1, true
	case 3:
		return 2, true
	case 4:
		return 3, true
	default:
		return 0, false
	}
}

func decodeEmbeddedLC9FromFragments(frags [4][4]byte) (lc9 [9]byte, rxCRC byte, calcCRC byte) {
	var matrix [8][16]byte
	for f := 0; f < 4; f++ {
		bitIdx := 0
		for c := 0; c < 4; c++ {
			for r := 0; r < 8; r++ {
				b := (frags[f][bitIdx/8] >> (7 - (bitIdx % 8))) & 1
				matrix[r][f*4+c] = b
				bitIdx++
			}
		}
	}

	var bits [77]byte
	idx := 0
	for r := 0; r < 7; r++ {
		for c := 0; c < 11; c++ {
			bits[idx] = matrix[r][c] & 1
			idx++
		}
	}
	for i := 0; i < 72; i++ {
		if bits[i] == 1 {
			lc9[i/8] |= 1 << (7 - (i % 8))
		}
	}
	for i := 0; i < 5; i++ {
		rxCRC |= (bits[72+i] & 1) << (4 - i)
	}
	calcCRC = intdmr.EmbeddedLCCRC5Residual(bits[:72])
	return lc9, rxCRC, calcCRC
}

func splitBMDMRDPackets(t *testing.T, packets []tcpdumpPacket) (outbound []proto.Packet, returned []proto.Packet, seenSrc map[string]int) {
	t.Helper()

	outbound = make([]proto.Packet, 0, 16)
	returned = make([]proto.Packet, 0, 16)
	seenSrc = make(map[string]int)
	for _, p := range packets {
		seenSrc[p.srcIP]++
		if len(p.payload) < 4 || string(p.payload[:4]) != "DMRD" {
			continue
		}
		decoded, ok := proto.Decode(p.payload)
		if !ok {
			t.Fatalf("decode proto payload failed: len=%d", len(p.payload))
		}
		switch p.srcIP {
		case "110.42.107.105":
			outbound = append(outbound, decoded)
		case "8.218.137.199":
			returned = append(returned, decoded)
		}
	}
	return outbound, returned, seenSrc
}

func parseTCPDumpPacketsFromFile(t *testing.T, path string) []tcpdumpPacket {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tcpdump file %s: %v", path, err)
	}

	lines := strings.Split(string(raw), "\n")
	packets := make([]tcpdumpPacket, 0, 64)

	type packetMeta struct {
		srcIP string
		dstIP string
	}

	var (
		meta      packetMeta
		hasMeta   bool
		ipPayload []byte
	)

	flush := func() {
		if !hasMeta || len(ipPayload) < 28 {
			return
		}
		p := extractUDPPayload(meta, ipPayload)
		if p == nil {
			return
		}
		packets = append(packets, *p)
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		pm, ok := parsePacketMeta(line)
		if ok {
			flush()
			meta = pm
			hasMeta = true
			ipPayload = ipPayload[:0]
			continue
		}

		if !hasMeta || !strings.HasPrefix(line, "0x") {
			continue
		}
		row, ok := parseHexRow(line)
		if !ok {
			continue
		}
		ipPayload = append(ipPayload, row...)
	}
	flush()

	return packets
}

func resolveCapturePath(t *testing.T, name string) string {
	t.Helper()
	candidates := []string{
		name,
		filepath.Join("..", "..", name),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("capture file not found: %s", name)
	return ""
}

func parsePacketMeta(line string) (struct {
	srcIP string
	dstIP string
}, bool) {
	idx := strings.Index(line, " IP ")
	if idx < 0 || !strings.Contains(line, " UDP, length ") {
		return struct {
			srcIP string
			dstIP string
		}{}, false
	}
	s := line[idx+4:]
	parts := strings.SplitN(s, " > ", 2)
	if len(parts) != 2 {
		return struct {
			srcIP string
			dstIP string
		}{}, false
	}

	srcHost, ok := splitHostAndPort(strings.TrimSpace(parts[0]))
	if !ok {
		return struct {
			srcIP string
			dstIP string
		}{}, false
	}
	dstSide := strings.SplitN(parts[1], ": UDP, length ", 2)[0]
	dstHost, ok := splitHostAndPort(strings.TrimSpace(dstSide))
	if !ok {
		return struct {
			srcIP string
			dstIP string
		}{}, false
	}
	return struct {
		srcIP string
		dstIP string
	}{srcIP: srcHost, dstIP: dstHost}, true
}

func splitHostAndPort(s string) (string, bool) {
	dot := strings.LastIndexByte(s, '.')
	if dot <= 0 || dot >= len(s)-1 {
		return "", false
	}
	if _, err := strconv.Atoi(s[dot+1:]); err != nil {
		return "", false
	}
	return s[:dot], true
}

func parseHexRow(line string) ([]byte, bool) {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return nil, false
	}
	raw := strings.TrimSpace(line[colon+1:])
	if raw == "" {
		return nil, false
	}
	fields := strings.Fields(raw)
	row := make([]byte, 0, 16)
	for _, f := range fields {
		switch len(f) {
		case 4:
			v, err := strconv.ParseUint(f, 16, 16)
			if err != nil {
				return row, len(row) > 0
			}
			row = append(row, byte(v>>8), byte(v))
		case 2:
			v, err := strconv.ParseUint(f, 16, 8)
			if err != nil {
				return row, len(row) > 0
			}
			row = append(row, byte(v))
		default:
			return row, len(row) > 0
		}
	}
	return row, len(row) > 0
}

func extractUDPPayload(meta struct {
	srcIP string
	dstIP string
}, ipPacket []byte) *tcpdumpPacket {
	ihl := int(ipPacket[0]&0x0F) * 4
	if ihl < 20 || len(ipPacket) < ihl+8 {
		return nil
	}
	if ipPacket[9] != 17 {
		return nil
	}
	udp := ipPacket[ihl:]
	udpLen := int(udp[4])<<8 | int(udp[5])
	if udpLen < 8 || len(udp) < udpLen {
		return nil
	}
	payloadLen := udpLen - 8
	payload := make([]byte, payloadLen)
	copy(payload, udp[8:8+payloadLen])
	return &tcpdumpPacket{
		srcIP:   meta.srcIP,
		dstIP:   meta.dstIP,
		payload: payload,
	}
}
