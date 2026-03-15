package config

import (
	"errors"
	"net"
	"regexp"
	"strings"
)

type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

type Config struct {
	LogLevel     LogLevel `name:"log-level" description:"Logging level for the application. One of debug, info, warn, or error" default:"info"`
	DisplayIP    string   `name:"display-ip" description:"Shared display IP for IPSC/Hytera runtime info in web UI"`
	Metrics      Metrics  `name:"metrics" description:"Configuration for Prometheus metrics"`
	DMRIDDB      DMRIDDB  `name:"dmrid-db" description:"Optional local DMR ID to callsign database loaded at startup"`
	Storage      Storage  `name:"storage" description:"Persistent storage configuration"`
	Web          Web      `name:"web" description:"Web management UI configuration"`
	Local        Local    `name:"local" description:"Local bridge identity used when forwarding between protocols"`
	MMDVMClients []MMDVM  `name:"mmdvm-client" description:"Configuration for outbound MMDVM client connections (multiple DMR masters)"`
	MMDVMServers []MMDVM  `name:"mmdvm-server" description:"Configuration for inbound MMDVM servers that accept hotspot/client connections"`
	IPSC         IPSC     `name:"ipsc" description:"Configuration for the IPSC server"`
	Hytera       Hytera   `name:"hytera" description:"Configuration for the Hytera frontend"`
}

type DMRIDDB struct {
	Path string `name:"path" description:"Path to the DMR ID CSV/TXT file used to resolve callsigns" default:""`
}

type Metrics struct {
	Enabled bool   `name:"enabled" description:"Whether to enable Prometheus metrics endpoint"`
	Address string `name:"address" description:"Address to serve Prometheus metrics on" default:":9100"`
}

type Storage struct {
	Path string `name:"path" description:"Path to the SQLite database file" default:"ipsc2mmdvm.db"`
}

type Web struct {
	Enabled        bool              `name:"enabled" description:"Whether to enable the web management UI" default:"true"`
	Address        string            `name:"address" description:"Address to serve the web management UI on" default:":9201"`
	SessionSecret  string            `name:"session-secret" description:"Secret used to sign web sessions"`
	BootstrapAdmin WebBootstrapAdmin `name:"bootstrap-admin" description:"Optional initial administrator account"`
}

type WebBootstrapAdmin struct {
	Username string `name:"username" description:"Initial administrator username"`
	Callsign string `name:"callsign" description:"Initial administrator callsign"`
	Email    string `name:"email" description:"Initial administrator email"`
	Password string `name:"password" description:"Initial administrator password"`
}

type Local struct {
	ID        uint32 `name:"id" description:"Local bridge DMR ID used when synthesizing packets without an upstream MMDVM identity" default:"9000000"`
	Callsign  string `name:"callsign" description:"Local bridge callsign or display name" default:"IPSC2MMDVM"`
	ColorCode uint8  `name:"color-code" description:"Default color code for synthesized local bridge traffic" default:"1"`
}

// IPSC listens for repeater UDP packets on IP:port.
type IPSC struct {
	Enabled    bool     `name:"enabled" description:"Enable Motorola IPSC frontend" default:"true"`
	Interface  string   `name:"interface" description:"Deprecated: no longer used"`
	Port       uint16   `name:"port" description:"Port to listen for moto IPSC packets on"`
	IP         string   `name:"ip" description:"Optional display IP shown in web runtime info"`
	SubnetMask int      `name:"subnet-mask" description:"Deprecated: no longer used" default:"24"`
	Auth       IPSCAuth `name:"auth" description:"Authentication configuration for moto IPSC packets"`
}

type Hytera struct {
	Enabled    bool   `name:"enabled" description:"Enable Hytera frontend" default:"true"`
	P2PPort    uint16 `name:"p2p-port" description:"Hytera P2P registration/control port" default:"50001"`
	DMRPort    uint16 `name:"dmr-port" description:"Hytera DMR traffic port" default:"30001"`
	RDACPort   uint16 `name:"rdac-port" description:"Hytera RDAC traffic port" default:"30002"`
	EnableRDAC bool   `name:"enable-rdac" description:"Enable Hytera RDAC listener" default:"false"`
}

type IPSCAuth struct {
	Enabled bool   `name:"enabled" description:"Whether to require authentication for IPSC clients"`
	Key     string `name:"key" description:"Authentication key for IPSC clients. Required if auth is enabled"`
}

