package bptc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/USA-RedDragon/dmrgo/dmr/layer2"
	intdmr "github.com/hicaoc/ipsc2mmdvm/internal/dmr"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

// TestDecodeBMBurst verifies that the corrected BPTC(196,96) decodes the
// BM-originated Voice LC Header burst to the correct src/dst addresses.
//
// From the user's capture:
//
//	BM packet outer fields: src=4604111 dst=46025
//	BM DMRData (33B) = 02 77 1C EA 22 74 07 D8 F2 F0 D6 A1 84
//	                    6D FF 57 D7 5D F5 DE
//	                    33 88 05 60 49 20 C5 01 41 C3 6E 00 F0
//
// Expected LC (12B): 00 00 20 00 B3 C9 46 40 CF C4 86 86
//
//	→ FLCO=0, FID=0, SO=0x20, dst=46025(0x00B3C9), src=4604111(0x4640CF)
func TestDecodeBMBurst(t *testing.T) {
	// 33-byte DMR burst from BM.
	dmrData := [33]byte{
		0x02, 0x77, 0x1C, 0xEA, 0x22, 0x74, 0x07, 0xD8,
		0xF2, 0xF0, 0xD6, 0xA1, 0x84, 0x6D, 0xFF, 0x57,
		0xD7, 0x5D, 0xF5, 0xDE, 0x33, 0x88, 0x05, 0x60,
		0x49, 0x20, 0xC5, 0x01, 0x41, 0xC3, 0x6E, 0x00,
		0xF0,
	}

	// Convert 33 bytes to 264 bits.
	var allBits [264]byte
	for i := 0; i < 264; i++ {
		allBits[i] = (dmrData[i/8] >> (7 - (i % 8))) & 1
	}

	// Extract 196 data bits: bits[0:97] + bits[166:263].
	var dataBits [196]byte
	copy(dataBits[:98], allBits[:98])
	copy(dataBits[98:], allBits[166:264])

	// Decode.
	info, corrected, uncorrectable := Decode(dataBits)

	t.Logf("corrected=%d uncorrectable=%v", corrected, uncorrectable)

	// Pack 96 info bits back to 12 bytes.
	var lcBytes [12]byte
	for i := 0; i < 96; i++ {
		lcBytes[i/8] |= info[i] << (7 - (i % 8))
	}

	t.Logf("LC bytes: %02X", lcBytes)

	// Parse addresses from LC.
	dst := int(lcBytes[3])<<16 | int(lcBytes[4])<<8 | int(lcBytes[5])
	src := int(lcBytes[6])<<16 | int(lcBytes[7])<<8 | int(lcBytes[8])

	if src != 4604111 {
		t.Errorf("src = %d, want 4604111", src)
	}
	if dst != 46025 {
		t.Errorf("dst = %d, want 46025", dst)
	}
}

// TestEncodeDecodeRoundtrip verifies encode→decode produces the same data.
func TestEncodeDecodeRoundtrip(t *testing.T) {
	// Build LC for src=4604111, dst=46025, group call.
	lc := [12]byte{0x00, 0x00, 0x20, 0x00, 0xB3, 0xC9, 0x46, 0x40, 0xCF, 0xC4, 0x86, 0x86}

	var infoBits [96]byte
	for i := 0; i < 96; i++ {
		infoBits[i] = (lc[i/8] >> (7 - (i % 8))) & 1
	}

	encoded := Encode(infoBits)
	decoded, _, uncorrectable := Decode(encoded)

	if uncorrectable {
		t.Fatal("roundtrip produced uncorrectable errors")
	}

	for i := 0; i < 96; i++ {
		if decoded[i] != infoBits[i] {
			t.Fatalf("bit %d mismatch: got %d want %d", i, decoded[i], infoBits[i])
		}
	}
}

