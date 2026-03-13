// Package bptc implements BPTC(196,96) encoding and decoding per
// ETSI TS 102 361-1 V2.5.1, Annex B.
//
// The 196-bit block is laid out as:
//
//	bit 0           : reserved R(0)
//	bits 1  – 15    : row  0  (columns 0-14)
//	bits 16 – 30    : row  1
//	…
//	bits 181 – 195  : row 12
//
// Rows 0-8 carry information + Hamming(15,11,3) parity.
// Rows 9-12 carry Hamming(13,9,3) column-parity.
// Row 0 has only 8 data bits (columns 3-10); columns 0-2 are reserved.
// Rows 1-8 have 11 data bits each (columns 0-10).
// Total data: 8 + 8×11 = 96 bits.
//
// Interleaving per B.1.1: i(a) = (a × 181) mod 196, 0 ≤ a ≤ 195.
package bptc

// Encode encodes 96 info bits into 196 interleaved bits using BPTC(196,96).
func Encode(data [96]byte) [196]byte {
	var grid [196]byte

	// Place 96 data bits into the grid.
	idx := 0

	// Row 0: data at columns 3-10 → grid[1+3 .. 1+10] = grid[4..11]
	for c := 3; c <= 10; c++ {
		grid[1+c] = data[idx]
		idx++
	}

	// Rows 1-8: data at columns 0-10
	for r := 1; r <= 8; r++ {
		base := 1 + r*15 // row r starts at grid[1 + r*15]
		for c := 0; c <= 10; c++ {
			grid[base+c] = data[idx]
			idx++
		}
	}

	// Row Hamming(15,11) parity for rows 0-8.
	for r := 0; r < 9; r++ {
		computeRowParity(&grid, r)
	}

	// Column Hamming(13,9) parity for columns 0-14.
	for c := 0; c < 15; c++ {
		computeColParity(&grid, c)
	}

	// Interleave: interleaved[i(a)] = grid[a], where i(a) = (a×181) mod 196.
	var out [196]byte
	for a := 0; a < 196; a++ {
		out[(a*181)%196] = grid[a]
	}
	return out
}

// Decode deinterleaves and error-corrects 196 interleaved bits, returning
// 96 data bits plus FEC statistics.
func Decode(bits [196]byte) (data [96]byte, corrected int, uncorrectable bool) {
	// Deinterleave: grid[a] = bits[i(a)] = bits[(a×181) mod 196].
	var grid [196]byte
	for a := 0; a < 196; a++ {
		grid[a] = bits[(a*181)%196]
	}

	// Hamming error correction: rows then columns (iterative).
	grid, corrected, uncorrectable = hammingCorrect(grid)

	// Extract 96 data bits.
	idx := 0

	// Row 0: columns 3-10
	for c := 3; c <= 10; c++ {
		data[idx] = grid[1+c]
		idx++
	}

	// Rows 1-8: columns 0-10
	for r := 1; r <= 8; r++ {
		base := 1 + r*15
		for c := 0; c <= 10; c++ {
			data[idx] = grid[base+c]
			idx++
		}
	}

	return data, corrected, uncorrectable
}

// ---------- Hamming(15,11,3) row parity ----------

func computeRowParity(grid *[196]byte, r int) {
	base := 1 + r*15
	d := grid[base : base+11]

	grid[base+11] = d[0] ^ d[1] ^ d[2] ^ d[3] ^ d[5] ^ d[7] ^ d[8]
	grid[base+12] = d[1] ^ d[2] ^ d[3] ^ d[4] ^ d[6] ^ d[8] ^ d[9]
	grid[base+13] = d[2] ^ d[3] ^ d[4] ^ d[5] ^ d[7] ^ d[9] ^ d[10]
	grid[base+14] = d[0] ^ d[1] ^ d[2] ^ d[4] ^ d[6] ^ d[7] ^ d[10]
}

// ---------- Hamming(13,9,3) column parity ----------

