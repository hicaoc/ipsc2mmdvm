package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS devices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			category TEXT NOT NULL,
			protocol TEXT NOT NULL,
			source_key TEXT NOT NULL UNIQUE,
			owner_user_id INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL DEFAULT '',
			callsign TEXT NOT NULL DEFAULT '',
			dmrid INTEGER NOT NULL DEFAULT 0,
			model TEXT NOT NULL DEFAULT '',
			serial TEXT NOT NULL DEFAULT '',
			ip TEXT NOT NULL DEFAULT '',
			port INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT '',
			online INTEGER NOT NULL DEFAULT 0,
			first_seen_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			last_call_at TEXT NOT NULL DEFAULT '',
			rx_freq INTEGER NOT NULL DEFAULT 0,
			tx_freq INTEGER NOT NULL DEFAULT 0,
			tx_power INTEGER NOT NULL DEFAULT 0,
			color_code INTEGER NOT NULL DEFAULT 0,
			latitude REAL NOT NULL DEFAULT 0,
			longitude REAL NOT NULL DEFAULT 0,
			height INTEGER NOT NULL DEFAULT 0,
			location TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			url TEXT NOT NULL DEFAULT '',
			slots INTEGER NOT NULL DEFAULT 0,
			notes TEXT NOT NULL DEFAULT '',
			device_password TEXT NOT NULL DEFAULT '',
			disabled INTEGER NOT NULL DEFAULT 0,
			extra_json TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS calls (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			ended_at TEXT NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			frontend TEXT NOT NULL,
			source_category TEXT NOT NULL,
			source_key TEXT NOT NULL,
			source_name TEXT NOT NULL DEFAULT '',
			source_callsign TEXT NOT NULL DEFAULT '',
			source_dmrid INTEGER NOT NULL DEFAULT 0,
			src_id INTEGER NOT NULL DEFAULT 0,
			dst_id INTEGER NOT NULL DEFAULT 0,
			repeater_id INTEGER NOT NULL DEFAULT 0,
			slot INTEGER NOT NULL DEFAULT 0,
			call_type TEXT NOT NULL DEFAULT '',
			stream_id INTEGER NOT NULL DEFAULT 0,
			from_ip TEXT NOT NULL DEFAULT '',
			from_port INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS static_groups (
			source_key TEXT NOT NULL,
			slot INTEGER NOT NULL,
			group_id INTEGER NOT NULL,
			PRIMARY KEY (source_key, slot, group_id)
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			callsign TEXT NOT NULL UNIQUE,
			email TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			last_login_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS app_stats (
			key TEXT PRIMARY KEY,
			value INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_created_at ON calls(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_devices_category ON devices(category)`,
		`CREATE INDEX IF NOT EXISTS idx_static_groups_source ON static_groups(source_key)`,
		`CREATE INDEX IF NOT EXISTS idx_users_role ON users(role)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	if err := s.ensureCallColumn("ended_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureCallColumn("duration_ms", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureDeviceColumn("owner_user_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureDeviceColumn("device_password", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureDeviceColumn("nrl_server_addr", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureDeviceColumn("nrl_server_port", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureDeviceColumn("nrl_ssid", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureDeviceColumn("nrl_udp_port", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_devices_owner_user_id ON devices(owner_user_id)`); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	// Initialize call_total once from historical rows.
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO app_stats(key, value)
		VALUES ('call_total', (SELECT COUNT(1) FROM calls))
	`); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	// Keep call details table bounded to the latest N rows.
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM calls
		WHERE id NOT IN (
			SELECT id FROM calls ORDER BY created_at DESC, id DESC LIMIT ?
		)
	`, recentCallLimit); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	return nil
}

func (s *Store) LoadStaticGroups() ([]StaticGroup, error) {
	rows, err := s.db.Query(`
		SELECT source_key, slot, group_id
		FROM static_groups
		ORDER BY source_key ASC, slot ASC, group_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []StaticGroup
	for rows.Next() {
		var group StaticGroup
		if err := rows.Scan(&group.SourceKey, &group.Slot, &group.GroupID); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Store) ReplaceStaticGroups(sourceKey string, slot int, groups []uint32) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.Exec(`DELETE FROM static_groups WHERE source_key=? AND slot=?`, sourceKey, slot); err != nil {
		return err
	}
	for _, groupID := range groups {
		if groupID == 0 {
			continue
		}
		if _, err = tx.Exec(`INSERT INTO static_groups (source_key, slot, group_id) VALUES (?, ?, ?)`, sourceKey, slot, groupID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ensureCallColumn(name, ddl string) error {
	rows, err := s.db.Query(`PRAGMA table_info(calls)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			colName    string
			colType    string
			notNull    int
			defaultV   sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultV, &primaryKey); err != nil {
			return err
		}
		if strings.EqualFold(colName, name) {
			return nil
		}
	}
	_, err = s.db.Exec(`ALTER TABLE calls ADD COLUMN ` + name + ` ` + ddl)
	return err
}

func (s *Store) ensureDeviceColumn(name, ddl string) error {
	rows, err := s.db.Query(`PRAGMA table_info(devices)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			colName    string
			colType    string
			notNull    int
			defaultV   sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultV, &primaryKey); err != nil {
			return err
		}
		if strings.EqualFold(colName, name) {
			return nil
		}
	}
	_, err = s.db.Exec(`ALTER TABLE devices ADD COLUMN ` + name + ` ` + ddl)
	return err
}

func (s *Store) LoadDevices() ([]Device, error) {
	rows, err := s.db.Query(`
		SELECT id, category, protocol, source_key, owner_user_id, name, callsign, dmrid, model, serial, ip, port,
		       status, online, first_seen_at, last_seen_at, last_call_at, rx_freq, tx_freq, tx_power,
		       color_code, latitude, longitude, height, location, description, url, slots, notes,
		       device_password, nrl_server_addr, nrl_server_port, nrl_ssid, nrl_udp_port, disabled, extra_json
		FROM devices ORDER BY last_seen_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		dev, err := scanDevice(rows.Scan)
		if err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

func (s *Store) LoadCalls(limit int) ([]CallRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, created_at, ended_at, duration_ms, frontend, source_category, source_key, source_name, source_callsign,
		       source_dmrid, src_id, dst_id, repeater_id, slot, call_type, stream_id, from_ip, from_port
		FROM calls ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var calls []CallRecord
	for rows.Next() {
		call, err := scanCall(rows.Scan)
		if err != nil {
			return nil, err
		}
		calls = append(calls, call)
	}
	return calls, rows.Err()
}

func (s *Store) CountCalls() (int64, error) {
	row := s.db.QueryRow(`SELECT value FROM app_stats WHERE key='call_total' LIMIT 1`)
	var total int64
	if err := row.Scan(&total); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return total, nil
}

func (s *Store) RepairStaleOpenCalls(maxAge time.Duration, now time.Time) error {
	if maxAge <= 0 {
		maxAge = 30 * time.Second
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rows, err := s.db.Query(`
		SELECT id, created_at
		FROM calls
		WHERE (ended_at = '' OR ended_at = '0001-01-01T00:00:00Z')`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type staleCall struct {
		id        int64
		createdAt time.Time
	}
	var stale []staleCall
	for rows.Next() {
		var (
			id         int64
			createdRaw string
		)
		if err := rows.Scan(&id, &createdRaw); err != nil {
			return err
		}
		createdAt := parseTime(createdRaw)
		if createdAt.IsZero() || now.Sub(createdAt) <= maxAge {
			continue
		}
		stale = append(stale, staleCall{id: id, createdAt: createdAt})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, call := range stale {
		durationMS := now.Sub(call.createdAt).Milliseconds()
		if durationMS < 0 {
			durationMS = 0
		}
		if err := s.UpdateCallDuration(call.id, now, durationMS); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) UpsertDevice(dev Device) (Device, error) {
	now := time.Now().UTC()
	if dev.FirstSeenAt.IsZero() {
		dev.FirstSeenAt = now
	}
	if dev.LastSeenAt.IsZero() {
		dev.LastSeenAt = now
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Device{}, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if dev, err = s.mergeDuplicateDMRID(tx, dev); err != nil {
		return Device{}, err
	}

	_, err = tx.Exec(`
		INSERT INTO devices (
			category, protocol, source_key, owner_user_id, name, callsign, dmrid, model, serial, ip, port, status, online,
			first_seen_at, last_seen_at, last_call_at, rx_freq, tx_freq, tx_power, color_code, latitude,
			longitude, height, location, description, url, slots, notes, device_password, nrl_server_addr, nrl_server_port,
			nrl_ssid, nrl_udp_port, disabled, extra_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_key) DO UPDATE SET
			category=excluded.category,
			protocol=excluded.protocol,
			owner_user_id=CASE WHEN devices.owner_user_id <> 0 THEN devices.owner_user_id ELSE excluded.owner_user_id END,
			name=CASE WHEN excluded.name = '' THEN devices.name ELSE excluded.name END,
			callsign=CASE WHEN excluded.callsign = '' THEN devices.callsign ELSE excluded.callsign END,
			dmrid=CASE WHEN excluded.dmrid = 0 THEN devices.dmrid ELSE excluded.dmrid END,
			model=CASE WHEN excluded.model = '' THEN devices.model ELSE excluded.model END,
			serial=CASE WHEN excluded.serial = '' THEN devices.serial ELSE excluded.serial END,
			ip=CASE WHEN excluded.ip = '' THEN devices.ip ELSE excluded.ip END,
			port=CASE WHEN excluded.port = 0 THEN devices.port ELSE excluded.port END,
			status=CASE WHEN excluded.status = '' THEN devices.status ELSE excluded.status END,
			online=excluded.online,
			last_seen_at=excluded.last_seen_at,
			last_call_at=CASE WHEN excluded.last_call_at = '' THEN devices.last_call_at ELSE excluded.last_call_at END,
			rx_freq=CASE WHEN excluded.rx_freq = 0 THEN devices.rx_freq ELSE excluded.rx_freq END,
			tx_freq=CASE WHEN excluded.tx_freq = 0 THEN devices.tx_freq ELSE excluded.tx_freq END,
			tx_power=CASE WHEN excluded.tx_power = 0 THEN devices.tx_power ELSE excluded.tx_power END,
			color_code=CASE WHEN excluded.color_code = 0 THEN devices.color_code ELSE excluded.color_code END,
			latitude=CASE WHEN excluded.latitude = 0 THEN devices.latitude ELSE excluded.latitude END,
			longitude=CASE WHEN excluded.longitude = 0 THEN devices.longitude ELSE excluded.longitude END,
			height=CASE WHEN excluded.height = 0 THEN devices.height ELSE excluded.height END,
			location=CASE WHEN excluded.location = '' THEN devices.location ELSE excluded.location END,
			description=CASE WHEN excluded.description = '' THEN devices.description ELSE excluded.description END,
			url=CASE WHEN excluded.url = '' THEN devices.url ELSE excluded.url END,
			slots=CASE WHEN excluded.slots = 0 THEN devices.slots ELSE excluded.slots END,
			notes=excluded.notes,
			device_password=CASE WHEN excluded.device_password = '' THEN devices.device_password ELSE excluded.device_password END,
			nrl_server_addr=CASE WHEN excluded.nrl_server_addr = '' THEN devices.nrl_server_addr ELSE excluded.nrl_server_addr END,
			nrl_server_port=CASE WHEN excluded.nrl_server_port = 0 THEN devices.nrl_server_port ELSE excluded.nrl_server_port END,
			nrl_ssid=CASE WHEN excluded.nrl_ssid = 0 THEN devices.nrl_ssid ELSE excluded.nrl_ssid END,
			nrl_udp_port=CASE WHEN excluded.nrl_udp_port = 0 THEN devices.nrl_udp_port ELSE excluded.nrl_udp_port END,
			disabled=excluded.disabled,
			extra_json=CASE WHEN excluded.extra_json = '' THEN devices.extra_json ELSE excluded.extra_json END`,
		string(dev.Category), dev.Protocol, dev.SourceKey, dev.OwnerUserID, dev.Name, dev.Callsign, dev.DMRID, dev.Model, dev.Serial,
		dev.IP, dev.Port, dev.Status, boolToInt(dev.Online), formatTime(dev.FirstSeenAt), formatTime(dev.LastSeenAt),
		formatOptionalTime(dev.LastCallAt), dev.RXFreq, dev.TXFreq, dev.TXPower, dev.ColorCode, dev.Latitude,
		dev.Longitude, dev.Height, dev.Location, dev.Description, dev.URL, dev.Slots, dev.Notes, dev.DevicePassword,
		dev.NRLServerAddr, dev.NRLServerPort, dev.NRLSSID, dev.NRLUDPPort, boolToInt(dev.Disabled), dev.ExtraJSON)
	if err != nil {
		return Device{}, err
	}
	if err = tx.Commit(); err != nil {
		return Device{}, err
	}
	return s.DeviceBySourceKey(dev.SourceKey)
}

type DevicePatch struct {
	OwnerUserID    *int64
	Name           *string
	Callsign       *string
	Notes          *string
	Disabled       *bool
	DMRID          *uint32
	Model          *string
	Description    *string
	Location       *string
	DevicePassword *string
	NRLServerAddr  *string
	NRLServerPort  *int
	NRLSSID        *uint8
	NRLUDPPort     *int
}

func (s *Store) UpdateDeviceMetadata(id int64, patch DevicePatch) (Device, error) {
	current, err := s.DeviceByID(id)
	if err != nil {
		return Device{}, err
	}
	if patch.OwnerUserID != nil {
		current.OwnerUserID = *patch.OwnerUserID
	}
	if patch.Name != nil {
		current.Name = *patch.Name
	}
	if patch.Callsign != nil {
		current.Callsign = *patch.Callsign
	}
	if patch.Notes != nil {
		current.Notes = *patch.Notes
	}
	if patch.Disabled != nil {
		current.Disabled = *patch.Disabled
	}
	if patch.DMRID != nil {
		current.DMRID = *patch.DMRID
	}
	if patch.Model != nil {
		current.Model = *patch.Model
	}
	if patch.Description != nil {
		current.Description = *patch.Description
	}
	if patch.Location != nil {
		current.Location = *patch.Location
	}
	if patch.DevicePassword != nil {
		current.DevicePassword = *patch.DevicePassword
	}
	if patch.NRLServerAddr != nil {
		current.NRLServerAddr = *patch.NRLServerAddr
	}
	if patch.NRLServerPort != nil {
		current.NRLServerPort = *patch.NRLServerPort
	}
	if patch.NRLSSID != nil {
		current.NRLSSID = *patch.NRLSSID
	}
	if patch.NRLUDPPort != nil {
		current.NRLUDPPort = *patch.NRLUDPPort
	}
	return s.UpsertDevice(current)
}

func (s *Store) DeleteDevice(id int64) (Device, error) {
	current, err := s.DeviceByID(id)
	if err != nil {
		return Device{}, err
	}
	if _, err := s.db.Exec(`DELETE FROM static_groups WHERE source_key=?`, current.SourceKey); err != nil {
		return Device{}, err
	}
	if _, err := s.db.Exec(`DELETE FROM devices WHERE id=?`, id); err != nil {
		return Device{}, err
	}
	return current, nil
}

func (s *Store) mergeDuplicateDMRID(tx *sql.Tx, dev Device) (Device, error) {
	if dev.DMRID == 0 || dev.SourceKey == "" || !dedupeByDMRID(dev.Protocol) {
		return dev, nil
	}

	rows, err := tx.Query(`
		SELECT id, category, protocol, source_key, owner_user_id, name, callsign, dmrid, model, serial, ip, port,
		       status, online, first_seen_at, last_seen_at, last_call_at, rx_freq, tx_freq, tx_power,
		       color_code, latitude, longitude, height, location, description, url, slots, notes,
		       device_password, nrl_server_addr, nrl_server_port, nrl_ssid, nrl_udp_port, disabled, extra_json
		FROM devices WHERE dmrid=? AND source_key<>? AND protocol <> 'mmdvm-upstream'
		ORDER BY last_seen_at DESC, id DESC`, dev.DMRID, dev.SourceKey)
	if err != nil {
		return Device{}, err
	}
	defer rows.Close()

	var duplicates []Device
	for rows.Next() {
		found, err := scanDevice(rows.Scan)
		if err != nil {
			return Device{}, err
		}
		duplicates = append(duplicates, found)
	}
	if err := rows.Err(); err != nil {
		return Device{}, err
	}

	for _, dup := range duplicates {
		dev = mergePreferredDevice(dev, dup)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO static_groups (source_key, slot, group_id) SELECT ?, slot, group_id FROM static_groups WHERE source_key=?`, dev.SourceKey, dup.SourceKey); err != nil {
			return Device{}, err
		}
		if _, err := tx.Exec(`DELETE FROM static_groups WHERE source_key=?`, dup.SourceKey); err != nil {
			return Device{}, err
		}
		if _, err := tx.Exec(`DELETE FROM devices WHERE id=?`, dup.ID); err != nil {
			return Device{}, err
		}
	}

	return dev, nil
}

func mergePreferredDevice(incoming, existing Device) Device {
	if incoming.FirstSeenAt.IsZero() || (!existing.FirstSeenAt.IsZero() && existing.FirstSeenAt.Before(incoming.FirstSeenAt)) {
		incoming.FirstSeenAt = existing.FirstSeenAt
	}
	if incoming.LastCallAt.IsZero() && !existing.LastCallAt.IsZero() {
		incoming.LastCallAt = existing.LastCallAt
	}
	if incoming.Name == "" {
		incoming.Name = existing.Name
	}
	if incoming.Callsign == "" {
		incoming.Callsign = existing.Callsign
	}
	if incoming.Model == "" {
		incoming.Model = existing.Model
	}
	if incoming.Serial == "" {
		incoming.Serial = existing.Serial
	}
	if incoming.Location == "" {
		incoming.Location = existing.Location
	}
	if incoming.Description == "" {
		incoming.Description = existing.Description
	}
	if incoming.URL == "" {
		incoming.URL = existing.URL
	}
	if incoming.Notes == "" {
		incoming.Notes = existing.Notes
	}
	if incoming.NRLServerAddr == "" {
		incoming.NRLServerAddr = existing.NRLServerAddr
	}
	if incoming.NRLServerPort == 0 {
		incoming.NRLServerPort = existing.NRLServerPort
	}
	if incoming.NRLSSID == 0 {
		incoming.NRLSSID = existing.NRLSSID
	}
	if incoming.NRLUDPPort == 0 {
		incoming.NRLUDPPort = existing.NRLUDPPort
	}
	if !incoming.Disabled {
		incoming.Disabled = existing.Disabled
	}
	if incoming.ExtraJSON == "" {
		incoming.ExtraJSON = existing.ExtraJSON
	}
	return incoming
}

func dedupeByDMRID(protocol string) bool {
	return protocol == "ipsc" || protocol == "hytera"
}

func (s *Store) InsertCall(call CallRecord) (CallRecord, error) {
	if call.CreatedAt.IsZero() {
		call.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CallRecord{}, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	res, err := tx.Exec(`
		INSERT INTO calls (
			created_at, ended_at, duration_ms, frontend, source_category, source_key, source_name, source_callsign, source_dmrid,
			src_id, dst_id, repeater_id, slot, call_type, stream_id, from_ip, from_port
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		formatTime(call.CreatedAt), formatOptionalTime(call.EndedAt), call.DurationMS, call.Frontend, string(call.SourceCategory), call.SourceKey, call.SourceName,
		call.SourceCallsign, call.SourceDMRID, call.SrcID, call.DstID, call.RepeaterID, call.Slot, call.CallType,
		call.StreamID, call.FromIP, call.FromPort)
	if err != nil {
		return CallRecord{}, err
	}
	id, _ := res.LastInsertId()
	if _, err = tx.Exec(`UPDATE app_stats SET value=value+1 WHERE key='call_total'`); err != nil {
		return CallRecord{}, err
	}
	if _, err = tx.Exec(`
		DELETE FROM calls
		WHERE id NOT IN (
			SELECT id FROM calls ORDER BY created_at DESC, id DESC LIMIT ?
		)
	`, recentCallLimit); err != nil {
		return CallRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return CallRecord{}, err
	}
	call.ID = id
	return call, nil
}

func (s *Store) UpdateCallDuration(id int64, endedAt time.Time, durationMS int64) error {
	_, err := s.db.Exec(`UPDATE calls SET ended_at=?, duration_ms=? WHERE id=?`, formatTime(endedAt), durationMS, id)
	return err
}

func (s *Store) DeviceByID(id int64) (Device, error) {
	row := s.db.QueryRow(`
		SELECT id, category, protocol, source_key, owner_user_id, name, callsign, dmrid, model, serial, ip, port,
		       status, online, first_seen_at, last_seen_at, last_call_at, rx_freq, tx_freq, tx_power,
		       color_code, latitude, longitude, height, location, description, url, slots, notes,
		       device_password, nrl_server_addr, nrl_server_port, nrl_ssid, nrl_udp_port, disabled, extra_json
		FROM devices WHERE id=?`, id)
	return scanDevice(row.Scan)
}

func (s *Store) DeviceBySourceKey(sourceKey string) (Device, error) {
	row := s.db.QueryRow(`
		SELECT id, category, protocol, source_key, owner_user_id, name, callsign, dmrid, model, serial, ip, port,
		       status, online, first_seen_at, last_seen_at, last_call_at, rx_freq, tx_freq, tx_power,
		       color_code, latitude, longitude, height, location, description, url, slots, notes,
		       device_password, nrl_server_addr, nrl_server_port, nrl_ssid, nrl_udp_port, disabled, extra_json
		FROM devices WHERE source_key=?`, sourceKey)
	return scanDevice(row.Scan)
}

func (s *Store) ListDevicesByOwner(ownerUserID int64) ([]Device, error) {
	rows, err := s.db.Query(`
		SELECT id, category, protocol, source_key, owner_user_id, name, callsign, dmrid, model, serial, ip, port,
		       status, online, first_seen_at, last_seen_at, last_call_at, rx_freq, tx_freq, tx_power,
		       color_code, latitude, longitude, height, location, description, url, slots, notes,
		       device_password, nrl_server_addr, nrl_server_port, nrl_ssid, nrl_udp_port, disabled, extra_json
		FROM devices WHERE owner_user_id=? ORDER BY id DESC`, ownerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		dev, err := scanDevice(rows.Scan)
		if err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

func (s *Store) AssignDeviceOwner(id, ownerUserID int64) (Device, error) {
	_, err := s.db.Exec(`UPDATE devices SET owner_user_id=? WHERE id=?`, ownerUserID, id)
	if err != nil {
		return Device{}, err
	}
	return s.DeviceByID(id)
}

func (s *Store) FindUserIDByCallsign(callsign string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM users WHERE UPPER(callsign)=UPPER(?) LIMIT 1`, strings.TrimSpace(callsign)).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) CreateUser(user User) (User, error) {
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt = now
	res, err := s.db.Exec(`
		INSERT INTO users (username, callsign, email, password_hash, role, enabled, created_at, updated_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		user.Username, user.Callsign, user.Email, user.PasswordHash, string(user.Role), boolToInt(user.Enabled),
		formatTime(user.CreatedAt), formatTime(user.UpdatedAt), formatOptionalTime(user.LastLoginAt))
	if err != nil {
		return User{}, err
	}
	user.ID, _ = res.LastInsertId()
	return s.UserByID(user.ID)
}

func (s *Store) UpdateUser(user User) (User, error) {
	user.UpdatedAt = time.Now().UTC()
	_, err := s.db.Exec(`
		UPDATE users
		SET username=?, callsign=?, email=?, password_hash=?, role=?, enabled=?, updated_at=?, last_login_at=?
		WHERE id=?`,
		user.Username, user.Callsign, user.Email, user.PasswordHash, string(user.Role), boolToInt(user.Enabled),
		formatTime(user.UpdatedAt), formatOptionalTime(user.LastLoginAt), user.ID)
	if err != nil {
		return User{}, err
	}
	return s.UserByID(user.ID)
}

func (s *Store) DeleteUser(id int64) error {
	if _, err := s.db.Exec(`UPDATE devices SET owner_user_id=0 WHERE owner_user_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM users WHERE id=?`, id)
	return err
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`
		SELECT id, username, callsign, email, password_hash, role, enabled, created_at, updated_at, last_login_at
		FROM users ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		user, err := scanUser(rows.Scan)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Store) UserByID(id int64) (User, error) {
	row := s.db.QueryRow(`
		SELECT id, username, callsign, email, password_hash, role, enabled, created_at, updated_at, last_login_at
		FROM users WHERE id=?`, id)
	return scanUser(row.Scan)
}

func (s *Store) UserByUsername(username string) (User, error) {
	row := s.db.QueryRow(`
		SELECT id, username, callsign, email, password_hash, role, enabled, created_at, updated_at, last_login_at
		FROM users WHERE LOWER(username)=LOWER(?)`, strings.TrimSpace(username))
	return scanUser(row.Scan)
}

func (s *Store) UserByCallsign(callsign string) (User, error) {
	row := s.db.QueryRow(`
		SELECT id, username, callsign, email, password_hash, role, enabled, created_at, updated_at, last_login_at
		FROM users WHERE UPPER(callsign)=UPPER(?)`, strings.TrimSpace(callsign))
	return scanUser(row.Scan)
}

func (s *Store) UserByEmail(email string) (User, error) {
	row := s.db.QueryRow(`
		SELECT id, username, callsign, email, password_hash, role, enabled, created_at, updated_at, last_login_at
		FROM users WHERE LOWER(email)=LOWER(?)`, strings.TrimSpace(email))
	return scanUser(row.Scan)
}

func (s *Store) UpdateUserLastLogin(id int64, when time.Time) error {
	_, err := s.db.Exec(`UPDATE users SET last_login_at=?, updated_at=? WHERE id=?`, formatTime(when), formatTime(when), id)
	return err
}

func (s *Store) UserCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count)
	return count, err
}

func scanDevice(scan func(dest ...any) error) (Device, error) {
	var (
		dev          Device
		category     string
		firstSeenRaw string
		lastSeenRaw  string
		lastCallRaw  string
		online       int
		disabled     int
	)
	err := scan(
		&dev.ID, &category, &dev.Protocol, &dev.SourceKey, &dev.OwnerUserID, &dev.Name, &dev.Callsign, &dev.DMRID, &dev.Model,
		&dev.Serial, &dev.IP, &dev.Port, &dev.Status, &online, &firstSeenRaw, &lastSeenRaw, &lastCallRaw,
		&dev.RXFreq, &dev.TXFreq, &dev.TXPower, &dev.ColorCode, &dev.Latitude, &dev.Longitude, &dev.Height,
		&dev.Location, &dev.Description, &dev.URL, &dev.Slots, &dev.Notes, &dev.DevicePassword,
		&dev.NRLServerAddr, &dev.NRLServerPort, &dev.NRLSSID, &dev.NRLUDPPort, &disabled, &dev.ExtraJSON,
	)
	if err != nil {
		return Device{}, err
	}
	dev.Category = DeviceCategory(category)
	dev.Online = online == 1
	dev.Disabled = disabled == 1
	dev.FirstSeenAt = parseTime(firstSeenRaw)
	dev.LastSeenAt = parseTime(lastSeenRaw)
	dev.LastCallAt = parseTime(lastCallRaw)
	return dev, nil
}

func scanUser(scan func(dest ...any) error) (User, error) {
	var (
		user         User
		role         string
		enabled      int
		createdAtRaw string
		updatedAtRaw string
		lastLoginRaw string
	)
	err := scan(
		&user.ID, &user.Username, &user.Callsign, &user.Email, &user.PasswordHash, &role, &enabled,
		&createdAtRaw, &updatedAtRaw, &lastLoginRaw,
	)
	if err != nil {
		return User{}, err
	}
	user.Role = UserRole(role)
	user.Enabled = enabled == 1
	user.CreatedAt = parseTime(createdAtRaw)
	user.UpdatedAt = parseTime(updatedAtRaw)
	user.LastLoginAt = parseTime(lastLoginRaw)
	return user, nil
}

func scanCall(scan func(dest ...any) error) (CallRecord, error) {
	var (
		call         CallRecord
		category     string
		createdAtRaw string
		endedAtRaw   string
	)
	err := scan(
		&call.ID, &createdAtRaw, &endedAtRaw, &call.DurationMS, &call.Frontend, &category, &call.SourceKey, &call.SourceName, &call.SourceCallsign,
		&call.SourceDMRID, &call.SrcID, &call.DstID, &call.RepeaterID, &call.Slot, &call.CallType, &call.StreamID,
		&call.FromIP, &call.FromPort,
	)
	if err != nil {
		return CallRecord{}, err
	}
	call.SourceCategory = DeviceCategory(category)
	call.CreatedAt = parseTime(createdAtRaw)
	call.EndedAt = parseTime(endedAtRaw)
	return call, nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func formatOptionalTime(t time.Time) string {
	return formatTime(t)
}

func parseTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
