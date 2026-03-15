package proto

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestNRLDMRTraceDecodeReencode(t *testing.T) {
	tracePath := filepath.Join("..", "..", "..", "nrldmr.txt")
	raw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	packets := extractDMRDPacketsFromTCPDumpHex(string(raw))
	if len(packets) == 0 {
		datagrams := extractDatagramsFromTCPDumpHex(string(raw))
		t.Logf("debug: datagrams=%d", len(datagrams))
		for i := 0; i < len(datagrams) && i < 3; i++ {
			preview := datagrams[i]
			if len(preview) > 80 {
				preview = preview[:80]
			}
			t.Logf("debug: datagram[%d] len=%d idxDMRD=%d prefix=%X", i, len(datagrams[i]), bytes.Index(datagrams[i], []byte("DMRD")), preview)
		}
		t.Fatalf("no DMRD packets parsed from %s", tracePath)
	}

	matched := 0
	mismatched := 0
	for i, p := range packets {
		decoded, ok := Decode(p)
		if !ok {
			t.Fatalf("packet[%d] failed Decode, len=%d", i, len(p))
		}
		reencoded := decoded.Encode()
		if bytes.Equal(p, reencoded) {
			matched++
			if i < 6 {
				t.Logf("packet[%d] match len=%d seq=%d bits=%02X stream=%08X",
					i, len(p), p[4], p[15], uint32(p[16])<<24|uint32(p[17])<<16|uint32(p[18])<<8|uint32(p[19]))
			}
			continue
		}
		mismatched++
		diffAt := firstDiff(p, reencoded)
		origAt := byte(0x00)
		regenAt := byte(0x00)
		if diffAt >= 0 && diffAt < len(p) {
			origAt = p[diffAt]
		}
		if diffAt >= 0 && diffAt < len(reencoded) {
			regenAt = reencoded[diffAt]
		}
		origTail0, origTail1 := tail2(p)
		regenTail0, regenTail1 := tail2(reencoded)
		t.Logf("packet[%d] mismatch len=%d diffAt=%d orig=%02X regen=%02X seq=%d bits=%02X stream=%08X",
			i, len(p), diffAt, origAt, regenAt, p[4], p[15], uint32(p[16])<<24|uint32(p[17])<<16|uint32(p[18])<<8|uint32(p[19]))
		t.Logf("packet[%d] tails orig=%02X%02X regen=%02X%02X", i, origTail0, origTail1, regenTail0, regenTail1)
	}
	t.Logf("summary: total=%d matched=%d mismatched=%d", len(packets), matched, mismatched)
}

func extractDMRDPacketsFromTCPDumpHex(input string) [][]byte {
	datagrams := extractDatagramsFromTCPDumpHex(input)
	var out [][]byte
	for _, dg := range datagrams {
		idx := bytes.Index(dg, []byte("DMRD"))
		if idx < 0 {
			continue
		}
		switch {
		case len(dg) >= idx+55:
			out = append(out, append([]byte(nil), dg[idx:idx+55]...))
		case len(dg) >= idx+53:
			out = append(out, append([]byte(nil), dg[idx:idx+53]...))
		}
	}
	return out
}

func extractDatagramsFromTCPDumpHex(input string) [][]byte {
	lines := strings.Split(input, "\n")
	var datagrams [][]byte
	var cur []byte
	capturing := false

	flush := func() {
		if len(cur) > 0 {
			datagrams = append(datagrams, append([]byte(nil), cur...))
		}
		cur = cur[:0]
		capturing = false
	}

	for _, line := range lines {
		l := strings.TrimRight(line, "\r")
		if strings.Contains(l, " UDP, length ") {
			flush()
			capturing = true
			continue
		}
		if !capturing {
			continue
		}
		if !strings.Contains(l, "0x") || !strings.Contains(l, ":") {
			continue
		}
		colon := strings.Index(l, ":")
		if colon < 0 || colon+1 >= len(l) {
			continue
		}
		hexField := strings.TrimSpace(l[colon+1:])
		// Keep only 16-bit hex groups before ASCII preview.
		parts := strings.Fields(hexField)
		for _, p := range parts {
			if len(p) != 4 && len(p) != 2 {
				break
			}
			if !isHexN(p) {
				break
			}
			b, err := hex.DecodeString(p)
			if err != nil {
				break
			}
			cur = append(cur, b...)
		}
	}
	flush()
	return datagrams
}