// TestEncodeMatchesBM verifies that decoding the BM burst then re-encoding
// produces identical BPTC data bits (proving BM uses standard BPTC encoding).
func TestEncodeMatchesBM(t *testing.T) {
	bmDMR := [33]byte{
		0x02, 0x77, 0x1C, 0xEA, 0x22, 0x74, 0x07, 0xD8,
		0xF2, 0xF0, 0xD6, 0xA1, 0x84, 0x6D, 0xFF, 0x57,
		0xD7, 0x5D, 0xF5, 0xDE, 0x33, 0x88, 0x05, 0x60,
		0x49, 0x20, 0xC5, 0x01, 0x41, 0xC3, 0x6E, 0x00,
		0xF0,
	}

	// Extract 196 data bits from BM burst.
	var allBits [264]byte
	for i := 0; i < 264; i++ {
		allBits[i] = (bmDMR[i/8] >> (7 - (i % 8))) & 1
	}
	var bmData [196]byte
	copy(bmData[:98], allBits[:98])
	copy(bmData[98:], allBits[166:264])

	// Decode BM burst.
	info, _, _ := Decode(bmData)

	// Re-encode the decoded info bits.
	reEncoded := Encode(info)

	// Compare: re-encoded should match BM's original BPTC data bits.
	mismatches := 0
	for i := 0; i < 196; i++ {
		if reEncoded[i] != bmData[i] {
			mismatches++
		}
	}

	if mismatches > 0 {
		t.Errorf("re-encoded data differs from BM in %d bit positions", mismatches)
		for i := 0; i < 196; i++ {
			if reEncoded[i] != bmData[i] {
				t.Logf("  bit %d: got %d want %d", i, reEncoded[i], bmData[i])
			}
		}
	}
}

// TestDecodeAndReencodeBMFullBurstWithNewAlgorithm decodes BM LC with this
// package's BPTC algorithm, prints all LC fields, and verifies full-burst
// roundtrip consistency after re-encoding.
func TestDecodeAndReencodeBMFullBurstWithNewAlgorithm(t *testing.T) {
	bmDMR := [33]byte{
		0x02, 0x77, 0x1C, 0xEA, 0x22, 0x74, 0x07, 0xD8,
		0xF2, 0xF0, 0xD6, 0xA1, 0x84, 0x6D, 0xFF, 0x57,
		0xD7, 0x5D, 0xF5, 0xDE, 0x33, 0x88, 0x05, 0x60,
		0x49, 0x20, 0xC5, 0x01, 0x41, 0xC3, 0x6E, 0x00,
		0xF0,
	}

	// 33B -> 264 bits
	var bits264 [264]byte
	for i := 0; i < 264; i++ {
		bits264[i] = (bmDMR[i/8] >> (7 - (i % 8))) & 1
	}

	// Extract BPTC 196 bits: [0:98] + [166:264]
	var data196 [196]byte
	copy(data196[:98], bits264[:98])
	copy(data196[98:], bits264[166:264])

	// Decode BPTC -> 96 info bits
	info96, corrected, uncorrectable := Decode(data196)
	if uncorrectable {
		t.Fatal("new BPTC decode reports uncorrectable errors for BM header")
	}
	t.Logf("decode stats: corrected=%d uncorrectable=%v", corrected, uncorrectable)

	// Pack 96 bits -> LC bytes (12B).
	var lc [12]byte
	for i := 0; i < 96; i++ {
		lc[i/8] |= info96[i] << (7 - (i % 8))
	}

	// Parse LC fields.
	pf := (lc[0] & 0x80) != 0
	flco := lc[0] & 0x3F
	fid := lc[1]
	so := lc[2]
	dst := int(lc[3])<<16 | int(lc[4])<<8 | int(lc[5])
	src := int(lc[6])<<16 | int(lc[7])<<8 | int(lc[8])
	rs := [3]byte{lc[9], lc[10], lc[11]}

	t.Logf("decoded LC12 = %02X", lc)
	t.Logf("decoded fields: PF=%t FLCO=0x%02X FID=0x%02X SO=0x%02X dst=%d src=%d RS=%02X %02X %02X",
		pf, flco, fid, so, dst, src, rs[0], rs[1], rs[2])

	// Re-encode BPTC(196,96) from decoded info bits.
	reEncoded196 := Encode(info96)
	for i := 0; i < 196; i++ {
		if reEncoded196[i] != data196[i] {
			t.Fatalf("196-bit data mismatch at bit %d: got=%d want=%d", i, reEncoded196[i], data196[i])
		}
	}

	// Rebuild full 264 bits by preserving original slot-type+sync bits.
	var rebuilt264 [264]byte
	copy(rebuilt264[:98], reEncoded196[:98])
	copy(rebuilt264[98:166], bits264[98:166]) // slot type (20) + sync (48)
	copy(rebuilt264[166:264], reEncoded196[98:])

	// Pack rebuilt 264 bits -> 33 bytes
	var rebuiltDMR [33]byte
	for i := 0; i < 264; i++ {
		if rebuilt264[i] == 1 {
			rebuiltDMR[i/8] |= 1 << (7 - (i % 8))
		}
	}

	t.Logf("rebuilt DMRData = %02X", rebuiltDMR)
	if rebuiltDMR != bmDMR {
		for i := 0; i < 33; i++ {
			if rebuiltDMR[i] != bmDMR[i] {
				t.Logf("byte[%02d] rebuilt=%02X bm=%02X xor=%02X", i, rebuiltDMR[i], bmDMR[i], rebuiltDMR[i]^bmDMR[i])
			}
		}
		t.Fatal("full 33-byte roundtrip mismatch")
	}
}

