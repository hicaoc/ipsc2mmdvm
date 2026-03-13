package cmd

import (
	"testing"

	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
	"github.com/hicaoc/ipsc2mmdvm/internal/routing"
)

func TestSortRouteTargetsPrioritizesLocalBridgeForMoto(t *testing.T) {
	targets := []routing.TargetRoute{
		{DeviceKey: "mmdvm-upstream:BM", Slot: routing.Slot1},
		{DeviceKey: "hytera:10.0.0.2", Slot: routing.Slot1},
	}

	sortRouteTargets("moto", targets)

	if got := targets[0].DeviceKey; got != "hytera:10.0.0.2" {
		t.Fatalf("expected hytera target first for moto source, got %q", got)
	}
}

func TestSortRouteTargetsPrioritizesLocalBridgeForHytera(t *testing.T) {
	targets := []routing.TargetRoute{
		{DeviceKey: "mmdvm-upstream:BM", Slot: routing.Slot1},
		{DeviceKey: "moto:4601816", Slot: routing.Slot1},
	}

	sortRouteTargets("hytera", targets)

	if got := targets[0].DeviceKey; got != "moto:4601816" {
		t.Fatalf("expected moto target first for hytera source, got %q", got)
	}
}

func TestPacketForTargetSlotRewritesSlot(t *testing.T) {
	pkt := proto.Packet{Slot: false}

	rewritten, changed := packetForTargetSlot(pkt, routing.Slot2)

	if !changed {
		t.Fatal("expected cross-slot routing to report slot change")
	}
	if !rewritten.Slot {
		t.Fatal("expected packet slot to be rewritten to slot 2")
	}
	if pkt.Slot {
		t.Fatal("expected original packet to remain unchanged")
	}
}

func TestShouldActivateDynamicGroupOnlyForVoiceLCHeader(t *testing.T) {
	header := proto.Packet{
		GroupCall:   true,
		Dst:         46025,
		FrameType:   2,
		DTypeOrVSeq: 1,
	}
	if !shouldActivateDynamicGroup(header) {
		t.Fatal("expected Voice LC Header to activate dynamic group")
	}

	voiceSync := proto.Packet{
		GroupCall:   true,
		Dst:         16896,
		FrameType:   1,
		DTypeOrVSeq: 0,
	}
	if shouldActivateDynamicGroup(voiceSync) {
		t.Fatal("expected voice sync burst not to activate dynamic group")
	}

	voiceBurst := proto.Packet{
		GroupCall:   true,
		Dst:         16896,
		FrameType:   0,
		DTypeOrVSeq: 3,
	}
	if shouldActivateDynamicGroup(voiceBurst) {
		t.Fatal("expected embedded voice burst not to activate dynamic group")
	}
}