type MMDVM struct {
	// SourceKey is a stable runtime identity for DB-managed networks.
	// It is not read from config files.
	SourceKey string `json:"-"`
	Name      string `name:"name" description:"Name for this MMDVM network (used in logging)"`
	Callsign  string `name:"callsign" description:"Callsign to use for the MMDVM connection"`
	ID        uint32 `name:"radio-id" description:"Radio ID for the MMDVM connection"`
	// RXFreq is in Hz
	RXFreq uint `name:"rx-freq" description:"Receive frequency in Hz for the MMDVM connection"`
	// TXFreq is in Hz
	TXFreq uint `name:"tx-freq" description:"Transmit frequency in Hz for the MMDVM connection"`
	// TXPower is in dBm
	TXPower uint8 `name:"tx-power" description:"Transmit power in dBm for the MMDVM connection"`
	// ColorCode is the DMR color code
	ColorCode uint8 `name:"color-code" description:"DMR color code for the MMDVM connection"`
	// Latitude with north as positive [-90,+90]
	Latitude float64 `name:"latitude" description:"Latitude with north as positive [-90,+90] for the MMDVM connection"`
	// Longitude with east as positive [-180+,180]
	Longitude float64 `name:"longitude" description:"Longitude with east as positive [-180+,180] for the MMDVM connection"`
	// Height in meters
	Height       uint16 `name:"height" description:"Height in meters for the MMDVM connection"`
	Location     string `name:"location" description:"Location for the MMDVM connection"`
	Description  string `name:"description" description:"Description for the MMDVM connection"`
	URL          string `name:"url" description:"URL for the MMDVM connection"`
	Slots        byte   `name:"slots" description:"Active timeslots bitmask (1=TS1, 2=TS2, 3=both)" default:"3"`
	MasterServer string `name:"master-server" description:"Master server for the MMDVM connection"`
	Listen       string `name:"listen" description:"UDP listen address for server mode" default:":62031"`
	Password     string `name:"password" description:"Password for the MMDVM connection"`

	// Rewrite rules for routing DMR data to/from this network.
	TGRewrites   []TGRewriteConfig   `name:"tg-rewrite" description:"Talkgroup rewrite rules"`
	PCRewrites   []PCRewriteConfig   `name:"pc-rewrite" description:"Private call rewrite rules"`
	TypeRewrites []TypeRewriteConfig `name:"type-rewrite" description:"Type rewrite rules (group TG to private call)"`
	SrcRewrites  []SrcRewriteConfig  `name:"src-rewrite" description:"Source rewrite rules (private call by source to group TG)"`

	// PassAll rules allow all traffic of a given type on a slot without rewriting.
	PassAllPC []int `name:"pass-all-pc" description:"Timeslots on which all private calls pass through unchanged (e.g. [1, 2])"`
	PassAllTG []int `name:"pass-all-tg" description:"Timeslots on which all group calls pass through unchanged (e.g. [1, 2])"`
}

// TGRewriteConfig maps group TG calls from one slot/TG to another.
// Modeled after DMRGateway's TGRewrite: fromSlot, fromTG, toSlot, toTG, range.
type TGRewriteConfig struct {
	FromSlot uint `name:"from-slot" description:"Source timeslot (1 or 2)"`
	FromTG   uint `name:"from-tg" description:"Source talkgroup start"`
	ToSlot   uint `name:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToTG     uint `name:"to-tg" description:"Destination talkgroup start"`
	Range    uint `name:"range" description:"Number of contiguous TGs to map" default:"1"`
}

// PCRewriteConfig maps private calls from one slot/ID to another.
// Modeled after DMRGateway's PCRewrite: fromSlot, fromId, toSlot, toId, range.
type PCRewriteConfig struct {
	FromSlot uint `name:"from-slot" description:"Source timeslot (1 or 2)"`
	FromID   uint `name:"from-id" description:"Source private call ID start"`
	ToSlot   uint `name:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToID     uint `name:"to-id" description:"Destination private call ID start"`
	Range    uint `name:"range" description:"Number of contiguous IDs to map" default:"1"`
}

