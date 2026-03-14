package web

import (
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hicaoc/ipsc2mmdvm/internal/audio"
	"github.com/hicaoc/ipsc2mmdvm/internal/config"
	"github.com/hicaoc/ipsc2mmdvm/internal/registry"
	"github.com/hicaoc/ipsc2mmdvm/internal/routing"
)

//go:embed dist/*
var staticFS embed.FS

const sessionCookieName = "ipsc2mmdvm_session"

type Server struct {
	registry *registry.Service
	router   *routing.SubscriptionManager
	audio    *audio.Hub
	runtime  RuntimeInfo
	upgrader websocket.Upgrader
	mux      *http.ServeMux

	sessionMu sync.RWMutex
	sessions  map[string]sessionState

	wsMu         sync.RWMutex
	wsClients    int
	audioClients int
	runtimeSubs  map[chan RuntimeInfo]struct{}
}

type sessionState struct {
	UserID    int64
	CreatedAt time.Time
}

type RuntimeInfo struct {
	AccessURL          string   `json:"accessUrl"`
	WebsocketURL       string   `json:"websocketUrl"`
	WebListenAddress   string   `json:"webListenAddress"`
	BrowserClients     int      `json:"browserClients"`
	AudioSubscribers   int      `json:"audioSubscribers"`
	LocalID            uint32   `json:"localId"`
	LocalCallsign      string   `json:"localCallsign"`
	IPSCListen         string   `json:"ipscListen"`
	HyteraP2PListen    string   `json:"hyteraP2pListen"`
	HyteraDMRListen    string   `json:"hyteraDmrListen"`
	HyteraRDACListen   string   `json:"hyteraRdacListen"`
	MMDVMServerListen  []string `json:"mmdvmServerListen"`
	MMDVMClientMasters []string `json:"mmdvmClientMasters"`
	StoragePath        string   `json:"storagePath"`
}

type snapshotPayload struct {
	Devices   []registry.Device                      `json:"devices"`
	Calls     []registry.CallRecord                  `json:"calls"`
	CallTotal int64                                  `json:"callTotal"`
	Groups    map[string]routing.DeviceSubscriptions `json:"groups"`
	Runtime   RuntimeInfo                            `json:"runtime"`
}

func NewServer(reg *registry.Service, router *routing.SubscriptionManager, audioHub *audio.Hub, cfg *config.Config) *Server {
	runtime := RuntimeInfo{
		WebListenAddress: cfg.Web.Address,
		LocalID:          cfg.Local.ID,
		LocalCallsign:    cfg.Local.Callsign,
		StoragePath:      cfg.Storage.Path,
	}
	if cfg.IPSC.Enabled {
		runtime.IPSCListen = displayListenAddress(cfg.DisplayAddressIP(), cfg.IPSC.Port)
	}
	if cfg.Hytera.Enabled {
		runtime.HyteraP2PListen = displayListenAddress(cfg.DisplayAddressIP(), cfg.Hytera.P2PPort)
		runtime.HyteraDMRListen = displayListenAddress(cfg.DisplayAddressIP(), cfg.Hytera.DMRPort)
		if cfg.Hytera.EnableRDAC {
			runtime.HyteraRDACListen = displayListenAddress(cfg.DisplayAddressIP(), cfg.Hytera.RDACPort)
		}
	}
	for _, server := range cfg.MMDVMServers {
		runtime.MMDVMServerListen = append(runtime.MMDVMServerListen, server.Listen)
	}
	for _, client := range cfg.MMDVMClients {
		if client.MasterServer != "" {
			runtime.MMDVMClientMasters = append(runtime.MMDVMClientMasters, client.MasterServer)
		}
	}
	s := &Server{
		registry: reg,
		router:   router,
		audio:    audioHub,
		runtime:  runtime,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
		mux:         http.NewServeMux(),
		sessions:    map[string]sessionState{},
		runtimeSubs: map[chan RuntimeInfo]struct{}{},
	}
	s.routes()
	return s
}

