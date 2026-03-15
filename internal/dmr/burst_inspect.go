package dmr

import "github.com/USA-RedDragon/dmrgo/dmr/layer2/pdu"

// Burst inspection helpers for extracting sync and color code information from
// 33-byte DMR bursts.
func BurstBits(data [33]byte) [264]bool {
	var bits [264]bool
	for i := 0; i < 264; i++ {
		bits[i] = (data[i/8] & (1 << (7 - (i % 8)))) != 0
	}
	return bits
}

func BurstSyncPattern(data [33]byte) SyncPattern {
	bits := BurstBits(data)
	var syncBytes [6]byte
	for i := 0; i < 6; i++ {
		for j := 0; j < 8; j++ {
			if bits[108+(i*8)+j] {
				syncBytes[i] |= 1 << (7 - j)
			}
		}
	}
	return SyncPatternFromBytes(syncBytes)
}

func BurstHasDataSync(data [33]byte) bool {
	sync := BurstSyncPattern(data)
	return sync == Tdma1Data || sync == Tdma2Data || sync == MsSourcedData || sync == BsSourcedData
}

func BurstHasEmbeddedSignalling(data [33]byte) bool {
	return BurstSyncPattern(data) == EmbeddedSignallingPattern
}

func BurstSlotTypeColorCode(data [33]byte) (uint8, bool) {
	if !BurstHasDataSync(data) {
		return 0, false
	}
	bits := BurstBits(data)
	var slotBits [20]byte
	for i := 0; i < 10; i++ {
		if bits[98+i] {
			slotBits[i] = 1
		}
	}
	for i := 0; i < 10; i++ {
		if bits[156+i] {
			slotBits[10+i] = 1
		}
	}
	slotType := pdu.NewSlotTypeFromBits(slotBits)
	return uint8(slotType.ColorCode) & 0x0F, true
}

func BurstEmbeddedColorCode(data [33]byte) (uint8, bool) {
	if !BurstHasEmbeddedSignalling(data) {
		return 0, false
	}
	bits := BurstBits(data)
	var embeddedBits [16]byte
	for i := 0; i < 8; i++ {
		if bits[108+i] {
			embeddedBits[i] = 1
		}
	}
	for i := 0; i < 8; i++ {
		if bits[148+i] {
			embeddedBits[8+i] = 1
		}
	}
	embedded := pdu.NewEmbeddedSignallingFromBits(embeddedBits)
	return uint8(embedded.ColorCode) & 0x0F, true
}