// TypeRewriteConfig converts group TG calls to private calls.
// Modeled after DMRGateway's TypeRewrite: fromSlot, fromTG, toSlot, toId, range.
type TypeRewriteConfig struct {
	FromSlot uint `name:"from-slot" description:"Source timeslot (1 or 2)"`
	FromTG   uint `name:"from-tg" description:"Source talkgroup start"`
	ToSlot   uint `name:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToID     uint `name:"to-id" description:"Destination private call ID start"`
	Range    uint `name:"range" description:"Number of contiguous entries to map" default:"1"`
}

// SrcRewriteConfig matches calls by source ID and remaps the source into a prefixed range.
type SrcRewriteConfig struct {
	FromSlot uint `name:"from-slot" description:"Source timeslot (1 or 2)"`
	FromID   uint `name:"from-id" description:"Source ID start"`
	ToSlot   uint `name:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToID     uint `name:"to-id" description:"Destination source ID start"`
	Range    uint `name:"range" description:"Number of contiguous source IDs to match" default:"1"`
}

var (
	ErrInvalidLogLevel          = errors.New("invalid log level provided")
	ErrInvalidMMDVMName         = errors.New("invalid MMDVM network name provided")
	ErrDuplicateMMDVMName       = errors.New("duplicate MMDVM network name provided")
	ErrInvalidMMDVMCallsign     = errors.New("invalid MMDVM callsign provided")
	ErrInvalidMMDVMColorCode    = errors.New("invalid MMDVM color code provided")
	ErrInvalidMMDVMLongitude    = errors.New("invalid MMDVM longitude provided")
	ErrInvalidMMDVMLatitude     = errors.New("invalid MMDVM latitude provided")
	ErrInvalidMMDVMMasterServer = errors.New("invalid MMDVM master server provided")
	ErrInvalidMMDVMListen       = errors.New("invalid MMDVM listen address provided")
	ErrDuplicateMMDVMListen     = errors.New("duplicate MMDVM listen address provided")
	ErrInvalidMMDVMPassword     = errors.New("invalid MMDVM password provided")
	ErrInvalidRewriteSlot       = errors.New("invalid rewrite slot (must be 1 or 2)")
	ErrInvalidRewriteRange      = errors.New("invalid rewrite range (must be >= 1)")
	ErrInvalidIPSCInterface     = errors.New("invalid IPSC interface provided")
	ErrInvalidIPSCSubnetMask    = errors.New("invalid IPSC subnet mask provided")
	ErrInvalidHyteraP2PPort     = errors.New("invalid Hytera P2P port provided")
	ErrInvalidIPSCDMRPort       = errors.New("invalid Hytera DMR port provided")
	ErrIPSCPortConflict         = errors.New("IPSC moto port conflicts with Hytera P2P port")
	ErrInvalidIPSCAuthKey       = errors.New("invalid IPSC authentication key provided")
	ErrInvalidMetricsAddress    = errors.New("invalid metrics address provided")
	ErrInvalidStoragePath       = errors.New("invalid storage path provided")
	ErrInvalidWebAddress        = errors.New("invalid web address provided")
	ErrInvalidLocalID           = errors.New("invalid local bridge ID provided")
	ErrInvalidLocalColorCode    = errors.New("invalid local bridge color code provided")
)

func (c Config) Validate() error {
	switch c.LogLevel {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		return ErrInvalidLogLevel
	}

	if c.Metrics.Enabled && c.Metrics.Address != "" {
		_, _, err := net.SplitHostPort(c.Metrics.Address)
		if err != nil {
			return ErrInvalidMetricsAddress
		}
	}
	if c.Storage.Path == "" {
		return ErrInvalidStoragePath
	}
	if c.Web.Enabled && c.Web.Address != "" {
		_, _, err := net.SplitHostPort(c.Web.Address)
		if err != nil {
			return ErrInvalidWebAddress
		}
	}
	if c.Local.ID == 0 {
		return ErrInvalidLocalID
	}
	if c.Local.ColorCode > 15 {
		return ErrInvalidLocalColorCode
	}

	names := make(map[string]struct{}, len(c.MMDVMClients)+len(c.MMDVMServers))
	listeners := map[string]struct{}{}
	if err := validateMMDVMNetworks(c.MMDVMClients, true, true, names, listeners); err != nil {
		return err
	}
	if err := validateMMDVMNetworks(c.MMDVMServers, false, false, names, listeners); err != nil {
		return err
	}

	if c.IPSC.Enabled || c.Hytera.Enabled {
		// Listeners bind to all local addresses (0.0.0.0).
	}

	if c.IPSC.Enabled {

		if c.IPSC.Auth.Enabled && c.IPSC.Auth.Key == "" {
			return ErrInvalidIPSCAuthKey
		}

		regexp := regexp.MustCompile(`^[0-9a-fA-F]{0,40}$`)
		if !regexp.MatchString(c.IPSC.Auth.Key) {
			return ErrInvalidIPSCAuthKey
		}
	}

	if c.Hytera.Enabled {
		if c.Hytera.P2PPort == 0 {
			return ErrInvalidHyteraP2PPort
		}
		if c.Hytera.DMRPort == 0 {
			return ErrInvalidIPSCDMRPort
		}
	}

	if c.IPSC.Enabled && c.Hytera.Enabled && c.Hytera.P2PPort == c.IPSC.Port {
		return ErrIPSCPortConflict
	}

	return nil
}

