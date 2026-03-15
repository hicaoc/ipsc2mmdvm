package nrl

import (
	"math/rand"

	"github.com/USA-RedDragon/dmrgo/dmr/layer2/elements"
	"github.com/hicaoc/ipsc2mmdvm/internal/dmr/bptc"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

var dmrSyncAudioBS = []byte{0x75, 0x5F, 0xD7, 0xDF, 0x75, 0xF7}

const (
	fullLCParityMaskVoiceHeader byte = 0x96
	fullLCParityMaskTerminator  byte = 0x99
)

var qr1676Table = []uint16{
	0x0000, 0x0273, 0x04E5, 0x0696, 0x09C9, 0x0BBA, 0x0D2C, 0x0F5F, 0x11E2, 0x1391, 0x1507, 0x1774,
	0x182B, 0x1A58, 0x1CCE, 0x1EBD, 0x21B7, 0x23C4, 0x2552, 0x2721, 0x287E, 0x2A0D, 0x2C9B, 0x2EE8,
	0x3055, 0x3226, 0x34B0, 0x36C3, 0x399C, 0x3BEF, 0x3D79, 0x3F0A, 0x411E, 0x436D, 0x45FB, 0x4788,
	0x48D7, 0x4AA4, 0x4C32, 0x4E41, 0x50FC, 0x528F, 0x5419, 0x566A, 0x5935, 0x5B46, 0x5DD0, 0x5FA3,
	0x60A9, 0x62DA, 0x644C, 0x663F, 0x6960, 0x6B13, 0x6D85, 0x6FF6, 0x714B, 0x7338, 0x75AE, 0x77DD,
	0x7882, 0x7AF1, 0x7C67, 0x7E14, 0x804F, 0x823C, 0x84AA, 0x86D9, 0x8986, 0x8BF5, 0x8D63, 0x8F10,
	0x91AD, 0x93DE, 0x9548, 0x973B, 0x9864, 0x9A17, 0x9C81, 0x9EF2, 0xA1F8, 0xA38B, 0xA51D, 0xA76E,
	0xA831, 0xAA42, 0xACD4, 0xAEA7, 0xB01A, 0xB269, 0xB4FF, 0xB68C, 0xB9D3, 0xBBA0, 0xBD36, 0xBF45,
	0xC151, 0xC322, 0xC5B4, 0xC7C7, 0xC898, 0xCAEB, 0xCC7D, 0xCE0E, 0xD0B3, 0xD2C0, 0xD456, 0xD625,
	0xD97A, 0xDB09, 0xDD9F, 0xDFEC, 0xE0E6, 0xE295, 0xE403, 0xE670, 0xE92F, 0xEB5C, 0xEDCA, 0xEFB9,
	0xF104, 0xF377, 0xF5E1, 0xF792, 0xF8CD, 0xFABE, 0xFC28, 0xFE5B,
}

func randomStreamID() uint32 {
	v := uint32(rand.Int31())
	if v == 0 {
		return 1
	}
	return v
}

func buildStandardLCBytesForDataType(srcID, dstID uint32, groupCall bool, so uint8, dataType elements.DataType) [12]byte {
	var lc [12]byte
	flco := byte(0x03) // unit to unit
	if groupCall {
		flco = 0x00 // group voice channel user
	}
	lc[0] = flco & 0x3F
	lc[1] = 0x00 // standardized FID
	lc[2] = so
	lc[3] = byte(dstID >> 16)
	lc[4] = byte(dstID >> 8)
	lc[5] = byte(dstID)
	lc[6] = byte(srcID >> 16)
	lc[7] = byte(srcID >> 8)
	lc[8] = byte(srcID)
	parity := reedSolomon129Parity(lc[:9])
	mask := fullLCParityMaskForDataType(dataType)
	lc[9] = parity[0] ^ mask
	lc[10] = parity[1] ^ mask
	lc[11] = parity[2] ^ mask
	return lc
}

func newHeaderPacket(srcID, dstID uint32, slot int, colorCode uint8, streamID, seq uint32) proto.Packet {
	return proto.Packet{
		Signature:   "DMRD",
		Seq:         uint(seq & 0xFF),
		Src:         uint(srcID),
		Dst:         uint(dstID),
		Slot:        slot == 2,
		GroupCall:   true,
		FrameType:   2,
		DTypeOrVSeq: 1,
		StreamID:    uint(streamID),
		DMRData: bptc.BuildLCDataBurst(
			buildStandardLCBytesForDataType(srcID, dstID, true, 0x20, elements.DataTypeVoiceLCHeader),
			uint8(elements.DataTypeVoiceLCHeader),
			colorCode,
		),
	}
}

func newTerminatorPacket(srcID, dstID uint32, slot int, colorCode uint8, streamID, seq uint32) proto.Packet {
	return proto.Packet{
		Signature:   "DMRD",
		Seq:         uint(seq & 0xFF),
		Src:         uint(srcID),
		Dst:         uint(dstID),
		Slot:        slot == 2,
		GroupCall:   true,
		FrameType:   2,
		DTypeOrVSeq: 2,
		StreamID:    uint(streamID),
		DMRData: bptc.BuildLCDataBurst(
			buildStandardLCBytesForDataType(srcID, dstID, true, 0x20, elements.DataTypeTerminatorWithLC),
			uint8(elements.DataTypeTerminatorWithLC),
			colorCode,
		),
	}
}

func newVoicePacket(srcID, dstID uint32, slot int, colorCode uint8, streamID, seq uint32, voiceSeq uint8, ambe [3][]byte) proto.Packet {
	frameType := uint(0)
	if voiceSeq == 0 {
		frameType = 1
	}
	var dmr [33]byte
	copy(dmr[:], assembleVoiceBurst(ambe[0], ambe[1], ambe[2], voiceSeq, srcID, dstID, colorCode))
	return proto.Packet{
		Signature:   "DMRD",
		Seq:         uint(seq & 0xFF),
		Src:         uint(srcID),
		Dst:         uint(dstID),
		Slot:        slot == 2,
		GroupCall:   true,
		FrameType:   frameType,
		DTypeOrVSeq: uint(voiceSeq),
		StreamID:    uint(streamID),
		DMRData:     dmr,
	}
}

type bitWriter struct {
	buf    []byte
	bitPos int
}

func newBitWriter(buf []byte) *bitWriter { return &bitWriter{buf: buf} }

func (w *bitWriter) writeBits(src []byte, srcBitStart, count int) {
	for i := 0; i < count; i++ {
		readPos := srcBitStart + i
		if readPos/8 >= len(src) {
			break
		}
		bit := (src[readPos/8] >> (7 - (readPos % 8))) & 1
		if bit == 1 {
			w.buf[w.bitPos/8] |= 1 << (7 - (w.bitPos % 8))
		}
		w.bitPos++
	}
}

func assembleVoiceBurst(frame1, frame2, frame3 []byte, seq uint8, srcID, dstID uint32, colorCode uint8) []byte {
	burst := make([]byte, 33)
	bw := newBitWriter(burst)
	bw.writeBits(frame1, 0, 72)
	bw.writeBits(frame2, 0, 36)
	if seq == 0 {
		bw.writeBits(dmrSyncAudioBS, 0, 48)
	} else {
		frags := encodeEmbeddedLC(srcID, dstID)
		frag := frags[(int(seq)-1)%4]
		region := make([]byte, 6)
		copy(region[1:5], frag[:])
		bw.writeBits(region, 0, 48)
	}
	bw.writeBits(frame2, 36, 36)
	bw.writeBits(frame3, 0, 72)
	if seq >= 1 && seq <= 5 {
		embedControl(burst, colorCode, seq)
	}
	return burst
}

func assembleHeaderLikeBurst(frame1, frame2, frame3 []byte) []byte {
	burst := make([]byte, 33)
	bw := newBitWriter(burst)
	bw.writeBits(frame1, 0, 72)
	bw.writeBits(frame2, 0, 36)
	bw.writeBits([]byte{0xDF, 0xF5, 0x7D, 0x75, 0xDF, 0x5D}, 0, 48)
	bw.writeBits(frame2, 36, 36)
	bw.writeBits(frame3, 0, 72)
	return burst
}

func fullLCParityMaskForDataType(dataType elements.DataType) byte {
	switch dataType {
	case elements.DataTypeVoiceLCHeader:
		return fullLCParityMaskVoiceHeader
	case elements.DataTypeTerminatorWithLC:
		return fullLCParityMaskTerminator
	default:
		return 0x00
	}
}

func reedSolomon129Parity(data []byte) [3]byte {
	var parity [3]byte
	if len(data) != 9 {
		return parity
	}
	for i := 0; i < 9; i++ {
		feedback := data[i] ^ parity[0]
		parity[0] = parity[1] ^ gf256Mul(feedback, 0x0E)
		parity[1] = parity[2] ^ gf256Mul(feedback, 0x38)
		parity[2] = gf256Mul(feedback, 0x40)
	}
	return parity
}

func gf256Mul(a, b byte) byte {
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

func embedControl(frame []byte, colorCode uint8, seq uint8) {
	lcss := uint8(2)
	switch seq {
	case 1, 5:
		lcss = 1
	case 4:
		lcss = 3
	}
	emb := []byte{(colorCode << 4) | ((lcss << 1) & 0x06), 0}
	value := (uint32(emb[0]) >> 1) & 0x7F
	code := qr1676Table[value]
	emb[0] = uint8(code >> 8)
	emb[1] = uint8(code)
	frame[13] = (frame[13] & 0xF0) | ((emb[0] >> 4) & 0x0F)
	frame[14] = (frame[14] & 0x0F) | ((emb[0] << 4) & 0xF0)
	frame[18] = (frame[18] & 0xF0) | ((emb[1] >> 4) & 0x0F)
	frame[19] = (frame[19] & 0x0F) | ((emb[1] << 4) & 0xF0)
}

func encodeEmbeddedLC(src, dst uint32) [4][4]byte {
	var lc [9]byte
	lc[0] = 0x00
	lc[1] = 0x00
	lc[2] = 0x00
	lc[3] = byte(dst >> 16)
	lc[4] = byte(dst >> 8)
	lc[5] = byte(dst)
	lc[6] = byte(src >> 16)
	lc[7] = byte(src >> 8)
	lc[8] = byte(src)
	bits := make([]byte, 77)
	for i := 0; i < 9; i++ {
		for j := 0; j < 8; j++ {
			bits[i*8+j] = (lc[i] >> (7 - j)) & 1
		}
	}
	crc := embeddedLCCRC5(bits[:72])
	for i := 0; i < 5; i++ {
		bits[72+i] = (crc >> (4 - i)) & 1
	}
	var matrix [8][16]byte
	idx := 0
	for r := 0; r < 7; r++ {
		for c := 0; c < 11; c++ {
			matrix[r][c] = bits[idx]
			idx++
		}
	}
	for c := 0; c < 11; c++ {
		var parity byte
		for r := 0; r < 7; r++ {
			parity ^= matrix[r][c]
		}
		matrix[7][c] = parity
	}
	var out [4][4]byte
	for frag := 0; frag < 4; frag++ {
		var packed [4]byte
		for i := 0; i < 32; i++ {
			col := frag*4 + i/8
			row := i % 8
			if matrix[row][col] == 1 {
				packed[i/8] |= 1 << (7 - (i % 8))
			}
		}
		out[frag] = packed
	}
	return out
}

func embeddedLCCRC5(bits []byte) byte {
	var crc byte
	for _, bit := range bits {
		feedback := ((crc >> 4) & 1) ^ (bit & 1)
		crc = (crc << 1) & 0x1F
		if feedback == 1 {
			crc ^= 0x15
		}
	}
	return crc & 0x1F
}
