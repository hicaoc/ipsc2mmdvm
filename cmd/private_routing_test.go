package cmd

import (
	"testing"
	"time"

	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

func TestRecentPrivateRouteCacheRememberAndLookup(t *testing.T) {
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	cache := newRecentPrivateRouteCache(2 * time.Minute)

	cache.Remember(4601001, "mmdvm:local:4601001", now)

	got, ok := cache.Lookup(4601001, now.Add(time.Minute))
	if !ok {
		t.Fatal("expected recent private route to be found")
	}
	if got != "mmdvm:local:4601001" {
		t.Fatalf("unexpected source key: got %q", got)
	}
}

func TestRecentPrivateRouteCacheExpires(t *testing.T) {
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	cache := newRecentPrivateRouteCache(time.Minute)

	cache.Remember(4601001, "mmdvm:local:4601001", now)

	if _, ok := cache.Lookup(4601001, now.Add(2*time.Minute)); ok {
		t.Fatal("expected expired private route to be removed")
	}
}

func TestRoutePrivateCallRoutesToRecentSourceKey(t *testing.T) {
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	cache := newRecentPrivateRouteCache(5 * time.Minute)
	cache.Remember(4602002, "hytera:10.0.0.2", now)

	pkt := proto.Packet{
		Src:       4601001,
		Dst:       4602002,
		GroupCall: false,
	}

	var gotTarget string
	var gotFrontend string
	var gotSourceKey string
	target, ok := routePrivateCall(cache, "moto", "moto:4601001", pkt, now, func(targetKey, sourceFrontend, sourceDeviceKey string, pkt proto.Packet) bool {
		gotTarget = targetKey
		gotFrontend = sourceFrontend
		gotSourceKey = sourceDeviceKey
		return true
	})
	if !ok {
		t.Fatal("expected private route to match recent target")
	}
	if target != "hytera:10.0.0.2" {
		t.Fatalf("unexpected target key: got %q", target)
	}
	if gotTarget != "hytera:10.0.0.2" || gotFrontend != "moto" || gotSourceKey != "moto:4601001" {
		t.Fatalf("unexpected send invocation: target=%q frontend=%q source=%q", gotTarget, gotFrontend, gotSourceKey)
	}
}

func TestRoutePrivateCallSkipsSameSourceKey(t *testing.T) {
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	cache := newRecentPrivateRouteCache(5 * time.Minute)
	cache.Remember(4601001, "moto:4601001", now)

	pkt := proto.Packet{
		Src:       4602002,
		Dst:       4601001,
		GroupCall: false,
	}

	_, ok := routePrivateCall(cache, "moto", "moto:4601001", pkt, now, func(targetKey, sourceFrontend, sourceDeviceKey string, pkt proto.Packet) bool {
		t.Fatal("did not expect private route send to be attempted")
		return false
	})
	if ok {
		t.Fatal("expected same-source private route to be skipped")
	}
}
