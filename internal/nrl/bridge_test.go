package nrl

import (
	"testing"
	"time"

	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

func TestBridgeActivateReturnsWithEndpointHandler(t *testing.T) {
	bridge := NewBridge(func(sourceKey string) (DeviceConfig, bool) {
		return DeviceConfig{
			SourceKey:  sourceKey,
			Callsign:   "D2TEST",
			ServerAddr: "127.0.0.1",
			ServerPort: 60050,
			Slot:       1,
			TargetTG:   46025,
		}, true
	}, func(string, proto.Packet) {})
	defer bridge.Close()

	bridge.SetEndpointHandler(func(sourceKey, ip string, port int) {})

	done := make(chan error, 1)
	go func() {
		done <- bridge.Activate("nrl-virtual:test")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("activate returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("activate blocked unexpectedly")
	}
}

func TestBridgeMatchesResolvedConfig(t *testing.T) {
	cfg := DeviceConfig{
		SourceKey:  "nrl-virtual:test",
		Callsign:   "D2TEST",
		ServerAddr: "127.0.0.1",
		ServerPort: 60050,
		Slot:       1,
		TargetTG:   46025,
	}
	bridge := NewBridge(func(sourceKey string) (DeviceConfig, bool) {
		return cfg, true
	}, func(string, proto.Packet) {})
	defer bridge.Close()

	if err := bridge.Activate(cfg.SourceKey); err != nil {
		t.Fatalf("activate returned error: %v", err)
	}
	if !bridge.MatchesResolvedConfig(cfg.SourceKey) {
		t.Fatal("expected active bridge config to match resolved config")
	}

	cfg.ServerPort = 60051
	if bridge.MatchesResolvedConfig(cfg.SourceKey) {
		t.Fatal("expected config mismatch after resolved endpoint changed")
	}
}
