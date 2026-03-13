package hytera

const (
	SEG_MASK   = 0x70 // 0b01110000
	QUANT_MASK = 0x0F // 0b00001111
	SEG_SHIFT  = 4
	BIAS       = 0x84 //

)

var (
	alaw2linearTable [256]int16
	linear2alawTable [65536]byte
	ulaw2linearTable [256]int16
	linear2ulawTable [65536]byte
)

func init() {
	// Initialize Alaw to Linear table
	for i := range 256 {
		alaw2linearTable[i] = rawAlaw2linear(byte(i))
	}

	// Initialize Linear to Alaw table
	for i := range 65536 {
		linear2alawTable[i] = rawLinear2Alaw(int16(i))
	}

	// Initialize Ulaw to Linear table
	for i := range 256 {
		ulaw2linearTable[i] = rawUlaw2linear(byte(i))
	}

	// Initialize Linear to Ulaw table
	for i := range 65536 {
		linear2ulawTable[i] = rawLinear2Ulaw(int16(i))
	}
}

func alaw2linear(code byte) int16 {
	return alaw2linearTable[code]
}

func Linear2Alaw(sample int16) byte {
	return linear2alawTable[uint16(sample)]
}

func ulaw2linear(code byte) int16 {
	return ulaw2linearTable[code]
}

func Linear2Ulaw(sample int16) byte {
	return linear2ulawTable[uint16(sample)]
}

func rawAlaw2linear(code byte) int16 {
	code ^= 0x55

	iexp := int16((code & 0x70) >> 4)
	mant := int16(code & 0x0F)

	if iexp > 0 {
		mant += 16
	}

	mant = (mant << 4) + 0x08

	if iexp > 1 {
		mant <<= (iexp - 1)
	}

	if (code & 0x80) != 0 {
		return mant
	}
	return -mant
}

func rawLinear2Alaw(sample int16) byte {
	var sign byte
	var ix int16

	if sample < 0 {
		sign = 0x80
		ix = (^sample) >> 4 // ✅ 按位取反
	} else {
		ix = sample >> 4
	}

	if ix > 15 {
		iexp := byte(1)
		for ix > 31 {
			ix >>= 1
			iexp++
		}
		ix -= 16
		ix += int16(iexp << 4)
	}

	if sign == 0 {
		ix |= 0x80
	}

	return byte(ix) ^ 0x55
}

func rawUlaw2linear(code byte) int16 {
	code = ^code

	sign := code & 0x80
	exponent := (code & SEG_MASK) >> SEG_SHIFT
	mantissa := code & QUANT_MASK

	sample := ((int16(mantissa) << 3) + BIAS) << exponent
	sample -= BIAS

	if sign != 0 {
		return -sample
	}
	return sample
}

func rawLinear2Ulaw(sample int16) byte {
	const clip = 32635

	s := int(sample)
	sign := byte(0)
	if s < 0 {
		sign = 0x80
		s = -s
	}
	if s > clip {
		s = clip
	}
	s += BIAS

	exponent := 7
	mask := 0x4000
	for exponent > 0 && (s&mask) == 0 {
		exponent--
		mask >>= 1
	}

	mantissa := (s >> (exponent + 3)) & int(QUANT_MASK)
	u := ^(sign | byte(exponent<<SEG_SHIFT) | byte(mantissa))
	return u
}

// G711Encode converts a slice of 16-bit linear PCM samples to a slice of 8-bit A-law samples.
func G711Encode(pcmData []int) []byte {
	encoded := make([]byte, len(pcmData))
	for i := range pcmData {
		encoded[i] = Linear2Alaw(int16(pcmData[i]))
	}
	return encoded
}

func G711Decode(encodedData []byte) []int {
	decoded := make([]int, len(encodedData))
	for i := range encodedData {
		decoded[i] = int(alaw2linear(encodedData[i]))
	}
	return decoded
}

// G711UlawEncode converts a slice of 16-bit linear PCM samples to a slice of 8-bit mu-law samples.
func G711UlawEncode(pcmData []int) []byte {
	encoded := make([]byte, len(pcmData))
	for i := range pcmData {
		encoded[i] = Linear2Ulaw(int16(pcmData[i]))
	}
	return encoded
}

// G711UlawDecode converts a slice of 8-bit mu-law samples to 16-bit linear PCM values.
func G711UlawDecode(encodedData []byte) []int {
	decoded := make([]int, len(encodedData))
	for i := range encodedData {
		decoded[i] = int(ulaw2linear(encodedData[i]))
	}
	return decoded
}
