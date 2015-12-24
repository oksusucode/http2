package http2

import (
	"errors"
	"fmt"
	"io"

	"github.com/nekolunar/http2/hpack"
)

func (c *Conn) WriteFrame(frame Frame) (err error) {
	// If the receiver of the GOAWAY has sent data on streams with a higher
	// stream identifier than what is indicated in the GOAWAY frame, those
	// streams are not or will not be processed.  The receiver of the GOAWAY
	// frame can treat the streams as though they had never been created at
	// all, thereby allowing those streams to be retried later on a new connection.
	if goAwayReceived, ok := c.goAwayReceived.Load().(*GoAwayFrame); ok {
		if frame.streamID() > goAwayReceived.LastStreamID {
			return ErrConnClosing
		}
	}

	switch v := frame.(type) {
	case *DataFrame:
		err = c.writeData(v)
	case *HeadersFrame:
		err = c.writeHeaders(v)
	case *PriorityFrame:
		err = c.writePriority(v)
	case *RSTStreamFrame:
		err = c.writeResetStream(v)
	case *SettingsFrame:
		err = c.writeSettings(v)
	case *PushPromiseFrame:
		err = c.writePushPromise(v)
	case *PingFrame:
		err = c.writeFrame(v, true)
	case *GoAwayFrame:
		err = c.writeGoAway(v)
	case *WindowUpdateFrame:
		err = c.writeWindowUpdate(v)
	case *UnknownFrame:
		err = c.writeFrame(v, true)
	}

	if err == ignoreFrame {
		return nil
	}

	if err != nil {

		// TODO: verify conn closing

		switch v := err.(type) {
		case StreamError:
			c.writeResetStream(&RSTStreamFrame{v.StreamID, v.ErrCode})
		case StreamErrorList:
			for _, se := range v {
				c.writeResetStream(&RSTStreamFrame{se.StreamID, se.ErrCode})
			}
		case ConnError:
			c.writeGoAway(&GoAwayFrame{c.remote.lastStreamID, v.ErrCode, []byte(v.Error())})
		}
	}

	return
}

func (c *Conn) writeData(frame *DataFrame) error {
	stream := c.stream(frame.StreamID)
	if stream == nil {
		return fmt.Errorf("stream %d does not exist", frame.StreamID)
	}
	if _, err := stream.transition(send, FrameData, false); err != nil {
		return err
	}

	if c.server {
		return c.enqueueFlowControlledFrame(stream, frame)
	}

	// TODO: client-side data write

	return nil
}

func (c *Conn) writeHeaders(frame *HeadersFrame) error {
	stream := c.stream(frame.StreamID)
	if stream == nil {
		var err error
		if stream, err = c.newStream(frame.StreamID); err != nil {
			return err
		}
		stream.transition(send, FrameHeaders, frame.EndStream)
		return c.writeFrame(frame, true)
	}

	if c.server {
		return c.enqueueFlowControlledFrame(stream, frame)
	}

	if _, err := stream.transition(send, FrameHeaders, frame.EndStream); err != nil {
		return err
	}
	return c.writeFrame(frame, true)
}

func (c *Conn) writeResetStream(frame *RSTStreamFrame) error {
	stream := c.stream(frame.StreamID)
	if stream == nil {
		return nil
	}
	if _, err := stream.transition(send, FrameRSTStream, false); err != nil {
		return err
	}
	return c.writeFrame(frame, true)
}

func (c *Conn) writePriority(frame *PriorityFrame) error {
	return nil
}

func (c *Conn) writeSettings(frame *SettingsFrame) error {
	c.wio.Lock()
	defer c.wio.Unlock()

	select {
	case c.settingsCh <- frame.Settings:
		err := c.writeFrameLocked(frame, true)
		if err != nil {
			<-c.settingsCh
		}
		return err
	default:
		return errors.New("xxx")
	}
}

