package http2

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/nekolunar/http2/hpack"
)

type frameReader struct {
	*bufio.Reader
	bufSize      int
	maxFrameSize uint32

	*hpack.Decoder
	maxHeaderListSize uint32
	pendingHeaders    frameReaderFrom

	payloadLen  uint32
	frameType   FrameType
	flags       Flags
	streamID    uint32
	lastPayload io.ReadCloser
}

func newFrameReader(r io.Reader, bufSize int) *frameReader {
	return &frameReader{
		Reader:       bufio.NewReaderSize(r, bufSize),
		bufSize:      bufSize,
		maxFrameSize: defaultMaxFrameSize,
		Decoder:      hpack.NewDecoder(defaultHeaderTableSize),
	}
}

type frameReaderFrom interface {
	Frame
	readFrom(*frameReader) error
}

var frameCtor = map[FrameType]func() frameReaderFrom{
	FrameData:         func() frameReaderFrom { return new(DataFrame) },
	FrameHeaders:      func() frameReaderFrom { return new(HeadersFrame) },
	FramePriority:     func() frameReaderFrom { return new(PriorityFrame) },
	FrameRSTStream:    func() frameReaderFrom { return new(RSTStreamFrame) },
	FrameSettings:     func() frameReaderFrom { return new(SettingsFrame) },
	FramePushPromise:  func() frameReaderFrom { return new(PushPromiseFrame) },
	FramePing:         func() frameReaderFrom { return new(PingFrame) },
	FrameGoAway:       func() frameReaderFrom { return new(GoAwayFrame) },
	FrameWindowUpdate: func() frameReaderFrom { return new(WindowUpdateFrame) },
}

func (r *frameReader) ReadFrame() (Frame, error) {
	if r.lastPayload != nil {
		err := r.lastPayload.Close()
		r.lastPayload = nil

		if err != nil {
			return nil, err
		}
	}

	const frameHeaderLen = 9

again:
	frameHeader, err := r.Peek(frameHeaderLen)
	if err != nil {
		return nil, err
	}

	r.payloadLen = (uint32(frameHeader[0])<<16 | uint32(frameHeader[1])<<8 | uint32(frameHeader[2]))
	r.frameType = FrameType(frameHeader[3])
	r.flags = Flags(frameHeader[4])
	r.streamID = binary.BigEndian.Uint32(frameHeader[5:]) & (1<<31 - 1)

	r.Discard(frameHeaderLen)

	// An endpoint MUST send an error code of FRAME_SIZE_ERROR if a frame
	// exceeds the size defined in SETTINGS_MAX_FRAME_SIZE, exceeds any
	// limit defined for the frame type, or is too small to contain
	// mandatory frame data.  A frame size error in a frame that could alter
	// the state of the entire connection MUST be treated as a connection
	// error (Section 5.4.1); this includes any frame carrying a header
	// block (Section 4.3) (that is, HEADERS, PUSH_PROMISE, and
	// CONTINUATION), SETTINGS, and any frame with a stream identifier of 0.
	if r.payloadLen > r.maxFrameSize {
		return nil, ConnError{
			fmt.Errorf("frame length %d exceeds maximum %d", r.payloadLen, r.maxFrameSize),
			ErrCodeFrameSize,
		}
	}

	// A HEADERS frame without the END_HEADERS flag set MUST be followed
	// by a CONTINUATION frame for the same stream.  A receiver MUST
	// treat the receipt of any other type of frame or a frame on a
	// different stream as a connection error (Section 5.4.1) of type PROTOCOL_ERROR.
	if r.pendingHeaders != nil && r.frameType != FrameContinuation {
		return nil, ConnError{
			fmt.Errorf("received frame of type %s while processing headers", r.frameType),
			ErrCodeProtocol,
		}
	}

	var frame frameReaderFrom

	if r.frameType == FrameContinuation {
		// A CONTINUATION frame MUST be preceded by a HEADERS, PUSH_PROMISE or
		// CONTINUATION frame without the END_HEADERS flag set.  A recipient
		// that observes violation of this rule MUST respond with a connection
		// error (Section 5.4.1) of type PROTOCOL_ERROR.
		if r.pendingHeaders == nil {
			return nil, ConnError{
				fmt.Errorf("received %s frame but not currently processing headers", r.frameType),
				ErrCodeProtocol,
			}
		}

		frame = r.pendingHeaders
	} else {
		if ctor, exists := frameCtor[r.frameType]; exists {
			frame = ctor()
		} else {
			frame = new(UnknownFrame)
		}
	}

	if err = frame.readFrom(r); err != nil {
		// A decoding error in a header block MUST be treated as a connection error
		// (Section 5.4.1) of type COMPRESSION_ERROR.
		if _, ok := err.(hpack.DecodingError); ok {
			return nil, ConnError{err, ErrCodeCompression}
		}

		// SEE 10.5.  Denial-of-Service Considerations
		//     10.5.1.  Limits on Header Block Size
		if err == hpack.ErrHeaderFieldsTooLarge {
			return nil, ConnError{err, ErrCodeEnhanceYourCalm}
		}
	}

	if r.pendingHeaders != nil {
		goto again
	}

	return frame, nil
}

