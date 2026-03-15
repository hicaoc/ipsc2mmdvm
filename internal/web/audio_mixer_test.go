package web

import (
	"encoding/binary"
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
	}, "group:1:1")
	mixer.Add(audio.Chunk{
		StreamID:  "b",
		PCM:       audio.PCM16Bytes([]int16{2000, 500, 10000}),
		CreatedAt: now,
	}, "group:2:1")

	frame := mixer.Flush(now.Add(mixedAudioFrameDuration))
	if got, want := len(frame), mixedAudioFrameSamples*2; got != want {
		t.Fatalf("frame length = %d, want %d", got, want)
	}

	samples := decodePCM16(frame[:6])
	if got, want := samples[0], int16(3000); got != want {
		t.Fatalf("sample[0] = %d, want %d", got, want)
	}
	if got, want := samples[1], int16(-500); got != want {
		t.Fatalf("sample[1] = %d, want %d", got, want)
	}
	if got, want := samples[2], int16(32767); got != want {
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
	}, "group:1:1")
	mixer.Add(audio.Chunk{
		StreamID:  "drop",
		PCM:       audio.PCM16Bytes([]int16{9, 9, 9}),
		CreatedAt: now,
	}, "group:2:1")
	mixer.RemoveTarget("group:2:1")

	frame := mixer.Flush(now.Add(mixedAudioFrameDuration))
	if got := int16(binary.LittleEndian.Uint16(frame[:2])); got != 1 {
		t.Fatalf("first mixed sample = %d, want 1", got)
	}

	mixer.Add(audio.Chunk{
		StreamID:  "keep",
		Ended:     true,
		CreatedAt: now.Add(mixedAudioFrameDuration),
	}, "group:1:1")
	_ = mixer.Flush(now.Add(2 * mixedAudioFrameDuration))
	if len(mixer.streams) != 0 {
		t.Fatalf("expected mixer streams to be empty, got %d", len(mixer.streams))
	}
}
