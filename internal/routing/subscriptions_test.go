package routing

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSubscriptionManagerResolveTargetsIncludesStaticAndDynamic(t *testing.T) {
	mgr := NewSubscriptionManager(2 * time.Minute)
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)

	mgr.ReplaceStatic("moto:1001", Slot1, []uint32{91, 3100})
	mgr.ReplaceStatic("hytera:10.0.0.2", Slot2, []uint32{91})
	mgr.ActivateDynamic("mmdvm:local:123456", Slot1, 91, now)

	targets := mgr.ResolveRoutes("moto:1001", 91, now.Add(10*time.Second))
	want := []TargetRoute{
		{DeviceKey: "hytera:10.0.0.2", Slot: Slot2},
		{DeviceKey: "mmdvm:local:123456", Slot: Slot1},
	}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("expected %v, got %v", want, targets)
	}
}

func TestSubscriptionManagerExpireDynamic(t *testing.T) {
	mgr := NewSubscriptionManager(30 * time.Second)
	start := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)
	mgr.ActivateDynamic("moto:1001", Slot2, 3100, start)

	targets := mgr.ResolveRoutes("", 3100, start.Add(31*time.Second))
	if len(targets) != 0 {
		t.Fatalf("expected expired dynamic subscription to be removed, got %v", targets)
	}
}

func TestSubscriptionManagerReplaceStaticRebuildsIndex(t *testing.T) {
	mgr := NewSubscriptionManager(2 * time.Minute)
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)

	mgr.ReplaceStatic("moto:1001", Slot1, []uint32{91, 92})
	mgr.ReplaceStatic("moto:1001", Slot1, []uint32{93})

	if got := mgr.ResolveRoutes("", 91, now); len(got) != 0 {
		t.Fatalf("expected old static group 91 to be removed, got %v", got)
	}
	want := []TargetRoute{{DeviceKey: "moto:1001", Slot: Slot1}}
	if got := mgr.ResolveRoutes("", 93, now); !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v for group 93, got %v", want, got)
	}
}

func TestSubscriptionManagerSnapshotPerSlot(t *testing.T) {
	mgr := NewSubscriptionManager(1 * time.Minute)
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)

	mgr.ReplaceStatic("moto:1001", Slot1, []uint32{91})
	mgr.ReplaceStatic("moto:1001", Slot2, []uint32{3100})
	mgr.ActivateDynamic("moto:1001", Slot1, 9990, now)

	snap := mgr.SnapshotDevice("moto:1001", now)
	if len(snap.Slots[Slot1]) != 2 {
		t.Fatalf("expected 2 subscriptions on slot1, got %d", len(snap.Slots[Slot1]))
	}
	if len(snap.Slots[Slot2]) != 1 {
		t.Fatalf("expected 1 subscription on slot2, got %d", len(snap.Slots[Slot2]))
	}
}

func TestSubscriptionManagerResolveRoutesAcrossSlots(t *testing.T) {
	mgr := NewSubscriptionManager(2 * time.Minute)
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)

	mgr.ActivateDynamic("moto:1001", Slot1, 3100, now)
	mgr.ReplaceStatic("mmdvm:local:2001", Slot2, []uint32{3100})

	got := mgr.ResolveRoutes("moto:1001", 3100, now)
	want := []TargetRoute{{DeviceKey: "mmdvm:local:2001", Slot: Slot2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected cross-slot route %v, got %v", want, got)
	}
}

func TestDeviceSubscriptionsJSONTags(t *testing.T) {
	payload, err := json.Marshal(DeviceSubscriptions{
		DeviceKey: "moto:1001",
		Slots: map[Slot][]GroupSubscription{
			Slot1: {{GroupID: 91, Kind: SubscriptionStatic}},
		},
	})
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	text := string(payload)
	for _, want := range []string{`"deviceKey"`, `"slots"`, `"groupId"`, `"kind"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected json to contain %s, got %s", want, text)
		}
	}
}

func TestActivateDynamicSkipsExistingStaticGroup(t *testing.T) {
	mgr := NewSubscriptionManager(2 * time.Minute)
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)

	mgr.ReplaceStatic("moto:1001", Slot1, []uint32{91})
	expiresAt := mgr.ActivateDynamic("moto:1001", Slot1, 91, now)
	if !expiresAt.IsZero() {
		t.Fatalf("expected no dynamic expiration for existing static group, got %v", expiresAt)
	}

	snap := mgr.SnapshotDevice("moto:1001", now)
	if len(snap.Slots[Slot1]) != 1 {
		t.Fatalf("expected only one subscription on slot1, got %d", len(snap.Slots[Slot1]))
	}
	if snap.Slots[Slot1][0].Kind != SubscriptionStatic {
		t.Fatalf("expected remaining subscription to be static, got %s", snap.Slots[Slot1][0].Kind)
	}
}