type framePayload struct {
	r *frameReader
	n int
	p uint8
}

func (p *framePayload) Read(dst []byte) (n int, err error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if p.n <= 0 {
		return 0, io.EOF
	}
	if len(dst) > p.n {
		dst = dst[:p.n]
	}
	n, err = p.r.Read(dst)
	p.n -= n
	return
}

func (p *framePayload) Close() (err error) {
	if n := p.n + int(p.p); n > 0 {
		_, err = p.r.Discard(n)
		p.n = 0
		p.p = 0
	}
	return
}

func (f *DataFrame) readFrom(r *frameReader) error {
	// DATA frames MUST be associated with a stream.  If a DATA frame is
	// received whose stream identifier field is 0x0, the recipient MUST
	// respond with a connection error (Section 5.4.1) of type PROTOCOL_ERROR.
	if r.streamID == 0 {
		return ConnError{errors.New("stream ID must be > 0"), ErrCodeProtocol}
	}

	f.DataLen = int(r.payloadLen)

	if r.flags.Has(FlagPadded) {
		f.PadLen, _ = r.ReadByte()

		// If the length of the padding is the length of the
		// frame payload or greater, the recipient MUST treat this as a
		// connection error (Section 5.4.1) of type PROTOCOL_ERROR.
		if uint32(f.PadLen) >= r.payloadLen {
			return ConnError{errors.New("payload too small for padding"), ErrCodeProtocol}
		}

		f.DataLen -= int(f.PadLen) + 1
	}

	f.StreamID = r.streamID
	f.EndStream = r.flags.Has(FlagEndStream)
	r.lastPayload = &framePayload{r, f.DataLen, f.PadLen}
	f.Data = r.lastPayload

	return nil
}

