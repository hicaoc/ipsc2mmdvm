package routing

import (
	"sort"
	"sync"
	"time"
)

type Slot uint8

const (
	Slot1 Slot = 1
	Slot2 Slot = 2
)

type SubscriptionKind string

const (
	SubscriptionStatic  SubscriptionKind = "static"
	SubscriptionDynamic SubscriptionKind = "dynamic"
)

type GroupSubscription struct {
	GroupID   uint32           `json:"groupId"`
	Kind      SubscriptionKind `json:"kind"`
	ExpiresAt time.Time        `json:"expiresAt"`
}

type DeviceSubscriptions struct {
	DeviceKey string                          `json:"deviceKey"`
	Slots     map[Slot][]GroupSubscription    `json:"slots"`
}

type TargetRoute struct {
	DeviceKey string
	Slot      Slot
}

type routeKey struct {
	slot  Slot
	group uint32
}

type deviceState struct {
	static  map[Slot]map[uint32]struct{}
	dynamic map[Slot]map[uint32]time.Time
}

type SubscriptionManager struct {
	mu           sync.RWMutex
	dynamicTTL   time.Duration
	devices      map[string]*deviceState
	staticIndex  map[routeKey]map[string]struct{}
	dynamicIndex map[routeKey]map[string]time.Time
}

func NewSubscriptionManager(dynamicTTL time.Duration) *SubscriptionManager {
	if dynamicTTL <= 0 {
		dynamicTTL = 5 * time.Minute
	}
	return &SubscriptionManager{
		dynamicTTL:   dynamicTTL,
		devices:      map[string]*deviceState{},
		staticIndex:  map[routeKey]map[string]struct{}{},
		dynamicIndex: map[routeKey]map[string]time.Time{},
	}
}

func (m *SubscriptionManager) ReplaceStatic(deviceKey string, slot Slot, groups []uint32) {
	if deviceKey == "" || !validSlot(slot) {
		return
	}
	unique := uniqueGroups(groups)

	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.getOrCreateLocked(deviceKey)
	old := st.static[slot]
	for groupID := range old {
		m.removeStaticIndexLocked(deviceKey, slot, groupID)
	}
	next := make(map[uint32]struct{}, len(unique))
	for _, groupID := range unique {
		next[groupID] = struct{}{}
		m.addStaticIndexLocked(deviceKey, slot, groupID)
	}
	st.static[slot] = next
}

func (m *SubscriptionManager) ActivateDynamic(deviceKey string, slot Slot, groupID uint32, now time.Time) time.Time {
	if deviceKey == "" || !validSlot(slot) || groupID == 0 {
		return time.Time{}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	expiresAt := now.Add(m.dynamicTTL)

	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.getOrCreateLocked(deviceKey)
	if _, exists := st.static[slot][groupID]; exists {
		m.removeDynamicLocked(deviceKey, slot, groupID)
		return time.Time{}
	}
	if st.dynamic[slot] == nil {
		st.dynamic[slot] = map[uint32]time.Time{}
	}
	st.dynamic[slot][groupID] = expiresAt

	key := routeKey{slot: slot, group: groupID}
	if m.dynamicIndex[key] == nil {
		m.dynamicIndex[key] = map[string]time.Time{}
	}
	m.dynamicIndex[key][deviceKey] = expiresAt
	return expiresAt
}

func (m *SubscriptionManager) RemoveDynamic(deviceKey string, slot Slot, groupID uint32) {
	if deviceKey == "" || !validSlot(slot) || groupID == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeDynamicLocked(deviceKey, slot, groupID)
}

func (m *SubscriptionManager) RemoveDevice(deviceKey string) {
	if deviceKey == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.devices[deviceKey]
	if st == nil {
		return
	}
	for slot, groups := range st.static {
		for groupID := range groups {
			m.removeStaticIndexLocked(deviceKey, slot, groupID)
		}
	}
	for slot, groups := range st.dynamic {
		for groupID := range groups {
			m.removeDynamicLocked(deviceKey, slot, groupID)
		}
	}
	delete(m.devices, deviceKey)
}

func (m *SubscriptionManager) ResolveRoutes(sourceDevice string, groupID uint32, now time.Time) []TargetRoute {
	if groupID == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	targets := map[TargetRoute]struct{}{}
	for _, slot := range []Slot{Slot1, Slot2} {
		key := routeKey{slot: slot, group: groupID}
		for deviceKey := range m.staticIndex[key] {
			if deviceKey == sourceDevice {
				continue
			}
			targets[TargetRoute{DeviceKey: deviceKey, Slot: slot}] = struct{}{}
		}
		for deviceKey, expiresAt := range m.dynamicIndex[key] {
			if expiresAt.IsZero() || !expiresAt.After(now) {
				m.removeDynamicLocked(deviceKey, slot, groupID)
				continue
			}
			if deviceKey == sourceDevice {
				continue
			}
			targets[TargetRoute{DeviceKey: deviceKey, Slot: slot}] = struct{}{}
		}
	}

	out := make([]TargetRoute, 0, len(targets))
	for route := range targets {
		out = append(out, route)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeviceKey == out[j].DeviceKey {
			return out[i].Slot < out[j].Slot
		}
		return out[i].DeviceKey < out[j].DeviceKey
	})
	return out
}

