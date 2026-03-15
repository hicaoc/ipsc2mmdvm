package nrl

import (
	"encoding/binary"
	"errors"
	"net"
	"strings"
)

const (
	headerLen       = 48
	packetTypeVoice = 1
	packetTypePing  = 2
	devModelDMR     = 200
)

type packet struct {
	Type             byte
	DMRID            uint32
	Count            uint16
	Callsign         string
	SSID             uint8
	OriginalCallsign string
	OriginalSSID     uint8
	OriginalIP       net.IP
	Data             []byte
}

func decodePacket(data []byte) (packet, error) {
	if len(data) < headerLen {
		return packet{}, errors.New("short nrl packet")
	}
	if string(data[:4]) != "NRL2" {
		return packet{}, errors.New("invalid nrl signature")
	}
	total := int(binary.BigEndian.Uint16(data[4:6]))
	if total < headerLen || total > len(data) {
		total = len(data)
	}
	p := packet{
		DMRID:    uint32(data[6])<<16 | uint32(data[7])<<8 | uint32(data[8]),
		Type:     data[20],
		Count:    binary.BigEndian.Uint16(data[22:24]),
		Callsign: strings.TrimRight(string(data[24:30]), "\x00\r "),
		SSID:     data[30],
	}
	if p.Type == 9 || data[31] == devModelDMR {
		p.OriginalCallsign = strings.TrimRight(string(data[32:38]), "\x00\r ")
		p.OriginalSSID = data[38]
		p.OriginalIP = append(net.IP(nil), data[39:43]...)
	}
	// Some NRL peers are inconsistent with the total-length field for voice:
	// 1) fill header-only length (48) even when payload exists;
	// 2) fill payload-only length (e.g. 500), not full datagram length (548).
	// For voice packets, prefer actual datagram length in these known variants.
	if (p.Type == packetTypeVoice || p.Type == 9) && len(data) > headerLen {
		switch {
		case total == headerLen:
			total = len(data)
		case total == len(data)-headerLen:
			total = len(data)
		}
	}
	if total > headerLen {
		p.Data = append([]byte(nil), data[headerLen:total]...)
	}
	return p, nil
}

func encodeVoicePacket(cfg DeviceConfig, count uint16, srcDMRID uint32, srcCallsign string, payload []byte, localIP net.IP) []byte {
	packet := make([]byte, headerLen+len(payload))
	copy(packet[:4], []byte("NRL2"))
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)))
	packet[6] = byte(srcDMRID >> 16)
	packet[7] = byte(srcDMRID >> 8)
	packet[8] = byte(srcDMRID)
	packet[20] = packetTypeVoice
	packet[21] = 1
	binary.BigEndian.PutUint16(packet[22:24], count)
	copy(packet[24:30], []byte(normalizeCallsign(cfg.Callsign)))
	packet[30] = cfg.SSID
	packet[31] = devModelDMR
	copy(packet[32:38], []byte(normalizeCallsign(srcCallsign)))
	packet[38] = cfg.SSID
	copy(packet[39:43], localIP.To4())
	copy(packet[headerLen:], payload)
	return packet
}

func encodeHeartbeatPacket(cfg DeviceConfig, count uint16) []byte {
	packet := make([]byte, headerLen)
	copy(packet[:4], []byte("NRL2"))
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)))
	packet[20] = packetTypePing
	packet[21] = 1
	binary.BigEndian.PutUint16(packet[22:24], count)
	copy(packet[24:30], []byte(normalizeCallsign(cfg.Callsign)))
	packet[30] = cfg.SSID
	packet[31] = devModelDMR
	return packet
}

func normalizeCallsign(callsign string) string {
	cs := strings.ToUpper(strings.TrimSpace(callsign))
	if len(cs) > 6 {
		cs = cs[:6]
	}
	if len(cs) < 6 {
		cs += strings.Repeat(" ", 6-len(cs))
	}
	return cs
}
