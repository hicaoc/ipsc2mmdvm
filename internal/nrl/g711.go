package nrl

const (
	segMask   = 0x70
	quantMask = 0x0F
	segShift  = 4
	bias      = 0x84
)

var (
	alaw2linearTable [256]int16
	linear2alawTable [65536]byte
	ulaw2linearTable [256]int16
	linear2ulawTable [65536]byte
)

func init() {
	for i := range 256 {
		alaw2linearTable[i] = rawAlaw2linear(byte(i))
		ulaw2linearTable[i] = rawUlaw2linear(byte(i))
	}
	for i := range 65536 {
		linear2alawTable[i] = rawLinear2Alaw(int16(i))
		linear2ulawTable[i] = rawLinear2Ulaw(int16(i))
	}
}

func AlawToLinear(code byte) int16 {
	return alaw2linearTable[code]
}

func LinearToAlaw(sample int16) byte {
	return linear2alawTable[uint16(sample)]
}

func UlawToLinear(code byte) int16 {
	return ulaw2linearTable[code]
}

func LinearToUlaw(sample int16) byte {
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
		ix = (^sample) >> 4
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
	exponent := (code & segMask) >> segShift
	mantissa := code & quantMask
	sample := ((int16(mantissa) << 3) + bias) << exponent
	sample -= bias
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
	s += bias

	exponent := 7
	mask := 0x4000
	for exponent > 0 && (s&mask) == 0 {
		exponent--
		mask >>= 1
	}

	mantissa := (s >> (exponent + 3)) & int(quantMask)
	return ^(sign | byte(exponent<<segShift) | byte(mantissa))
}
