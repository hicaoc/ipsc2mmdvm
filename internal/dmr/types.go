package dmr

type DataType uint8

const (
	DataTypePIHeader DataType = iota
	DataTypeVoiceLCHeader
	DataTypeTerminatorWithLC
	DataTypeCSBK
	DataTypeMBCHeader
	DataTypeMBCContinuation
	DataTypeDataHeader
	DataTypeRate12
	DataTypeRate34
	DataTypeIdle
	DataTypeRate1
	DataTypeUnifiedSingleBlock
	DataTypeReserved
)

type FLCO byte

const (
	FLCOGroupVoiceChannelUser      FLCO = 0b000000
	FLCOUnitToUnitVoiceChannelUser FLCO = 0b000011
)

type FeatureSetID byte

const (
	StandardizedFID FeatureSetID = 0x00
)

type LCSS int

const (
	SingleFragmentLCorCSBK LCSS = iota
	FirstFragmentLC
	LastFragmentLCorCSBK
	ContinuationFragmentLCorCSBK
)

func LCSSFromInt(i int) LCSS {
	switch LCSS(i) {
	case SingleFragmentLCorCSBK:
		return SingleFragmentLCorCSBK
	case FirstFragmentLC:
		return FirstFragmentLC
	case LastFragmentLCorCSBK:
		return LastFragmentLCorCSBK
	case ContinuationFragmentLCorCSBK:
		return ContinuationFragmentLCorCSBK
	default:
		return SingleFragmentLCorCSBK
	}
}

type SyncPattern int64

const (
	BsSourcedVoice            SyncPattern = 0x755FD7DF75F7
	BsSourcedData             SyncPattern = 0xDFF57D75DF5D
	MsSourcedVoice            SyncPattern = 0x7F7D5DD57DFD
	MsSourcedData             SyncPattern = 0xD5D7F77FD757
	MsSourcedRcSync           SyncPattern = 0x77D55F7DFD77
	Tdma1Voice                SyncPattern = 0x5D577F7757FF
	Tdma1Data                 SyncPattern = 0xF7FDD5DDFD55
	Tdma2Voice                SyncPattern = 0x7DFFD5F55D5F
	Tdma2Data                 SyncPattern = 0xD7557F5FF7F5
	Reserved                  SyncPattern = 0xDD7FF5D757DD
	EmbeddedSignallingPattern SyncPattern = -1
)

func SyncPatternFromBytes(syncOrEmbeddedSignalling [6]byte) SyncPattern {
	var v int64
	for i := 0; i < 6; i++ {
		v |= int64(syncOrEmbeddedSignalling[i]) << (8 * (5 - i))
	}
	switch SyncPattern(v) {
	case BsSourcedVoice:
		return BsSourcedVoice
	case BsSourcedData:
		return BsSourcedData
	case MsSourcedVoice:
		return MsSourcedVoice
	case MsSourcedData:
		return MsSourcedData
	case MsSourcedRcSync:
		return MsSourcedRcSync
	case Tdma1Voice:
		return Tdma1Voice
	case Tdma1Data:
		return Tdma1Data
	case Tdma2Voice:
		return Tdma2Voice
	case Tdma2Data:
		return Tdma2Data
	case Reserved:
		return Reserved
	default:
		return EmbeddedSignallingPattern
	}
}