func (c *Conn) writeSettingsAck(settings Settings) error {
	c.wio.Lock()
	defer c.wio.Unlock()

	for _, setting := range settings {
		switch setting.ID {
		case SettingEnablePush:
			if setting.Value == 1 {
				c.remote.pushEnabled = true
			} else {
				c.remote.pushEnabled = false
			}
		case SettingMaxConcurrentStreams:
			c.maxStreams = setting.Value
		case SettingInitialWindowSize:
			if err := c.setInitialSendWindow(setting.Value); err != nil {
				return err
			}
		case SettingHeaderTableSize:
			c.frameWriter.SetMaxHeaderTableSize(setting.Value)
		case SettingMaxHeaderListSize:
			c.frameWriter.maxHeaderListSize = setting.Value
		case SettingMaxFrameSize:
			c.frameWriter.maxFrameSize = setting.Value
		}
	}

	return c.writeFrameLocked(&SettingsFrame{Ack: true}, true)
}

func (c *Conn) writePushPromise(frame *PushPromiseFrame) error {
	return nil
}

func (c *Conn) writeGoAway(frame *GoAwayFrame) error {
	// An endpoint MAY send multiple GOAWAY frames if circumstances change.
	// For instance, an endpoint that sends GOAWAY with NO_ERROR during
	// graceful shutdown could subsequently encounter a condition that
	// requires immediate termination of the connection.  The last stream
	// identifier from the last GOAWAY frame received indicates which
	// streams could have been acted upon.  Endpoints MUST NOT increase the
	// value they send in the last stream identifier, since the peers might
	// already have retried unprocessed requests on another connection.
	if goAwaySent, ok := c.goAwaySent.Load().(*GoAwayFrame); ok {
		if frame.LastStreamID > goAwaySent.LastStreamID {
			return fmt.Errorf("last stream ID must be <= %d", goAwaySent.LastStreamID)
		}
	}
	return nil
}

func (c *Conn) writeWindowUpdate(frame *WindowUpdateFrame) error {
	return nil
}

func (c *Conn) writeFrame(frame Frame, flush bool) error {
	c.wio.Lock()
	defer c.wio.Unlock()

	return c.writeFrameLocked(frame, flush)
}

func (c *Conn) writeFrameLocked(frame Frame, flush bool) error {
	err := c.frameWriter.WriteFrame(frame)
	if err == nil && flush {
		err = c.buf.Flush()
	}
	return err
}

type frameWriter struct {
	io.Writer
	buf          []byte
	maxFrameSize uint32
	err          error

	*hpack.Encoder
	hpackBuf          []byte
	maxHeaderListSize uint32
}

func newFrameWriter(w io.Writer) *frameWriter {
	return &frameWriter{
		Writer:       w,
		maxFrameSize: defaultMaxFrameSize,
		Encoder:      hpack.NewEncoder(defaultHeaderTableSize),
	}
}

type frameWriterTo interface {
	Frame
	writeTo(*frameWriter) error
}

func (w *frameWriter) WriteFrame(frame Frame) error {
	return frame.(frameWriterTo).writeTo(w)
}

var zeroBuf = make([]byte, 255)

func (f *DataFrame) writeTo(w *frameWriter) error {
	if !validStreamID(f.StreamID) {
		return fmt.Errorf("bad stream ID: %d", f.StreamID)
	}

	if f.DataLen < 0 || (f.DataLen > 0 && f.Data == nil) {
		return errors.New("bad data")
	}

	remainingData := uint32(f.DataLen)
	padding := uint32(f.PadLen)

	var lastFrame bool

	for !lastFrame && w.err == nil {
		dataLen := remainingData
		if dataLen > w.maxFrameSize {
			dataLen = w.maxFrameSize
		}

		padLen := w.maxFrameSize - dataLen
		if padLen > 0 {
			padLen -= 1
		}
		if padLen > padding {
			padLen = padding
		}

		remainingData -= dataLen
		padding -= padLen
		lastFrame = remainingData == 0 && padding == 0

		var flags Flags

		if f.EndStream && lastFrame {
			flags = FlagEndStream
		}

		if padLen > 0 {
			writeFrameHeader(w, dataLen+padLen+1, FrameData, flags|FlagPadded, f.StreamID)
			writeByte(w, uint8(padLen))
		} else {
			writeFrameHeader(w, dataLen, FrameData, flags, f.StreamID)
		}

		w.Write(w.buf)

		if dataLen > 0 {
			_, w.err = io.CopyN(w.Writer, f.Data, int64(dataLen))
		}

		w.Write(zeroBuf[:padLen])
	}

	return w.err
}