func (f *HeadersFrame) readFrom(r *frameReader) error {
	// HEADERS frames MUST be associated with a stream.  If a HEADERS frame
	// is received whose stream identifier field is 0x0, the recipient MUST
	// respond with a connection error (Section 5.4.1) of type PROTOCOL_ERROR.
	if r.streamID == 0 {
		return ConnError{errors.New("stream ID must be > 0"), ErrCodeProtocol}
	}

	fragmentLen := int(r.payloadLen)

	if r.frameType == FrameContinuation {
		// If the END_HEADERS bit is not set, this frame MUST be followed by
		// another CONTINUATION frame.  A receiver MUST treat the receipt of
		// any other type of frame or a frame on a different stream as a
		// connection error (Section 5.4.1) of type PROTOCOL_ERROR.
		if r.streamID != f.StreamID {
			return ConnError{
				fmt.Errorf("continuation stream ID does not match pending headers: expected %d, but received %d",
					f.StreamID, r.streamID),
				ErrCodeProtocol,
			}
		}
	} else {
		f.StreamID = r.streamID

		if r.flags.Has(FlagPadded) {
			f.PadLen, _ = r.ReadByte()
			fragmentLen -= int(f.PadLen) + 1
		}

		if r.flags.Has(FlagPriority) {
			v := r.readUint32()
			f.StreamDependency = v & 0x7fffffff
			f.Exclusive = f.StreamDependency != v
			f.Weight, _ = r.ReadByte()
			fragmentLen -= 5
		}

		f.EndStream = r.flags.Has(FlagEndStream)

		// Padding that exceeds the size remaining for the header block fragment MUST be
		// treated as a PROTOCOL_ERROR.
		if fragmentLen < 0 {
			return ConnError{errors.New("header block fragment too small for padding"), ErrCodeProtocol}
		}
	}

	var (
		chunkSize int
		chunk     []byte
		err       error
	)

	for fragmentLen > 0 {
		chunkSize = fragmentLen
		if chunkSize > r.bufSize {
			chunkSize = r.bufSize
		}

		if chunk, err = r.Peek(chunkSize); err != nil {
			return err
		}

		if _, err = r.Decode(chunk, r.maxHeaderListSize, f.Header.add); err != nil {
			return err
		}

		r.Discard(chunkSize)

		fragmentLen -= chunkSize
	}

	if r.frameType == f.Type() {
		r.Discard(int(f.PadLen))
	}

	if r.flags.Has(FlagEndHeaders) {
		err = r.Decoder.Reset()
		r.pendingHeaders = nil
	} else {
		r.pendingHeaders = f
	}

	return err
}

func (f *PriorityFrame) readFrom(r *frameReader) error {
	// The PRIORITY frame always identifies a stream.  If a PRIORITY frame
	// is received with a stream identifier of 0x0, the recipient MUST
	// respond with a connection error (Section 5.4.1) of type PROTOCOL_ERROR.
	if r.streamID == 0 {
		return ConnError{errors.New("stream ID must be > 0"), ErrCodeProtocol}
	}

	// A PRIORITY frame with a length other than 5 octets MUST be treated as
	// a stream error (Section 5.4.2) of type FRAME_SIZE_ERROR.
	if r.payloadLen != 5 {
		if _, err := r.Discard(int(r.payloadLen)); err != nil {
			return err
		}

		return StreamError{
			fmt.Errorf("bad frame length %d", r.payloadLen),
			ErrCodeFrameSize,
			r.streamID,
		}
	}

	x := r.readUint32()
	f.StreamDependency = x & 0x7fffffff
	f.Exclusive = f.StreamDependency != x
	f.Weight, _ = r.ReadByte()

	return nil
}

func (f *RSTStreamFrame) readFrom(r *frameReader) error {
	// RST_STREAM frames MUST be associated with a stream.  If a RST_STREAM
	// frame is received with a stream identifier of 0x0, the recipient MUST
	// treat this as a connection error (Section 5.4.1) of type PROTOCOL_ERROR.
	if r.streamID == 0 {
		return ConnError{errors.New("stream ID must be > 0"), ErrCodeProtocol}
	}

	// A RST_STREAM frame with a length other than 4 octets MUST be treated
	// as a connection error (Section 5.4.1) of type FRAME_SIZE_ERROR.
	if r.payloadLen != 4 {
		return ConnError{fmt.Errorf("bad frame length %d", r.payloadLen), ErrCodeFrameSize}
	}

	f.StreamID = r.streamID
	f.ErrCode = ErrCode(r.readUint32())

	return nil
}

