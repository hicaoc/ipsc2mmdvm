package dmr

import (
	"github.com/USA-RedDragon/dmrgo/dmr/fec/golay"
	"github.com/USA-RedDragon/dmrgo/dmr/fec/prng"
)

var ambeATable = []int{0, 4, 8, 12, 16, 20, 24, 28, 32, 36, 40, 44, 48, 52, 56, 60, 64, 68, 1, 5, 9, 13, 17, 21}
var ambeBTable = []int{25, 29, 33, 37, 41, 45, 49, 53, 57, 61, 65, 69, 2, 6, 10, 14, 18, 22, 26, 30, 34, 38, 42}
var ambeCTable = []int{46, 50, 54, 58, 62, 66, 70, 3, 7, 11, 15, 19, 23, 27, 31, 35, 39, 43, 47, 51, 55, 59, 63, 67, 71}

func EncodeAMBEFrame(decodedBits [49]byte) [72]byte {
	var ambe72 [72]byte

	var aOrig uint32
	var bOrig uint32
	var cOrig uint32
	var mask uint32 = 0x000800

	for i := 0; i < 12; i, mask = i+1, mask>>1 {
		if decodedBits[i] == 1 {
			aOrig |= mask
		}
		if decodedBits[i+12] == 1 {
			bOrig |= mask
		}
	}

	mask = 0x1000000
	for i := 0; i < 25; i, mask = i+1, mask>>1 {
		if decodedBits[i+24] == 1 {
			cOrig |= mask
		}
	}

	a := golay.Golay_24_12_8_EncodingTable[aOrig]
	p := prng.PRNG_TABLE[aOrig] >> 1
	b := golay.Golay_23_12_7_EncodingTable[bOrig] >> 1
	b ^= p

	mask = 0x800000
	for i := 0; i < 24; i, mask = i+1, mask>>1 {
		if (a & mask) != 0 {
			ambe72[ambeATable[i]] = 1
		}
	}

	mask = 0x400000
	for i := 0; i < 23; i, mask = i+1, mask>>1 {
		if (b & mask) != 0 {
			ambe72[ambeBTable[i]] = 1
		}
	}

	mask = 0x1000000
	for i := 0; i < 25; i, mask = i+1, mask>>1 {
		if (cOrig & mask) != 0 {
			ambe72[ambeCTable[i]] = 1
		}
	}

	return ambe72
}

func PackAMBEVoiceFrames(frames [3][49]byte) [19]byte {
	var bits [152]bool
	for i := 0; i < 49; i++ {
		bits[i] = frames[0][i] == 1
		bits[50+i] = frames[1][i] == 1
		bits[100+i] = frames[2][i] == 1
	}
	var data [19]byte
	for i := 0; i < 152; i++ {
		if bits[i] {
			data[i/8] |= 1 << (7 - (i % 8))
		}
	}
	return data
}

func UnpackAMBEVoiceFrames(data [19]byte) [3][49]byte {
	var bits [152]bool
	for i := 0; i < 152; i++ {
		bits[i] = (data[i/8]>>(7-(i%8)))&1 == 1
	}
	var frames [3][49]byte
	for i := 0; i < 49; i++ {
		if bits[i] {
			frames[0][i] = 1
		}
		if bits[50+i] {
			frames[1][i] = 1
		}
		if bits[100+i] {
			frames[2][i] = 1
		}
	}
	return frames
}
