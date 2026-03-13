package config

import (
	"errors"
	"testing"
)

// validConfig returns a minimal Config that passes all validation checks.
func validConfig() Config {
	return Config{
		LogLevel: LogLevelInfo,
		Storage: Storage{
			Path: "/tmp/ipsc2mmdvm-test.db",
		},
		Web: Web{
			Enabled: true,
			Address: ":9201",
		},
		Local: Local{
			ID:        9000000,
			Callsign:  "IPSC2MMDVM",
			ColorCode: 1,
		},
		MMDVMClients: []MMDVM{
			{
				Name:         "BM",
				Callsign:     "N0CALL",
				ID:           12345,
				ColorCode:    7,
				Latitude:     30.0,
				Longitude:    -97.0,
				MasterServer: "master.example.com:62030",
				Password:     "password",
			},
		},
		IPSC: IPSC{
			Enabled: true,
			Port:    50005,
			Auth: IPSCAuth{
				Enabled: false,
			},
		},
		Hytera: Hytera{
			Enabled: true,
			P2PPort: 50000,
			DMRPort: 50001,
		},
	}
}

func TestValidateLogLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		level    LogLevel
		wantErr  error
		hasError bool
	}{
		{"debug", LogLevelDebug, nil, false},
		{"info", LogLevelInfo, nil, false},
		{"warn", LogLevelWarn, nil, false},
		{"error", LogLevelError, nil, false},
		{"invalid", LogLevel("trace"), ErrInvalidLogLevel, true},
		{"empty", LogLevel(""), ErrInvalidLogLevel, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.LogLevel = tt.level
			err := c.Validate()
			if tt.hasError {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
			}
			// For valid levels, only assert that log-level itself is accepted.
			if !tt.hasError && errors.Is(err, ErrInvalidLogLevel) {
				t.Fatalf("did not expect %v, got %v", ErrInvalidLogLevel, err)
			}
		})
	}
}

func TestValidateWithoutMMDVMNetworks(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVMClients = nil
	c.MMDVMServers = nil
	err := c.Validate()
	if err != nil {
		t.Fatalf("expected nil error without MMDVM networks, got %v", err)
	}
}

func TestValidateMMDVMOnlyMode(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Enabled = false
	c.Hytera.Enabled = false
	c.Hytera.P2PPort = 0
	c.Hytera.DMRPort = 0
	err := c.Validate()
	if err != nil {
		t.Fatalf("expected nil error in mmdvm-only mode, got %v", err)
	}
}

func TestValidateMMDVMCallsign(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVMClients[0].Callsign = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidMMDVMCallsign) {
		t.Fatalf("expected %v, got %v", ErrInvalidMMDVMCallsign, err)
	}
}

func TestValidateMMDVMColorCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cc      uint8
		wantErr bool
	}{
		{"valid 0", 0, false},
		{"valid 15", 15, false},
		{"invalid 16", 16, true},
		{"invalid 255", 255, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.MMDVMClients[0].ColorCode = tt.cc
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidMMDVMColorCode) {
				t.Fatalf("expected %v, got %v", ErrInvalidMMDVMColorCode, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidMMDVMColorCode) {
				t.Fatalf("did not expect %v", ErrInvalidMMDVMColorCode)
			}
		})
	}
}

func TestValidateMMDVMLatitude(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		lat     float64
		wantErr bool
	}{
		{"valid 0", 0, false},
		{"valid -90", -90, false},
		{"valid 90", 90, false},
		{"invalid -91", -91, true},
		{"invalid 91", 91, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.MMDVMClients[0].Latitude = tt.lat
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidMMDVMLatitude) {
				t.Fatalf("expected %v, got %v", ErrInvalidMMDVMLatitude, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidMMDVMLatitude) {
				t.Fatalf("did not expect %v", ErrInvalidMMDVMLatitude)
			}
		})
	}
}

func TestValidateMMDVMLongitude(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		lng     float64
		wantErr bool
	}{
		{"valid 0", 0, false},
		{"valid -180", -180, false},
		{"valid 180", 180, false},
		{"invalid -181", -181, true},
		{"invalid 181", 181, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.MMDVMClients[0].Longitude = tt.lng
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidMMDVMLongitude) {
				t.Fatalf("expected %v, got %v", ErrInvalidMMDVMLongitude, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidMMDVMLongitude) {
				t.Fatalf("did not expect %v", ErrInvalidMMDVMLongitude)
			}
		})
	}
}

func TestValidateMMDVMMasterServer(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVMClients[0].MasterServer = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidMMDVMMasterServer) {
		t.Fatalf("expected %v, got %v", ErrInvalidMMDVMMasterServer, err)
	}
}

func TestValidateMMDVMServerListen(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVMServers = []MMDVM{{
		Name:      "HS",
		Callsign:  "N0CALL",
		ID:        67890,
		ColorCode: 1,
		Latitude:  30.0,
		Longitude: -97.0,
		Listen:    "bad-listen",
		Password:  "password",
	}}
	err := c.Validate()
	if !errors.Is(err, ErrInvalidMMDVMListen) {
		t.Fatalf("expected %v, got %v", ErrInvalidMMDVMListen, err)
	}
}