func TestDecodeBMAndCurrentWithInternalBPTCOnly(t *testing.T) {
	// BM returned LC header DMRData (33B)
	bm := [33]byte{
		0x02, 0x77, 0x1C, 0xEA, 0x22, 0x74, 0x07, 0xD8,
		0xF2, 0xF0, 0xD6, 0xA1, 0x84, 0x6D, 0xFF, 0x57,
		0xD7, 0x5D, 0xF5, 0xDE, 0x33, 0x88, 0x05, 0x60,
		0x49, 0x20, 0xC5, 0x01, 0x41, 0xC3, 0x6E, 0x00,
		0xF0,
	}
	// Current project output LC header DMRData (33B)
	cur := [33]byte{
		0x0E, 0xB1, 0x11, 0x26, 0x02, 0x78, 0x7C, 0x60,
		0x6D, 0xB0, 0xD5, 0x00, 0x04, 0x6D, 0xFF, 0x57,
		0xD7, 0x5D, 0xF5, 0xDE, 0x32, 0x30, 0x27, 0x49,
		0x62, 0xD2, 0xA5, 0x41, 0xA9, 0x00, 0x63, 0x91,
		0x7C,
	}

	decodeParse := func(name string, d [33]byte) (lc [12]byte, ok bool) {
		lc, ok = DecodeLCFromBurst(d)
		if !ok {
			t.Logf("%s decode failed", name)
			return lc, false
		}
		pf := (lc[0] & 0x80) != 0
		flco := lc[0] & 0x3F
		fid := lc[1]
		so := lc[2]
		dst := int(lc[3])<<16 | int(lc[4])<<8 | int(lc[5])
		src := int(lc[6])<<16 | int(lc[7])<<8 | int(lc[8])
		t.Logf("%s lc12=%02X", name, lc)
		t.Logf("%s parsed: PF=%t FLCO=0x%02X FID=0x%02X SO=0x%02X dst=%d src=%d RS=%02X %02X %02X",
			name, pf, flco, fid, so, dst, src, lc[9], lc[10], lc[11])

		regen := BuildLCDataBurst(lc, 0x01, 0x01)
		match := regen == d
		t.Logf("%s regen33=%02X", name, regen)
		t.Logf("%s roundtripMatch=%v", name, match)
		return lc, true
	}

	bmLC, ok := decodeParse("BM", bm)
	if !ok {
		t.Fatal("BM decode failed")
	}
	curLC, ok := decodeParse("CUR", cur)
	if !ok {
		t.Log("CUR cannot be decoded by internal bptc DecodeLCFromBurst (expected when burst came from non-matching interleave/FEC path)")
		return
	}

	t.Logf("LC xor=%02X %02X %02X %02X %02X %02X %02X %02X %02X %02X %02X %02X",
		bmLC[0]^curLC[0], bmLC[1]^curLC[1], bmLC[2]^curLC[2], bmLC[3]^curLC[3],
		bmLC[4]^curLC[4], bmLC[5]^curLC[5], bmLC[6]^curLC[6], bmLC[7]^curLC[7],
		bmLC[8]^curLC[8], bmLC[9]^curLC[9], bmLC[10]^curLC[10], bmLC[11]^curLC[11])
}

