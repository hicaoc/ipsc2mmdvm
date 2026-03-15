package proto

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

var refQR1676Table = []uint16{
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

func TestCompareCaptureFilesWithReferenceAlgorithm(t *testing.T) {
	type one struct {
		name string
		path string
	}
	files := []one{
		{name: "nrldmr(normal)", path: filepath.Join("..", "..", "..", "nrldmr.txt")},
		{name: "nrldrm-b(current)", path: filepath.Join("..", "..", "..", "nrldrm-b.txt")},
	}

	for _, f := range files {
		raw, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatalf("read %s: %v", f.path, err)
		}
		data := extractDMRDPacketsFromTCPDumpHex(string(raw))
		if len(data) == 0 {
			t.Fatalf("%s no DMRD packets", f.name)
		}
		t.Logf("[%s] packets=%d", f.name, len(data))

		var (
			headerCount     int
			voiceSyncCount  int
			voiceDataCount  int
			terminatorCount int
			voiceInvalid    int
			orderIssues     int
		)

		var prevSeq int = -1
		var started bool
		var expectVSeq uint = 0

		for i, rawPkt := range data {
			pkt, ok := Decode(rawPkt)
			if !ok {
				t.Fatalf("[%s] idx=%d decode failed", f.name, i)
			}

			seq := int(pkt.Seq)
			if prevSeq >= 0 {
				want := (prevSeq + 1) & 0xFF
				if seq != want {
					orderIssues++
					t.Logf("[%s] idx=%d sequence jump prev=%d now=%d", f.name, i, prevSeq, seq)
				}
			}
			prevSeq = seq

			switch {
			case pkt.FrameType == 2 && pkt.DTypeOrVSeq == 1:
				headerCount++
				started = true
				expectVSeq = 0
			case pkt.FrameType == 2 && pkt.DTypeOrVSeq == 2:
				terminatorCount++
				started = false
			case pkt.FrameType == 1 && pkt.DTypeOrVSeq == 0:
				voiceSyncCount++
				if started && expectVSeq != 0 {
					orderIssues++
					t.Logf("[%s] idx=%d unexpected sync, expected vseq=%d", f.name, i, expectVSeq)
				}
				okVoice, cc := validateVoiceWithReference(pkt)
				if !okVoice {
					voiceInvalid++
					t.Logf("[%s] idx=%d voice-sync payload fails reference rebuild", f.name, i)
				} else {
					t.Logf("[%s] idx=%d voice-sync reference-cc=%d", f.name, i, cc)
				}
				expectVSeq = 1
			case pkt.FrameType == 0 && pkt.DTypeOrVSeq >= 1 && pkt.DTypeOrVSeq <= 5:
				voiceDataCount++
				if started && pkt.DTypeOrVSeq != expectVSeq {
					orderIssues++
					t.Logf("[%s] idx=%d vseq order mismatch got=%d expected=%d", f.name, i, pkt.DTypeOrVSeq, expectVSeq)
				}
				okVoice, cc := validateVoiceWithReference(pkt)
				if !okVoice {
					voiceInvalid++
					t.Logf("[%s] idx=%d voice-data vseq=%d fails reference rebuild", f.name, i, pkt.DTypeOrVSeq)
				} else if i < 12 {
					t.Logf("[%s] idx=%d voice-data vseq=%d reference-cc=%d", f.name, i, pkt.DTypeOrVSeq, cc)
				}
				expectVSeq = (pkt.DTypeOrVSeq + 1) % 6
				if expectVSeq == 0 {
					expectVSeq = 0
				}
			default:
				t.Logf("[%s] idx=%d unsupported frame ft/v=%d/%d", f.name, i, pkt.FrameType, pkt.DTypeOrVSeq)
			}
		}

		if started && terminatorCount == 0 {
			t.Logf("[%s] ERROR: stream ended in capture without terminator(2/2)", f.name)
		}
		t.Logf("[%s] summary: header=%d voiceSync=%d voiceData=%d terminator=%d invalidVoice=%d orderIssues=%d",
			f.name, headerCount, voiceSyncCount, voiceDataCount, terminatorCount, voiceInvalid, orderIssues)
	}
}

