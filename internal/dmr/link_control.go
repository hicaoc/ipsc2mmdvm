package dmr

// Link Control parity masks for full LC RS(12,9) encoding.
const (
	FullLCParityMaskVoiceHeader byte = 0x96
	FullLCParityMaskTerminator  byte = 0x99
)

func BuildStandardLCBytes(src, dst uint, groupCall bool) [12]byte {
	return BuildStandardLCBytesWithSO(src, dst, groupCall, 0x20)
}

func BuildStandardLCBytesWithSO(src, dst uint, groupCall bool, so uint8) [12]byte {
	return BuildStandardLCBytesForDataType(src, dst, groupCall, so, DataTypeVoiceLCHeader)
}

func BuildStandardLCBytesForDataType(src, dst uint, groupCall bool, so uint8, dataType DataType) [12]byte {
	var lc [12]byte
	flco := FLCOUnitToUnitVoiceChannelUser
	if groupCall {
		flco = FLCOGroupVoiceChannelUser
	}
	lc[0] = byte(flco) & 0x3F // PF=0, R=0
	lc[1] = byte(StandardizedFID)
	lc[2] = so
	lc[3] = byte(dst >> 16)
	lc[4] = byte(dst >> 8)
	lc[5] = byte(dst)
	lc[6] = byte(src >> 16)
	lc[7] = byte(src >> 8)
	lc[8] = byte(src)
	parity := ReedSolomon129Parity(lc[:9])
	mask := FullLCParityMaskForDataType(dataType)
	lc[9] = parity[0] ^ mask
	lc[10] = parity[1] ^ mask
	lc[11] = parity[2] ^ mask
	return lc
}

func FullLCParityMaskForDataType(dataType DataType) byte {
	switch dataType {
	case DataTypeVoiceLCHeader:
		return FullLCParityMaskVoiceHeader
	case DataTypeTerminatorWithLC:
		return FullLCParityMaskTerminator
	default:
		return 0x00
	}
}

func ReedSolomon129Parity(data []byte) [3]byte {
	var parity [3]byte
	if len(data) != 9 {
		return parity
	}
	for i := 0; i < 9; i++ {
		feedback := data[i] ^ parity[0]
		parity[0] = parity[1] ^ GF256Mul(feedback, 0x0E)
		parity[1] = parity[2] ^ GF256Mul(feedback, 0x38)
		parity[2] = GF256Mul(feedback, 0x40)
	}
	return parity
}

func GF256Mul(a, b byte) byte {
	var p byte
	aa := a
	bb := b
	for i := 0; i < 8; i++ {
		if (bb & 1) != 0 {
			p ^= aa
		}
		hi := (aa & 0x80) != 0
		aa <<= 1
		if hi {
			aa ^= 0x1D
		}
		bb >>= 1
	}
	return p
}