func TestDecodeReencodeLCFromMMDVMCapture(t *testing.T) {
	if os.Getenv("DUMP_MMDVM_LC") != "1" {
		t.Skip("set DUMP_MMDVM_LC=1 to dump mmdvm capture LC decode/re-encode details")
	}

	path := resolveCapturePathBPTC(t, "mmdvm.txt")
	payloads := parseDMRDPayloadsFromCapture(t, path)
	if len(payloads) == 0 {
		t.Fatalf("no DMRD payload found in %s", path)
	}

	totalLC := 0
	exactMatch := 0
	for i, raw := range payloads {
		pkt, ok := proto.Decode(raw)
		if !ok {
			continue
		}
		if pkt.FrameType != 2 {
			continue
		}
		dt := uint8(pkt.DTypeOrVSeq & 0x0F)
		if dt != 1 && dt != 2 {
			continue
		}
		totalLC++

		lc, ok := DecodeLCFromBurst(pkt.DMRData)
		if !ok {
			t.Logf("pkt[%d] LC decode failed ft/dt=%d/%d seq=%d src=%d dst=%d", i, pkt.FrameType, pkt.DTypeOrVSeq, pkt.Seq, pkt.Src, pkt.Dst)
			continue
		}

		var burst layer2.Burst
		burst.DecodeFromBytes(pkt.DMRData)
		cc := uint8(0)
		if burst.HasSlotType {
			cc = uint8(burst.SlotType.ColorCode)
		}

		regen := BuildLCDataBurst(lc, dt, cc)
		if regen == pkt.DMRData {
			exactMatch++
		}

		t.Logf("pkt[%d] ft/dt=%d/%d seq=%d slot=%t src=%d dst=%d stream=%d cc=%d",
			i, pkt.FrameType, pkt.DTypeOrVSeq, pkt.Seq, pkt.Slot, pkt.Src, pkt.Dst, pkt.StreamID, cc)
		t.Logf("pkt[%d] raw53=% X", i, raw)
		t.Logf("pkt[%d] orig33=% X", i, pkt.DMRData)
		t.Logf("pkt[%d] lc12 =% X", i, lc)
		t.Logf("pkt[%d] regen33=% X", i, regen)
		logByteDiffBPTC(t, "mmdvm orig vs regen", i, pkt.DMRData[:], regen[:])
	}

	t.Logf("mmdvm lc summary: totalLC=%d exactMatch=%d", totalLC, exactMatch)
}

func logByteDiffBPTC(t *testing.T, title string, idx int, left, right []byte) {
	t.Helper()
	if len(left) != len(right) {
		t.Logf("%s pkt[%d] len mismatch left=%d right=%d", title, idx, len(left), len(right))
		return
	}
	t.Logf("%s pkt[%d] diff start len=%d", title, idx, len(left))
	for i := 0; i < len(left); i++ {
		x := left[i] ^ right[i]
		t.Logf("%s pkt[%d] [%02d] left=%02X right=%02X xor=%02X", title, idx, i, left[i], right[i], x)
	}
	t.Logf("%s pkt[%d] diff end", title, idx)
}

func TestValidateVoiceEmbeddedLCFromMMDVMCapture(t *testing.T) {
	if os.Getenv("DUMP_MMDVM_VOICE_EMB") != "1" {
		t.Skip("set DUMP_MMDVM_VOICE_EMB=1 to validate voice embedded LC fragments")
	}

	type streamFragments struct {
		frags [4][4]byte
		have  [4]bool
	}
	byStream := map[uint]*streamFragments{}

	path := resolveCapturePathBPTC(t, "mmdvm.txt")
	payloads := parseDMRDPayloadsFromCapture(t, path)
	if len(payloads) == 0 {
		t.Fatalf("no DMRD payload found in %s", path)
	}

	totalVoice := 0
	totalWithEmbedded := 0
	totalComplete := 0
	mismatchOuter := 0

	for i, raw := range payloads {
		pkt, ok := proto.Decode(raw)
		if !ok {
			continue
		}
		if pkt.FrameType != 0 {
			continue
		}
		totalVoice++

		var burst layer2.Burst
		burst.DecodeFromBytes(pkt.DMRData)
		if !burst.HasEmbeddedSignalling {
			continue
		}
		totalWithEmbedded++

		fragIdx, ok := embeddedFragmentIndexFromDType(pkt.DTypeOrVSeq)
		if !ok {
			continue
		}

		ss := byStream[pkt.StreamID]
		if ss == nil {
			ss = &streamFragments{}
			byStream[pkt.StreamID] = ss
		}
		ss.frags[fragIdx] = burst.PackEmbeddedSignallingData()
		ss.have[fragIdx] = true

		if !(ss.have[0] && ss.have[1] && ss.have[2] && ss.have[3]) {
			continue
		}

		totalComplete++
		lc9, rxCRC, calcCRC := decodeEmbeddedLCFromFragments(ss.frags)
		embDst := int(lc9[3])<<16 | int(lc9[4])<<8 | int(lc9[5])
		embSrc := int(lc9[6])<<16 | int(lc9[7])<<8 | int(lc9[8])
		if embSrc != int(pkt.Src) || embDst != int(pkt.Dst) {
			mismatchOuter++
		}

		t.Logf(
			"voice stream=%d pkt[%d] seq=%d dt=%d outer(src,dst)=(%d,%d) emb(src,dst)=(%d,%d) crc(rx/calc)=%02X/%02X",
			pkt.StreamID, i, pkt.Seq, pkt.DTypeOrVSeq, pkt.Src, pkt.Dst, embSrc, embDst, rxCRC, calcCRC,
		)
		t.Logf("voice stream=%d pkt[%d] raw53 =% X", pkt.StreamID, i, raw)
		t.Logf("voice stream=%d pkt[%d] orig33=% X", pkt.StreamID, i, pkt.DMRData)
		t.Logf(
			"voice stream=%d frags B-E: % X | % X | % X | % X",
			pkt.StreamID, ss.frags[0], ss.frags[1], ss.frags[2], ss.frags[3],
		)
		t.Logf("voice stream=%d embedded-lc9=% X", pkt.StreamID, lc9)

		// Start a new collection window for next B-E cycle in same stream.
		ss.have = [4]bool{}
	}

	t.Logf(
		"voice embedded summary: totalVoice=%d withEmbedded=%d completeBtoE=%d mismatchOuter=%d",
		totalVoice, totalWithEmbedded, totalComplete, mismatchOuter,
	)
	if totalComplete == 0 {
		t.Fatal("no complete B-E embedded LC fragment sets found in capture")
	}
}

