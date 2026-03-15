package nrl

import (
	"encoding/hex"
	"testing"

	intdmr "github.com/hicaoc/ipsc2mmdvm/internal/dmr"
	"github.com/hicaoc/ipsc2mmdvm/internal/dmr/bptc"
)

var dmrSyncAudioBSReference = []byte{0x75, 0x5F, 0xD7, 0xDF, 0x75, 0xF7}

var qr1676TableReference = []uint16{
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

func TestVoiceBurstMatchesReferenceAlgorithm(t *testing.T) {
	srcID := uint32(4600553)
	dstID := uint32(46025)
	colorCode := uint8(1)
	frame1 := []byte{0x0d, 0xf8, 0xfd, 0x05, 0xbb, 0xea, 0x81, 0x72, 0x42}
	frame2 := []byte{0x72, 0x00, 0x6a, 0x6f, 0xbb, 0xea, 0x81, 0x72, 0x42}
	frame3 := []byte{0x72, 0x00, 0x6a, 0x6f, 0xbb, 0xea, 0x81, 0x72, 0x42}

	for seq := uint8(0); seq <= 5; seq++ {
		got := intdmr.AssembleVoiceBurst(frame1, frame2, frame3, seq, srcID, dstID, colorCode)
		want := assembleVoiceBurstReference(frame1, frame2, frame3, seq, srcID, dstID, colorCode)
		t.Logf("voice seq=%d\n  project =%X\n  reference=%X", seq, got, want)
		if len(got) != len(want) {
			t.Fatalf("seq=%d burst length mismatch: got=%d want=%d", seq, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("seq=%d mismatch at byte %d: got=%02X want=%02X", seq, i, got[i], want[i])
			}
		}
	}
}

func TestTerminatorBurstMatchesReferenceAlgorithm(t *testing.T) {
	pkt := newTerminatorPacket(4600553, 46025, 1, 1, 0x0DF8FD05, 6)
	want := bptc.BuildLCDataBurst(
		intdmr.BuildStandardLCBytesForDataType(4600553, 46025, true, 0x20, intdmr.DataTypeTerminatorWithLC),
		uint8(intdmr.DataTypeTerminatorWithLC),
		1,
	)
	t.Logf("terminator\n  project =%X\n  reference=%X", pkt.DMRData, want)
	for i := range pkt.DMRData {
		if pkt.DMRData[i] != want[i] {
			t.Fatalf("terminator mismatch at byte %d: got=%02X want=%02X", i, pkt.DMRData[i], want[i])
		}
	}
}

func TestHeaderLCFieldsMatchReferenceAlgorithm(t *testing.T) {
	srcID := uint32(4600553)
	dstID := uint32(46025)
	full := intdmr.BuildStandardLCBytesForDataType(uint(srcID), uint(dstID), true, 0x20, intdmr.DataTypeVoiceLCHeader)
	want := buildLCReference(srcID, dstID, true, 0x20)
	t.Logf("header-lc(first9)\n  project =%X\n  reference=%X", full[:9], want)
	for i := 0; i < 9; i++ {
		if full[i] != want[i] {
			t.Fatalf("lc byte mismatch at %d: got=%02X want=%02X", i, full[i], want[i])
		}
	}
}

func TestVoiceBurstMatchesCapturedVectors(t *testing.T) {
	srcID := uint32(4600553)
	dstID := uint32(46025)

	captured := map[uint8]string{
		0: "db8ac634650c916ae3fb8ae4562755fd7df75f7748c56ae399e883520011116a2b",
		1: "99e883520011116a2bbbea81724020a11120c73272006a6fbbea81724272006a6f",
		2: "bbea81724272006a6fbbea8172404051b1114e5272006a6fbbea81724272006a6f",
		3: "bbea81724272006a6fbbea8172404110c2d00e5272006a6fbbea81724272006a6f",
		4: "bbea81724272006a6fbbea81724060000000096272006a6fbbea81724272006a6f",
		5: "bbea81724272006a6fbbea81724020a11120c73272006a6fbbea81724272006a6f",
	}

	colorCode := uint8(0)
	foundCC := false
	for cc := uint8(0); cc <= 15; cc++ {
		ok := true
		for seq := uint8(1); seq <= 5; seq++ {
			raw, err := hex.DecodeString(captured[seq])
			if err != nil {
				t.Fatalf("seq=%d decode hex: %v", seq, err)
			}
			frame1, frame2, frame3 := intdmr.ExtractVoiceFramesFromBurst(raw)
			got := intdmr.AssembleVoiceBurst(frame1, frame2, frame3, seq, srcID, dstID, cc)
			if len(got) != len(raw) {
				ok = false
				break
			}
			for i := range got {
				if got[i] != raw[i] {
					ok = false
					break
				}
			}
			if !ok {
				break
			}
		}
		if ok {
			colorCode = cc
			foundCC = true
			break
		}
	}
	if !foundCC {
		t.Fatal("failed to infer color code from captured seq1..5 vectors")
	}
	t.Logf("inferred colorCode(CC)=%d from captured vectors", colorCode)

	for seq := uint8(0); seq <= 5; seq++ {
		raw, err := hex.DecodeString(captured[seq])
		if err != nil {
			t.Fatalf("seq=%d decode hex: %v", seq, err)
		}
		if len(raw) != 33 {
			t.Fatalf("seq=%d expected 33 bytes, got %d", seq, len(raw))
		}
		frame1, frame2, frame3 := intdmr.ExtractVoiceFramesFromBurst(raw)
		got := intdmr.AssembleVoiceBurst(frame1, frame2, frame3, seq, srcID, dstID, colorCode)
		if len(got) != len(raw) {
			t.Fatalf("seq=%d burst length mismatch: got=%d want=%d", seq, len(got), len(raw))
		}
		for i := range got {
			if got[i] != raw[i] {
				t.Fatalf("seq=%d mismatch at byte %d: got=%02X want=%02X", seq, i, got[i], raw[i])
			}
		}
		f1, f2, f3 := intdmr.ExtractVoiceFramesFromBurst(raw)
		mid := intdmr.ExtractMiddle48(raw)
		t.Logf("seq=%d frame1=%X frame2=%X frame3=%X middle48=%X emb_nibbles=[b13=%X b14=%X b18=%X b19=%X]",
			seq, f1, f2, f3, mid, raw[13], raw[14], raw[18], raw[19])
	}
}

func assembleVoiceBurstReference(frame1, frame2, frame3 []byte, seq uint8, srcID, dstID uint32, colorCode uint8) []byte {
	burst := make([]byte, 33)
	bw := newBitWriter(burst)
	bw.writeBits(frame1, 0, 72)
	bw.writeBits(frame2, 0, 36)
	if seq == 0 {
		bw.writeBits(dmrSyncAudioBSReference, 0, 48)
	} else {
		frags := encodeEmbeddedLCReference(srcID, dstID)
		frag := frags[(int(seq)-1)%4]
		embRegion := make([]byte, 6)
		copy(embRegion[1:5], frag[:])
		bw.writeBits(embRegion, 0, 48)
	}
	bw.writeBits(frame2, 36, 36)
	bw.writeBits(frame3, 0, 72)
	if seq >= 1 && seq <= 5 {
		embedControlReference(burst, colorCode, seq)
	}
	return burst
}

func embedControlReference(frame []byte, colorCode uint8, seq uint8) {
	lcss := uint8(2)
	switch seq {
	case 1, 5:
		lcss = 1
	case 4:
		lcss = 3
	}
	emb := []byte{(colorCode << 4) | ((lcss << 1) & 0x06), 0}
	value := (uint32(emb[0]) >> 1) & 0x7F
	code := qr1676TableReference[value]
	emb[0] = uint8(code >> 8)
	emb[1] = uint8(code)
	frame[13] = (frame[13] & 0xF0) | ((emb[0] >> 4) & 0x0F)
	frame[14] = (frame[14] & 0x0F) | ((emb[0] << 4) & 0xF0)
	frame[18] = (frame[18] & 0xF0) | ((emb[1] >> 4) & 0x0F)
	frame[19] = (frame[19] & 0x0F) | ((emb[1] << 4) & 0xF0)
}

func copyBits(dst []byte, dstBitStart int, src []byte, srcBitStart int, count int) {
	for i := 0; i < count; i++ {
		srcPos := srcBitStart + i
		dstPos := dstBitStart + i
		bit := (src[srcPos/8] >> (7 - (srcPos % 8))) & 1
		if bit == 1 {
			dst[dstPos/8] |= 1 << (7 - (dstPos % 8))
		}
	}
}

func encodeEmbeddedLCReference(src, dst uint32) [4][4]byte {
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
	crc := intdmr.EmbeddedLCCRC5(bits[:72])
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

func buildLCReference(srcID, dstID uint32, isGroup bool, so uint8) []byte {
	lc := make([]byte, 9)
	if isGroup {
		lc[0] = 0x00
	} else {
		lc[0] = 0x03
	}
	lc[1] = 0x00
	lc[2] = so
	lc[3] = byte(dstID >> 16)
	lc[4] = byte(dstID >> 8)
	lc[5] = byte(dstID)
	lc[6] = byte(srcID >> 16)
	lc[7] = byte(srcID >> 8)
	lc[8] = byte(srcID)
	return lc
}
