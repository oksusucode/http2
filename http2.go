package http2

import "io"

// HTTP/2 Version Identification, defined in RFC 7540 section 3.1.
const (
	ProtocolTLS = "h2"
	ProtocolTCP = "h2c"
)

// ClientPreface represents client-side HTTP/2 connection preface,
// defined in RFC 7540 section 3.5.
const ClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// ErrCode represents error codes, defined in RFC 7540 section 7.
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

// ConnError represents connection error, defined in RFC 7540 section 5.4.1.
type ConnError struct {
	Err error
	ErrCode
}

// StreamError represents stream error, defined in RFC 7540 section 5.4.2.
type StreamError struct {
	Err error
	ErrCode
	StreamID uint32
}

// StreamErrorList is a list of *StreamErrors.
type StreamErrorList []*StreamError

// MalformedError represents Malformed Requests and Responses,
// defined in RFC 7540 section 8.1.2.6.
type MalformedError string

const (
	maxConcurrentStreams   = 1<<31 - 1
	maxInitialWindowSize   = 1<<31 - 1
	maxFrameSizeLowerBound = 1 << 14
	maxFrameSizeUpperBound = 1<<24 - 1

	defaultHeaderTableSize      = 4096
	defaultEnablePush           = 1
	defaultMaxConcurrentStreams = maxConcurrentStreams
	defaultInitialWindowSize    = 65535
	defaultMaxFrameSize         = maxFrameSizeLowerBound
)

// SettingID represents SETTINGS Parameters, defined in RFC 7540 section 6.5.2.
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

// Settings for local or remote in an HTTP/2 connection.
type Settings []setting

// Header is A collection of headers sent or received via HTTP/2.
type Header map[string][]string

// FrameType represents Frame Type Registry, defined in RFC 7540 section 11.2.
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

// Flags is An 8-bit field reserved for boolean flags specific to the frame type.
type Flags uint8

const (
	FlagEndStream  Flags = 0x1
	FlagEndHeaders Flags = 0x4
	FlagAck        Flags = 0x1
	FlagPadded     Flags = 0x8
	FlagPriority   Flags = 0x20
)

// Frame is the base interface implemented by all frame types.
type Frame interface {
	Type() FrameType
	Stream() uint32
	EndOfStream() bool
}

// DataFrame represents the DATA frame,
// defined in RFC 7540 section 6.1.
type DataFrame struct {
	StreamID  uint32
	Data      io.Reader
	DataLen   int
	PadLen    uint8
	EndStream bool
}

// Priority is the Stream prioritzation parameters.
type Priority struct {
	StreamDependency uint32
	Weight           uint8
	Exclusive        bool
}

// HeadersFrame represents the HEADERS frame,
// defined in RFC 7540 section 6.2.
type HeadersFrame struct {
	StreamID uint32
	Header
	Priority
	PadLen    uint8
	EndStream bool
}

// PriorityFrame represents the PRIORITY frame,
// defined in RFC 7540 section 6.3.
type PriorityFrame struct {
	StreamID uint32
	Priority
}

// RSTStreamFrame represents the RST_STREAM frame,
// defined in RFC 7540 section 6.4.
type RSTStreamFrame struct {
	StreamID uint32
	ErrCode
}

// SettingsFrame represents the SETTINGS frame,
// defined in RFC 7540 section 6.5.
type SettingsFrame struct {
	Ack bool
	Settings
}

// PushPromiseFrame represents the PUSH_PROMISE frame,
// defined in RFC 7540 section 6.6.
type PushPromiseFrame struct {
	StreamID         uint32
	PromisedStreamID uint32
	Header
	PadLen uint8
}

// PingFrame represents the PING frame,
// defined in RFC 7540 section 6.7.
type PingFrame struct {
	Ack  bool
	Data [8]byte
}

// GoAwayFrame represents the GOAWAY frame,
// defined in RFC 7540 section 6.8.
type GoAwayFrame struct {
	LastStreamID uint32
	ErrCode
	DebugData []byte
}

// WindowUpdateFrame represents the WINDOW_UPDATE frame,
// defined in RFC 7540 section 6.9.
type WindowUpdateFrame struct {
	StreamID            uint32
	WindowSizeIncrement uint32
}

// UnknownFrame represents not defined by the HTTP/2 spec.
type UnknownFrame struct {
	FrameType
	StreamID uint32
	Flags
	Payload    io.Reader
	PayloadLen int
}