func embeddedFragmentIndexFromDType(dtype uint) (int, bool) {
	switch dtype {
	case 1: // Burst B
		return 0, true
	case 2: // Burst C
		return 1, true
	case 3: // Burst D
		return 2, true
	case 4: // Burst E
		return 3, true
	default:
		return 0, false
	}
}

func decodeEmbeddedLCFromFragments(frags [4][4]byte) (lc9 [9]byte, rxCRC byte, calcCRC byte) {
	var matrix [8][16]byte
	for f := 0; f < 4; f++ {
		bitIdx := 0
		for c := 0; c < 4; c++ {
			for r := 0; r < 8; r++ {
				b := (frags[f][bitIdx/8] >> (7 - (bitIdx % 8))) & 1
				matrix[r][f*4+c] = b
				bitIdx++
			}
		}
	}

	var bits [77]byte
	idx := 0
	for r := 0; r < 7; r++ {
		for c := 0; c < 11; c++ {
			bits[idx] = matrix[r][c] & 1
			idx++
		}
	}

	for i := 0; i < 72; i++ {
		if bits[i] == 1 {
			lc9[i/8] |= 1 << (7 - (i % 8))
		}
	}
	for i := 0; i < 5; i++ {
		rxCRC |= (bits[72+i] & 1) << (4 - i)
	}
	calcCRC = intdmr.EmbeddedLCCRC5Residual(bits[:72])
	return lc9, rxCRC, calcCRC
}

func TestDecodeEmbeddedLCFromFragments_InverseCheck(t *testing.T) {
	src := uint(4604111)
	dst := uint(46025)
	frags := encodeEmbeddedLCForTest(src, dst, true)
	lc9, rxCRC, calcCRC := decodeEmbeddedLCFromFragments(frags)
	gotDst := int(lc9[3])<<16 | int(lc9[4])<<8 | int(lc9[5])
	gotSrc := int(lc9[6])<<16 | int(lc9[7])<<8 | int(lc9[8])
	if gotSrc != int(src) || gotDst != int(dst) {
		t.Fatalf("inverse decode mismatch: got src/dst=%d/%d want=%d/%d lc9=% X", gotSrc, gotDst, src, dst, lc9)
	}
	if rxCRC != calcCRC {
		t.Fatalf("inverse decode crc mismatch: rx=%02X calc=%02X lc9=% X", rxCRC, calcCRC, lc9)
	}
}

