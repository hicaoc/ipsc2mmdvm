package cmd

import (
	"sync"
	"time"

	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

const recentPrivateRouteTTL = 15 * time.Minute

type recentPrivateRoute struct {
	sourceKey string
	lastSeen  time.Time
}

type recentPrivateRouteCache struct {
	mu     sync.RWMutex
	ttl    time.Duration
	routes map[uint32]recentPrivateRoute
}

func newRecentPrivateRouteCache(ttl time.Duration) *recentPrivateRouteCache {
	if ttl <= 0 {
		ttl = recentPrivateRouteTTL
	}
	return &recentPrivateRouteCache{
		ttl:    ttl,
		routes: map[uint32]recentPrivateRoute{},
	}
}

func (c *recentPrivateRouteCache) Remember(dmrid uint32, sourceKey string, now time.Time) {
	if c == nil || dmrid == 0 || sourceKey == "" {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	c.routes[dmrid] = recentPrivateRoute{
		sourceKey: sourceKey,
		lastSeen:  now,
	}
}

func (c *recentPrivateRouteCache) Lookup(dmrid uint32, now time.Time) (string, bool) {
	if c == nil || dmrid == 0 {
		return "", false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)

	route, ok := c.routes[dmrid]
	if !ok {
		return "", false
	}
	return route.sourceKey, true
}

func (c *recentPrivateRouteCache) pruneLocked(now time.Time) {
	if c.ttl <= 0 {
		return
	}
	cutoff := now.Add(-c.ttl)
	for dmrid, route := range c.routes {
		if route.lastSeen.Before(cutoff) {
			delete(c.routes, dmrid)
		}
	}
}

func routePrivateCall(
	cache *recentPrivateRouteCache,
	sourceFrontend string,
	sourceDeviceKey string,
	pkt proto.Packet,
	now time.Time,
	send func(targetKey, sourceFrontend, sourceDeviceKey string, pkt proto.Packet) bool,
) (string, bool) {
	if cache == nil || send == nil || pkt.GroupCall || pkt.Dst == 0 {
		return "", false
	}
	targetKey, ok := cache.Lookup(uint32(pkt.Dst), now)
	if !ok || targetKey == "" || targetKey == sourceDeviceKey {
		return "", false
	}
	if !send(targetKey, sourceFrontend, sourceDeviceKey, pkt) {
		return "", false
	}
	return targetKey, true
}
