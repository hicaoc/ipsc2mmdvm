package registry

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hicaoc/ipsc2mmdvm/internal/dmrid"
)

func TestServiceEnrichesDeviceAndCallsignFromDMRIDDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	dmridPath := filepath.Join(dir, "dmrid.csv")
	if err := os.WriteFile(dmridPath, []byte("1023092,VE3FIS,Tom,,Toronto,Ontario,Canada\n"), 0o644); err != nil {
		t.Fatalf("write dmrid db: %v", err)
	}
	resolver, err := dmrid.Load(dmridPath)
	if err != nil {
		t.Fatalf("load dmrid db: %v", err)
	}

	svc, err := NewService(store, resolver)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	dev, err := svc.UpsertDevice(Device{
		Category:  CategoryMMDVM,
		Protocol:  "mmdvm",
		SourceKey: "mmdvm:test",
		DMRID:     1023092,
		Online:    true,
	})
	if err != nil {
		t.Fatalf("upsert device: %v", err)
	}
	if dev.Callsign != "VE3FIS" {
		t.Fatalf("expected resolved callsign VE3FIS, got %q", dev.Callsign)
	}

	call, _, err := svc.RecordCall(CallRecord{
		Frontend:       "mmdvm",
		SourceCategory: CategoryMMDVM,
		SourceKey:      "mmdvm:test",
		SrcID:          1023092,
	}, false)
	if err != nil {
		t.Fatalf("record call: %v", err)
	}
	if call.SourceCallsign != "VE3FIS" {
		t.Fatalf("expected resolved call source callsign VE3FIS, got %q", call.SourceCallsign)
	}
}

func TestServiceReplaceAndLoadStaticGroups(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	svc, err := NewService(store, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	if err := svc.ReplaceStaticGroups("moto:1001", 1, []uint32{91, 92}); err != nil {
		t.Fatalf("replace static groups slot1: %v", err)
	}
	if err := svc.ReplaceStaticGroups("moto:1001", 2, []uint32{3100}); err != nil {
		t.Fatalf("replace static groups slot2: %v", err)
	}
	if err := svc.ReplaceStaticGroups("moto:1001", 1, []uint32{9990}); err != nil {
		t.Fatalf("replace static groups slot1 second time: %v", err)
	}

	groups, err := svc.LoadStaticGroups()
	if err != nil {
		t.Fatalf("load static groups: %v", err)
	}

	want := []StaticGroup{
		{SourceKey: "moto:1001", Slot: 1, GroupID: 9990},
		{SourceKey: "moto:1001", Slot: 2, GroupID: 3100},
	}
	if len(groups) != len(want) {
		t.Fatalf("expected %d groups, got %d: %#v", len(want), len(groups), groups)
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Fatalf("expected group %v at index %d, got %v", want[i], i, groups[i])
		}
	}
}