func validateVoiceWithReference(pkt Packet) (bool, uint8) {
	dmr := pkt.DMRData[:]
	f1, f2, f3 := refExtractVoiceFrames(dmr)
	seq := uint8(pkt.DTypeOrVSeq)
	if seq == 0 {
		rebuilt := refAssembleVoiceBurst(f1, f2, f3, 0, uint32(pkt.Src), uint32(pkt.Dst), 0)
		return bytes.Equal(rebuilt, dmr), 0
	}
	for cc := uint8(0); cc <= 15; cc++ {
		rebuilt := refAssembleVoiceBurst(f1, f2, f3, seq, uint32(pkt.Src), uint32(pkt.Dst), cc)
		if bytes.Equal(rebuilt, dmr) {
			return true, cc
		}
	}
	return false, 0
}

func refExtractVoiceFrames(burst []byte) ([]byte, []byte, []byte) {
	f1 := make([]byte, 9)
	f2 := make([]byte, 9)
	f3 := make([]byte, 9)
	refCopyBits(f1, 0, burst, 0, 72)
	refCopyBits(f2, 0, burst, 72, 36)
	refCopyBits(f2, 36, burst, 156, 36)
	refCopyBits(f3, 0, burst, 192, 72)
	return f1, f2, f3
}

func refAssembleVoiceBurst(frame1, frame2, frame3 []byte, seq uint8, src, dst uint32, colorCode uint8) []byte {
	const syncAudioBS = "\x75\x5F\xD7\xDF\x75\xF7"
	out := make([]byte, 33)
	refCopyBits(out, 0, frame1, 0, 72)
	refCopyBits(out, 72, frame2, 0, 36)
	if seq == 0 {
		refCopyBits(out, 108, []byte(syncAudioBS), 0, 48)
	} else {
		frags := refEncodeEmbeddedLC(src, dst)
		frag := frags[(int(seq)-1)%4]
		region := make([]byte, 6)
		copy(region[1:5], frag[:])
		refCopyBits(out, 108, region, 0, 48)
	}
	refCopyBits(out, 156, frame2, 36, 36)
	refCopyBits(out, 192, frame3, 0, 72)
	if seq >= 1 && seq <= 5 {
		refEmbedControl(out, colorCode, seq)
	}
	return out
}

func refEmbedControl(frame []byte, colorCode uint8, seq uint8) {
	lcss := uint8(2)
	switch seq {
	case 1, 5:
		lcss = 1
	case 4:
		lcss = 3
	}
	emb0 := (colorCode << 4) | ((lcss << 1) & 0x06)
	code := refQR1676Table[(uint32(emb0)>>1)&0x7F]
	b0 := uint8(code >> 8)
	b1 := uint8(code)
	frame[13] = (frame[13] & 0xF0) | ((b0 >> 4) & 0x0F)
	frame[14] = (frame[14] & 0x0F) | ((b0 << 4) & 0xF0)
	frame[18] = (frame[18] & 0xF0) | ((b1 >> 4) & 0x0F)
	frame[19] = (frame[19] & 0x0F) | ((b1 << 4) & 0xF0)
}

func refEncodeEmbeddedLC(src, dst uint32) [4][4]byte {
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
	crc := refEmbeddedLCCRC5(bits[:72])
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

func refEmbeddedLCCRC5(bits []byte) byte {
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

func refCopyBits(dst []byte, dstBitStart int, src []byte, srcBitStart int, count int) {
	for i := 0; i < count; i++ {
		sPos := srcBitStart + i
		dPos := dstBitStart + i
		bit := (src[sPos/8] >> (7 - (sPos % 8))) & 1
		if bit == 1 {
			dst[dPos/8] |= 1 << (7 - (dPos % 8))
		}
	}
}