func (f *HeadersFrame) writeTo(w *frameWriter) error {
	if !validStreamID(f.StreamID) {
		return fmt.Errorf("bad stream ID: %d", f.StreamID)
	}
	if f.HasPriority() && !validStreamID(f.StreamDependency) {
		return fmt.Errorf("bad stream dependency: %d", f.StreamDependency)
	}

	var (
		flags          Flags
		nonFragmentLen uint32
	)

	if f.PadLen > 0 {
		flags |= FlagPadded
		nonFragmentLen += uint32(f.PadLen) + 1
	}
	if f.HasPriority() {
		flags |= FlagPriority
		nonFragmentLen += 5
	}
	if f.EndStream {
		flags |= FlagEndStream
	}

	remainingHeader := f.Header.Len()

	if remainingHeader == 0 {
		writeFrameHeader(w, nonFragmentLen, FrameHeaders, flags|FlagEndHeaders, f.StreamID)
		if flags.Has(FlagPadded) {
			writeByte(w, f.PadLen)
		}
		if flags.Has(FlagPriority) {
			if f.Exclusive {
				writeUint32(w, f.StreamDependency|(1<<31))
			} else {
				writeUint32(w, f.StreamDependency)
			}
			writeByte(w, f.Weight)
		}

		w.Write(w.buf)
		w.Write(zeroBuf[:f.PadLen])

		return w.err
	}

	var (
		n, written     uint32
		firstFrameSent bool
	)

	w.hpackBuf = w.hpackBuf[:0]

	for k, _ := range pseudoHeader {
		if vv, ok := f.Header[k]; ok {
			if len(vv) > 1 {
				return ErrMalformedHeader
			}
			n, w.hpackBuf = w.EncodeHeaderField(w.hpackBuf, k, vv[0], false)
			written += n
			remainingHeader--
		}
	}

	for k, vv := range f.Header {
		if _, pseudo := pseudoHeader[k]; pseudo {
			continue
		}

		if k == "" || k[0] == ':' {
			return ErrMalformedHeader
		}

		k = CanonicalHTTP2HeaderKey(k)

		for _, v := range vv {
			n, w.hpackBuf = w.EncodeHeaderField(w.hpackBuf, k, v, false)
			written += n
			remainingHeader--

			if w.maxHeaderListSize != 0 && written > w.maxHeaderListSize {
				return hpack.ErrHeaderFieldsTooLarge
			}
		}

		if remainingHeader > 0 && uint32(len(w.hpackBuf)) < w.maxFrameSize {
			continue
		}

	write:
		fragmentLen := uint32(len(w.hpackBuf))

		if !firstFrameSent {
			maxFragmentLen := w.maxFrameSize - nonFragmentLen
			if fragmentLen > maxFragmentLen {
				fragmentLen = maxFragmentLen
			} else if remainingHeader == 0 {
				flags |= FlagEndHeaders
			}

			writeFrameHeader(w, fragmentLen+nonFragmentLen, FrameHeaders, flags, f.StreamID)
			if flags.Has(FlagPadded) {
				writeByte(w, f.PadLen)
			}
			if flags.Has(FlagPriority) {
				if f.Exclusive {
					writeUint32(w, f.StreamDependency|(1<<31))
				} else {
					writeUint32(w, f.StreamDependency)
				}
				writeByte(w, f.Weight)
			}

			w.Write(w.buf)
			w.Write(w.hpackBuf[:fragmentLen])
			w.Write(zeroBuf[:f.PadLen])

			firstFrameSent = true
		} else {
			var flags Flags

			if fragmentLen > w.maxFrameSize {
				fragmentLen = w.maxFrameSize
			} else if remainingHeader == 0 {
				flags |= FlagEndHeaders
			}

			writeFrameHeader(w, fragmentLen, FrameContinuation, flags, f.StreamID)

			w.Write(w.buf)
			w.Write(w.hpackBuf[:fragmentLen])
		}

		if w.err != nil {
			return w.err
		}

		remainingBytes := uint32(len(w.hpackBuf)) - fragmentLen

		if remainingBytes > 0 {
			copy(w.hpackBuf[0:remainingBytes], w.hpackBuf[fragmentLen:len(w.hpackBuf)])
		}

		w.hpackBuf = w.hpackBuf[:remainingBytes]

		if remainingBytes >= w.maxFrameSize || (remainingHeader == 0 && remainingBytes > 0) {
			goto write
		}
	}

	return nil
}