func (f *SettingsFrame) readFrom(r *frameReader) error {
	// The stream identifier for a SETTINGS frame MUST be zero (0x0).  If an
	// endpoint receives a SETTINGS frame whose stream identifier field is
	// anything other than 0x0, the endpoint MUST respond with a connection
	// error (Section 5.4.1) of type PROTOCOL_ERROR.
	if r.streamID != 0 {
		return ConnError{errors.New("stream ID must be zero"), ErrCodeProtocol}
	}

	// Receipt of a SETTINGS frame with the ACK flag set and a length
	// field value other than 0 MUST be treated as a connection error
	// (Section 5.4.1) of type FRAME_SIZE_ERROR.
	if r.flags.Has(FlagAck) {
		if r.payloadLen > 0 {
			return ConnError{errors.New("ACK settings frame must have an empty payload"), ErrCodeFrameSize}
		}

		f.Ack = true

		return nil
	}

	// A SETTINGS frame with a length other than a multiple of 6 octets MUST
	// be treated as a connection error (Section 5.4.1) of type FRAME_SIZE_ERROR.
	if r.payloadLen%settingLen != 0 {
		return ConnError{fmt.Errorf("bad frame length %d", r.payloadLen), ErrCodeFrameSize}
	}

	var err error

	for i := 0; i < int(r.payloadLen/settingLen); i++ {
		setting, _ := r.Peek(settingLen)
		id := SettingID(binary.BigEndian.Uint16(setting[:2]))
		value := binary.BigEndian.Uint32(setting[2:6])
		r.Discard(settingLen)

		if err = f.Settings.SetValue(id, value); err != nil {
			switch id {
			case SettingInitialWindowSize:
				// Values above the maximum flow-control window size of 2^31-1 MUST
				// be treated as a connection error (Section 5.4.1) of type FLOW_CONTROL_ERROR.
				err = ConnError{err, ErrCodeFlowControl}
			case SettingMaxFrameSize:
				// The initial value is 2^14 (16,384) octets.  The value advertised
				// by an endpoint MUST be between this initial value and the maximum
				// allowed frame size (2^24-1 or 16,777,215 octets), inclusive.
				// Values outside this range MUST be treated as a connection error
				// (Section 5.4.1) of type PROTOCOL_ERROR.
				err = ConnError{err, ErrCodeFrameSize}
			default:
				err = ConnError{err, ErrCodeProtocol}
			}

			break
		}
	}

	return err
}

func (f *PushPromiseFrame) readFrom(r *frameReader) error {
	// The stream identifier of a PUSH_PROMISE frame indicates the stream it is
	// associated with.  If the stream identifier field specifies the value
	// 0x0, a recipient MUST respond with a connection error (Section 5.4.1) of type PROTOCOL_ERROR.
	if r.streamID == 0 {
		return ConnError{errors.New("stream ID must be > 0"), ErrCodeProtocol}
	}

	fragmentLen := int(r.payloadLen)

	if r.frameType == FrameContinuation {
		// If the END_HEADERS bit is not set, this frame MUST be followed by
		// another CONTINUATION frame.  A receiver MUST treat the receipt of
		// any other type of frame or a frame on a different stream as a
		// connection error (Section 5.4.1) of type PROTOCOL_ERROR.
		if r.streamID != f.StreamID {
			return ConnError{
				fmt.Errorf("continuation stream ID does not match pending headers: expected %d, but received %d",
					f.StreamID, r.streamID),
				ErrCodeProtocol,
			}
		}
	} else {
		f.StreamID = r.streamID

		if r.flags.Has(FlagPadded) {
			f.PadLen, _ = r.ReadByte()
			fragmentLen -= int(f.PadLen) + 1

			// The PUSH_PROMISE frame can include padding.  Padding fields and flags
			// are identical to those defined for DATA frames (Section 6.1).
			if uint32(f.PadLen) >= r.payloadLen {
				return ConnError{errors.New("payload too small for padding"), ErrCodeProtocol}
			}
		}

		f.PromisedStreamID = r.readUint32() & (1<<31 - 1)

		fragmentLen -= 4
	}

	var (
		chunkSize int
		chunk     []byte
		err       error
	)

	for fragmentLen > 0 {
		chunkSize = fragmentLen
		if chunkSize > r.bufSize {
			chunkSize = r.bufSize
		}

		if chunk, err = r.Peek(chunkSize); err != nil {
			return err
		}

		if _, err = r.Decode(chunk, r.maxHeaderListSize, f.Header.add); err != nil {
			return err
		}

		r.Discard(chunkSize)

		fragmentLen -= chunkSize
	}

	if r.frameType == f.Type() {
		r.Discard(int(f.PadLen))
	}

	if r.flags.Has(FlagEndHeaders) {
		err = r.Decoder.Reset()
		r.pendingHeaders = nil
	} else {
		r.pendingHeaders = f
	}

	return err
}

