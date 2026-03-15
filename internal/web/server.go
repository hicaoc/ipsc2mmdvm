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
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm"
	"github.com/hicaoc/ipsc2mmdvm/internal/registry"
	"github.com/hicaoc/ipsc2mmdvm/internal/routing"
)

//go:embed dist/*
var staticFS embed.FS

const sessionCookieName = "ipsc2mmdvm_session"

type Server struct {
	registry            *registry.Service
	router              *routing.SubscriptionManager
	audio               *audio.Hub
	runtime             RuntimeInfo
	deviceChangeHandler func(eventType string, device registry.Device)
	upgrader            websocket.Upgrader
	mux                 *http.ServeMux

	sessionMu sync.RWMutex
	sessions  map[string]sessionState

	wsMu         sync.RWMutex
	wsClients    int
	audioTargets int
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

type wsControlMessage struct {
	Type   string `json:"type"`
	Target string `json:"target"`
}

type deviceUpdatePayload struct {
	Protocol       *string                     `json:"protocol"`
	OwnerUserID    *int64                      `json:"ownerUserId"`
	Name           *string                     `json:"name"`
	Callsign       *string                     `json:"callsign"`
	Notes          *string                     `json:"notes"`
	Disabled       *bool                       `json:"disabled"`
	DMRID          *uint32                     `json:"dmrid"`
	Model          *string                     `json:"model"`
	Description    *string                     `json:"description"`
	Location       *string                     `json:"location"`
	DevicePassword *string                     `json:"devicePassword"`
	NRLServerAddr  *string                     `json:"nrlServerAddr"`
	NRLServerPort  *int                        `json:"nrlServerPort"`
	NRLSSID        *uint8                      `json:"nrlSsid"`
	NRLUDPPort     *int                        `json:"nrlUdpPort"`
	NRLSlot        *int                        `json:"nrlSlot"`
	RXFreq         *uint                       `json:"rxFreq"`
	TXFreq         *uint                       `json:"txFreq"`
	TXPower        *uint8                      `json:"txPower"`
	ColorCode      *uint8                      `json:"colorCode"`
	Latitude       *float64                    `json:"latitude"`
	Longitude      *float64                    `json:"longitude"`
	Height         *uint16                     `json:"height"`
	URL            *string                     `json:"url"`
	Slots          *byte                       `json:"slots"`
	MasterServer   *string                     `json:"mmdvmMasterServer"`
	TGRewrites     *[]config.TGRewriteConfig   `json:"tgRewrites"`
	PCRewrites     *[]config.PCRewriteConfig   `json:"pcRewrites"`
	TypeRewrites   *[]config.TypeRewriteConfig `json:"typeRewrites"`
	SrcRewrites    *[]config.SrcRewriteConfig  `json:"srcRewrites"`
	PassAllPC      *[]int                      `json:"passAllPC"`
	PassAllTG      *[]int                      `json:"passAllTG"`
	StaticSlot1    *[]uint32                   `json:"staticSlot1"`
	StaticSlot2    *[]uint32                   `json:"staticSlot2"`
}

type deviceCreatePayload struct {
	Protocol       string                     `json:"protocol"`
	Name           string                     `json:"name"`
	Callsign       string                     `json:"callsign"`
	DMRID          uint32                     `json:"dmrid"`
	Model          string                     `json:"model"`
	Description    string                     `json:"description"`
	Location       string                     `json:"location"`
	Notes          string                     `json:"notes"`
	NRLServerAddr  string                     `json:"nrlServerAddr"`
	NRLServerPort  int                        `json:"nrlServerPort"`
	NRLSSID        uint8                      `json:"nrlSsid"`
	NRLUDPPort     int                        `json:"nrlUdpPort"`
	NRLSlot        int                        `json:"nrlSlot"`
	DevicePassword string                     `json:"devicePassword"`
	RXFreq         uint                       `json:"rxFreq"`
	TXFreq         uint                       `json:"txFreq"`
	TXPower        uint8                      `json:"txPower"`
	ColorCode      uint8                      `json:"colorCode"`
	Latitude       float64                    `json:"latitude"`
	Longitude      float64                    `json:"longitude"`
	Height         uint16                     `json:"height"`
	URL            string                     `json:"url"`
	Slots          byte                       `json:"slots"`
	MasterServer   string                     `json:"mmdvmMasterServer"`
	TGRewrites     []config.TGRewriteConfig   `json:"tgRewrites"`
	PCRewrites     []config.PCRewriteConfig   `json:"pcRewrites"`
	TypeRewrites   []config.TypeRewriteConfig `json:"typeRewrites"`
	SrcRewrites    []config.SrcRewriteConfig  `json:"srcRewrites"`
	PassAllPC      []int                      `json:"passAllPC"`
	PassAllTG      []int                      `json:"passAllTG"`
	StaticGroups   []uint32                   `json:"staticGroups"`
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

func (s *Server) SetDeviceChangeHandler(handler func(eventType string, device registry.Device)) {
	if s == nil {
		return
	}
	s.deviceChangeHandler = handler
}

func (s *Server) routes() {
	s.mux.HandleFunc("/api/auth/register", s.handleRegister)
	s.mux.HandleFunc("/api/auth/login", s.handleLogin)
	s.mux.HandleFunc("/api/auth/logout", s.withAuth(s.handleLogout))
	s.mux.HandleFunc("/api/auth/me", s.withAuth(s.handleMe))
	s.mux.HandleFunc("/api/snapshot", s.handleSnapshotPublic)
	s.mux.HandleFunc("/api/devices", s.withAdmin(s.handleDeviceCreate))
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
		if s.deviceChangeHandler != nil {
			s.deviceChangeHandler("device_deleted", device)
		}
		writeJSON(w, http.StatusOK, device)
		return
	}
	var payload deviceUpdatePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if device.Protocol == "nrl-virtual" && payload.Disabled != nil {
		slog.Warn("received NRL virtual link disable toggle",
			"sourceKey", device.SourceKey,
			"currentDisabled", device.Disabled,
			"nextDisabled", *payload.Disabled,
			"user", user.Username)
	}
	if device.Protocol == "mmdvm-upstream" {
		cfg, err := buildManagedMMDVMConfig(device, payload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := config.ValidateMMDVMClients([]config.MMDVM{cfg}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		updatedDevice, err := mmdvm.ConfigToDevice(cfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		updatedDevice.ID = device.ID
		updatedDevice.OwnerUserID = device.OwnerUserID
		updatedDevice.Status = device.Status
		updatedDevice.Online = device.Online
		updatedDevice.FirstSeenAt = device.FirstSeenAt
		updatedDevice.LastSeenAt = device.LastSeenAt
		updatedDevice.LastCallAt = device.LastCallAt
		updatedDevice.Disabled = device.Disabled
		if user.Role == registry.RoleAdmin {
			if payload.OwnerUserID != nil {
				updatedDevice.OwnerUserID = *payload.OwnerUserID
			}
		}
		if payload.Disabled != nil {
			updatedDevice.Disabled = *payload.Disabled
		}
		stored, err := s.registry.UpsertDevice(updatedDevice)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, stored)
		return
	}
	if device.Protocol == "nrl-virtual" && payload.Callsign != nil && strings.TrimSpace(*payload.Callsign) == "" {
		payload.Callsign = nil
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
		NRLSlot:        payload.NRLSlot,
	}
	if user.Role == registry.RoleAdmin {
		patch.OwnerUserID = payload.OwnerUserID
		patch.Callsign = normalizeStringPtr(payload.Callsign, true)
		patch.Disabled = payload.Disabled
		patch.DMRID = payload.DMRID
	} else if payload.Callsign != nil || payload.OwnerUserID != nil || payload.DMRID != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	} else {
		patch.Disabled = payload.Disabled
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
		if deviceByID.Protocol == "nrl-virtual" {
			slot := deviceByID.NRLSlot
			if slot != 2 {
				slot = 1
			}
			var groups []uint32
			if slot == 1 && payload.StaticSlot1 != nil {
				groups = *payload.StaticSlot1
			}
			if slot == 2 && payload.StaticSlot2 != nil {
				groups = *payload.StaticSlot2
			}
			if len(groups) > 1 {
				groups = groups[:1]
			}
			if err := s.registry.ReplaceStaticGroups(deviceByID.SourceKey, slot, groups); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := s.registry.ReplaceStaticGroups(deviceByID.SourceKey, 3-slot, nil); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if s.router != nil {
				s.router.ReplaceStatic(deviceByID.SourceKey, routing.Slot(slot), groups)
				s.router.ReplaceStatic(deviceByID.SourceKey, routing.Slot(3-slot), nil)
			}
			if s.deviceChangeHandler != nil {
				s.deviceChangeHandler("device_updated", updated)
			}
			writeJSON(w, http.StatusOK, updated)
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
	if s.deviceChangeHandler != nil {
		s.deviceChangeHandler("device_updated", updated)
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeviceCreate(w http.ResponseWriter, r *http.Request, user registry.User) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload deviceCreatePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if strings.EqualFold(strings.TrimSpace(payload.Protocol), "mmdvm-upstream") {
		token, err := randomToken(6)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cfg := config.MMDVM{
			SourceKey:    "mmdvm-upstream:" + token,
			Name:         strings.TrimSpace(payload.Name),
			Callsign:     strings.TrimSpace(payload.Callsign),
			ID:           payload.DMRID,
			RXFreq:       payload.RXFreq,
			TXFreq:       payload.TXFreq,
			TXPower:      payload.TXPower,
			ColorCode:    payload.ColorCode,
			Latitude:     payload.Latitude,
			Longitude:    payload.Longitude,
			Height:       payload.Height,
			Location:     strings.TrimSpace(payload.Location),
			Description:  strings.TrimSpace(payload.Description),
			URL:          strings.TrimSpace(payload.URL),
			Slots:        payload.Slots,
			MasterServer: strings.TrimSpace(payload.MasterServer),
			Password:     payload.DevicePassword,
			TGRewrites:   payload.TGRewrites,
			PCRewrites:   payload.PCRewrites,
			TypeRewrites: payload.TypeRewrites,
			SrcRewrites:  payload.SrcRewrites,
			PassAllPC:    payload.PassAllPC,
			PassAllTG:    payload.PassAllTG,
		}
		if cfg.Slots == 0 {
			cfg.Slots = 3
		}
		if err := config.ValidateMMDVMClients([]config.MMDVM{cfg}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		dev, err := mmdvm.ConfigToDevice(cfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		stored, err := s.registry.UpsertDevice(dev)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, stored)
		return
	}
	slot := payload.NRLSlot
	if slot != 2 {
		slot = 1
	}
	callsign := strings.ToUpper(strings.TrimSpace(payload.Callsign))
	if callsign == "" {
		callsign = strings.ToUpper(strings.TrimSpace(user.Callsign))
	}
	if callsign == "" {
		http.Error(w, "callsign is required", http.StatusBadRequest)
		return
	}
	if len(payload.StaticGroups) > 1 {
		payload.StaticGroups = payload.StaticGroups[:1]
	}
	name := strings.TrimSpace(payload.Name)
	if name == "" {
		name = "NRL Virtual Link"
	}
	token, err := randomToken(6)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dev, err := s.registry.UpsertDevice(registry.Device{
		Category:      registry.CategoryNRL,
		Protocol:      "nrl-virtual",
		SourceKey:     "nrl-virtual:" + token,
		Name:          name,
		Callsign:      callsign,
		DMRID:         payload.DMRID,
		Model:         strings.TrimSpace(payload.Model),
		Description:   strings.TrimSpace(payload.Description),
		Location:      strings.TrimSpace(payload.Location),
		Notes:         payload.Notes,
		NRLServerAddr: strings.TrimSpace(payload.NRLServerAddr),
		NRLServerPort: payload.NRLServerPort,
		NRLSSID:       payload.NRLSSID,
		NRLUDPPort:    payload.NRLUDPPort,
		NRLSlot:       slot,
		Slots:         1,
		Status:        "configured",
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.registry.ReplaceStaticGroups(dev.SourceKey, slot, payload.StaticGroups); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.router != nil {
		s.router.ReplaceStatic(dev.SourceKey, routing.Slot(slot), payload.StaticGroups)
		if slot == 1 {
			s.router.ReplaceStatic(dev.SourceKey, routing.Slot2, nil)
		} else {
			s.router.ReplaceStatic(dev.SourceKey, routing.Slot1, nil)
		}
	}
	if s.deviceChangeHandler != nil {
		s.deviceChangeHandler("device_created", dev)
	}
	writeJSON(w, http.StatusCreated, dev)
}

func buildManagedMMDVMConfig(current registry.Device, payload deviceUpdatePayload) (config.MMDVM, error) {
	cfg, err := mmdvm.DeviceToConfig(current)
	if err != nil {
		return config.MMDVM{}, err
	}
	if payload.Name != nil {
		cfg.Name = strings.TrimSpace(*payload.Name)
	}
	if payload.Callsign != nil {
		cfg.Callsign = strings.TrimSpace(*payload.Callsign)
	}
	if payload.DMRID != nil {
		cfg.ID = *payload.DMRID
	}
	if payload.Description != nil {
		cfg.Description = strings.TrimSpace(*payload.Description)
	}
	if payload.Location != nil {
		cfg.Location = strings.TrimSpace(*payload.Location)
	}
	if payload.DevicePassword != nil {
		cfg.Password = *payload.DevicePassword
	}
	if payload.RXFreq != nil {
		cfg.RXFreq = *payload.RXFreq
	}
	if payload.TXFreq != nil {
		cfg.TXFreq = *payload.TXFreq
	}
	if payload.TXPower != nil {
		cfg.TXPower = *payload.TXPower
	}
	if payload.ColorCode != nil {
		cfg.ColorCode = *payload.ColorCode
	}
	if payload.Latitude != nil {
		cfg.Latitude = *payload.Latitude
	}
	if payload.Longitude != nil {
		cfg.Longitude = *payload.Longitude
	}
	if payload.Height != nil {
		cfg.Height = *payload.Height
	}
	if payload.URL != nil {
		cfg.URL = strings.TrimSpace(*payload.URL)
	}
	if payload.Slots != nil {
		cfg.Slots = *payload.Slots
	}
	if payload.MasterServer != nil {
		cfg.MasterServer = strings.TrimSpace(*payload.MasterServer)
	}
	if payload.TGRewrites != nil {
		cfg.TGRewrites = append([]config.TGRewriteConfig(nil), (*payload.TGRewrites)...)
	}
	if payload.PCRewrites != nil {
		cfg.PCRewrites = append([]config.PCRewriteConfig(nil), (*payload.PCRewrites)...)
	}
	if payload.TypeRewrites != nil {
		cfg.TypeRewrites = append([]config.TypeRewriteConfig(nil), (*payload.TypeRewrites)...)
	}
	if payload.SrcRewrites != nil {
		cfg.SrcRewrites = append([]config.SrcRewriteConfig(nil), (*payload.SrcRewrites)...)
	}
	if payload.PassAllPC != nil {
		cfg.PassAllPC = append([]int(nil), (*payload.PassAllPC)...)
	}
	if payload.PassAllTG != nil {
		cfg.PassAllTG = append([]int(nil), (*payload.PassAllTG)...)
	}
	if cfg.Slots == 0 {
		cfg.Slots = 3
	}
	return cfg, nil
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
	conn.EnableWriteCompression(true)

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
	audioTargets := map[string]struct{}{}
	audioMixer := newWSAudioMixer()
	audioTicker := time.NewTicker(mixedAudioFrameDuration)
	defer audioTicker.Stop()
	defer func() {
		if unsubscribeAudio != nil {
			unsubscribeAudio()
			s.trackAudioTargets(-len(audioTargets))
		}
	}()

	done := make(chan struct{})
	control := make(chan wsControlMessage, 8)
	go func() {
		defer close(done)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			msg := parseWSControlMessage(data)
			if msg.Type == "" {
				continue
			}
			select {
			case control <- msg:
			case <-done:
				return
			}
		}
	}()

	for {
		select {
		case msg := <-control:
			switch msg.Type {
			case "audio_subscribe":
				target := strings.TrimSpace(msg.Target)
				if target == "" {
					continue
				}
				if _, ok := audioTargets[target]; ok {
					continue
				}
				audioTargets[target] = struct{}{}
				if s.audio != nil && audioEvents == nil {
					audioEvents, unsubscribeAudio = s.audio.Subscribe()
				}
				s.trackAudioTargets(1)
			case "audio_unsubscribe":
				target := strings.TrimSpace(msg.Target)
				if target == "" {
					s.trackAudioTargets(-len(audioTargets))
					audioTargets = map[string]struct{}{}
					audioMixer.Reset()
				} else {
					if _, ok := audioTargets[target]; ok {
						delete(audioTargets, target)
						s.trackAudioTargets(-1)
						audioMixer.RemoveTarget(target)
					}
				}
				if len(audioTargets) == 0 && unsubscribeAudio != nil {
					unsubscribeAudio()
					unsubscribeAudio = nil
					audioEvents = nil
					audioMixer.Reset()
				}
			}
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
			target := audioTargetKeyForChunk(chunk)
			if target == "" {
				continue
			}
			if _, ok := audioTargets[target]; !ok {
				continue
			}
			audioMixer.Add(chunk, target)
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
		case <-audioTicker.C:
			if len(audioTargets) == 0 {
				continue
			}
			frame := audioMixer.Flush(time.Now().UTC())
			if len(frame) == 0 {
				continue
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

func parseWSControlMessage(data []byte) wsControlMessage {
	raw := strings.TrimSpace(string(data))
	switch raw {
	case "audio_subscribe":
		return wsControlMessage{Type: "audio_subscribe"}
	case "audio_unsubscribe":
		return wsControlMessage{Type: "audio_unsubscribe"}
	}

	var msg wsControlMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return wsControlMessage{}
	}
	msg.Type = strings.TrimSpace(msg.Type)
	msg.Target = strings.TrimSpace(msg.Target)
	return msg
}

func audioTargetKeyForChunk(chunk audio.Chunk) string {
	switch chunk.CallType {
	case "analog":
		if strings.TrimSpace(chunk.SourceKey) == "" {
			return ""
		}
		return "analog:" + strings.TrimSpace(chunk.SourceKey)
	case "private":
		if chunk.DstID == 0 {
			return ""
		}
		return "private:" + strconv.FormatUint(uint64(chunk.DstID), 10) + ":" + strconv.Itoa(chunk.Slot)
	default:
		if chunk.DstID == 0 {
			return ""
		}
		return "group:" + strconv.FormatUint(uint64(chunk.DstID), 10) + ":" + strconv.Itoa(chunk.Slot)
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

func (s *Server) trackAudioTargets(delta int) {
	s.wsMu.Lock()
	s.audioTargets += delta
	if s.audioTargets < 0 {
		s.audioTargets = 0
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
	runtime.AudioSubscribers = s.audioTargets
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
