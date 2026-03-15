package registry

import (
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/hicaoc/ipsc2mmdvm/internal/dmrid"
)

const recentCallLimit = 50

type Service struct {
	store *Store
	dmrid *dmrid.Resolver

	mu         sync.RWMutex
	devices    map[string]Device
	recent     []CallRecord
	callTotal  int64
	activeCall map[string]activeCallState

	subMu sync.RWMutex
	subs  map[chan Event]struct{}

	done chan struct{}
	wg   sync.WaitGroup
}

type activeCallState struct {
	ID         int64
	StartedAt  time.Time
	LastSeenAt time.Time
	Frontend   string
	SourceKey  string
	Slot       int
	CallType   string
}

func NewService(store *Store, resolver *dmrid.Resolver) (*Service, error) {
	devices, err := store.LoadDevices()
	if err != nil {
		return nil, err
	}
	if err := store.RepairStaleOpenCalls(30*time.Second, time.Now().UTC()); err != nil {
		return nil, err
	}
	calls, err := store.LoadCalls(recentCallLimit)
	if err != nil {
		return nil, err
	}
	callTotal, err := store.CountCalls()
	if err != nil {
		return nil, err
	}

	s := &Service{
		store:      store,
		dmrid:      resolver,
		devices:    make(map[string]Device, len(devices)),
		recent:     reverseCalls(calls),
		callTotal:  callTotal,
		activeCall: map[string]activeCallState{},
		subs:       map[chan Event]struct{}{},
		done:       make(chan struct{}),
	}
	for _, dev := range devices {
		s.devices[dev.SourceKey] = s.enrichDevice(dev)
	}

	s.wg.Add(1)
	go s.sweeper()
	return s, nil
}

func (s *Service) Close() error {
	close(s.done)
	s.wg.Wait()
	return s.store.Close()
}

func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	devices := make([]Device, 0, len(s.devices))
	for _, dev := range s.devices {
		devices = append(devices, s.enrichDevice(dev))
	}
	calls := make([]CallRecord, 0, len(s.recent))
	for _, call := range s.recent {
		calls = append(calls, s.enrichCall(call))
	}
	return Snapshot{Devices: devices, Calls: calls, CallTotal: s.callTotal}
}

func (s *Service) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	return ch, func() {
		s.subMu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.subMu.Unlock()
	}
}

func (s *Service) UpsertDevice(dev Device) (Device, error) {
	dev = s.enrichDevice(dev)
	stored, err := s.store.UpsertDevice(dev)
	if err != nil {
		return Device{}, err
	}
	if stored.OwnerUserID == 0 && stored.Callsign != "" {
		if ownerID, err := s.store.FindUserIDByCallsign(stored.Callsign); err == nil && ownerID != 0 {
			if reassigned, err := s.store.AssignDeviceOwner(stored.ID, ownerID); err == nil {
				stored = reassigned
			}
		}
	}
	s.mu.Lock()
	if stored.DMRID != 0 && dedupeByDMRID(stored.Protocol) {
		for sourceKey, existing := range s.devices {
			if sourceKey != stored.SourceKey && existing.DMRID == stored.DMRID && dedupeByDMRID(existing.Protocol) {
				delete(s.devices, sourceKey)
			}
		}
	}
	s.devices[stored.SourceKey] = stored
	s.mu.Unlock()
	stored = s.enrichDevice(stored)
	s.publish(Event{Type: "device_updated", Device: &stored})
	return stored, nil
}

func (s *Service) MarkDeviceOffline(sourceKey, status string) error {
	s.mu.RLock()
	dev, ok := s.devices[sourceKey]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	dev.Online = false
	dev.Status = status
	dev.LastSeenAt = time.Now().UTC()
	_, err := s.UpsertDevice(dev)
	return err
}

func (s *Service) UpdateDeviceMetadata(id int64, patch DevicePatch) (Device, error) {
	dev, err := s.store.UpdateDeviceMetadata(id, patch)
	if err != nil {
		return Device{}, err
	}
	s.mu.Lock()
	dev = s.enrichDevice(dev)
	s.devices[dev.SourceKey] = dev
	s.mu.Unlock()
	s.publish(Event{Type: "device_updated", Device: &dev})
	return dev, nil
}

func (s *Service) DeleteDevice(id int64) (Device, error) {
	dev, err := s.store.DeleteDevice(id)
	if err != nil {
		return Device{}, err
	}
	s.mu.Lock()
	delete(s.devices, dev.SourceKey)
	s.mu.Unlock()
	dev = s.enrichDevice(dev)
	s.publish(Event{Type: "device_deleted", Device: &dev})
	return dev, nil
}

