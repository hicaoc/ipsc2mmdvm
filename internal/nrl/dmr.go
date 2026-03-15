package nrl

import (
	"math/rand"

	intdmr "github.com/hicaoc/ipsc2mmdvm/internal/dmr"
	"github.com/hicaoc/ipsc2mmdvm/internal/dmr/bptc"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

func randomStreamID() uint32 {
	v := uint32(rand.Int31())
	if v == 0 {
		return 1
	}
	return v
}

func newHeaderPacket(srcID, dstID uint32, slot int, colorCode uint8, streamID, seq uint32) proto.Packet {
	return proto.Packet{
		Signature:   "DMRD",
		Seq:         uint(seq & 0xFF),
		Src:         uint(srcID),
		Dst:         uint(dstID),
		Slot:        slot == 2,
		GroupCall:   true,
		FrameType:   2,
		DTypeOrVSeq: 1,
		StreamID:    uint(streamID),
		DMRData: bptc.BuildLCDataBurst(
			intdmr.BuildStandardLCBytesForDataType(uint(srcID), uint(dstID), true, 0x20, intdmr.DataTypeVoiceLCHeader),
			uint8(intdmr.DataTypeVoiceLCHeader),
			colorCode,
		),
	}
}

func newTerminatorPacket(srcID, dstID uint32, slot int, colorCode uint8, streamID, seq uint32) proto.Packet {
	return proto.Packet{
		Signature:   "DMRD",
		Seq:         uint(seq & 0xFF),
		Src:         uint(srcID),
		Dst:         uint(dstID),
		Slot:        slot == 2,
		GroupCall:   true,
		FrameType:   2,
		DTypeOrVSeq: 2,
		StreamID:    uint(streamID),
		DMRData: bptc.BuildLCDataBurst(
			intdmr.BuildStandardLCBytesForDataType(uint(srcID), uint(dstID), true, 0x20, intdmr.DataTypeTerminatorWithLC),
			uint8(intdmr.DataTypeTerminatorWithLC),
			colorCode,
		),
	}
}

func newVoicePacket(srcID, dstID uint32, slot int, colorCode uint8, streamID, seq uint32, voiceSeq uint8, ambe [3][]byte) proto.Packet {
	frameType := uint(0)
	if voiceSeq == 0 {
		frameType = 1
	}
	var dmr [33]byte
	copy(dmr[:], intdmr.AssembleVoiceBurst(ambe[0], ambe[1], ambe[2], voiceSeq, srcID, dstID, colorCode))
	return proto.Packet{
		Signature:   "DMRD",
		Seq:         uint(seq & 0xFF),
		Src:         uint(srcID),
		Dst:         uint(dstID),
		Slot:        slot == 2,
		GroupCall:   true,
		FrameType:   frameType,
		DTypeOrVSeq: uint(voiceSeq),
		StreamID:    uint(streamID),
		DMRData:     dmr,
	}
}