func TestServiceDeleteDeviceRemovesStaticGroups(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	svc, err := NewService(store, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	dev, err := svc.UpsertDevice(Device{
		Category:  CategoryHytera,
		Protocol:  "hytera",
		SourceKey: "hytera:1.2.3.4",
		Name:      "Test Repeater",
		Online:    true,
	})
	if err != nil {
		t.Fatalf("upsert device: %v", err)
	}
	if err := svc.ReplaceStaticGroups(dev.SourceKey, 1, []uint32{91, 92}); err != nil {
		t.Fatalf("replace static groups: %v", err)
	}

	deleted, err := svc.DeleteDevice(dev.ID)
	if err != nil {
		t.Fatalf("delete device: %v", err)
	}
	if deleted.SourceKey != dev.SourceKey {
		t.Fatalf("unexpected deleted device: got %q want %q", deleted.SourceKey, dev.SourceKey)
	}
	if _, ok := svc.FindDevice(dev.SourceKey); ok {
		t.Fatal("expected device to be removed from service cache")
	}

	groups, err := svc.LoadStaticGroups()
	if err != nil {
		t.Fatalf("load static groups: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("expected static groups to be removed, got %#v", groups)
	}
}

func TestServiceUpsertDeviceDeduplicatesByDMRID(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	svc, err := NewService(store, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	first, err := svc.UpsertDevice(Device{
		Category:  CategoryHytera,
		Protocol:  "hytera",
		SourceKey: "hytera:1.1.1.1",
		DMRID:     4601816,
		Name:      "Old Device",
		Notes:     "keep me",
		Disabled:  true,
		Online:    true,
	})
	if err != nil {
		t.Fatalf("upsert first device: %v", err)
	}
	if err := svc.ReplaceStaticGroups(first.SourceKey, 1, []uint32{91}); err != nil {
		t.Fatalf("replace static groups: %v", err)
	}

	second, err := svc.UpsertDevice(Device{
		Category:  CategoryHytera,
		Protocol:  "hytera",
		SourceKey: "hytera:2.2.2.2",
		DMRID:     4601816,
		IP:        "2.2.2.2",
		Port:      30001,
		Online:    true,
	})
	if err != nil {
		t.Fatalf("upsert second device: %v", err)
	}

	if second.SourceKey != "hytera:2.2.2.2" {
		t.Fatalf("unexpected source key: %s", second.SourceKey)
	}
	if second.Name != "Old Device" {
		t.Fatalf("expected migrated name, got %q", second.Name)
	}
	if second.Notes != "keep me" {
		t.Fatalf("expected migrated notes, got %q", second.Notes)
	}
	if !second.Disabled {
		t.Fatal("expected disabled flag to migrate")
	}
	if _, ok := svc.FindDevice("hytera:1.1.1.1"); ok {
		t.Fatal("expected old device entry to be removed")
	}

	snap := svc.Snapshot()
	count := 0
	for _, dev := range snap.Devices {
		if dev.DMRID == 4601816 {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected one device with DMRID 4601816, got %d", count)
	}

	groups, err := svc.LoadStaticGroups()
	if err != nil {
		t.Fatalf("load static groups: %v", err)
	}
	if len(groups) != 1 || groups[0].SourceKey != "hytera:2.2.2.2" || groups[0].GroupID != 91 {
		t.Fatalf("expected migrated static group on new device, got %#v", groups)
	}
}

func TestServiceDoesNotDeduplicateMMDVMUpstreamAgainstPeer(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	svc, err := NewService(store, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	if _, err := svc.UpsertDevice(Device{
		Category:  CategoryMMDVM,
		Protocol:  "mmdvm-upstream",
		SourceKey: "mmdvm-upstream:BM",
		DMRID:     4601816,
		Name:      "BM",
		Online:    true,
	}); err != nil {
		t.Fatalf("upsert upstream: %v", err)
	}
	if _, err := svc.UpsertDevice(Device{
		Category:  CategoryMMDVM,
		Protocol:  "mmdvm",
		SourceKey: "mmdvm:local:4601816",
		DMRID:     4601816,
		Name:      "Hotspot",
		Online:    true,
	}); err != nil {
		t.Fatalf("upsert peer: %v", err)
	}

	snap := svc.Snapshot()
	count := 0
	for _, dev := range snap.Devices {
		if dev.DMRID == 4601816 {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected upstream and peer to both remain visible, got %d", count)
	}
}

func TestServiceKeepsMultipleMMDVMPeersWithDifferentDMRID(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	svc, err := NewService(store, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	if _, err := svc.UpsertDevice(Device{
		Category:  CategoryMMDVM,
		Protocol:  "mmdvm",
		SourceKey: "mmdvm:local:1001",
		DMRID:     1001,
		Name:      "Peer A",
		Online:    true,
	}); err != nil {
		t.Fatalf("upsert peer a: %v", err)
	}
	if _, err := svc.UpsertDevice(Device{
		Category:  CategoryMMDVM,
		Protocol:  "mmdvm",
		SourceKey: "mmdvm:local:1002",
		DMRID:     1002,
		Name:      "Peer B",
		Online:    true,
	}); err != nil {
		t.Fatalf("upsert peer b: %v", err)
	}

	snap := svc.Snapshot()
	count := 0
	for _, dev := range snap.Devices {
		if dev.Protocol == "mmdvm" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected two mmdvm peers to remain visible, got %d", count)
	}
}

func TestServiceExpireCallsMarksEnded(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	svc, err := NewService(store, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	call, inserted, err := svc.RecordCall(CallRecord{
		Frontend:       "mmdvm",
		SourceCategory: CategoryMMDVM,
		SourceKey:      "mmdvm:test",
		SourceDMRID:    4602706,
		SrcID:          4602706,
		DstID:          91,
		CallType:       "group",
		StreamID:       1234,
	}, false)
	if err != nil || !inserted {
		t.Fatalf("record call: inserted=%v err=%v", inserted, err)
	}

	svc.mu.Lock()
	svc.activeCall[callDedupKey(call)] = activeCallState{
		ID:        call.ID,
		StartedAt: time.Now().Add(-31 * time.Second),
	}
	svc.mu.Unlock()

	svc.expireCalls()

	snap := svc.Snapshot()
	if len(snap.Calls) == 0 {
		t.Fatal("expected calls in snapshot")
	}
	if snap.Calls[len(snap.Calls)-1].EndedAt.IsZero() {
		t.Fatal("expected expired call to be marked ended")
	}
	if snap.Calls[len(snap.Calls)-1].DurationMS <= 0 {
		t.Fatal("expected expired call to get duration")
	}
}

func TestNewServiceRepairsStaleOpenCalls(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	oldStart := time.Now().Add(-2 * time.Minute).UTC()
	call, err := store.InsertCall(CallRecord{
		CreatedAt:      oldStart,
		Frontend:       "mmdvm",
		SourceCategory: CategoryMMDVM,
		SourceKey:      "mmdvm:test",
		SourceDMRID:    4602706,
		SrcID:          4602706,
		DstID:          91,
		CallType:       "group",
		StreamID:       777,
	})
	if err != nil {
		t.Fatalf("insert call: %v", err)
	}
	if !call.EndedAt.IsZero() {
		t.Fatal("expected inserted call to be open")
	}

	svc, err := NewService(store, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	snap := svc.Snapshot()
	if len(snap.Calls) == 0 {
		t.Fatal("expected repaired call in snapshot")
	}
	found := false
	for _, item := range snap.Calls {
		if item.ID != call.ID {
			continue
		}
		found = true
		if item.EndedAt.IsZero() {
			t.Fatal("expected stale open call to be repaired on startup")
		}
		if item.DurationMS <= 0 {
			t.Fatal("expected repaired call to have positive duration")
		}
	}
	if !found {
		t.Fatal("expected repaired call to remain in snapshot")
	}
}

func TestServiceRecordCallIgnoresStreamIDDriftForActiveCall(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	svc, err := NewService(store, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	first, inserted, err := svc.RecordCall(CallRecord{
		Frontend:       "moto",
		SourceCategory: CategoryMoto,
		SourceKey:      "moto:4604111",
		SourceDMRID:    4604111,
		SrcID:          4604111,
		DstID:          46025,
		Slot:           1,
		CallType:       "group",
		StreamID:       1,
	}, false)
	if err != nil || !inserted {
		t.Fatalf("record first call: inserted=%v err=%v", inserted, err)
	}

	second, inserted, err := svc.RecordCall(CallRecord{
		Frontend:       "moto",
		SourceCategory: CategoryMoto,
		SourceKey:      "moto:4604111",
		SourceDMRID:    4259892,
		SrcID:          4259892,
		DstID:          16896,
		Slot:           1,
		CallType:       "group",
		StreamID:       4,
	}, false)
	if err != nil {
		t.Fatalf("record drifted call: %v", err)
	}
	if inserted {
		t.Fatalf("expected drifted stream to reuse active call, got inserted record %#v", second)
	}

	snap := svc.Snapshot()
	if len(snap.Calls) != 1 {
		t.Fatalf("expected 1 call in snapshot, got %d: %#v", len(snap.Calls), snap.Calls)
	}
	if snap.Calls[0].ID != first.ID {
		t.Fatalf("expected first call to remain active, got %#v", snap.Calls[0])
	}
	if snap.Calls[0].SrcID != 4604111 || snap.Calls[0].DstID != 46025 {
		t.Fatalf("expected original src/dst to be preserved, got src=%d dst=%d", snap.Calls[0].SrcID, snap.Calls[0].DstID)
	}
}

func TestServiceRecordCallEndsActiveCallWhenTerminatorStreamIDDrifts(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	svc, err := NewService(store, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	first, inserted, err := svc.RecordCall(CallRecord{
		Frontend:       "moto",
		SourceCategory: CategoryMoto,
		SourceKey:      "moto:4604111",
		SourceDMRID:    4604111,
		SrcID:          4604111,
		DstID:          46025,
		Slot:           1,
		CallType:       "group",
		StreamID:       1,
	}, false)
	if err != nil || !inserted {
		t.Fatalf("record first call: inserted=%v err=%v", inserted, err)
	}

	svc.mu.Lock()
	state := svc.activeCall[callDedupKey(first)]
	state.StartedAt = time.Now().Add(-2 * time.Second)
	svc.activeCall[callDedupKey(first)] = state
	svc.mu.Unlock()

	updated, inserted, err := svc.RecordCall(CallRecord{
		Frontend:       "moto",
		SourceCategory: CategoryMoto,
		SourceKey:      "moto:4604111",
		SourceDMRID:    4259892,
		SrcID:          4259892,
		DstID:          16896,
		Slot:           1,
		CallType:       "group",
		StreamID:       4,
	}, true)
	if err != nil {
		t.Fatalf("record drifted terminator: %v", err)
	}
	if inserted {
		t.Fatalf("expected drifted terminator to finish active call, got inserted record %#v", updated)
	}
	if updated.ID != first.ID {
		t.Fatalf("expected terminator to finish first call, got %#v", updated)
	}
	if updated.EndedAt.IsZero() {
		t.Fatal("expected active call to be ended")
	}
	if updated.DurationMS <= 0 {
		t.Fatalf("expected positive duration, got %d", updated.DurationMS)
	}
}

func TestServiceRecordCallNRLUsesSingleActivePerSourceKey(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	svc, err := NewService(store, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	first, inserted, err := svc.RecordCall(CallRecord{
		Frontend:       "nrl",
		SourceCategory: CategoryHytera,
		SourceKey:      "nrl:hytera:49.77.145.131",
		SourceName:     "BY4RSA-2",
		SourceCallsign: "BY4RSA-2",
		CallType:       "analog",
		Slot:           1,
		StreamID:       1,
	}, false)
	if err != nil || !inserted {
		t.Fatalf("record first nrl call: inserted=%v err=%v", inserted, err)
	}

	second, inserted, err := svc.RecordCall(CallRecord{
		Frontend:       "nrl",
		SourceCategory: CategoryHytera,
		SourceKey:      "nrl:hytera:49.77.145.131",
		SourceName:     "BY4RSA-2",
		SourceCallsign: "BY4RSA-2",
		CallType:       "private", // drifted type should still map to same active call
		Slot:           2,         // drifted slot should still map to same active call
		StreamID:       99,
	}, false)
	if err != nil {
		t.Fatalf("record second nrl call: %v", err)
	}
	if inserted {
		t.Fatalf("expected nrl second call to reuse active call, got inserted %#v", second)
	}

	snap := svc.Snapshot()
	if len(snap.Calls) != 1 {
		t.Fatalf("expected 1 active call, got %d: %#v", len(snap.Calls), snap.Calls)
	}
	if snap.Calls[0].ID != first.ID {
		t.Fatalf("expected first call ID to remain active, got %#v", snap.Calls[0])
	}
}