func (s *Service) RecordCall(call CallRecord, ended bool) (CallRecord, bool, error) {
	call = s.enrichCall(call)
	key := callDedupKey(call)
	now := time.Now().UTC()

	s.mu.Lock()
	if active, ok := s.activeCall[key]; ok {
		if ended {
			s.mu.Unlock()
			updated, err := s.finishActiveCall(key, active, now)
			return updated, false, err
		}
		active.LastSeenAt = now
		s.activeCall[key] = active
		s.mu.Unlock()
		return CallRecord{}, false, nil
	}
	if fallbackKey, active, ok := s.findMatchingActiveCallLocked(call, ended); ok {
		if ended {
			s.mu.Unlock()
			updated, err := s.finishActiveCall(fallbackKey, active, now)
			return updated, false, err
		}
		active.LastSeenAt = now
		s.activeCall[fallbackKey] = active
		s.mu.Unlock()
		return CallRecord{}, false, nil
	}
	s.mu.Unlock()

	call.CreatedAt = now
	if ended {
		call.EndedAt = now
		call.DurationMS = 0
	}
	stored, err := s.store.InsertCall(call)
	if err != nil {
		return CallRecord{}, false, err
	}

	s.mu.Lock()
	if !ended {
		s.activeCall[key] = activeCallState{
			ID:         stored.ID,
			StartedAt:  stored.CreatedAt,
			LastSeenAt: stored.CreatedAt,
			Frontend:   stored.Frontend,
			SourceKey:  stored.SourceKey,
			Slot:       stored.Slot,
			CallType:   stored.CallType,
		}
	}
	s.callTotal++
	stored = s.enrichCall(stored)
	s.recent = append(s.recent, stored)
	if len(s.recent) > recentCallLimit {
		s.recent = append([]CallRecord(nil), s.recent[len(s.recent)-recentCallLimit:]...)
	}
	if dev, ok := s.devices[call.SourceKey]; ok {
		dev.LastCallAt = stored.CreatedAt
		s.devices[call.SourceKey] = dev
	}
	s.mu.Unlock()

	s.publish(Event{Type: "call_recorded", Call: &stored})
	return stored, true, nil
}

func (s *Service) enrichDevice(dev Device) Device {
	if dev.Callsign == "" && dev.DMRID != 0 && s.dmrid != nil {
		dev.Callsign = s.dmrid.Lookup(dev.DMRID)
	}
	return dev
}

func (s *Service) enrichCall(call CallRecord) CallRecord {
	if call.SourceCallsign == "" && s.dmrid != nil {
		if call.SourceDMRID != 0 {
			call.SourceCallsign = s.dmrid.Lookup(call.SourceDMRID)
		}
		if call.SourceCallsign == "" && call.SrcID != 0 {
			call.SourceCallsign = s.dmrid.Lookup(uint32(call.SrcID))
		}
	}
	return call
}

func (s *Service) FindDevice(sourceKey string) (Device, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	dev, ok := s.devices[sourceKey]
	return dev, ok
}

func (s *Service) DeviceByID(id int64) (Device, error) {
	dev, err := s.store.DeviceByID(id)
	if err != nil {
		return Device{}, err
	}
	return s.enrichDevice(dev), nil
}

func (s *Service) LoadStaticGroups() ([]StaticGroup, error) {
	return s.store.LoadStaticGroups()
}

func (s *Service) ReplaceStaticGroups(sourceKey string, slot int, groups []uint32) error {
	return s.store.ReplaceStaticGroups(sourceKey, slot, groups)
}

func (s *Service) ListUsers() ([]User, error) {
	return s.store.ListUsers()
}

func (s *Service) UserByID(id int64) (User, error) {
	return s.store.UserByID(id)
}

func (s *Service) UserByUsername(username string) (User, error) {
	return s.store.UserByUsername(username)
}

func (s *Service) UserByCallsign(callsign string) (User, error) {
	return s.store.UserByCallsign(callsign)
}

func (s *Service) UserByEmail(email string) (User, error) {
	return s.store.UserByEmail(email)
}

func (s *Service) CreateUser(user User) (User, error) {
	return s.store.CreateUser(user)
}

func (s *Service) UpdateUser(user User) (User, error) {
	return s.store.UpdateUser(user)
}

func (s *Service) DeleteUser(id int64) error {
	return s.store.DeleteUser(id)
}

func (s *Service) UpdateUserLastLogin(id int64, when time.Time) error {
	return s.store.UpdateUserLastLogin(id, when)
}

func (s *Service) EnsureAdminUser(user User) (User, bool, error) {
	count, err := s.store.UserCount()
	if err != nil {
		return User{}, false, err
	}
	if count > 0 {
		if existing, err := s.store.UserByUsername(user.Username); err == nil {
			return existing, false, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return User{}, false, err
		}
		return User{}, false, nil
	}
	user.Role = RoleAdmin
	user.Enabled = true
	created, err := s.store.CreateUser(user)
	return created, err == nil, err
}