func isHexN(s string) bool {
	if len(s) != 4 && len(s) != 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

func Test_extractDMRDPacketsFromTCPDumpHex_smoke(t *testing.T) {
	in := `14:00:00 IP x > y: UDP, length 55
        0x0010:  0000 0000 444d 5244 0102 0304 0506 0708
        0x0020:  090a 0b0c 0d0e 0f10 1112 1314 1516 1718
        0x0030:  191a 1b1c 1d1e 1f20 2122 2324 2526 2728
        0x0040:  292a 2b2c 2d2e 2f30 3132 3334 3536 37`
	got := extractDMRDPacketsFromTCPDumpHex(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(got))
	}
	if len(got[0]) != 55 {
		t.Fatalf("expected 55 bytes, got %d", len(got[0]))
	}
	if string(got[0][:4]) != "DMRD" {
		t.Fatalf("expected DMRD signature, got %q", fmt.Sprintf("%x", got[0][:4]))
	}
}

func TestCompareNRLDMRFiles(t *testing.T) {
	fileA := filepath.Join("..", "..", "..", "nrldmr.txt")
	fileB := filepath.Join("..", "..", "..", "nrldrm-b.txt")
	rawA, err := os.ReadFile(fileA)
	if err != nil {
		t.Fatalf("read %s: %v", fileA, err)
	}
	rawB, err := os.ReadFile(fileB)
	if err != nil {
		t.Fatalf("read %s: %v", fileB, err)
	}
	pktsA := extractDMRDPacketsFromTCPDumpHex(string(rawA))
	pktsB := extractDMRDPacketsFromTCPDumpHex(string(rawB))
	t.Logf("parsed packets: %s=%d, %s=%d", filepath.Base(fileA), len(pktsA), filepath.Base(fileB), len(pktsB))
	if len(pktsA) == 0 || len(pktsB) == 0 {
		t.Fatalf("empty parsed packets: a=%d b=%d", len(pktsA), len(pktsB))
	}

	type key struct {
		src, dst, rpt, stream uint
	}
	countByKey := func(pkts [][]byte) map[key]int {
		m := map[key]int{}
		for _, p := range pkts {
			d, ok := Decode(p)
			if !ok {
				continue
			}
			m[key{src: d.Src, dst: d.Dst, rpt: d.Repeater, stream: d.StreamID}]++
		}
		return m
	}
	mA := countByKey(pktsA)
	mB := countByKey(pktsB)
	var onlyA []string
	for k, v := range mA {
		if _, ok := mB[k]; !ok {
			onlyA = append(onlyA, fmt.Sprintf("A-only src=%d dst=%d rpt=%d stream=%08X count=%d", k.src, k.dst, k.rpt, k.stream, v))
		}
	}
	var onlyB []string
	for k, v := range mB {
		if _, ok := mA[k]; !ok {
			onlyB = append(onlyB, fmt.Sprintf("B-only src=%d dst=%d rpt=%d stream=%08X count=%d", k.src, k.dst, k.rpt, k.stream, v))
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	for _, s := range onlyA {
		t.Log(s)
	}
	for _, s := range onlyB {
		t.Log(s)
	}

	n := len(pktsA)
	if len(pktsB) < n {
		n = len(pktsB)
	}
	for i := 0; i < n; i++ {
		a := pktsA[i]
		b := pktsB[i]
		da, oka := Decode(a)
		db, okb := Decode(b)
		if !oka || !okb {
			t.Logf("idx=%d decode failed oka=%v okb=%v lenA=%d lenB=%d", i, oka, okb, len(a), len(b))
			continue
		}
		diff := byteDiffCount(a, b)
		tailA0, tailA1 := tail2(a)
		tailB0, tailB1 := tail2(b)
		if i < 20 || diff > 0 {
			t.Logf("idx=%d diffBytes=%d | A(seq=%d bits=%02X ft/v=%d/%d rpt=%d stream=%08X tail=%02X%02X) | B(seq=%d bits=%02X ft/v=%d/%d rpt=%d stream=%08X tail=%02X%02X)",
				i, diff,
				da.Seq, a[15], da.FrameType, da.DTypeOrVSeq, da.Repeater, da.StreamID, tailA0, tailA1,
				db.Seq, b[15], db.FrameType, db.DTypeOrVSeq, db.Repeater, db.StreamID, tailB0, tailB1,
			)
		}
	}
}

func byteDiffCount(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	c := 0
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			c++
		}
	}
	if len(a) > n {
		c += len(a) - n
	}
	if len(b) > n {
		c += len(b) - n
	}
	return c
}

func tail2(data []byte) (byte, byte) {
	if len(data) >= 55 {
		return data[53], data[54]
	}
	return 0x00, 0x00
}