func TestValidateDuplicateMMDVMServerListen(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVMServers = []MMDVM{
		{
			Name:      "HS1",
			Callsign:  "N0CALL",
			ID:        67890,
			ColorCode: 1,
			Latitude:  30.0,
			Longitude: -97.0,
			Listen:    ":62031",
			Password:  "password",
		},
		{
			Name:      "HS2",
			Callsign:  "N0CALL",
			ID:        67891,
			ColorCode: 1,
			Latitude:  30.0,
			Longitude: -97.0,
			Listen:    ":62031",
			Password:  "password",
		},
	}
	err := c.Validate()
	if !errors.Is(err, ErrDuplicateMMDVMListen) {
		t.Fatalf("expected %v, got %v", ErrDuplicateMMDVMListen, err)
	}
}

func TestValidateMMDVMPassword(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVMClients[0].Password = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidMMDVMPassword) {
		t.Fatalf("expected %v, got %v", ErrInvalidMMDVMPassword, err)
	}
}

func TestValidateIPSCInterfaceIgnored(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Interface = ""
	err := c.Validate()
	if errors.Is(err, ErrInvalidIPSCInterface) {
		t.Fatalf("did not expect %v", ErrInvalidIPSCInterface)
	}
}

func TestValidateIPSCSubnetMaskIgnored(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mask int
	}{
		{"valid 1", 1},
		{"valid 24", 24},
		{"valid 32", 32},
		{"invalid 0", 0},
		{"invalid 33", 33},
		{"invalid -1", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.IPSC.SubnetMask = tt.mask
			err := c.Validate()
			if errors.Is(err, ErrInvalidIPSCSubnetMask) {
				t.Fatalf("did not expect %v", ErrInvalidIPSCSubnetMask)
			}
		})
	}
}

func TestValidateIPSCAuthKeyRequired(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Auth.Enabled = true
	c.IPSC.Auth.Key = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidIPSCAuthKey) {
		t.Fatalf("expected %v, got %v", ErrInvalidIPSCAuthKey, err)
	}
}

func TestValidateIPSCAuthKeyBadHex(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Auth.Enabled = true
	c.IPSC.Auth.Key = "ZZZZ" // Not valid hex
	err := c.Validate()
	if !errors.Is(err, ErrInvalidIPSCAuthKey) {
		t.Fatalf("expected %v, got %v", ErrInvalidIPSCAuthKey, err)
	}
}

func TestValidateIPSCAuthKeyValid(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Auth.Enabled = true
	c.IPSC.Auth.Key = "deadbeef"
	err := c.Validate()
	// Should not fail on auth key validation itself
	if errors.Is(err, ErrInvalidIPSCAuthKey) {
		t.Fatalf("did not expect %v", ErrInvalidIPSCAuthKey)
	}
}

func TestValidateHyteraDMRPort(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.Hytera.DMRPort = 0
	err := c.Validate()
	if !errors.Is(err, ErrInvalidIPSCDMRPort) {
		t.Fatalf("expected %v, got %v", ErrInvalidIPSCDMRPort, err)
	}
}

func TestValidateHyteraP2PPort(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.Hytera.P2PPort = 0
	err := c.Validate()
	if !errors.Is(err, ErrInvalidHyteraP2PPort) {
		t.Fatalf("expected %v, got %v", ErrInvalidHyteraP2PPort, err)
	}
}

func TestValidateBothPortConflict(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Port = 50000
	c.Hytera.P2PPort = 50000
	err := c.Validate()
	if !errors.Is(err, ErrIPSCPortConflict) {
		t.Fatalf("expected %v, got %v", ErrIPSCPortConflict, err)
	}
}

func TestLogLevelConstants(t *testing.T) {
	t.Parallel()
	if LogLevelDebug != "debug" {
		t.Fatalf("expected 'debug', got %q", LogLevelDebug)
	}
	if LogLevelInfo != "info" {
		t.Fatalf("expected 'info', got %q", LogLevelInfo)
	}
	if LogLevelWarn != "warn" {
		t.Fatalf("expected 'warn', got %q", LogLevelWarn)
	}
	if LogLevelError != "error" {
		t.Fatalf("expected 'error', got %q", LogLevelError)
	}
}

func TestValidateMetricsAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{"default port only", ":9100", false},
		{"host and port", "0.0.0.0:9100", false},
		{"localhost", "127.0.0.1:2112", false},
		{"empty disables", "", false},
		{"missing port", "localhost", true},
		{"invalid format", ":::bad", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.Metrics.Enabled = true
			c.Metrics.Address = tt.addr
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidMetricsAddress) {
				t.Fatalf("expected %v, got %v", ErrInvalidMetricsAddress, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidMetricsAddress) {
				t.Fatalf("did not expect %v, got %v", ErrInvalidMetricsAddress, err)
			}
		})
	}
}

func TestValidateStoragePath(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.Storage.Path = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidStoragePath) {
		t.Fatalf("expected %v, got %v", ErrInvalidStoragePath, err)
	}
}

func TestValidateWebAddress(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.Web.Address = "bad-web-address"
	err := c.Validate()
	if !errors.Is(err, ErrInvalidWebAddress) {
		t.Fatalf("expected %v, got %v", ErrInvalidWebAddress, err)
	}
}

func TestValidateLocalBridgeID(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.Local.ID = 0
	err := c.Validate()
	if !errors.Is(err, ErrInvalidLocalID) {
		t.Fatalf("expected %v, got %v", ErrInvalidLocalID, err)
	}
}

func TestValidateLocalBridgeColorCode(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.Local.ColorCode = 16
	err := c.Validate()
	if !errors.Is(err, ErrInvalidLocalColorCode) {
		t.Fatalf("expected %v, got %v", ErrInvalidLocalColorCode, err)
	}
}
