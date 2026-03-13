package mmdvm

import (
	"net"

	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
	"github.com/hicaoc/ipsc2mmdvm/internal/timeslot"
)

type Network interface {
	Name() string
	Start() error
	Stop()
	SetIPSCHandler(func(data []byte))
	SetPacketHandler(func(packet proto.Packet))
	SetOutboundTSManager(*timeslot.Manager)
	MatchesPacket(pkt proto.Packet, passallOnly bool) bool
	HandleTranslatedPacket(pkt proto.Packet, addr *net.UDPAddr) bool
}
