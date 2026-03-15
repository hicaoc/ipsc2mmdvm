package web

import (
	"testing"

	"github.com/hicaoc/ipsc2mmdvm/internal/audio"
)

func TestAudioTargetKeyForChunkIgnoresSlotForDigitalCalls(t *testing.T) {
	tests := []struct {
		name  string
		chunk audio.Chunk
		want  string
	}{
		{
			name: "group ts1",
			chunk: audio.Chunk{
				CallType: "group",
				DstID:    46001,
				Slot:     1,
			},
			want: "group:46001",
		},
		{
			name: "group ts2",
			chunk: audio.Chunk{
				CallType: "group",
				DstID:    46001,
				Slot:     2,
			},
			want: "group:46001",
		},
		{
			name: "private ts2",
			chunk: audio.Chunk{
				CallType: "private",
				DstID:    1234567,
				Slot:     2,
			},
			want: "private:1234567",
		},
		{
			name: "analog keeps source key",
			chunk: audio.Chunk{
				CallType:  "analog",
				SourceKey: "nrl:hytera:1.2.3.4",
			},
			want: "analog:nrl:hytera:1.2.3.4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := audioTargetKeyForChunk(tt.chunk); got != tt.want {
				t.Fatalf("audioTargetKeyForChunk() = %q, want %q", got, tt.want)
			}
		})
	}
}