func displayListenAddress(ip string, port uint16) string {
	if strings.TrimSpace(ip) == "" {
		return ":" + strconv.FormatUint(uint64(port), 10)
	}
	return strings.TrimSpace(ip) + ":" + strconv.FormatUint(uint64(port), 10)
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("/api/auth/register", s.handleRegister)
	s.mux.HandleFunc("/api/auth/login", s.handleLogin)
	s.mux.HandleFunc("/api/auth/logout", s.withAuth(s.handleLogout))
	s.mux.HandleFunc("/api/auth/me", s.withAuth(s.handleMe))
	s.mux.HandleFunc("/api/snapshot", s.handleSnapshotPublic)
	s.mux.HandleFunc("/api/devices/", s.withAuth(s.handleDeviceUpdate))
	s.mux.HandleFunc("/api/users", s.withAdmin(s.handleUsers))
	s.mux.HandleFunc("/api/users/", s.withAdmin(s.handleUserByID))
	s.mux.HandleFunc("/ws", s.handleWS)
	s.mux.HandleFunc("/", s.handleApp)
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws" {
		http.NotFound(w, r)
		return
	}
	assetsFS, root, err := s.uiFS()
	if err != nil {
		http.Error(w, "ui not found", http.StatusInternalServerError)
		return
	}
	path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/"), root+"/")
	if path == "" || r.URL.Path == "/" {
		path = "index.html"
	}
	if path != "index.html" {
		if _, err := fs.Stat(assetsFS, path); err == nil {
			http.FileServer(http.FS(assetsFS)).ServeHTTP(w, r)
			return
		}
	}
	data, err := fs.ReadFile(assetsFS, "index.html")
	if err != nil {
		http.Error(w, "index not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		Username string `json:"username"`
		Callsign string `json:"callsign"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if err := validateRegistration(payload.Username, payload.Callsign, payload.Email, payload.Password); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hash, err := HashPassword(payload.Password)
	if err != nil {
		http.Error(w, "failed to hash password", http.StatusInternalServerError)
		return
	}
	user, err := s.registry.CreateUser(registry.User{
		Username:     strings.TrimSpace(payload.Username),
		Callsign:     strings.ToUpper(strings.TrimSpace(payload.Callsign)),
		Email:        strings.TrimSpace(payload.Email),
		PasswordHash: hash,
		Role:         registry.RoleHAM,
		Enabled:      false,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, sanitizeUser(user))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	loginID := strings.TrimSpace(payload.Username)
	user, err := s.registry.UserByUsername(loginID)
	if errors.Is(err, sql.ErrNoRows) {
		user, err = s.registry.UserByCallsign(loginID)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !user.Enabled {
		http.Error(w, "account disabled", http.StatusForbidden)
		return
	}
	if err := CheckPassword(user.PasswordHash, payload.Password); err != nil {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	token, err := randomToken(32)
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	s.sessionMu.Lock()
	s.sessions[token] = sessionState{UserID: user.ID, CreatedAt: now}
	s.sessionMu.Unlock()
	_ = s.registry.UpdateUserLastLogin(user.ID, now)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, sanitizeUser(user))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, _ registry.User) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.sessionMu.Lock()
		delete(s.sessions, cookie.Value)
		s.sessionMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, user registry.User) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeUser(user))
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request, user registry.User) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshotPayload(requestBaseURL(r), user))
}

func (s *Server) handleSnapshotPublic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, err := s.currentUser(r)
	if err == nil {
		writeJSON(w, http.StatusOK, s.snapshotPayload(requestBaseURL(r), user))
		return
	}
	writeJSON(w, http.StatusOK, s.publicSnapshotPayload(requestBaseURL(r)))
}

func (s *Server) handleDeviceUpdate(w http.ResponseWriter, r *http.Request, user registry.User) {
	if r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idPart := strings.TrimPrefix(r.URL.Path, "/api/devices/")
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}
	device, err := s.registry.DeviceByID(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !canManageDevice(user, device) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodDelete {
		if user.Role != registry.RoleAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		device, err := s.registry.DeleteDevice(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if s.router != nil {
			s.router.RemoveDevice(device.SourceKey)
		}
		writeJSON(w, http.StatusOK, device)
		return
	}
	var payload struct {
		OwnerUserID    *int64    `json:"ownerUserId"`
		Name           *string   `json:"name"`
		Callsign       *string   `json:"callsign"`
		Notes          *string   `json:"notes"`
		Disabled       *bool     `json:"disabled"`
		DMRID          *uint32   `json:"dmrid"`
		Model          *string   `json:"model"`
		Description    *string   `json:"description"`
		Location       *string   `json:"location"`
		DevicePassword *string   `json:"devicePassword"`
		NRLServerAddr  *string   `json:"nrlServerAddr"`
		NRLServerPort  *int      `json:"nrlServerPort"`
		NRLSSID        *uint8    `json:"nrlSsid"`
		NRLUDPPort     *int      `json:"nrlUdpPort"`
		StaticSlot1    *[]uint32 `json:"staticSlot1"`
		StaticSlot2    *[]uint32 `json:"staticSlot2"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	patch := registry.DevicePatch{
		Name:           payload.Name,
		Notes:          payload.Notes,
		Model:          payload.Model,
		Description:    payload.Description,
		Location:       payload.Location,
		DevicePassword: payload.DevicePassword,
		NRLServerAddr:  payload.NRLServerAddr,
		NRLServerPort:  payload.NRLServerPort,
		NRLSSID:        payload.NRLSSID,
		NRLUDPPort:     payload.NRLUDPPort,
	}
	if user.Role == registry.RoleAdmin {
		patch.OwnerUserID = payload.OwnerUserID
		patch.Callsign = normalizeStringPtr(payload.Callsign, true)
		patch.Disabled = payload.Disabled
		patch.DMRID = payload.DMRID
	} else if payload.Callsign != nil || payload.OwnerUserID != nil || payload.Disabled != nil || payload.DMRID != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	updated, err := s.registry.UpdateDeviceMetadata(id, patch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if payload.StaticSlot1 != nil || payload.StaticSlot2 != nil {
		deviceByID, err := s.registry.DeviceByID(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if payload.StaticSlot1 != nil {
			if err := s.registry.ReplaceStaticGroups(deviceByID.SourceKey, 1, *payload.StaticSlot1); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if s.router != nil {
				s.router.ReplaceStatic(deviceByID.SourceKey, routing.Slot1, *payload.StaticSlot1)
			}
		}
		if payload.StaticSlot2 != nil {
			if err := s.registry.ReplaceStaticGroups(deviceByID.SourceKey, 2, *payload.StaticSlot2); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if s.router != nil {
				s.router.ReplaceStatic(deviceByID.SourceKey, routing.Slot2, *payload.StaticSlot2)
			}
		}
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, _ registry.User) {
	switch r.Method {
	case http.MethodGet:
		users, err := s.registry.ListUsers()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sanitized := make([]registry.User, 0, len(users))
		for _, user := range users {
			sanitized = append(sanitized, sanitizeUser(user))
		}
		writeJSON(w, http.StatusOK, sanitized)
	case http.MethodPost:
		var payload struct {
			Username string            `json:"username"`
			Callsign string            `json:"callsign"`
			Email    string            `json:"email"`
			Password string            `json:"password"`
			Role     registry.UserRole `json:"role"`
			Enabled  bool              `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		if err := validateRegistration(payload.Username, payload.Callsign, payload.Email, payload.Password); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if payload.Role != registry.RoleAdmin {
			payload.Role = registry.RoleHAM
		}
		hash, err := HashPassword(payload.Password)
		if err != nil {
			http.Error(w, "failed to hash password", http.StatusInternalServerError)
			return
		}
		user, err := s.registry.CreateUser(registry.User{
			Username:     strings.TrimSpace(payload.Username),
			Callsign:     strings.ToUpper(strings.TrimSpace(payload.Callsign)),
			Email:        strings.TrimSpace(payload.Email),
			PasswordHash: hash,
			Role:         payload.Role,
			Enabled:      payload.Enabled,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, sanitizeUser(user))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleUserByID(w http.ResponseWriter, r *http.Request, _ registry.User) {
	path := strings.TrimPrefix(r.URL.Path, "/api/users/")
	if strings.HasSuffix(path, "/reset-password") {
		idPart := strings.TrimSuffix(path, "/reset-password")
		id, err := strconv.ParseInt(idPart, 10, 64)
		if err != nil {
			http.Error(w, "invalid user id", http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		if len(payload.Password) < 8 {
			http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
			return
		}
		user, err := s.registry.UserByID(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		user.PasswordHash, err = HashPassword(payload.Password)
		if err != nil {
			http.Error(w, "failed to hash password", http.StatusInternalServerError)
			return
		}
		user, err = s.registry.UpdateUser(user)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, sanitizeUser(user))
		return
	}
	id, err := strconv.ParseInt(path, 10, 64)
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var payload struct {
			Username *string            `json:"username"`
			Callsign *string            `json:"callsign"`
			Email    *string            `json:"email"`
			Role     *registry.UserRole `json:"role"`
			Enabled  *bool              `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		user, err := s.registry.UserByID(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if payload.Username != nil {
			user.Username = strings.TrimSpace(*payload.Username)
		}
		if payload.Callsign != nil {
			user.Callsign = strings.ToUpper(strings.TrimSpace(*payload.Callsign))
		}
		if payload.Email != nil {
			user.Email = strings.TrimSpace(*payload.Email)
		}
		if payload.Role != nil {
			if *payload.Role == registry.RoleAdmin {
				user.Role = registry.RoleAdmin
			} else {
				user.Role = registry.RoleHAM
			}
		}
		if payload.Enabled != nil {
			user.Enabled = *payload.Enabled
		}
		if err := validateUserProfile(user.Username, user.Callsign, user.Email); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		user, err = s.registry.UpdateUser(user)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, sanitizeUser(user))
	case http.MethodDelete:
		if err := s.registry.DeleteUser(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("websocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	s.trackWSClient(1)
	defer s.trackWSClient(-1)

	user, err := s.currentUser(r)
	authenticated := err == nil
	initial := map[string]any{"type": "snapshot"}
	if authenticated {
		initial["snapshot"] = s.snapshotPayload(requestBaseURL(r), user)
	} else {
		initial["snapshot"] = s.publicSnapshotPayload(requestBaseURL(r))
	}

	if err := conn.WriteJSON(initial); err != nil {
		return
	}

	events, unsubscribe := s.registry.Subscribe()
	defer unsubscribe()
	var (
		audioEvents      <-chan audio.Chunk
		unsubscribeAudio func()
		runtimeEvents    <-chan RuntimeInfo
		unsubscribeRT    func()
	)
	runtimeEvents, unsubscribeRT = s.subscribeRuntime()
	defer unsubscribeRT()
	defer func() {
		if unsubscribeAudio != nil {
			unsubscribeAudio()
			s.trackAudioClient(-1)
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch strings.TrimSpace(string(data)) {
			case "audio_subscribe":
				if s.audio != nil && audioEvents == nil {
					audioEvents, unsubscribeAudio = s.audio.Subscribe()
					s.trackAudioClient(1)
				}
			case "audio_unsubscribe":
				if unsubscribeAudio != nil {
					unsubscribeAudio()
					unsubscribeAudio = nil
					audioEvents = nil
					s.trackAudioClient(-1)
				}
			}
		}
	}()

	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := conn.WriteJSON(event); err != nil {
				return
			}
		case chunk, ok := <-audioEvents:
			if audioEvents == nil {
				continue
			}
			if !ok {
				return
			}
			if err := conn.WriteJSON(chunk); err != nil {
				return
			}
		case runtime, ok := <-runtimeEvents:
			if !ok {
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"type":    "runtime_updated",
				"runtime": runtime,
			}); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

func (s *Server) subscribeRuntime() (<-chan RuntimeInfo, func()) {
	ch := make(chan RuntimeInfo, 8)
	s.wsMu.Lock()
	s.runtimeSubs[ch] = struct{}{}
	s.wsMu.Unlock()
	return ch, func() {
		s.wsMu.Lock()
		if _, ok := s.runtimeSubs[ch]; ok {
			delete(s.runtimeSubs, ch)
			close(ch)
		}
		s.wsMu.Unlock()
	}
}

func (s *Server) trackWSClient(delta int) {
	s.wsMu.Lock()
	s.wsClients += delta
	if s.wsClients < 0 {
		s.wsClients = 0
	}
	runtime := s.runtimeSnapshotLocked()
	subs := make([]chan RuntimeInfo, 0, len(s.runtimeSubs))
	for ch := range s.runtimeSubs {
		subs = append(subs, ch)
	}
	s.wsMu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- runtime:
		default:
		}
	}
}

func (s *Server) trackAudioClient(delta int) {
	s.wsMu.Lock()
	s.audioClients += delta
	if s.audioClients < 0 {
		s.audioClients = 0
	}
	runtime := s.runtimeSnapshotLocked()
	subs := make([]chan RuntimeInfo, 0, len(s.runtimeSubs))
	for ch := range s.runtimeSubs {
		subs = append(subs, ch)
	}
	s.wsMu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- runtime:
		default:
		}
	}
}

func (s *Server) runtimeSnapshot() RuntimeInfo {
	s.wsMu.RLock()
	defer s.wsMu.RUnlock()
	return s.runtimeSnapshotLocked()
}

func (s *Server) runtimeSnapshotLocked() RuntimeInfo {
	runtime := s.runtime
	runtime.BrowserClients = s.wsClients
	runtime.AudioSubscribers = s.audioClients
	return runtime
}

func (s *Server) snapshotPayload(baseURL string, user registry.User) snapshotPayload {
	snap := s.registry.Snapshot()
	runtime := s.runtimeSnapshot()
	if baseURL != "" {
		runtime.AccessURL = baseURL
		if strings.HasPrefix(baseURL, "https://") {
			runtime.WebsocketURL = "wss://" + strings.TrimPrefix(baseURL, "https://")
		} else if strings.HasPrefix(baseURL, "http://") {
			runtime.WebsocketURL = "ws://" + strings.TrimPrefix(baseURL, "http://")
		}
	}
	if user.Role == registry.RoleAdmin {
		return snapshotPayload{
			Devices:   snap.Devices,
			Calls:     snap.Calls,
			CallTotal: snap.CallTotal,
			Groups:    s.groupSnapshot(0),
			Runtime:   runtime,
		}
	}
	return snapshotPayload{
		Devices:   snap.Devices,
		Calls:     snap.Calls,
		CallTotal: snap.CallTotal,
		Groups:    s.groupSnapshot(0),
		Runtime:   runtime,
	}
}

func (s *Server) publicSnapshotPayload(baseURL string) snapshotPayload {
	snap := s.registry.Snapshot()
	runtime := s.runtimeSnapshot()
	if baseURL != "" {
		runtime.AccessURL = baseURL
		if strings.HasPrefix(baseURL, "https://") {
			runtime.WebsocketURL = "wss://" + strings.TrimPrefix(baseURL, "https://")
		} else if strings.HasPrefix(baseURL, "http://") {
			runtime.WebsocketURL = "ws://" + strings.TrimPrefix(baseURL, "http://")
		}
	}
	return snapshotPayload{
		Devices:   snap.Devices,
		Calls:     snap.Calls,
		CallTotal: snap.CallTotal,
		Groups:    s.groupSnapshot(0),
		Runtime:   runtime,
	}
}

func (s *Server) groupSnapshot(ownerUserID int64) map[string]routing.DeviceSubscriptions {
	if s.router == nil {
		return map[string]routing.DeviceSubscriptions{}
	}
	all := s.router.SnapshotAll(timeNowUTC())
	if ownerUserID == 0 {
		return all
	}
	filtered := make(map[string]routing.DeviceSubscriptions)
	for sourceKey, sub := range all {
		dev, ok := s.registry.FindDevice(sourceKey)
		if ok && dev.OwnerUserID == ownerUserID {
			filtered[sourceKey] = sub
		}
	}
	return filtered
}

func (s *Server) withAuth(next func(http.ResponseWriter, *http.Request, registry.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := s.currentUser(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r, user)
	}
}

func (s *Server) withAdmin(next func(http.ResponseWriter, *http.Request, registry.User)) http.HandlerFunc {
	return s.withAuth(func(w http.ResponseWriter, r *http.Request, user registry.User) {
		if user.Role != registry.RoleAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r, user)
	})
}

func (s *Server) currentUser(r *http.Request) (registry.User, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return registry.User{}, err
	}
	s.sessionMu.RLock()
	session, ok := s.sessions[cookie.Value]
	s.sessionMu.RUnlock()
	if !ok {
		return registry.User{}, errors.New("session not found")
	}
	user, err := s.registry.UserByID(session.UserID)
	if err != nil {
		return registry.User{}, err
	}
	if !user.Enabled {
		return registry.User{}, errors.New("user disabled")
	}
	return user, nil
}

func canManageDevice(user registry.User, device registry.Device) bool {
	if user.Role == registry.RoleAdmin {
		return true
	}
	if device.OwnerUserID != 0 && device.OwnerUserID == user.ID {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(device.Callsign), strings.TrimSpace(user.Callsign))
}

func (s *Server) uiFS() (fs.FS, string, error) {
	sub, err := fs.Sub(staticFS, "dist")
	return sub, "dist", err
}

func sanitizeUser(user registry.User) registry.User {
	user.PasswordHash = ""
	return user
}

func normalizeStringPtr(value *string, upper bool) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if upper {
		trimmed = strings.ToUpper(trimmed)
	}
	return &trimmed
}

func randomToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

var timeNowUTC = func() time.Time {
	return time.Now().UTC()
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	} else if r.TLS != nil {
		scheme = "https"
	}
	if r.Host == "" {
		return ""
	}
	return scheme + "://" + r.Host
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
