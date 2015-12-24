package http2

import (
	"io"
)

type ErrCode uint32

const (
	ErrCodeNo                 ErrCode = 0x0
	ErrCodeProtocol           ErrCode = 0x1
	ErrCodeInternal           ErrCode = 0x2
	ErrCodeFlowControl        ErrCode = 0x3
	ErrCodeSettingsTimeout    ErrCode = 0x4
	ErrCodeStreamClosed       ErrCode = 0x5
	ErrCodeFrameSize          ErrCode = 0x6
	ErrCodeRefusedStream      ErrCode = 0x7
	ErrCodeCancel             ErrCode = 0x8
	ErrCodeCompression        ErrCode = 0x9
	ErrCodeConnect            ErrCode = 0xa
	ErrCodeEnhanceYourCalm    ErrCode = 0xb
	ErrCodeInadequateSecurity ErrCode = 0xc
	ErrCodeHTTP11Required     ErrCode = 0xd
)

type ConnError struct {
	Err error
	ErrCode
}

type StreamError struct {
	Err error
	ErrCode
	StreamID uint32
}

type StreamErrorList []*StreamError

const (
	maxConcurrentStreams   = 1<<31 - 1
	maxInitialWindowSize   = 1<<31 - 1
	maxFrameSizeLowerBound = 1 << 14
	maxFrameSizeUpperBound = 1<<24 - 1

	defaultHeaderTableSize      = 4096
	defaultEnablePush           = true
	defaultMaxConcurrentStreams = maxConcurrentStreams
	defaultInitialWindowSize    = 65535
	defaultMaxFrameSize         = maxFrameSizeLowerBound
)

type SettingID uint16

const (
	SettingHeaderTableSize      SettingID = 0x1
	SettingEnablePush           SettingID = 0x2
	SettingMaxConcurrentStreams SettingID = 0x3
	SettingInitialWindowSize    SettingID = 0x4
	SettingMaxFrameSize         SettingID = 0x5
	SettingMaxHeaderListSize    SettingID = 0x6
)

const settingLen = 6

type setting struct {
	ID    SettingID
	Value uint32
}

type Settings []setting

type Header map[string][]string

type FrameType uint8

const (
	FrameData         FrameType = 0x0
	FrameHeaders      FrameType = 0x1
	FramePriority     FrameType = 0x2
	FrameRSTStream    FrameType = 0x3
	FrameSettings     FrameType = 0x4
	FramePushPromise  FrameType = 0x5
	FramePing         FrameType = 0x6
	FrameGoAway       FrameType = 0x7
	FrameWindowUpdate FrameType = 0x8
	FrameContinuation FrameType = 0x9
)

type Flags uint8

const (
	FlagEndStream  Flags = 0x1
	FlagEndHeaders Flags = 0x4
	FlagAck        Flags = 0x1
	FlagPadded     Flags = 0x8
	FlagPriority   Flags = 0x20
)

func (f Flags) Has(v Flags) bool {
	return (f & v) == v
}

type Frame interface {
	Type() FrameType
	streamID() uint32
}

type DataFrame struct {
	StreamID  uint32
	Data      io.Reader
	DataLen   int
	PadLen    uint8
	EndStream bool
}

type Priority struct {
	StreamDependency uint32
	Weight           uint8
	Exclusive        bool
}

type HeadersFrame struct {
	StreamID uint32
	Header
	Priority
	PadLen    uint8
	EndStream bool
}

type PriorityFrame struct {
	StreamID uint32
	Priority
}

type RSTStreamFrame struct {
	StreamID uint32
	ErrCode
}

type SettingsFrame struct {
	Ack bool
	Settings
}

type PushPromiseFrame struct {
	StreamID         uint32
	PromisedStreamID uint32
	Header
	PadLen uint8
}

type PingFrame struct {
	Ack  bool
	Data [8]byte
}

type GoAwayFrame struct {
	LastStreamID uint32
	ErrCode
	DebugData []byte
}

type WindowUpdateFrame struct {
	StreamID            uint32
	WindowSizeIncrement uint32
}

type UnknownFrame struct {
	FrameType
	StreamID uint32
	Flags
	Payload    io.Reader
	PayloadLen int
}