func computeColParity(grid *[196]byte, c int) {
	// Collect the 9 data rows.
	var d [9]byte
	for r := 0; r < 9; r++ {
		d[r] = grid[1+r*15+c]
	}

	// Parity rows 9-12.
	grid[1+9*15+c] = d[0] ^ d[1] ^ d[3] ^ d[5] ^ d[6]
	grid[1+10*15+c] = d[0] ^ d[1] ^ d[2] ^ d[4] ^ d[6] ^ d[7]
	grid[1+11*15+c] = d[0] ^ d[1] ^ d[2] ^ d[3] ^ d[5] ^ d[7] ^ d[8]
	grid[1+12*15+c] = d[0] ^ d[2] ^ d[4] ^ d[5] ^ d[8]
}

// ---------- Error correction ----------

// hamming15_11SyndromeTable maps 4-bit syndrome → error position (0-14).
// -1 means no single-bit error.
var hamming15_11SyndromeTable = [16]int{
	-1, 11, 12, 8, 13, 5, 9, 3,
	14, 0, 6, 1, 10, 7, 4, 2,
}

// hamming13_9SyndromeTable maps 4-bit syndrome → error position (0-12).
var hamming13_9SyndromeTable = [16]int{
	-1, 9, 10, 6, 11, 3, 7, 1,
	12, -1, 4, -1, 8, 5, 2, 0,
}

func hammingCorrect(grid [196]byte) ([196]byte, int, bool) {
	total := 0
	unc := false

	// Row correction (Hamming 15,11).
	for r := 0; r < 9; r++ {
		base := 1 + r*15
		var row [15]byte
		copy(row[:], grid[base:base+15])

		s := rowSyndrome(row)
		if s != 0 {
			pos := hamming15_11SyndromeTable[s]
			if pos >= 0 {
				grid[base+pos] ^= 1
				total++
			} else {
				unc = true
			}
		}
	}

	// Column correction (Hamming 13,9).
	for c := 0; c < 15; c++ {
		var col [13]byte
		for r := 0; r < 13; r++ {
			col[r] = grid[1+r*15+c]
		}

		s := colSyndrome(col)
		if s != 0 {
			pos := hamming13_9SyndromeTable[s]
			if pos >= 0 {
				grid[1+pos*15+c] ^= 1
				total++
			} else {
				unc = true
			}
		}
	}

	return grid, total, unc
}

func rowSyndrome(b [15]byte) int {
	s0 := b[0] ^ b[1] ^ b[2] ^ b[3] ^ b[5] ^ b[7] ^ b[8] ^ b[11]
	s1 := b[1] ^ b[2] ^ b[3] ^ b[4] ^ b[6] ^ b[8] ^ b[9] ^ b[12]
	s2 := b[2] ^ b[3] ^ b[4] ^ b[5] ^ b[7] ^ b[9] ^ b[10] ^ b[13]
	s3 := b[0] ^ b[1] ^ b[2] ^ b[4] ^ b[6] ^ b[7] ^ b[10] ^ b[14]
	return int(s0) | int(s1)<<1 | int(s2)<<2 | int(s3)<<3
}

func colSyndrome(b [13]byte) int {
	s0 := b[0] ^ b[1] ^ b[3] ^ b[5] ^ b[6] ^ b[9]
	s1 := b[0] ^ b[1] ^ b[2] ^ b[4] ^ b[6] ^ b[7] ^ b[10]
	s2 := b[0] ^ b[1] ^ b[2] ^ b[3] ^ b[5] ^ b[7] ^ b[8] ^ b[11]
	s3 := b[0] ^ b[2] ^ b[4] ^ b[5] ^ b[8] ^ b[12]
	return int(s0) | int(s1)<<1 | int(s2)<<2 | int(s3)<<3
}

// ---------- Burst building ----------

// BsSourcedData is the 48-bit BS-sourced data sync pattern (ETSI TS 102 361-1 Table 9.2).
const bsSourcedData int64 = 0xDFF57D75DF5D