func (f *PriorityFrame) writeTo(w *frameWriter) error {
	if !validStreamID(f.StreamID) {
		return fmt.Errorf("bad stream ID: %d", f.StreamID)
	}
	if !validStreamID(f.StreamDependency) {
		return fmt.Errorf("bad stream dependency: %d", f.StreamDependency)
	}

	writeFrameHeader(w, 5, f.Type(), 0, f.StreamID)
	if f.Exclusive {
		writeUint32(w, f.StreamDependency|(1<<31))
	} else {
		writeUint32(w, f.StreamDependency)
	}
	writeByte(w, f.Weight)

	w.Write(w.buf)

	return w.err
}

func (f *RSTStreamFrame) writeTo(w *frameWriter) error {
	if !validStreamID(f.StreamID) {
		return fmt.Errorf("bad stream ID: %d", f.StreamID)
	}

	writeFrameHeader(w, 4, f.Type(), 0, f.StreamID)
	writeUint32(w, uint32(f.ErrCode))

	w.Write(w.buf)

	return w.err
}

func (f *SettingsFrame) writeTo(w *frameWriter) error {
	if f.Ack {
		if len(f.Settings) > 0 {
			return fmt.Errorf("ACK settings frame must have an empty payload")
		}

		writeFrameHeader(w, 0, f.Type(), FlagAck, 0)
	} else {
		writeFrameHeader(w, uint32(settingLen*len(f.Settings)), f.Type(), 0, 0)

		for _, setting := range f.Settings {
			writeUint16(w, uint16(setting.ID))
			writeUint32(w, setting.Value)
		}
	}

	w.Write(w.buf)

	return w.err
}

func (f *PushPromiseFrame) writeTo(w *frameWriter) error {
	if !validStreamID(f.StreamID) {
		return fmt.Errorf("bad stream ID: %d", f.StreamID)
	}
	if !validStreamID(f.PromisedStreamID) {
		return fmt.Errorf("bad promised stream ID: %d", f.PromisedStreamID)
	}

	var (
		flags          Flags
		nonFragmentLen uint32
	)

	nonFragmentLen += 4

	if f.PadLen > 0 {
		flags |= FlagPadded
		nonFragmentLen += uint32(f.PadLen) + 1
	}

	remainingHeader := f.Header.Len()

	if remainingHeader == 0 {
		writeFrameHeader(w, nonFragmentLen, FramePushPromise, flags|FlagEndHeaders, f.StreamID)
		if flags.Has(FlagPadded) {
			writeByte(w, f.PadLen)
		}
		writeUint32(w, f.PromisedStreamID)

		w.Write(w.buf)
		w.Write(zeroBuf[:f.PadLen])

		return w.err
	}

	var (
		n, written     uint32
		firstFrameSent bool
	)

	w.hpackBuf = w.hpackBuf[:0]

	for k, _ := range pseudoHeader {
		if vv, ok := f.Header[k]; ok {
			if len(vv) > 1 {
				return ErrMalformedHeader
			}
			n, w.hpackBuf = w.EncodeHeaderField(w.hpackBuf, k, vv[0], false)
			written += n
			remainingHeader--
		}
	}

	for k, vv := range f.Header {
		if _, pseudo := pseudoHeader[k]; pseudo {
			continue
		}

		if k == "" || k[0] == ':' {
			return ErrMalformedHeader
		}

		k = CanonicalHTTP2HeaderKey(k)

		for _, v := range vv {
			n, w.hpackBuf = w.EncodeHeaderField(w.hpackBuf, k, v, false)
			written += n
			remainingHeader--

			if w.maxHeaderListSize != 0 && written > w.maxHeaderListSize {
				return hpack.ErrHeaderFieldsTooLarge
			}
		}

		if remainingHeader > 0 && uint32(len(w.hpackBuf)) < w.maxFrameSize {
			continue
		}

	write:
		fragmentLen := uint32(len(w.hpackBuf))

		if !firstFrameSent {
			maxFragmentLen := w.maxFrameSize - nonFragmentLen
			if fragmentLen > maxFragmentLen {
				fragmentLen = maxFragmentLen
			} else if remainingHeader == 0 {
				flags |= FlagEndHeaders
			}

			writeFrameHeader(w, fragmentLen+nonFragmentLen, FramePushPromise, flags, f.StreamID)
			if flags.Has(FlagPadded) {
				writeByte(w, f.PadLen)
			}
			writeUint32(w, f.PromisedStreamID)

			w.Write(w.buf)
			w.Write(w.hpackBuf[:fragmentLen])
			w.Write(zeroBuf[:f.PadLen])

			firstFrameSent = true
		} else {
			var flags Flags

			if fragmentLen > w.maxFrameSize {
				fragmentLen = w.maxFrameSize
			} else if remainingHeader == 0 {
				flags |= FlagEndHeaders
			}

			writeFrameHeader(w, fragmentLen, FrameContinuation, flags, f.StreamID)

			w.Write(w.buf)
			w.Write(w.hpackBuf[:fragmentLen])
		}

		if w.err != nil {
			return w.err
		}

		remainingBytes := uint32(len(w.hpackBuf)) - fragmentLen

		if remainingBytes > 0 {
			copy(w.hpackBuf[0:remainingBytes], w.hpackBuf[fragmentLen:len(w.hpackBuf)])
		}

		w.hpackBuf = w.hpackBuf[:remainingBytes]

		if remainingBytes >= w.maxFrameSize || (remainingHeader == 0 && remainingBytes > 0) {
			goto write
		}
	}

	return nil
}