func (c Config) DisplayAddressIP() string {
	if ip := strings.TrimSpace(c.DisplayIP); ip != "" {
		return ip
	}
	return strings.TrimSpace(c.IPSC.IP)
}

func validateSlot(slot uint) bool {
	return slot == 1 || slot == 2
}

func (c Config) FirstMMDVM() *MMDVM {
	if len(c.MMDVMClients) > 0 {
		return &c.MMDVMClients[0]
	}
	if len(c.MMDVMServers) > 0 {
		return &c.MMDVMServers[0]
	}
	return nil
}

func (c Config) BridgeID() uint32 {
	if c.Local.ID != 0 {
		return c.Local.ID
	}
	if network := c.FirstMMDVM(); network != nil {
		return network.ID
	}
	return 9000000
}

func validateMMDVMNetworks(networks []MMDVM, requireMaster, requireIdentity bool, names, listeners map[string]struct{}) error {
	for i := range networks {
		h := &networks[i]

		if h.Name == "" {
			return ErrInvalidMMDVMName
		}
		if _, ok := names[h.Name]; ok {
			return ErrDuplicateMMDVMName
		}
		names[h.Name] = struct{}{}

		if requireIdentity {
			if h.Callsign == "" {
				return ErrInvalidMMDVMCallsign
			}
			if h.ColorCode > 15 {
				return ErrInvalidMMDVMColorCode
			}
			if h.Longitude < -180 || h.Longitude > 180 {
				return ErrInvalidMMDVMLongitude
			}
			if h.Latitude < -90 || h.Latitude > 90 {
				return ErrInvalidMMDVMLatitude
			}
		}

		if requireMaster {
			if h.MasterServer == "" {
				return ErrInvalidMMDVMMasterServer
			}
		} else {
			if h.Listen == "" {
				h.Listen = ":62031"
			}
			if _, _, err := net.SplitHostPort(h.Listen); err != nil {
				return ErrInvalidMMDVMListen
			}
			if _, ok := listeners[h.Listen]; ok {
				return ErrDuplicateMMDVMListen
			}
			listeners[h.Listen] = struct{}{}
		}

		if h.Password == "" {
			return ErrInvalidMMDVMPassword
		}
		if err := validateRewrites(h); err != nil {
			return err
		}
	}
	return nil
}

func validateRewrites(h *MMDVM) error {
	for _, r := range h.TGRewrites {
		if !validateSlot(r.FromSlot) || !validateSlot(r.ToSlot) {
			return ErrInvalidRewriteSlot
		}
		if r.Range < 1 {
			return ErrInvalidRewriteRange
		}
	}
	for _, r := range h.PCRewrites {
		if !validateSlot(r.FromSlot) || !validateSlot(r.ToSlot) {
			return ErrInvalidRewriteSlot
		}
		if r.Range < 1 {
			return ErrInvalidRewriteRange
		}
	}
	for _, r := range h.TypeRewrites {
		if !validateSlot(r.FromSlot) || !validateSlot(r.ToSlot) {
			return ErrInvalidRewriteSlot
		}
		if r.Range < 1 {
			return ErrInvalidRewriteRange
		}
	}
	for _, r := range h.SrcRewrites {
		if !validateSlot(r.FromSlot) || !validateSlot(r.ToSlot) {
			return ErrInvalidRewriteSlot
		}
		if r.Range < 1 {
			return ErrInvalidRewriteRange
		}
	}
	return nil
}

func ValidateMMDVMClients(networks []MMDVM) error {
	names := make(map[string]struct{}, len(networks))
	listeners := map[string]struct{}{}
	return validateMMDVMNetworks(networks, true, true, names, listeners)
}