func (s *Service) sweeper() {
	defer s.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.expireDevices()
			s.expireCalls()
		case <-s.done:
			return
		}
	}
}

func (s *Service) expireDevices() {
	now := time.Now().UTC()
	var toOffline []Device

	s.mu.RLock()
	for _, dev := range s.devices {
		if !dev.Online {
			continue
		}
		timeout := deviceTimeout(dev.Category)
		if timeout == 0 || now.Sub(dev.LastSeenAt) <= timeout {
			continue
		}
		copyDev := dev
		copyDev.Online = false
		copyDev.Status = "offline"
		toOffline = append(toOffline, copyDev)
	}
	s.mu.RUnlock()

	for _, dev := range toOffline {
		_, _ = s.UpsertDevice(dev)
	}
}

func (s *Service) expireCalls() {
	now := time.Now().UTC()
	var expired []struct {
		key    string
		active activeCallState
	}
	s.mu.Lock()
	for key, active := range s.activeCall {
		lastSeenAt := active.LastSeenAt
		if lastSeenAt.IsZero() {
			lastSeenAt = active.StartedAt
		}
		if now.Sub(lastSeenAt) > callIdleTimeout(active.Frontend) {
			expired = append(expired, struct {
				key    string
				active activeCallState
			}{key: key, active: active})
		}
	}
	s.mu.Unlock()
	for _, item := range expired {
		_, _ = s.finishActiveCall(item.key, item.active, now)
	}
}

func callIdleTimeout(frontend string) time.Duration {
	if frontend == "nrl" {
		return 4 * time.Second
	}
	return 12 * time.Second
}

func (s *Service) finishActiveCall(key string, active activeCallState, endedAt time.Time) (CallRecord, error) {
	durationMS := endedAt.Sub(active.StartedAt).Milliseconds()
	if err := s.store.UpdateCallDuration(active.ID, endedAt, durationMS); err != nil {
		return CallRecord{}, err
	}

	s.mu.Lock()
	delete(s.activeCall, key)
	var updated CallRecord
	for i := range s.recent {
		if s.recent[i].ID == active.ID {
			s.recent[i].EndedAt = endedAt
			s.recent[i].DurationMS = durationMS
			updated = s.enrichCall(s.recent[i])
			break
		}
	}
	s.mu.Unlock()
	if updated.ID != 0 {
		s.publish(Event{Type: "call_recorded", Call: &updated})
	}
	return updated, nil
}

func (s *Service) findMatchingActiveCallLocked(call CallRecord, ended bool) (string, activeCallState, bool) {
	for key, active := range s.activeCall {
		if active.Frontend != call.Frontend {
			continue
		}
		if active.SourceKey != call.SourceKey {
			continue
		}
		// NRL analog traffic can legitimately drift slot/type/stream markers while
		// still representing the same ongoing source channel. Keep only one active
		// call per NRL sourceKey.
		if call.Frontend == "nrl" {
			return key, active, true
		}
		if active.Slot != call.Slot {
			continue
		}
		if active.CallType != call.CallType {
			continue
		}
		// For non-terminator packets, require dst/src consistency so different TG/PC
		// conversations are not collapsed into one endless active record.
		if !ended {
			if call.DstID != 0 {
				existing, ok := s.recentCallByIDLocked(active.ID)
				if !ok || existing.DstID != call.DstID {
					continue
				}
				if call.SrcID != 0 && existing.SrcID != 0 && existing.SrcID != call.SrcID {
					continue
				}
			}
		}
		return key, active, true
	}
	return "", activeCallState{}, false
}

func (s *Service) recentCallByIDLocked(id int64) (CallRecord, bool) {
	for i := range s.recent {
		if s.recent[i].ID == id {
			return s.recent[i], true
		}
	}
	return CallRecord{}, false
}

func (s *Service) publish(event Event) {
	s.subMu.RLock()
	defer s.subMu.RUnlock()
	for ch := range s.subs {
		select {
		case ch <- event:
		default:
		}
	}
}

func deviceTimeout(category DeviceCategory) time.Duration {
	switch category {
	case CategoryMMDVM:
		return 5 * time.Minute
	case CategoryMoto, CategoryHytera:
		return 90 * time.Second
	default:
		return 60 * time.Second
	}
}

func callDedupKey(call CallRecord) string {
	return call.Frontend + "|" + call.SourceKey + "|" + streamIDString(call.StreamID)
}

func streamIDString(v uint) string {
	return strconv.FormatUint(uint64(v), 10)
}

func reverseCalls(in []CallRecord) []CallRecord {
	out := make([]CallRecord, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		out = append(out, in[i])
	}
	return out
}