func (f *PingFrame) readFrom(r *frameReader) error {
	// PING frames are not associated with any individual stream.  If a PING
	// frame is received with a stream identifier field value other than
	// 0x0, the recipient MUST respond with a connection error
	// (Section 5.4.1) of type PROTOCOL_ERROR.
	if r.streamID != 0 {
		return ConnError{errors.New("stream ID must be zero"), ErrCodeProtocol}
	}

	// Receipt of a PING frame with a length field value other than 8 MUST
	// be treated as a connection error (Section 5.4.1) of type FRAME_SIZE_ERROR.
	if r.payloadLen != 8 {
		return ConnError{fmt.Errorf("bad frame length %d", r.payloadLen), ErrCodeFrameSize}
	}

	f.Ack = r.flags.Has(FlagAck)

	_, err := io.ReadFull(r, f.Data[:])

	return err
}

func (f *GoAwayFrame) readFrom(r *frameReader) error {
	// The GOAWAY frame applies to the connection, not a specific stream.
	// An endpoint MUST treat a GOAWAY frame with a stream identifier other
	// than 0x0 as a connection error (Section 5.4.1) of type PROTOCOL_ERROR.
	if r.streamID != 0 {
		return ConnError{errors.New("stream ID must be zero"), ErrCodeProtocol}
	}

	if r.payloadLen < 8 {
		return ConnError{fmt.Errorf("bad frame length %d", r.payloadLen), ErrCodeProtocol}
	}

	f.LastStreamID = r.readUint32() & (1<<31 - 1)
	f.ErrCode = ErrCode(r.readUint32())
	f.DebugData = make([]byte, r.payloadLen-8)

	_, err := io.ReadFull(r, f.DebugData)

	return err
}

func (f *WindowUpdateFrame) readFrom(r *frameReader) error {
	// A WINDOW_UPDATE frame with a length other than 4 octets MUST be
	// treated as a connection error (Section 5.4.1) of type FRAME_SIZE_ERROR.
	if r.payloadLen != 4 {
		return ConnError{fmt.Errorf("bad frame length %d", r.payloadLen), ErrCodeFrameSize}
	}

	f.StreamID = r.streamID
	f.WindowSizeIncrement = r.readUint32() & 0x7fffffff

	// A receiver MUST treat the receipt of a WINDOW_UPDATE frame with an
	// flow-control window increment of 0 as a stream error (Section 5.4.2)
	// of type PROTOCOL_ERROR; errors on the connection flow-control window
	// MUST be treated as a connection error (Section 5.4.1).
	if f.WindowSizeIncrement == 0 {
		err := fmt.Errorf("received WINDOW_UPDATE with delta 0 for stream ID: %d", f.StreamID)
		if f.StreamID == 0 {
			return ConnError{err, ErrCodeProtocol}
		}
		return StreamError{err, ErrCodeProtocol, f.StreamID}
	}

	return nil
}

func (f *UnknownFrame) readFrom(r *frameReader) error {
	f.FrameType = r.frameType
	f.StreamID = r.streamID
	f.Flags = r.flags
	f.PayloadLen = int(r.payloadLen)
	r.lastPayload = &framePayload{r, f.PayloadLen, 0}
	f.Payload = r.lastPayload

	return nil
}

func (r *frameReader) readUint32() (v uint32) {
	b, _ := r.Peek(4)
	v = binary.BigEndian.Uint32(b)
	r.Discard(4)

	return
}
