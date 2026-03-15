package audio

import (
	"testing"

	nrlcodec "github.com/hicaoc/ipsc2mmdvm/internal/nrl"
)

func TestFrontendALawDecodeMatchesBackendTable(t *testing.T) {
	for code := 0; code < 256; code++ {
		got := frontendALawToLinear(byte(code))
		want := nrlcodec.AlawToLinear(byte(code))
		if got != want {
			t.Fatalf("code 0x%02X decoded to %d, want %d", code, got, want)
		}
	}
}

func TestALawBytesRoundTripMatchesBackendDecode(t *testing.T) {
	samples := []int16{
		-32768, -30000, -12345, -1024, -255, -1,
		0, 1, 8, 255, 1024, 12345, 30000, 32767,
	}
	encoded := ALawBytes(samples)
	if len(encoded) != len(samples) {
		t.Fatalf("encoded length = %d, want %d", len(encoded), len(samples))
	}
	for i, code := range encoded {
		got := frontendALawToLinear(code)
		want := nrlcodec.AlawToLinear(code)
		if got != want {
			t.Fatalf("sample[%d] code 0x%02X decoded to %d, want %d", i, code, got, want)
		}
	}
}

func frontendALawToLinear(code byte) int16 {
	value := code ^ 0x55
	exponent := int16((value & 0x70) >> 4)
	mantissa := int16(value & 0x0F)
	if exponent > 0 {
		mantissa += 16
	}
	sample := (mantissa << 4) + 0x08
	if exponent > 1 {
		sample <<= (exponent - 1)
	}
	if (value & 0x80) != 0 {
		return sample
	}
	return -sample
}
