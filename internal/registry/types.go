package registry

import "time"

type DeviceCategory string

const (
	CategoryMMDVM  DeviceCategory = "mmdvm"
	CategoryMoto   DeviceCategory = "moto"
	CategoryHytera DeviceCategory = "hytera"
	CategoryNRL    DeviceCategory = "nrl"
)

type Device struct {
	ID             int64          `json:"id"`
	Category       DeviceCategory `json:"category"`
	Protocol       string         `json:"protocol"`
	SourceKey      string         `json:"sourceKey"`
	OwnerUserID    int64          `json:"ownerUserId"`
	Name           string         `json:"name"`
	Callsign       string         `json:"callsign"`
	DMRID          uint32         `json:"dmrid"`
	Model          string         `json:"model"`
	Serial         string         `json:"serial"`
	IP             string         `json:"ip"`
	Port           int            `json:"port"`
	Status         string         `json:"status"`
	Online         bool           `json:"online"`
	FirstSeenAt    time.Time      `json:"firstSeenAt"`
	LastSeenAt     time.Time      `json:"lastSeenAt"`
	LastCallAt     time.Time      `json:"lastCallAt"`
	RXFreq         uint           `json:"rxFreq"`
	TXFreq         uint           `json:"txFreq"`
	TXPower        uint8          `json:"txPower"`
	ColorCode      uint8          `json:"colorCode"`
	Latitude       float64        `json:"latitude"`
	Longitude      float64        `json:"longitude"`
	Height         uint16         `json:"height"`
	Location       string         `json:"location"`
	Description    string         `json:"description"`
	URL            string         `json:"url"`
	Slots          byte           `json:"slots"`
	Notes          string         `json:"notes"`
	DevicePassword string         `json:"devicePassword"`
	NRLServerAddr  string         `json:"nrlServerAddr"`
	NRLServerPort  int            `json:"nrlServerPort"`
	NRLSSID        uint8          `json:"nrlSsid"`
	NRLUDPPort     int            `json:"nrlUdpPort"`
	NRLSlot        int            `json:"nrlSlot"`
	Disabled       bool           `json:"disabled"`
	ExtraJSON      string         `json:"extraJson"`
}

type UserRole string

const (
	RoleAdmin UserRole = "admin"
	RoleHAM   UserRole = "ham"
)

type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	Callsign     string    `json:"callsign"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Role         UserRole  `json:"role"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	LastLoginAt  time.Time `json:"lastLoginAt"`
}

type CallRecord struct {
	ID             int64          `json:"id"`
	CreatedAt      time.Time      `json:"createdAt"`
	EndedAt        time.Time      `json:"endedAt"`
	DurationMS     int64          `json:"durationMs"`
	Frontend       string         `json:"frontend"`
	SourceCategory DeviceCategory `json:"sourceCategory"`
	SourceKey      string         `json:"sourceKey"`
	SourceName     string         `json:"sourceName"`
	SourceCallsign string         `json:"sourceCallsign"`
	SourceDMRID    uint32         `json:"sourceDmrid"`
	SrcID          uint           `json:"srcId"`
	DstID          uint           `json:"dstId"`
	RepeaterID     uint           `json:"repeaterId"`
	Slot           int            `json:"slot"`
	CallType       string         `json:"callType"`
	StreamID       uint           `json:"streamId"`
	FromIP         string         `json:"fromIp"`
	FromPort       int            `json:"fromPort"`
}

type StaticGroup struct {
	SourceKey string `json:"sourceKey"`
	Slot      int    `json:"slot"`
	GroupID   uint32 `json:"groupId"`
}

type Snapshot struct {
	Devices   []Device     `json:"devices"`
	Calls     []CallRecord `json:"calls"`
	CallTotal int64        `json:"callTotal"`
}

type Event struct {
	Type   string      `json:"type"`
	Device *Device     `json:"device,omitempty"`
	Call   *CallRecord `json:"call,omitempty"`
}
