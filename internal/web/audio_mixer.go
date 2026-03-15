package web

import (
	"encoding/binary"
	"time"

	"github.com/hicaoc/ipsc2mmdvm/internal/audio"
)

const (
	mixedAudioFrameDuration = 20 * time.Millisecond
	mixedAudioFrameSamples  = audio.SampleRate8000 / 50
	mixedAudioIdleTimeout   = 5 * time.Second
)

type wsAudioMixer struct {
	streams map[string]*mixedAudioStream
}

type mixedAudioStream struct {
	target   string
	samples  []int16
	lastSeen time.Time
	ended    bool
}

func newWSAudioMixer() *wsAudioMixer {
	return &wsAudioMixer{
		streams: map[string]*mixedAudioStream{},
	}
}

func (m *wsAudioMixer) Add(chunk audio.Chunk, target string) {
	if m == nil || chunk.StreamID == "" || target == "" {
		return
	}
	stream, ok := m.streams[chunk.StreamID]
	if !ok {
		stream = &mixedAudioStream{target: target}
		m.streams[chunk.StreamID] = stream
	}
	stream.target = target
	stream.lastSeen = chunk.CreatedAt
	if stream.lastSeen.IsZero() {
		stream.lastSeen = time.Now().UTC()
	}
	if len(chunk.PCM) >= 2 {
		samples := decodePCM16(chunk.PCM)
		stream.samples = append(stream.samples, samples...)
	}
	if chunk.Ended {
		stream.ended = true
	}
}

func (m *wsAudioMixer) RemoveTarget(target string) {
	if m == nil || target == "" {
		return
	}
	for streamID, stream := range m.streams {
		if stream.target == target {
			delete(m.streams, streamID)
		}
	}
}

func (m *wsAudioMixer) Reset() {
	if m == nil {
		return
	}
	clear(m.streams)
}

func (m *wsAudioMixer) Flush(now time.Time) []byte {
	if m == nil {
		return nil
	}

	mix := make([]int32, mixedAudioFrameSamples)
	active := false

	for streamID, stream := range m.streams {
		if len(stream.samples) == 0 {
			if stream.ended || now.Sub(stream.lastSeen) > mixedAudioIdleTimeout {
				delete(m.streams, streamID)
			}
			continue
		}

		active = true
		n := mixedAudioFrameSamples
		if len(stream.samples) < n {
			n = len(stream.samples)
		}
		for i := 0; i < n; i++ {
			mix[i] += int32(stream.samples[i])
		}
		stream.samples = stream.samples[n:]
		if len(stream.samples) == 0 && stream.ended {
			delete(m.streams, streamID)
		}
	}

	if !active {
		return nil
	}

	outSamples := make([]int16, mixedAudioFrameSamples)
	for i, sample := range mix {
		outSamples[i] = clampPCM16(sample)
	}
	return audio.ALawBytes(outSamples)
}

func decodePCM16(raw []byte) []int16 {
	if len(raw) < 2 {
		return nil
	}
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return out
}

func clampPCM16(sample int32) int16 {
	switch {
	case sample > 32767:
		return 32767
	case sample < -32768:
		return -32768
	default:
		return int16(sample)
	}
}
