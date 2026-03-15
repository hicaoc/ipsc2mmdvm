package audio

import (
	"fmt"
	"sync"
	"time"

	"github.com/USA-RedDragon/dmrgo/dmr/layer2"
	intdmr "github.com/hicaoc/ipsc2mmdvm/internal/dmr"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
	md380vocoder "github.com/hicaoc/md380_vocoder_cgo"
)

const dmrStreamIdleTimeout = 3 * time.Second
const dmrDecodeQueueSize = 256

type dmrDecodeTask struct {
	frontend  string
	sourceKey string
	pkt       proto.Packet
}

type DMRDecoderPool struct {
	hub        *Hub
	warmSpares int

	mu     sync.Mutex
	idle   []*md380vocoder.Vocoder
	active map[string]*dmrStreamDecoder

	queue chan dmrDecodeTask
	wg    sync.WaitGroup
}

type dmrStreamDecoder struct {
	vocoder  *md380vocoder.Vocoder
	lastSeen time.Time
}

func NewDMRDecoderPool(hub *Hub, warmSpares int) (*DMRDecoderPool, error) {
	if warmSpares < 1 {
		warmSpares = 2
	}
	pool := &DMRDecoderPool{
		hub:        hub,
		warmSpares: warmSpares,
		active:     map[string]*dmrStreamDecoder{},
		queue:      make(chan dmrDecodeTask, dmrDecodeQueueSize),
	}
	for range warmSpares {
		v, err := md380vocoder.NewVocoder()
		if err != nil {
			pool.Close()
			return nil, err
		}
		pool.idle = append(pool.idle, v)
	}
	pool.wg.Add(1)
	go pool.run()
	return pool, nil
}

func (p *DMRDecoderPool) Close() error {
	if p == nil {
		return nil
	}
	if p.queue != nil {
		close(p.queue)
		p.wg.Wait()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, decoder := range p.idle {
		_ = decoder.Close()
	}
	for _, stream := range p.active {
		_ = stream.vocoder.Close()
	}
	p.idle = nil
	p.active = map[string]*dmrStreamDecoder{}
	return nil
}

func (p *DMRDecoderPool) HandlePacket(frontend, sourceKey string, pkt proto.Packet) {
	if p == nil || p.hub == nil || sourceKey == "" {
		return
	}
	select {
	case p.queue <- dmrDecodeTask{
		frontend:  frontend,
		sourceKey: sourceKey,
		pkt:       pkt,
	}:
	default:
	}
}

func (p *DMRDecoderPool) run() {
	defer p.wg.Done()
	for task := range p.queue {
		p.processPacket(task.frontend, task.sourceKey, task.pkt)
	}
}

func (p *DMRDecoderPool) processPacket(frontend, sourceKey string, pkt proto.Packet) {
	if p == nil || p.hub == nil || sourceKey == "" {
		return
	}
	now := time.Now().UTC()
	streamID := fmt.Sprintf("dmr:%s:%d", sourceKey, pkt.StreamID)

	if pkt.FrameType == 2 && pkt.DTypeOrVSeq == 2 {
		p.endStream(streamID, frontend, sourceKey, pkt, now)
		return
	}

	if pkt.FrameType != 0 && pkt.FrameType != 1 {
		p.expireIdle(now)
		return
	}

	var burst layer2.Burst
	burst.DecodeFromBytes(pkt.DMRData)
	if burst.IsData {
		p.expireIdle(now)
		return
	}

	decoder, err := p.decoderForStream(streamID, now)
	if err != nil {
		return
	}

	samples := make([]int16, 0, md380vocoder.PCMFrameSize*len(burst.VoiceData.Frames))
	for _, frame := range burst.VoiceData.Frames {
		pcm, err := decoder.Decode(ambeFrameBytes(frame.DecodedBits))
		if err != nil {
			return
		}
		samples = append(samples, pcm...)
	}

	p.hub.Publish(Chunk{
		StreamID:    streamID,
		Frontend:    frontend,
		SourceKey:   sourceKey,
		SourceDMRID: uint32(pkt.Src),
		DstID:       uint32(pkt.Dst),
		Slot:        packetSlot(pkt),
		GroupCall:   pkt.GroupCall,
		CallType:    callType(pkt.GroupCall),
		SampleRate:  SampleRate8000,
		Channels:    1,
		PCM:         PCM16Bytes(samples),
		CreatedAt:   now,
	})

	p.expireIdle(now)
}

func (p *DMRDecoderPool) decoderForStream(streamID string, now time.Time) (*md380vocoder.Vocoder, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if stream, ok := p.active[streamID]; ok {
		stream.lastSeen = now
		return stream.vocoder, nil
	}

	var decoder *md380vocoder.Vocoder
	if n := len(p.idle); n > 0 {
		decoder = p.idle[n-1]
		p.idle = p.idle[:n-1]
	} else {
		var err error
		decoder, err = md380vocoder.NewVocoder()
		if err != nil {
			return nil, err
		}
	}

	p.active[streamID] = &dmrStreamDecoder{
		vocoder:  decoder,
		lastSeen: now,
	}
	return decoder, nil
}

func (p *DMRDecoderPool) endStream(streamID, frontend, sourceKey string, pkt proto.Packet, now time.Time) {
	p.mu.Lock()
	stream, ok := p.active[streamID]
	if ok {
		delete(p.active, streamID)
		p.idle = append(p.idle, stream.vocoder)
	}
	p.mu.Unlock()

	p.hub.Publish(Chunk{
		StreamID:    streamID,
		Frontend:    frontend,
		SourceKey:   sourceKey,
		SourceDMRID: uint32(pkt.Src),
		DstID:       uint32(pkt.Dst),
		Slot:        packetSlot(pkt),
		GroupCall:   pkt.GroupCall,
		CallType:    callType(pkt.GroupCall),
		SampleRate:  SampleRate8000,
		Channels:    1,
		Ended:       true,
		CreatedAt:   now,
	})
}

func (p *DMRDecoderPool) expireIdle(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for streamID, stream := range p.active {
		if now.Sub(stream.lastSeen) <= dmrStreamIdleTimeout {
			continue
		}
		delete(p.active, streamID)
		p.idle = append(p.idle, stream.vocoder)
	}
}

func ambeFrameBytes(decodedBits [49]byte) []byte {
	bits := intdmr.EncodeAMBEFrame(decodedBits)
	out := make([]byte, md380vocoder.AMBEFrameSize)
	for i, bit := range bits {
		if bit == 1 {
			out[i/8] |= 1 << (7 - (i % 8))
		}
	}
	return out
}

func packetSlot(pkt proto.Packet) int {
	if pkt.Slot {
		return 2
	}
	return 1
}

func callType(groupCall bool) string {
	if groupCall {
		return "group"
	}
	return "private"
}
