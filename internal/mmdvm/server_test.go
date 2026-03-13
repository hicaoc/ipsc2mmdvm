package mmdvm

import (
	"crypto/sha256"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/hicaoc/ipsc2mmdvm/internal/config"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
	"github.com/hicaoc/ipsc2mmdvm/internal/timeslot"
)

func testServerConfig() *config.MMDVM {
	cfg := testMMDVMConfig()
	cfg.Listen = "127.0.0.1:0"
	cfg.PassAllTG = []int{1}
	return cfg
}

func readUDP(t *testing.T, conn *net.UDPConn) []byte {
	t.Helper()
	buf := make([]byte, 512)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("ReadFromUDP: %v", err)
	}
	return buf[:n]
}

func writeUDP(t *testing.T, conn *net.UDPConn, data []byte) {
	t.Helper()
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func performServerHandshake(t *testing.T, conn *net.UDPConn, cfg *config.MMDVM) {
	t.Helper()

	login := make([]byte, 8)
	copy(login, "RPTL")
	binary.BigEndian.PutUint32(login[4:], cfg.ID)
	writeUDP(t, conn, login)

	ack := readUDP(t, conn)
	if len(ack) != 10 || string(ack[:6]) != rptAck {
		t.Fatalf("expected RPTACK+challenge, got %q", string(ack))
	}

	s256 := sha256.New()
	s256.Write(ack[6:])
	s256.Write([]byte(cfg.Password))
	token := s256.Sum(nil)

	auth := make([]byte, 40)
	copy(auth, "RPTK")
	binary.BigEndian.PutUint32(auth[4:], cfg.ID)
	copy(auth[8:], token)
	writeUDP(t, conn, auth)

	ack = readUDP(t, conn)
	if string(ack) != rptAck {
		t.Fatalf("expected RPTACK after auth, got %q", string(ack))
	}

	conf := make([]byte, 8)
	copy(conf, "RPTC")
	binary.BigEndian.PutUint32(conf[4:], cfg.ID)
	writeUDP(t, conn, conf)

	ack = readUDP(t, conn)
	if string(ack) != rptAck {
		t.Fatalf("expected RPTACK after config, got %q", string(ack))
	}
}

func TestMMDVMServerHandshakeAndPing(t *testing.T) {
	t.Parallel()

	cfg := testServerConfig()
	server := NewMMDVMServer(cfg, nil)
	server.SetOutboundTSManager(timeslot.NewManager())
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer server.Stop()

	remoteAddr := server.conn.LocalAddr().(*net.UDPAddr)
	conn, err := net.DialUDP("udp", nil, remoteAddr)
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer conn.Close()

	performServerHandshake(t, conn, cfg)

	ping := make([]byte, 11)
	copy(ping, "RPTPING")
	binary.BigEndian.PutUint32(ping[7:], cfg.ID)
	writeUDP(t, conn, ping)

	resp := readUDP(t, conn)
	if len(resp) != 11 || string(resp[:7]) != "MSTPONG" {
		t.Fatalf("expected MSTPONG, got %q", string(resp))
	}
}

func TestMMDVMServerForwardsInboundDMRD(t *testing.T) {
	t.Parallel()

	cfg := testServerConfig()
	server := NewMMDVMServer(cfg, nil)
	server.SetOutboundTSManager(timeslot.NewManager())

	got := make(chan proto.Packet, 1)
	server.SetPacketHandler(func(packet proto.Packet) {
		got <- packet
	})

	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer server.Stop()

	remoteAddr := server.conn.LocalAddr().(*net.UDPAddr)
	conn, err := net.DialUDP("udp", nil, remoteAddr)
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer conn.Close()

	performServerHandshake(t, conn, cfg)

	pkt := proto.Packet{
		Signature: "DMRD",
		Seq:       1,
		Src:       123456,
		Dst:       9,
		Repeater:  uint(cfg.ID),
		GroupCall: true,
		Slot:      false,
		StreamID:  0x1234,
	}
	writeUDP(t, conn, pkt.Encode())

	select {
	case forwarded := <-got:
		if !forwarded.Equal(pkt) {
			t.Fatalf("expected forwarded packet %+v, got %+v", pkt, forwarded)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for forwarded packet")
	}
}

func TestMMDVMServerPingSurvivesPortChange(t *testing.T) {
	t.Parallel()

	cfg := testServerConfig()
	server := NewMMDVMServer(cfg, nil)
	server.SetOutboundTSManager(timeslot.NewManager())
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer server.Stop()

	remoteAddr := server.conn.LocalAddr().(*net.UDPAddr)
	connA, err := net.DialUDP("udp", nil, remoteAddr)
	if err != nil {
		t.Fatalf("DialUDP A: %v", err)
	}
	performServerHandshake(t, connA, cfg)
	_ = connA.Close()

	connB, err := net.DialUDP("udp", nil, remoteAddr)
	if err != nil {
		t.Fatalf("DialUDP B: %v", err)
	}
	defer connB.Close()

	ping := make([]byte, 11)
	copy(ping, "RPTPING")
	binary.BigEndian.PutUint32(ping[7:], cfg.ID)
	writeUDP(t, connB, ping)

	resp := readUDP(t, connB)
	if len(resp) != 11 || string(resp[:7]) != "MSTPONG" {
		t.Fatalf("expected MSTPONG after port change, got %q", string(resp))
	}

	server.sessionMu.RLock()
	ready := server.readySessionCountLocked()
	server.sessionMu.RUnlock()
	if ready != 1 {
		t.Fatalf("expected one ready session after port change, got %d", ready)
	}
}

func TestMMDVMServerPingWithoutReadySessionStartsHandshake(t *testing.T) {
	t.Parallel()

	cfg := testServerConfig()
	server := NewMMDVMServer(cfg, nil)
	server.SetOutboundTSManager(timeslot.NewManager())
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer server.Stop()

	remoteAddr := server.conn.LocalAddr().(*net.UDPAddr)
	conn, err := net.DialUDP("udp", nil, remoteAddr)
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer conn.Close()

	ping := make([]byte, 11)
	copy(ping, "RPTPING")
	binary.BigEndian.PutUint32(ping[7:], cfg.ID)
	writeUDP(t, conn, ping)

	resp := readUDP(t, conn)
	if len(resp) != 10 || string(resp[:6]) != rptAck {
		t.Fatalf("expected RPTACK+challenge without ready session, got %q", string(resp))
	}
}