func (m *SubscriptionManager) Expire(now time.Time) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	for deviceKey, st := range m.devices {
		for slot, groups := range st.dynamic {
			for groupID, expiresAt := range groups {
				if expiresAt.IsZero() || expiresAt.After(now) {
					continue
				}
				m.removeDynamicLocked(deviceKey, slot, groupID)
			}
		}
	}
}

func (m *SubscriptionManager) SnapshotDevice(deviceKey string, now time.Time) DeviceSubscriptions {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotDeviceLocked(deviceKey, now)
}

func (m *SubscriptionManager) SnapshotAll(now time.Time) map[string]DeviceSubscriptions {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]DeviceSubscriptions, len(m.devices))
	for deviceKey := range m.devices {
		out[deviceKey] = m.snapshotDeviceLocked(deviceKey, now)
	}
	return out
}

func (m *SubscriptionManager) snapshotDeviceLocked(deviceKey string, now time.Time) DeviceSubscriptions {
	out := DeviceSubscriptions{
		DeviceKey: deviceKey,
		Slots:     map[Slot][]GroupSubscription{Slot1: {}, Slot2: {}},
	}
	st := m.devices[deviceKey]
	if st == nil {
		return out
	}
	for _, slot := range []Slot{Slot1, Slot2} {
		for groupID := range st.static[slot] {
			out.Slots[slot] = append(out.Slots[slot], GroupSubscription{
				GroupID: groupID,
				Kind:    SubscriptionStatic,
			})
		}
		for groupID, expiresAt := range st.dynamic[slot] {
			if expiresAt.IsZero() || !expiresAt.After(now) {
				m.removeDynamicLocked(deviceKey, slot, groupID)
				continue
			}
			out.Slots[slot] = append(out.Slots[slot], GroupSubscription{
				GroupID:   groupID,
				Kind:      SubscriptionDynamic,
				ExpiresAt: expiresAt,
			})
		}
		sort.Slice(out.Slots[slot], func(i, j int) bool {
			if out.Slots[slot][i].GroupID == out.Slots[slot][j].GroupID {
				return out.Slots[slot][i].Kind < out.Slots[slot][j].Kind
			}
			return out.Slots[slot][i].GroupID < out.Slots[slot][j].GroupID
		})
	}
	return out
}

func (m *SubscriptionManager) getOrCreateLocked(deviceKey string) *deviceState {
	st := m.devices[deviceKey]
	if st != nil {
		return st
	}
	st = &deviceState{
		static: map[Slot]map[uint32]struct{}{
			Slot1: {},
			Slot2: {},
		},
		dynamic: map[Slot]map[uint32]time.Time{
			Slot1: {},
			Slot2: {},
		},
	}
	m.devices[deviceKey] = st
	return st
}

func (m *SubscriptionManager) addStaticIndexLocked(deviceKey string, slot Slot, groupID uint32) {
	key := routeKey{slot: slot, group: groupID}
	if m.staticIndex[key] == nil {
		m.staticIndex[key] = map[string]struct{}{}
	}
	m.staticIndex[key][deviceKey] = struct{}{}
}

func (m *SubscriptionManager) removeStaticIndexLocked(deviceKey string, slot Slot, groupID uint32) {
	key := routeKey{slot: slot, group: groupID}
	devices := m.staticIndex[key]
	if devices == nil {
		return
	}
	delete(devices, deviceKey)
	if len(devices) == 0 {
		delete(m.staticIndex, key)
	}
}

func (m *SubscriptionManager) removeDynamicLocked(deviceKey string, slot Slot, groupID uint32) {
	st := m.devices[deviceKey]
	if st != nil && st.dynamic[slot] != nil {
		delete(st.dynamic[slot], groupID)
	}
	key := routeKey{slot: slot, group: groupID}
	devices := m.dynamicIndex[key]
	if devices == nil {
		return
	}
	delete(devices, deviceKey)
	if len(devices) == 0 {
		delete(m.dynamicIndex, key)
	}
}

func validSlot(slot Slot) bool {
	return slot == Slot1 || slot == Slot2
}

func uniqueGroups(groups []uint32) []uint32 {
	set := make(map[uint32]struct{}, len(groups))
	for _, groupID := range groups {
		if groupID == 0 {
			continue
		}
		set[groupID] = struct{}{}
	}
	out := make([]uint32, 0, len(set))
	for groupID := range set {
		out = append(out, groupID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
