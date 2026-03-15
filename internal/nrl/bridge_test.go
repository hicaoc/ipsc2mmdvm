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

