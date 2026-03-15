package nrl

import "testing"

func TestDecodePacketVoiceUsesDatagramLengthWhenTotalIsHeaderOnly(t *testing.T) {
	data := make([]byte, headerLen+160)
	copy(data[:4], []byte("NRL2"))
	// Peer reports 48-byte header only even though datagram carries voice payload.
	data[4] = 0x00
	data[5] = headerLen
	data[20] = packetTypeVoice
	data[31] = devModelDMR
	copy(data[24:30], []byte("D2TEST"))
	copy(data[32:38], []byte("BH4RPN"))
	for i := headerLen; i < len(data); i++ {
		data[i] = 0xD5
	}

	pkt, err := decodePacket(data)
	if err != nil {
		t.Fatalf("decode packet: %v", err)
	}
	if len(pkt.Data) != 160 {
		t.Fatalf("expected 160 payload bytes, got %d", len(pkt.Data))
	}
}

func TestDecodePacketVoiceUsesDatagramLengthWhenTotalIsPayloadOnly(t *testing.T) {
	data := make([]byte, headerLen+500)
	copy(data[:4], []byte("NRL2"))
	// Peer reports payload length only (500), not full datagram length (548).
	data[4] = 0x01
	data[5] = 0xF4
	data[20] = packetTypeVoice
	data[31] = devModelDMR
	copy(data[24:30], []byte("D2TEST"))
	copy(data[32:38], []byte("BH4RPN"))
	for i := headerLen; i < len(data); i++ {
		data[i] = 0xD5
	}

	pkt, err := decodePacket(data)
	if err != nil {
		t.Fatalf("decode packet: %v", err)
	}
	if len(pkt.Data) != 500 {
		t.Fatalf("expected 500 payload bytes, got %d", len(pkt.Data))
	}
}
