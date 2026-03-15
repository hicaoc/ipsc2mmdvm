package hytera

import (
	"bytes"
	"testing"

	intdmr "github.com/hicaoc/ipsc2mmdvm/internal/dmr"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
)

func TestCompareCaptureFilesWithHyteraOutboundAlgorithm(t *testing.T) {
	type one struct {
		name string
		file string
	}
	files := []one{
		{name: "nrldmr(normal)", file: "nrldmr.txt"},
		{name: "nrldrm-b(current)", file: "nrldrm-b.txt"},
	}

	for _, f := range files {
		path := resolveCapturePath(t, f.file)
		packets := parseTCPDumpPacketsFromFile(t, path)
		decoded := make([]proto.Packet, 0, 32)
		for _, p := range packets {
			if len(p.payload) < 4 || string(p.payload[:4]) != "DMRD" {
				continue
			}
			dp, ok := proto.Decode(p.payload)
			if !ok {
				t.Fatalf("[%s] decode failed len=%d", f.name, len(p.payload))
			}
			decoded = append(decoded, dp)
		}
		if len(decoded) == 0 {
			t.Fatalf("[%s] no DMRD packets", f.name)
		}

		var (
			headerCount, syncCount, voiceCount, termCount int
			changedCount                                  int
			voiceRebuildOK                                int
			voiceRebuildFail                              int
		)

		for i, pkt := range decoded {
			cc, source := packetColorCodeWithSource(pkt, 1)
			cc, _ = normalizeMotoColorCode(cc, source, 1)
			norm := pkt

			switch {
			case pkt.FrameType == 2 && pkt.DTypeOrVSeq == 1:
				headerCount++
				norm = standardizeMotoLCPacketWithSO(pkt, cc, 0x20)
			case pkt.FrameType == 2 && pkt.DTypeOrVSeq == 2:
				termCount++
				norm = standardizeMotoLCPacketWithSO(pkt, cc, 0x20)
			case pkt.FrameType == 1 && pkt.DTypeOrVSeq == 0:
				syncCount++
			case pkt.FrameType == 0 && pkt.DTypeOrVSeq >= 1 && pkt.DTypeOrVSeq <= 5:
				voiceCount++
				norm = patchMotoVoiceEmbeddedControl(pkt, cc)
			}

			if !bytes.Equal(norm.DMRData[:], pkt.DMRData[:]) {
				changedCount++
				if i < 12 {
					t.Logf("[%s] idx=%d normalized changed ft/v=%d/%d cc=%d", f.name, i, pkt.FrameType, pkt.DTypeOrVSeq, cc)
				}
			}

			if pkt.FrameType == 1 && pkt.DTypeOrVSeq == 0 || (pkt.FrameType == 0 && pkt.DTypeOrVSeq >= 1 && pkt.DTypeOrVSeq <= 5) {
				if hyteraVoiceRebuildCheck(norm, uint8(pkt.DTypeOrVSeq), uint32(pkt.Src), uint32(pkt.Dst), cc) {
					voiceRebuildOK++
				} else {
					voiceRebuildFail++
					t.Logf("[%s] idx=%d voice rebuild fail ft/v=%d/%d cc=%d", f.name, i, pkt.FrameType, pkt.DTypeOrVSeq, cc)
				}
			}
		}

		if termCount == 0 {
			t.Logf("[%s] ERROR: no terminator(2/2) in capture", f.name)
		}
		t.Logf("[%s] summary: total=%d header=%d sync=%d voice=%d term=%d changedByHyteraNorm=%d voiceRebuildOK=%d voiceRebuildFail=%d",
			f.name, len(decoded), headerCount, syncCount, voiceCount, termCount, changedCount, voiceRebuildOK, voiceRebuildFail)
	}
}

func hyteraVoiceRebuildCheck(pkt proto.Packet, seq uint8, src, dst uint32, cc uint8) bool {
	frame1, frame2, frame3 := intdmr.ExtractVoiceFramesFromBurst(pkt.DMRData[:])
	rebuilt := intdmr.AssembleVoiceBurst(frame1, frame2, frame3, seq, src, dst, cc)
	return bytes.Equal(rebuilt, pkt.DMRData[:])
}