// BuildLCDataBurst builds a complete 33-byte DMR data burst from 12 LC bytes,
// a data type, and a color code. This is a drop-in replacement for
// layer2.BuildLCDataBurst with correct BPTC(196,96) interleaving.
func BuildLCDataBurst(lcBytes [12]byte, dataType uint8, colorCode uint8) [33]byte {
	// 1. Convert 12 LC bytes → 96 info bits.
	var infoBits [96]byte
	for i := 0; i < 12; i++ {
		for j := 0; j < 8; j++ {
			infoBits[i*8+j] = (lcBytes[i] >> (7 - j)) & 1
		}
	}

	// 2. BPTC(196,96) encode.
	encoded := Encode(infoBits)

	// 3. Assemble 264-bit burst.
	var bits [264]byte

	// Data part 1: encoded[0:97] → bits[0:97]
	copy(bits[:98], encoded[:98])
	// Data part 2: encoded[98:195] → bits[166:263]
	copy(bits[166:264], encoded[98:196])

	// 4. Slot type: Golay(20,8) encode CC(4)|DataType(4).
	slotInput := (colorCode&0xF)<<4 | (dataType & 0xF)
	slotBits := golayEncode208(slotInput)
	copy(bits[98:108], slotBits[:10])
	copy(bits[156:166], slotBits[10:20])

	// 5. SYNC pattern: BS-sourced data.
	syncVal := bsSourcedData
	for i := 0; i < 48; i++ {
		bits[108+i] = byte((syncVal >> (47 - i)) & 1)
	}

	// 6. Pack 264 bits → 33 bytes.
	var data [33]byte
	for i := 0; i < 264; i++ {
		if bits[i] == 1 {
			data[i/8] |= 1 << (7 - (i % 8))
		}
	}
	return data
}

// DecodeLCFromBurst extracts and decodes the 12-byte Full LC from a 33-byte
// DMR data burst using correct BPTC(196,96) deinterleaving.
func DecodeLCFromBurst(dmrData [33]byte) (lcBytes [12]byte, ok bool) {
	// Unpack 33 bytes → 264 bits.
	var allBits [264]byte
	for i := 0; i < 264; i++ {
		allBits[i] = (dmrData[i/8] >> (7 - (i % 8))) & 1
	}

	// Extract 196 data bits: bits[0:97] + bits[166:263].
	var dataBits [196]byte
	copy(dataBits[:98], allBits[:98])
	copy(dataBits[98:], allBits[166:264])

	// BPTC decode.
	info, _, uncorrectable := Decode(dataBits)
	if uncorrectable {
		return lcBytes, false
	}

	// Pack 96 info bits → 12 bytes.
	for i := 0; i < 96; i++ {
		lcBytes[i/8] |= info[i] << (7 - (i % 8))
	}
	return lcBytes, true
}

// golayEncode208 encodes an 8-bit value into 20 Golay(20,8,7) coded bits.
// ETSI TS 102 361-1 B.3.8.
func golayEncode208(input byte) [20]byte {
	var bits [8]byte
	for i := 0; i < 8; i++ {
		bits[i] = (input >> (7 - i)) & 1
	}

	var out [20]byte
	copy(out[:8], bits[:])

	// Parity per ETSI TS 102 361-1 Table B.18.
	out[8] = bits[1] ^ bits[4] ^ bits[5] ^ bits[6] ^ bits[7]
	out[9] = bits[1] ^ bits[2] ^ bits[4]
	out[10] = bits[0] ^ bits[2] ^ bits[3] ^ bits[5]
	out[11] = bits[0] ^ bits[1] ^ bits[3] ^ bits[4] ^ bits[6]
	out[12] = bits[0] ^ bits[1] ^ bits[2] ^ bits[4] ^ bits[5] ^ bits[7]
	out[13] = bits[0] ^ bits[2] ^ bits[3] ^ bits[4] ^ bits[7]
	out[14] = bits[3] ^ bits[6] ^ bits[7]
	out[15] = bits[0] ^ bits[1] ^ bits[5] ^ bits[6]
	out[16] = bits[0] ^ bits[1] ^ bits[2] ^ bits[6] ^ bits[7]
	out[17] = bits[2] ^ bits[3] ^ bits[4] ^ bits[5] ^ bits[6]
	out[18] = bits[0] ^ bits[3] ^ bits[4] ^ bits[5] ^ bits[6] ^ bits[7]
	out[19] = bits[1] ^ bits[2] ^ bits[3] ^ bits[5] ^ bits[7]

	return out
}