func (f *PingFrame) writeTo(w *frameWriter) error {
	if f.Ack {
		writeFrameHeader(w, uint32(len(f.Data)), f.Type(), FlagAck, 0)
	} else {
		writeFrameHeader(w, uint32(len(f.Data)), f.Type(), 0, 0)
	}

	w.Write(w.buf)
	w.Write(f.Data[:])

	return w.err
}

func (f *GoAwayFrame) writeTo(w *frameWriter) error {
	writeFrameHeader(w, uint32(8+len(f.DebugData)), f.Type(), 0, 0)
	writeUint32(w, f.LastStreamID&(1<<31-1))
	writeUint32(w, uint32(f.ErrCode))

	w.Write(w.buf)
	w.Write(f.DebugData)

	return w.err
}

func (f *WindowUpdateFrame) writeTo(w *frameWriter) error {
	if f.WindowSizeIncrement == 0 || f.WindowSizeIncrement > maxInitialWindowSize {
		return fmt.Errorf("bad window size increment: %d", f.WindowSizeIncrement)
	}

	writeFrameHeader(w, 4, f.Type(), 0, f.StreamID)
	writeUint32(w, f.WindowSizeIncrement)

	w.Write(w.buf)

	return w.err
}

func (f *UnknownFrame) writeTo(w *frameWriter) error {
	if f.PayloadLen < 0 || (f.PayloadLen > 0 && f.Payload == nil) {
		return errors.New("bad payload")
	}

	writeFrameHeader(w, uint32(f.PayloadLen), f.Type(), f.Flags, f.StreamID)

	w.Write(w.buf)

	if f.PayloadLen > 0 {
		_, w.err = io.CopyN(w.Writer, f.Payload, int64(f.PayloadLen))
	}

	return w.err
}

func (w *frameWriter) Write(src []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.Writer.Write(src)
	w.err = err
	return n, err
}

func writeFrameHeader(w *frameWriter, payloadLen uint32, frameType FrameType, flags Flags, streamID uint32) {
	w.buf = append(w.buf[:0],
		byte(payloadLen>>16),
		byte(payloadLen>>8),
		byte(payloadLen),
		byte(frameType),
		byte(flags),
		byte(streamID>>24),
		byte(streamID>>16),
		byte(streamID>>8),
		byte(streamID))
}

func writeByte(w *frameWriter, v byte) {
	w.buf = append(w.buf, v)
}

func writeUint16(w *frameWriter, v uint16) {
	w.buf = append(w.buf, byte(v>>8), byte(v))
}

func writeUint32(w *frameWriter, v uint32) {
	w.buf = append(w.buf, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func validStreamID(streamID uint32) bool {
	return streamID != 0 && streamID&(1<<31) == 0
}
