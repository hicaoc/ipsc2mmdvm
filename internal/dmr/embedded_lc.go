package dmr

// EmbeddedLCCRC5 computes the CRC5 used when constructing embedded LC bits.
func EmbeddedLCCRC5(bits []byte) byte {
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

// EmbeddedLCCRC5Residual computes the CRC5 residual form used by capture
// analysis helpers when validating decoded embedded LC fragments.
func EmbeddedLCCRC5Residual(bits []byte) byte {
	var reg byte
	for _, bit := range bits {
		msb := (reg >> 4) & 1
		reg = ((reg << 1) & 0x1F) | (bit & 1)
		if msb == 1 {
			reg ^= 0x15
		}
	}
	return (reg & 0x1F) ^ 0x1F
}
