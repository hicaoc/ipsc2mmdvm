package repeater

import (
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

const (
	defaultCallTimeout = 1200 * time.Millisecond
	defaultDedupTTL    = 400 * time.Millisecond
	defaultEchoTTL     = 250 * time.Millisecond
	defaultGroupTTL    = 6 * time.Second
)

type slotOwner struct {
	source   string
	streamID uint
	lastSeen time.Time
}

type groupOwner struct {
	source   string
	streamID uint
	lastSeen time.Time
}

type Guard struct {
	mu sync.Mutex

	callTimeout time.Duration
	dedupTTL    time.Duration
	echoTTL     time.Duration
	groupTTL    time.Duration

	owners [2]*slotOwner

	groupOwners map[uint]*groupOwner

	recentIngress map[string]time.Time
	recentEgress  map[string]time.Time
}

func NewGuard() *Guard {
	return &Guard{
		callTimeout:   defaultCallTimeout,
		dedupTTL:      defaultDedupTTL,
		echoTTL:       defaultEchoTTL,
		groupTTL:      defaultGroupTTL,
		groupOwners:   make(map[uint]*groupOwner),
		recentIngress: make(map[string]time.Time),
		recentEgress:  make(map[string]time.Time),
	}
}

func (g *Guard) AllowIngress(source string, pkt proto.Packet) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()

	g.evictExpired(now)

	fp := fingerprint(pkt)
	echoKey := "echo:" + source + ":" + fp
	if ts, ok := g.recentEgress[echoKey]; ok && now.Sub(ts) <= g.echoTTL {
		return false
	}
	ingressKey := "in:" + source + ":" + fp
	if ts, ok := g.recentIngress[ingressKey]; ok && now.Sub(ts) <= g.dedupTTL {
		return false
	}
	if pkt.GroupCall && pkt.Dst != 0 {
		if !g.allowGroupLocked(source, pkt, now) {
			return false
		}
	}

	idx := 0
	if pkt.Slot {
		idx = 1
	}
	isTerm := isTerminator(pkt)
	owner := g.owners[idx]
	if owner == nil {
		if !isTerm {
			g.owners[idx] = &slotOwner{source: source, streamID: pkt.StreamID, lastSeen: now}
		}
		g.recentIngress[ingressKey] = now
		return true
	}

	if now.Sub(owner.lastSeen) > g.callTimeout {
		g.owners[idx] = nil
		if !isTerm {
			g.owners[idx] = &slotOwner{source: source, streamID: pkt.StreamID, lastSeen: now}
		}
		g.recentIngress[ingressKey] = now
		return true
	}

	if owner.source != source {
		return false
	}

	owner.lastSeen = now
	owner.streamID = pkt.StreamID
	if isTerm {
		g.owners[idx] = nil
	}
	g.recentIngress[ingressKey] = now
	return true
}

func (g *Guard) allowGroupLocked(source string, pkt proto.Packet, now time.Time) bool {
	isTerm := isTerminator(pkt)
	owner := g.groupOwners[pkt.Dst]
	if owner == nil || now.Sub(owner.lastSeen) > g.groupTTL {
		if isTerm {
			delete(g.groupOwners, pkt.Dst)
			return true
		}
		g.groupOwners[pkt.Dst] = &groupOwner{
			source:   source,
			streamID: pkt.StreamID,
			lastSeen: now,
		}
		return true
	}

	if owner.source != source {
		return false
	}

	owner.lastSeen = now
	owner.streamID = pkt.StreamID
	if isTerm {
		delete(g.groupOwners, pkt.Dst)
	}
	return true
}

func (g *Guard) MarkForwarded(targetFrontend string, pkt proto.Packet) {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.evictExpired(now)
	g.recentEgress["echo:"+targetFrontend+":"+fingerprint(pkt)] = now
}

func (g *Guard) evictExpired(now time.Time) {
	for k, ts := range g.recentIngress {
		if now.Sub(ts) > g.dedupTTL {
			delete(g.recentIngress, k)
		}
	}
	for k, ts := range g.recentEgress {
		if now.Sub(ts) > g.echoTTL {
			delete(g.recentEgress, k)
		}
	}
	for i, owner := range g.owners {
		if owner != nil && now.Sub(owner.lastSeen) > g.callTimeout {
			g.owners[i] = nil
		}
	}
	for groupID, owner := range g.groupOwners {
		if owner != nil && now.Sub(owner.lastSeen) > g.groupTTL {
			delete(g.groupOwners, groupID)
		}
	}
}

func isTerminator(pkt proto.Packet) bool {
	return pkt.FrameType == 2 && pkt.DTypeOrVSeq == 2
}

func fingerprint(pkt proto.Packet) string {
	return fmt.Sprintf("%d:%t:%d:%d:%d:%d:%d:%s",
		pkt.Seq, pkt.Slot, pkt.Src, pkt.Dst, pkt.StreamID, pkt.FrameType, pkt.DTypeOrVSeq, hex.EncodeToString(pkt.DMRData[:]))
}
