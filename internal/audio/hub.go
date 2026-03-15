package audio

import (
	"encoding/binary"
	"sync"
	"time"

	nrlcodec "github.com/hicaoc/ipsc2mmdvm/internal/nrl"
)

const SampleRate8000 = 8000

type Chunk struct {
	Type        string    `json:"type"`
	StreamID    string    `json:"streamId"`
	Frontend    string    `json:"frontend"`
	SourceKey   string    `json:"sourceKey"`
	SourceDMRID uint32    `json:"sourceDmrid,omitempty"`
	DstID       uint32    `json:"dstId,omitempty"`
	Slot        int       `json:"slot,omitempty"`
	GroupCall   bool      `json:"groupCall,omitempty"`
	CallType    string    `json:"callType"`
	SampleRate  int       `json:"sampleRate"`
	Channels    int       `json:"channels"`
	PCM         []byte    `json:"pcm,omitempty"`
	Ended       bool      `json:"ended,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Hub struct {
	mu   sync.RWMutex
	subs map[chan Chunk]struct{}
}

func NewHub() *Hub {
	return &Hub{
		subs: map[chan Chunk]struct{}{},
	}
}

func (h *Hub) Subscribe() (<-chan Chunk, func()) {
	ch := make(chan Chunk, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) Publish(chunk Chunk) {
	if h == nil || chunk.StreamID == "" {
		return
	}
	if chunk.Type == "" {
		chunk.Type = "audio_chunk"
	}
	if chunk.SampleRate == 0 {
		chunk.SampleRate = SampleRate8000
	}
	if chunk.Channels == 0 {
		chunk.Channels = 1
	}
	if chunk.CreatedAt.IsZero() {
		chunk.CreatedAt = time.Now().UTC()
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.subs {
		select {
		case sub <- chunk:
		default:
		}
	}
}

func PCM16Bytes(samples []int16) []byte {
	if len(samples) == 0 {
		return nil
	}
	out := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(sample))
	}
	return out
}

func ALawBytes(samples []int16) []byte {
	if len(samples) == 0 {
		return nil
	}
	out := make([]byte, len(samples))
	for i, sample := range samples {
		out[i] = nrlcodec.LinearToAlaw(sample)
	}
	return out
}