func encodeEmbeddedLCForTest(src, dst uint, groupCall bool) [4][4]byte {
	var lc [9]byte
	if groupCall {
		lc[0] = 0x00
	} else {
		lc[0] = 0x03
	}
	lc[1] = 0x00
	lc[2] = 0x20
	lc[3] = byte(dst >> 16)
	lc[4] = byte(dst >> 8)
	lc[5] = byte(dst)
	lc[6] = byte(src >> 16)
	lc[7] = byte(src >> 8)
	lc[8] = byte(src)

	var bits [77]byte
	for i := 0; i < 9; i++ {
		for j := 0; j < 8; j++ {
			bits[i*8+j] = (lc[i] >> (7 - j)) & 1
		}
	}
	crc := intdmr.EmbeddedLCCRC5Residual(bits[:72])
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
		p := byte(0)
		for r := 0; r < 7; r++ {
			p ^= matrix[r][c]
		}
		matrix[7][c] = p
	}
	for r := 0; r < 8; r++ {
		d := matrix[r][:11]
		matrix[r][11] = d[0] ^ d[1] ^ d[2] ^ d[3] ^ d[5] ^ d[7] ^ d[8]
		matrix[r][12] = d[1] ^ d[2] ^ d[3] ^ d[4] ^ d[6] ^ d[8] ^ d[9]
		matrix[r][13] = d[2] ^ d[3] ^ d[4] ^ d[5] ^ d[7] ^ d[9] ^ d[10]
		matrix[r][14] = d[0] ^ d[1] ^ d[2] ^ d[4] ^ d[6] ^ d[7] ^ d[10]
		p := byte(0)
		for c := 0; c < 15; c++ {
			p ^= matrix[r][c]
		}
		matrix[r][15] = p
	}

	var fragments [4][4]byte
	for f := 0; f < 4; f++ {
		bitIdx := 0
		for c := 0; c < 4; c++ {
			for r := 0; r < 8; r++ {
				if matrix[r][f*4+c] == 1 {
					fragments[f][bitIdx/8] |= 1 << (7 - (bitIdx % 8))
				}
				bitIdx++
			}
		}
	}
	return fragments
}

type capturePacket struct {
	payload []byte
}

func parseDMRDPayloadsFromCapture(t *testing.T, path string) [][]byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture %s: %v", path, err)
	}

	lines := strings.Split(string(raw), "\n")
	var (
		haveMeta bool
		ipPayload []byte
		out      [][]byte
	)

	flush := func() {
		if !haveMeta || len(ipPayload) < 28 {
			return
		}
		ihl := int(ipPayload[0]&0x0F) * 4
		if ihl < 20 || len(ipPayload) < ihl+8 {
			return
		}
		udpStart := ihl
		udpLen := int(ipPayload[udpStart+4])<<8 | int(ipPayload[udpStart+5])
		if udpLen < 8 {
			return
		}
		udpEnd := udpStart + udpLen
		if udpEnd > len(ipPayload) {
			udpEnd = len(ipPayload)
		}
		if udpEnd <= udpStart+8 {
			return
		}
		payload := append([]byte(nil), ipPayload[udpStart+8:udpEnd]...)
		if len(payload) >= 4 && string(payload[:4]) == "DMRD" {
			out = append(out, payload)
			return
		}
		if idx := indexMarker(payload, []byte("DMRD")); idx >= 0 && len(payload[idx:]) >= 53 {
			out = append(out, append([]byte(nil), payload[idx:]...))
		}
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, " IP ") && strings.Contains(line, " UDP, length ") {
			flush()
			haveMeta = true
			ipPayload = ipPayload[:0]
			continue
		}
		if !haveMeta || !strings.HasPrefix(line, "0x") {
			continue
		}
		row, ok := parseHexRowBPTC(line)
		if !ok {
			continue
		}
		ipPayload = append(ipPayload, row...)
	}
	flush()

	return out
}

func parseHexRowBPTC(line string) ([]byte, bool) {
	colon := strings.Index(line, ":")
	if colon < 0 || colon+1 >= len(line) {
		return nil, false
	}
	rest := strings.TrimSpace(line[colon+1:])
	parts := strings.Fields(rest)
	if len(parts) == 0 {
		return nil, false
	}
	var hexParts []string
	for _, p := range parts {
		if len(p) != 4 {
			break
		}
		if _, err := strconv.ParseUint(p, 16, 16); err != nil {
			break
		}
		hexParts = append(hexParts, p)
	}
	if len(hexParts) == 0 {
		return nil, false
	}
	row := make([]byte, 0, len(hexParts)*2)
	for _, hp := range hexParts {
		v, _ := strconv.ParseUint(hp, 16, 16)
		row = append(row, byte(v>>8), byte(v))
	}
	return row, true
}

func indexMarker(buf []byte, marker []byte) int {
	if len(marker) == 0 || len(buf) < len(marker) {
		return -1
	}
	for i := 0; i <= len(buf)-len(marker); i++ {
		match := true
		for j := 0; j < len(marker); j++ {
			if buf[i+j] != marker[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func resolveCapturePathBPTC(t *testing.T, name string) string {
	t.Helper()
	candidates := []string{
		name,
		filepath.Join("..", "..", "..", name),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("capture file not found: %s", name)
	return ""
}
