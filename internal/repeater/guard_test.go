package repeater

import (
	"testing"

	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

func mkPkt(slot bool, streamID, dtype uint) proto.Packet {
	p := proto.Packet{Slot: slot, StreamID: streamID, FrameType: 2, DTypeOrVSeq: dtype, Src: 1001, Dst: 9}
	for i := range p.DMRData {
		p.DMRData[i] = byte(i)
	}
	return p
}

func TestAllowIngressFirstWins(t *testing.T) {
	g := NewGuard()
	if !g.AllowIngress("moto:1", mkPkt(false, 1, 1)) {
		t.Fatal("first source should be accepted")
	}
	if g.AllowIngress("hytera:1", mkPkt(false, 2, 1)) {
		t.Fatal("second source should be blocked while slot is active")
	}
	if !g.AllowIngress("moto:1", mkPkt(false, 1, 2)) {
		t.Fatal("owner terminator should be accepted")
	}
	if !g.AllowIngress("hytera:1", mkPkt(false, 2, 1)) {
		t.Fatal("new source should be accepted after terminator")
	}
}

func TestAllowIngressDedupAndEcho(t *testing.T) {
	g := NewGuard()
	p := mkPkt(true, 10, 1)
	if !g.AllowIngress("moto", p) {
		t.Fatal("expected first ingress accepted")
	}
	if g.AllowIngress("moto", p) {
		t.Fatal("expected duplicate ingress dropped")
	}

	g.MarkForwarded("hytera", p)
	if g.AllowIngress("hytera", p) {
		t.Fatal("expected echo ingress dropped")
	}
}

func TestAllowIngressBlocksConcurrentGroupStreams(t *testing.T) {
	g := NewGuard()

	first := mkPkt(false, 100, 1)
	first.GroupCall = true
	first.Dst = 91
	if !g.AllowIngress("moto:1", first) {
		t.Fatal("expected first group stream to be accepted")
	}

	second := mkPkt(true, 200, 1)
	second.GroupCall = true
	second.Dst = 91
	if g.AllowIngress("hytera:1", second) {
		t.Fatal("expected concurrent stream on same group to be blocked")
	}
}

func TestAllowIngressAllowsNewGroupStreamAfterTerminator(t *testing.T) {
	g := NewGuard()

	first := mkPkt(false, 100, 1)
	first.GroupCall = true
	first.Dst = 91
	if !g.AllowIngress("moto:1", first) {
		t.Fatal("expected first group stream to be accepted")
	}

	term := mkPkt(false, 100, 2)
	term.GroupCall = true
	term.Dst = 91
	if !g.AllowIngress("moto:1", term) {
		t.Fatal("expected group terminator to be accepted")
	}

	next := mkPkt(true, 200, 1)
	next.GroupCall = true
	next.Dst = 91
	if !g.AllowIngress("hytera:1", next) {
		t.Fatal("expected next group stream after terminator to be accepted")
	}
}

func TestAllowIngressAllowsSameSourceGroupStreamIDChange(t *testing.T) {
	g := NewGuard()

	first := mkPkt(false, 100, 1)
	first.GroupCall = true
	first.Dst = 91
	if !g.AllowIngress("hytera:1", first) {
		t.Fatal("expected first group stream to be accepted")
	}

	second := mkPkt(false, 101, 0)
	second.GroupCall = true
	second.Dst = 91
	if !g.AllowIngress("hytera:1", second) {
		t.Fatal("expected same source group stream id change to be accepted")
	}
}
