package nrl

import (
	"encoding/hex"
	"testing"

	"github.com/USA-RedDragon/dmrgo/dmr/layer2/elements"
	"github.com/hicaoc/ipsc2mmdvm/internal/dmr/bptc"
)

func TestVoiceBurstMatchesReferenceAlgorithm(t *testing.T) {
	srcID := uint32(4600553)
	dstID := uint32(46025)
	colorCode := uint8(1)
	frame1 := []byte{0x0d, 0xf8, 0xfd, 0x05, 0xbb, 0xea, 0x81, 0x72, 0x42}
	frame2 := []byte{0x72, 0x00, 0x6a, 0x6f, 0xbb, 0xea, 0x81, 0x72, 0x42}
	frame3 := []byte{0x72, 0x00, 0x6a, 0x6f, 0xbb, 0xea, 0x81, 0x72, 0x42}

	for seq := uint8(0); seq <= 5; seq++ {
		got := assembleVoiceBurst(frame1, frame2, frame3, seq, srcID, dstID, colorCode)
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
		buildStandardLCBytesForDataType(4600553, 46025, true, 0x20, elements.DataTypeTerminatorWithLC),
		uint8(elements.DataTypeTerminatorWithLC),
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
	full := buildStandardLCBytesForDataType(srcID, dstID, true, 0x20, elements.DataTypeVoiceLCHeader)
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
			frame1, frame2, frame3 := extractVoiceFramesFromBurst(raw)
			got := assembleVoiceBurst(frame1, frame2, frame3, seq, srcID, dstID, cc)
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
		frame1, frame2, frame3 := extractVoiceFramesFromBurst(raw)
		got := assembleVoiceBurst(frame1, frame2, frame3, seq, srcID, dstID, colorCode)
		if len(got) != len(raw) {
			t.Fatalf("seq=%d burst length mismatch: got=%d want=%d", seq, len(got), len(raw))
		}
		for i := range got {
			if got[i] != raw[i] {
				t.Fatalf("seq=%d mismatch at byte %d: got=%02X want=%02X", seq, i, got[i], raw[i])
			}
		}
		f1, f2, f3 := extractVoiceFramesFromBurst(raw)
		mid := extractMiddle48(raw)
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
		bw.writeBits(dmrSyncAudioBS, 0, 48)
	} else {
		frags := encodeEmbeddedLC(srcID, dstID)
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
	code := qr1676Table[value]
	emb[0] = uint8(code >> 8)
	emb[1] = uint8(code)
	frame[13] = (frame[13] & 0xF0) | ((emb[0] >> 4) & 0x0F)
	frame[14] = (frame[14] & 0x0F) | ((emb[0] << 4) & 0xF0)
	frame[18] = (frame[18] & 0xF0) | ((emb[1] >> 4) & 0x0F)
	frame[19] = (frame[19] & 0x0F) | ((emb[1] << 4) & 0xF0)
}

func assembleHeaderLikeBurstReference(frame1, frame2, frame3 []byte) []byte {
	burst := make([]byte, 33)
	bw := newBitWriter(burst)
	bw.writeBits(frame1, 0, 72)
	bw.writeBits(frame2, 0, 36)
	bw.writeBits([]byte{0xDF, 0xF5, 0x7D, 0x75, 0xDF, 0x5D}, 0, 48)
	bw.writeBits(frame2, 36, 36)
	bw.writeBits(frame3, 0, 72)
	return burst
}

func extractVoiceFramesFromBurst(burst []byte) ([]byte, []byte, []byte) {
	frame1 := make([]byte, 9)
	frame2 := make([]byte, 9)
	frame3 := make([]byte, 9)
	// Frame 1 from bits 0..71.
	copyBits(frame1, 0, burst, 0, 72)
	// Frame 2 split across bits 72..107 and 156..191.
	copyBits(frame2, 0, burst, 72, 36)
	copyBits(frame2, 36, burst, 156, 36)
	// Frame 3 from bits 192..263.
	copyBits(frame3, 0, burst, 192, 72)
	return frame1, frame2, frame3
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

func extractMiddle48(burst []byte) []byte {
	out := make([]byte, 6)
	copyBits(out, 0, burst, 108, 48)
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
