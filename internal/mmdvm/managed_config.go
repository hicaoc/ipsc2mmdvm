package mmdvm

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/hicaoc/ipsc2mmdvm/internal/config"
	"github.com/hicaoc/ipsc2mmdvm/internal/registry"
)

type ManagedExtra struct {
	MasterServer string                     `json:"masterServer,omitempty"`
	TGRewrites   []config.TGRewriteConfig   `json:"tgRewrites,omitempty"`
	PCRewrites   []config.PCRewriteConfig   `json:"pcRewrites,omitempty"`
	TypeRewrites []config.TypeRewriteConfig `json:"typeRewrites,omitempty"`
	SrcRewrites  []config.SrcRewriteConfig  `json:"srcRewrites,omitempty"`
	PassAllPC    []int                      `json:"passAllPC,omitempty"`
	PassAllTG    []int                      `json:"passAllTG,omitempty"`
}

func SourceKey(cfg *config.MMDVM) string {
	if cfg == nil {
		return ""
	}
	if strings.TrimSpace(cfg.SourceKey) != "" {
		return strings.TrimSpace(cfg.SourceKey)
	}
	return "mmdvm-upstream:" + strings.TrimSpace(cfg.Name)
}

func ParseManagedExtra(raw string) (ManagedExtra, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ManagedExtra{}, nil
	}
	var extra ManagedExtra
	if err := json.Unmarshal([]byte(raw), &extra); err != nil {
		return ManagedExtra{}, err
	}
	return extra, nil
}

func MarshalManagedExtra(extra ManagedExtra) (string, error) {
	data, err := json.Marshal(extra)
	if err != nil {
		return "", err
	}
	if string(data) == "{}" {
		return "", nil
	}
	return string(data), nil
}

func DeviceToConfig(dev registry.Device) (config.MMDVM, error) {
	extra, err := ParseManagedExtra(dev.ExtraJSON)
	if err != nil {
		return config.MMDVM{}, fmt.Errorf("parse managed extra: %w", err)
	}
	masterServer := strings.TrimSpace(extra.MasterServer)
	if masterServer == "" {
		masterServer = masterServerFromDevice(dev)
	}
	cfg := config.MMDVM{
		SourceKey:    dev.SourceKey,
		Name:         strings.TrimSpace(dev.Name),
		Callsign:     strings.TrimSpace(dev.Callsign),
		ID:           dev.DMRID,
		RXFreq:       dev.RXFreq,
		TXFreq:       dev.TXFreq,
		TXPower:      dev.TXPower,
		ColorCode:    dev.ColorCode,
		Latitude:     dev.Latitude,
		Longitude:    dev.Longitude,
		Height:       dev.Height,
		Location:     strings.TrimSpace(dev.Location),
		Description:  strings.TrimSpace(dev.Description),
		URL:          strings.TrimSpace(dev.URL),
		Slots:        dev.Slots,
		MasterServer: masterServer,
		Password:     dev.DevicePassword,
		TGRewrites:   extra.TGRewrites,
		PCRewrites:   extra.PCRewrites,
		TypeRewrites: extra.TypeRewrites,
		SrcRewrites:  extra.SrcRewrites,
		PassAllPC:    extra.PassAllPC,
		PassAllTG:    extra.PassAllTG,
	}
	if cfg.Slots == 0 {
		cfg.Slots = 3
	}
	return cfg, nil
}

func HasManagedConfig(dev registry.Device) bool {
	if dev.Protocol != "mmdvm-upstream" || !strings.HasPrefix(strings.TrimSpace(dev.SourceKey), "mmdvm-upstream:") {
		return false
	}
	extra, err := ParseManagedExtra(dev.ExtraJSON)
	if err != nil {
		return false
	}
	masterServer := strings.TrimSpace(extra.MasterServer)
	if masterServer == "" {
		masterServer = masterServerFromDevice(dev)
	}
	return strings.TrimSpace(dev.DevicePassword) != "" && masterServer != ""
}

func ConfigToDevice(cfg config.MMDVM) (registry.Device, error) {
	extraJSON, err := MarshalManagedExtra(ManagedExtra{
		MasterServer: strings.TrimSpace(cfg.MasterServer),
		TGRewrites:   cfg.TGRewrites,
		PCRewrites:   cfg.PCRewrites,
		TypeRewrites: cfg.TypeRewrites,
		SrcRewrites:  cfg.SrcRewrites,
		PassAllPC:    cfg.PassAllPC,
		PassAllTG:    cfg.PassAllTG,
	})
	if err != nil {
		return registry.Device{}, err
	}
	host, port := splitMasterServer(cfg.MasterServer)
	dev := registry.Device{
		Category:       registry.CategoryMMDVM,
		Protocol:       "mmdvm-upstream",
		SourceKey:      SourceKey(&cfg),
		Name:           strings.TrimSpace(cfg.Name),
		Callsign:       strings.TrimSpace(cfg.Callsign),
		DMRID:          cfg.ID,
		Model:          "MMDVM Master",
		IP:             host,
		Port:           port,
		Status:         "configured",
		Online:         false,
		RXFreq:         cfg.RXFreq,
		TXFreq:         cfg.TXFreq,
		TXPower:        cfg.TXPower,
		ColorCode:      cfg.ColorCode,
		Latitude:       cfg.Latitude,
		Longitude:      cfg.Longitude,
		Height:         cfg.Height,
		Location:       strings.TrimSpace(cfg.Location),
		Description:    strings.TrimSpace(cfg.Description),
		URL:            strings.TrimSpace(cfg.URL),
		Slots:          cfg.Slots,
		DevicePassword: cfg.Password,
		ExtraJSON:      extraJSON,
	}
	if dev.Slots == 0 {
		dev.Slots = 3
	}
	return dev, nil
}

func masterServerFromDevice(dev registry.Device) string {
	if strings.TrimSpace(dev.IP) == "" {
		return ""
	}
	if dev.Port > 0 {
		return net.JoinHostPort(strings.TrimSpace(dev.IP), strconv.Itoa(dev.Port))
	}
	return strings.TrimSpace(dev.IP)
}

func splitMasterServer(value string) (string, int) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", 0
	}
	host, portRaw, err := net.SplitHostPort(value)
	if err != nil {
		return value, 0
	}
	port, _ := strconv.Atoi(portRaw)
	return host, port
}
