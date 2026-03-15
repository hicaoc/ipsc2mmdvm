package web

import (
	"testing"
	"time"

	"github.com/hicaoc/ipsc2mmdvm/internal/audio"
)

func TestWSAudioMixerFlushMixesStreams(t *testing.T) {
	mixer := newWSAudioMixer()
	now := time.Now().UTC()

	mixer.Add(audio.Chunk{
		StreamID:  "a",
		PCM:       audio.PCM16Bytes([]int16{1000, -1000, 30000}),
		CreatedAt: now,
	}, "group:1")
	mixer.Add(audio.Chunk{
		StreamID:  "b",
		PCM:       audio.PCM16Bytes([]int16{2000, 500, 10000}),
		CreatedAt: now,
	}, "group:2")

	frame := mixer.Flush(now.Add(mixedAudioFrameDuration))
	if got, want := len(frame), mixedAudioFrameSamples; got != want {
		t.Fatalf("frame length = %d, want %d", got, want)
	}

	samples := decodeALaw(frame[:3])
	if got, want := samples[0], int16(3008); got != want {
		t.Fatalf("sample[0] = %d, want %d", got, want)
	}
	if got, want := samples[1], int16(-504); got != want {
		t.Fatalf("sample[1] = %d, want %d", got, want)
	}
	if got, want := samples[2], int16(32256); got != want {
		t.Fatalf("sample[2] = %d, want %d", got, want)
	}
}

func TestWSAudioMixerRemoveTargetAndExpireEnded(t *testing.T) {
	mixer := newWSAudioMixer()
	now := time.Now().UTC()

	mixer.Add(audio.Chunk{
		StreamID:  "keep",
		PCM:       audio.PCM16Bytes([]int16{1, 2, 3}),
		CreatedAt: now,
	}, "group:1")
	mixer.Add(audio.Chunk{
		StreamID:  "drop",
		PCM:       audio.PCM16Bytes([]int16{9, 9, 9}),
		CreatedAt: now,
	}, "group:2")
	mixer.RemoveTarget("group:2")

	frame := mixer.Flush(now.Add(mixedAudioFrameDuration))
	if got := decodeALaw(frame[:1])[0]; got != 8 {
		t.Fatalf("first mixed sample = %d, want 8", got)
	}

	mixer.Add(audio.Chunk{
		StreamID:  "keep",
		Ended:     true,
		CreatedAt: now.Add(mixedAudioFrameDuration),
	}, "group:1")
	_ = mixer.Flush(now.Add(2 * mixedAudioFrameDuration))
	if len(mixer.streams) != 0 {
		t.Fatalf("expected mixer streams to be empty, got %d", len(mixer.streams))
	}
}

func decodeALaw(raw []byte) []int16 {
	out := make([]int16, len(raw))
	for i, code := range raw {
		out[i] = aLawToLinear(code)
	}
	return out
}

func aLawToLinear(code byte) int16 {
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
