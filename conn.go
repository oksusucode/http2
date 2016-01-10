package http2

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type Conn struct {
	rwc io.ReadWriteCloser
	buf *bufio.ReadWriter

	rio         sync.Mutex
	frameReader *frameReader
	lastData    *data

	frameWriter *frameWriter
	writeQueue  *writeQueue

	initialRecvWindow,
	initialSendWindow uint32

	flowControlWriter flowControlWriter

	settingsCh chan Settings

	connStream *stream

	streamL sync.RWMutex
	streams map[uint32]*stream

	closing int32
	closed  int32
	closeCh chan struct{}

	goAwaySent,
	goAwayReceived atomic.Value

	*connState
	remote *connState

	idCh chan uint32
}

const defaultBufSize = 4096

func NewConn(rwc io.ReadWriteCloser, server bool) *Conn {
	return NewConnSize(rwc, server, defaultBufSize, defaultBufSize)
}

func NewConnSize(rwc io.ReadWriteCloser, server bool, readBufSize, writeBufSize int) *Conn {
	if readBufSize < defaultBufSize {
		readBufSize = defaultBufSize
	}
	if writeBufSize < defaultBufSize {
		writeBufSize = defaultBufSize
	}

	conn := new(Conn)
	conn.rwc = rwc
	conn.buf = bufio.NewReadWriter(bufio.NewReaderSize(rwc, readBufSize), bufio.NewWriterSize(rwc, writeBufSize))
	conn.frameReader = newFrameReader(conn.buf.Reader, readBufSize)
	conn.frameWriter = newFrameWriter(conn.buf.Writer)
	conn.writeQueue = &writeQueue{ch: make(chan Frame, 1)}
	conn.initialRecvWindow = defaultInitialWindowSize
	conn.initialSendWindow = defaultInitialWindowSize
	conn.flowControlWriter = &streamWriter{}
	conn.settingsCh = make(chan Settings, 4)
	conn.connStream = &stream{conn: conn, id: 0, weight: defaultWeight}
	w := int(defaultInitialWindowSize)
	conn.connStream.recvFlow = &flowController{s: conn.connStream, win: w, winUpperBound: w, processedWin: w}
	conn.connStream.sendFlow = &remoteFlowController{s: conn.connStream, winCh: make(chan int, 1)}
	conn.connStream.sendFlow.incrementInitialWindow(w)
	conn.streams = make(map[uint32]*stream)
	conn.closeCh = make(chan struct{})
	conn.connState = &connState{conn: conn, server: server, maxStreams: defaultMaxConcurrentStreams}
	conn.remote = &connState{conn: conn, server: !server, maxStreams: defaultMaxConcurrentStreams}
	if server {
		conn.nextStreamID = 2
		conn.remote.nextStreamID = 1
		conn.remote.pushEnabled = defaultEnablePush
	} else {
		conn.nextStreamID = 1
		conn.remote.nextStreamID = 2
		conn.pushEnabled = defaultEnablePush
	}
	//
	conn.idCh = make(chan uint32, 1)
	conn.idCh <- conn.nextStreamID

	go conn.writeLoop()

	return conn
}

type idCh struct {
	i, o chan uint32
}

func (c *Conn) NumActiveStreams() uint32 {
	return atomic.LoadUint32(&c.numStreams) + atomic.LoadUint32(&c.remote.numStreams)
}

func (c *Conn) NextStreamID() uint32 {
	select {
	case <-c.closeCh:
		return 0
	case n := <-c.idCh:
		return n
	}
}

func (c *Conn) LastStreamID() uint32 {
	return c.remote.lastStreamID
}

func (c *Conn) GoAwaySent() (bool, *GoAwayFrame) {
	goAway, sent := c.goAwaySent.Load().(*GoAwayFrame)
	return sent, goAway
}

func (c *Conn) GoAwayReceived() (bool, *GoAwayFrame) {
	goAway, received := c.goAwayReceived.Load().(*GoAwayFrame)
	return received, goAway
}

func (c *Conn) goingAway() bool {
	return c.goAwaySent.Load() != nil || c.goAwayReceived.Load() != nil
}

func (c *Conn) stream(streamID uint32) *stream {
	c.streamL.RLock()
	stream := c.streams[streamID]
	c.streamL.RUnlock()

	return stream
}

func (c *Conn) addStream(stream *stream) {
	c.streamL.Lock()
	c.streams[stream.id] = stream
	c.streamL.Unlock()
}

func (c *Conn) removeStream(stream *stream) {
	c.streamL.Lock()
	delete(c.streams, stream.id)
	c.streamL.Unlock()

	if c.goAwaySent.Load() != nil && c.NumActiveStreams() == 0 {
		c.close()
	}
}

type connState struct {
	conn *Conn
	server,
	pushEnabled bool
	maxStreams,
	numStreams uint32
	nextStreamID,
	lastStreamID uint32
}

var errClosedStream = ConnError{errors.New("closed stream"), ErrCodeProtocol}

func (s *connState) newStream(streamID uint32, reserve bool) (*stream, error) {
	// Receivers of a GOAWAY frame MUST NOT open
	// additional streams on the connection, although a new connection can
	// be established for new streams.
	if goAway, ok := s.conn.goAwayReceived.Load().(*GoAwayFrame); ok {
		return nil, ConnError{
			fmt.Errorf("received a GOAWAY frame with last stream id %d", goAway.LastStreamID),
			ErrCodeProtocol,
		}
	}

	if !s.validStreamID(streamID) {
		return nil, ConnError{fmt.Errorf("bad stream id %d", streamID), ErrCodeProtocol}
	}

	if streamID < s.nextStreamID {
		return nil, errClosedStream
	}

	if atomic.LoadUint32(&s.numStreams)+1 > atomic.LoadUint32(&s.maxStreams) {
		return nil, ConnError{errors.New("maximum streams exceeded"), ErrCodeRefusedStream}
	}

	stream := &stream{
		conn:    s.conn,
		id:      streamID,
		state:   StateIdle,
		weight:  defaultWeight,
		wio:     make(chan struct{}, 1),
		werr:    make(chan error),
		closeCh: make(chan struct{}),
	}
	if reserve {
		if stream.local() {
			stream.state = StateReservedLocal
		} else {
			stream.state = StateReservedRemote
		}
	}
	stream.wio <- struct{}{}
	s.nextStreamID = streamID + 2
	s.lastStreamID = streamID

	return stream, nil
}

func (s *connState) validStreamID(streamID uint32) bool {
	return s.server == ((streamID&1) == 0) && streamID > 0
}

func (c *Conn) WriteFrame(frame Frame) (err error) {
	if frame == nil {
		return errors.New("frame must be not nil")
	}

	switch frame.Type() {
	case FrameData:
		stream := c.stream(frame.streamID())
		if stream == nil {
			return fmt.Errorf("stream %d does not exist", frame.streamID())
		}
		if _, err = stream.transition(false, FrameData, false); err == nil {
			return c.flowControlWriter.Write(stream, frame)
		}
	case FrameHeaders:
		stream := c.stream(frame.streamID())
		if stream == nil {
			defer func() {
				select {
				case c.idCh <- c.nextStreamID:
				default:
				}
			}()
			if stream, err = c.newStream(frame.streamID(), false); err != nil {
				break
			}
		}
		if _, err = stream.transition(false, FrameHeaders, false); err == nil {
			return c.flowControlWriter.Write(stream, frame)
		}
	case FramePriority:
		//
		// An endpoint MUST NOT send frames other than PRIORITY on a closed stream.
		//
	case FrameRSTStream:
		stream := c.stream(frame.streamID())
		if stream == nil {
			return
		}
		if _, err = stream.transition(false, FrameRSTStream, false); err == nil {
			c.writeQueue.add(frame, true)
		}
	case FrameSettings:
		v := frame.(*SettingsFrame)
		if v.Ack {
			return errors.New("not allowed to send ACK settings frame")
		}
		select {
		case c.settingsCh <- v.Settings:
			c.writeQueue.add(frame, true)
		default:
			return errors.New("settings pool overflow")
		}
	case FramePushPromise:
		if c.goAwayReceived.Load() != nil {
			err = ConnError{errors.New("sending PUSH_PROMISE after GO_AWAY received"), ErrCodeProtocol}
			break
		}
		stream := c.stream(frame.streamID())
		if stream == nil {
			err = ConnError{fmt.Errorf("stream %d does not exist", frame.streamID()), ErrCodeProtocol}
			break
		}
		// if !stream.active() {
		// 	err = ConnError{fmt.Errorf("stream %d is not active", frame.streamID()), ErrCodeProtocol}
		// 	break
		// }
		// var cs *connState
		// if stream.local() {
		// 	cs = c.remote
		// } else {
		// 	cs = c.connState
		// }
		// if !cs.pushEnabled {
		// 	err = ConnError{errors.New("server push not allowed"), ErrCodeProtocol}
		// 	break
		// }
		// promisedID := frame.(*PushPromiseFrame).PromisedStreamID
		// if err = cs.newStream(promisedID, true); err != nil {
		// 	//
		// 	defer func() {
		// 		select {
		// 		case c.nextIDCh <- c.nextStreamID:
		// 		default:
		// 		}
		// 	}()
		// 	break
		// }
		// c.writeQueue.add(frame, true)
		// c.writeQueue.flush(true)
	case FramePing:
		c.writeQueue.add(frame, true)
	case FrameGoAway:
		// An endpoint MAY send multiple GOAWAY frames if circumstances change.
		// For instance, an endpoint that sends GOAWAY with NO_ERROR during
		// graceful shutdown could subsequently encounter a condition that
		// requires immediate termination of the connection.  The last stream
		// identifier from the last GOAWAY frame received indicates which
		// streams could have been acted upon.  Endpoints MUST NOT increase the
		// value they send in the last stream identifier, since the peers might
		// already have retried unprocessed requests on another connection.
		if goAwaySent, ok := c.goAwaySent.Load().(*GoAwayFrame); ok {
			if frame.(*GoAwayFrame).LastStreamID > goAwaySent.LastStreamID {
				return fmt.Errorf("last stream ID must be <= %d", goAwaySent.LastStreamID)
			}
		}

		c.goAwaySent.Store(frame)

		var streams []*stream

		c.streamL.RLock()
		for _, stream := range c.streams {
			if stream.id > frame.(*GoAwayFrame).LastStreamID && c.remote.validStreamID(stream.id) {
				streams = append(streams, stream)
			}
		}
		c.streamL.RUnlock()

		for _, stream := range streams {
			stream.close()
		}

		c.writeQueue.add(frame, true)
	case FrameWindowUpdate:
		if frame.streamID() == 0 {
			err = c.connStream.recvFlow.incrementWindow(int(frame.(*WindowUpdateFrame).WindowSizeIncrement))
			break
		}
		if stream := c.stream(frame.streamID()); stream != nil {
			// if _, err = stream.transition(false, FrameWindowUpdate, false); err == nil {
			// 	err = stream.recvFlow.incrementWindow(int(frame.(*WindowUpdateFrame).WindowSizeIncrement))
			// }
			err = stream.recvFlow.incrementWindow(int(frame.(*WindowUpdateFrame).WindowSizeIncrement))
		}
	default:
		c.writeQueue.add(frame, false)
	}

	if err != nil {
		c.handleErr(err)
	}

	return
}

func (c *Conn) Flush() error {
	return c.flowControlWriter.Flush()
}

func (c *Conn) Closed() bool {
	return atomic.LoadInt32(&c.closed) == 1
}

func (c *Conn) Close() error {
	if atomic.CompareAndSwapInt32(&c.closing, 0, 1) {
		c.WriteFrame(&GoAwayFrame{LastStreamID: c.remote.lastStreamID, ErrCode: ErrCodeNo})
		c.Flush()

		select {
		case <-c.closeCh:
			return nil
		case <-time.After(3 * time.Second):
			return c.close()
		}
	}

	<-c.closeCh
	return nil
}

func (c *Conn) close() error {
	if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		return ErrClosed
	}

	if c.NumActiveStreams() > 0 {
		var streams []*stream

		c.streamL.RLock()
		for _, stream := range c.streams {
			streams = append(streams, stream)
		}
		c.streamL.RUnlock()

		for _, stream := range streams {
			stream.close()
		}
	}
	close(c.closeCh)
	return c.rwc.Close()
}

func (c *Conn) writeLoop() {
	var (
		frame Frame
		err   error
	)

	for {
		select {
		case frame = <-c.writeQueue.get():
			flush := !c.writeQueue.set()

			if frame == nil {
				err = c.buf.Flush()
				if flush {
					if c.goingAway() && c.NumActiveStreams() == 0 {
						c.close()
						return
					}
				}
				if err != nil {
					c.handleErr(err)
				}
				continue
			}

			switch frame.Type() {
			case FrameSettings:
				v := frame.(*SettingsFrame)
				if v.Ack {
					for _, setting := range v.Settings {
						switch setting.ID {
						case SettingEnablePush:
							if setting.Value == 1 {
								c.remote.pushEnabled = true
							} else {
								c.remote.pushEnabled = false
							}
						case SettingMaxConcurrentStreams:
							atomic.StoreUint32(&c.maxStreams, setting.Value)
						case SettingInitialWindowSize:
							if err = c.setInitialSendWindow(setting.Value); err != nil {
								c.handleErr(err)
								continue
							}
						case SettingHeaderTableSize:
							c.frameWriter.SetMaxHeaderTableSize(setting.Value)
						case SettingMaxHeaderListSize:
							c.frameWriter.maxHeaderListSize = setting.Value
						case SettingMaxFrameSize:
							c.frameWriter.maxFrameSize = setting.Value
						}
					}
					v.Settings = nil
				}
			}

			err = c.frameWriter.WriteFrame(frame)
			if flush {
				if err == nil {
					err = c.buf.Flush()
				}
				if c.goingAway() && c.NumActiveStreams() == 0 {
					c.close()
					return
				}
			}
			if err != nil {
				c.handleErr(err)
			}
		case <-c.closeCh:
			return
		}
	}
}

type writeQueue struct {
	sync.Mutex
	cbuf, buf []Frame
	ch        chan Frame
}

func (w *writeQueue) get() <-chan Frame {
	return w.ch
}

func (w *writeQueue) set() bool {
	w.Lock()
	defer w.Unlock()

	if len(w.cbuf) > 0 {
		select {
		case w.ch <- w.cbuf[0]:
			w.cbuf = w.cbuf[1:]
		default:
		}
		return true
	} else if len(w.buf) > 0 {
		select {
		case w.ch <- w.buf[0]:
			w.buf = w.buf[1:]
		default:
		}
		return true
	}
	return false
}

func (w *writeQueue) add(frame Frame, control bool) {
	w.Lock()
	defer w.Unlock()

	if control {
		w.cbuf = append(w.cbuf, frame)
		select {
		case w.ch <- w.cbuf[0]:
			w.cbuf = w.cbuf[1:]
		default:
		}
	} else {
		w.buf = append(w.buf, frame)
		select {
		case w.ch <- w.buf[0]:
			w.buf = w.buf[1:]
		default:
		}
	}
}

func (c *Conn) ReadFrame() (frame Frame, err error) {
	c.rio.Lock()
	defer c.rio.Unlock()

	if c.lastData != nil {
		err = c.lastData.returnBytesLocked()
		c.lastData = nil
		if err != nil {
			goto exit
		}
	}

again:
	if frame, err = c.frameReader.ReadFrame(); err != nil {
		goto exit
	}

	// After sending a GOAWAY frame, the sender can discard frames for
	// streams initiated by the receiver with identifiers higher than the
	// identified last stream.
	if goAwaySent, ok := c.goAwaySent.Load().(*GoAwayFrame); ok {
		if c.remote.validStreamID(frame.streamID()) && frame.streamID() > goAwaySent.LastStreamID {
			// Flow-controlled frames (i.e., DATA) MUST be counted toward the
			// connection flow-control window.
			if dataFrame, ok := frame.(*DataFrame); ok {
				if dataLen := dataFrame.DataLen + int(dataFrame.PadLen); dataLen > 0 {
					if err = c.connStream.recvFlow.consumeBytes(dataLen); err == nil {
						err = c.connStream.recvFlow.returnBytes(dataLen)
					}
					if err != nil {
						goto exit
					}
				}
			}
			goto again
		}
	}

	switch v := frame.(type) {
	case *DataFrame:
		dataLen := v.DataLen + int(v.PadLen)
		stream := c.stream(v.StreamID)
		if stream == nil {
			if dataLen > 0 {
				if err = c.connStream.recvFlow.consumeBytes(dataLen); err == nil {
					err = c.connStream.recvFlow.returnBytes(dataLen)
				}
				if err != nil {
					goto exit
				}
			}
			goto again
		}

		if _, err = stream.transition(true, FrameData, false); err != nil {
			switch err.(type) {
			case ConnError:
			default:
				err1 := stream.recvFlow.consumeBytes(dataLen)
				if _, ok := err1.(ConnError); !ok {
					err1 = stream.recvFlow.returnBytes(dataLen)
					if _, ok := err1.(ConnError); ok {
						err = err1
					}
				}
			}
			break
		}

		if err = stream.recvFlow.consumeBytes(dataLen); err != nil {
			switch err.(type) {
			case ConnError:
			default:
				err1 := stream.recvFlow.returnBytes(dataLen)
				if _, ok := err1.(ConnError); ok {
					err = err1
				}
			}
			break
		}
		c.lastData = &data{stream: stream, src: v.Data, endStream: v.EndStream}
		v.Data = c.lastData
	case *HeadersFrame:
		stream := c.stream(v.StreamID)
		if stream == nil {
			if stream, err = c.remote.newStream(v.StreamID, false); err != nil {
				break
			}
		}
		if _, err = stream.transition(true, FrameHeaders, v.EndStream); err == nil {
			if v.HasPriority() {
				err = stream.setPriority(v.Priority)
			}
		}
	case *PriorityFrame:
		//
	case *RSTStreamFrame:
		stream := c.stream(v.StreamID)
		if stream == nil {
			goto again
		}
		if _, err = stream.transition(true, FrameRSTStream, false); err != nil {
			goto again
		}
	case *SettingsFrame:
		if v.Ack {
			select {
			case settings := <-c.settingsCh:
				if pushEnabled, ok := settings.PushEnabled(); ok {
					if pushEnabled && c.server {
						err = ConnError{
							errors.New("server sent SETTINGS frame with ENABLE_PUSH specified"),
							ErrCodeProtocol,
						}
						goto exit
					}
				}
				for _, setting := range settings {
					switch setting.ID {
					case SettingEnablePush:
						if setting.Value == 1 {
							c.pushEnabled = true
						} else {
							c.pushEnabled = false
						}
					case SettingMaxConcurrentStreams:
						atomic.StoreUint32(&c.remote.maxStreams, setting.Value)
					case SettingInitialWindowSize:
						if err = c.setInitialRecvWindow(setting.Value); err != nil {
							goto exit
						}
					case SettingHeaderTableSize:
						c.frameReader.SetMaxHeaderTableSize(setting.Value)
					case SettingMaxHeaderListSize:
						c.frameReader.maxHeaderListSize = setting.Value
					case SettingMaxFrameSize:
						c.frameReader.maxFrameSize = setting.Value
					}
				}
			default:
				err = errors.New("received SETTINGS frame but empty pool")
			}
			break
		}

		if pushEnabled, ok := v.PushEnabled(); ok {
			if pushEnabled && c.remote.server {
				err = ConnError{
					errors.New("client received SETTINGS frame with ENABLE_PUSH specified for remote server"),
					ErrCodeProtocol,
				}
				break
			}
		}
		c.writeQueue.add(&SettingsFrame{true, v.Settings}, true)
	case *PushPromiseFrame:
		//
	case *PingFrame:
		if !v.Ack {
			c.writeQueue.add(&PingFrame{true, v.Data}, true)
		}
	case *GoAwayFrame:
		c.goAwayReceived.Store(v)

		var streams []*stream

		c.streamL.RLock()
		for _, stream := range c.streams {
			if stream.id > v.LastStreamID && c.validStreamID(stream.id) {
				streams = append(streams, stream)
			}
		}
		c.streamL.RUnlock()

		for _, stream := range streams {
			stream.close()
		}
		err = c.Flush()
	case *WindowUpdateFrame:
		if v.StreamID == 0 {
			err = c.connStream.sendFlow.incrementWindow(int(v.WindowSizeIncrement))
			break
		}
		if stream := c.stream(v.StreamID); stream != nil {
			// if _, err = stream.transition(true, FrameWindowUpdate, false); err == nil {
			// 	err = stream.sendFlow.incrementWindow(int(v.WindowSizeIncrement))
			// }
			err = stream.sendFlow.incrementWindow(int(v.WindowSizeIncrement))
		}
	}

exit:
	if err == io.EOF {
		if c.goingAway() || c.Closed() {
			return nil, ErrClosed
		}
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		c.handleErr(err)
	}
	return
}

type data struct {
	stream *stream
	src    io.Reader

	processed int
	endStream,
	sawEOF bool
	err error
}

func (r *data) Read(p []byte) (int, error) {
	r.stream.conn.rio.Lock()
	defer r.stream.conn.rio.Unlock()

	n, err := r.src.Read(p)
	if n > 0 {
		r.processed += n
	}
	if err == io.EOF {
		r.returnBytesLocked()
	}
	return n, err
}

func (r *data) returnBytesLocked() error {
	if r.sawEOF {
		return r.err
	}
	r.sawEOF = true
	if payload, ok := r.src.(*framePayload); ok {
		r.processed += int(payload.p)
	}
	r.err = r.stream.recvFlow.returnBytes(r.processed)
	if r.err == nil && r.endStream {
		_, r.err = r.stream.transition(true, FrameData, true)
	}
	return r.err
}

func (c *Conn) handleErr(err error) {
	if err == nil || err == ErrClosed {
		return
	}

	switch e := err.(type) {
	case StreamError:
		c.WriteFrame(&RSTStreamFrame{e.StreamID, e.ErrCode})
	case StreamErrorList:
		for _, se := range e {
			c.WriteFrame(&RSTStreamFrame{se.StreamID, se.ErrCode})
		}
	case ConnError:
		c.WriteFrame(&GoAwayFrame{c.remote.lastStreamID, e.ErrCode, []byte(e.Error())})
	default:
		c.WriteFrame(&GoAwayFrame{c.remote.lastStreamID, ErrCodeInternal, []byte(e.Error())})
	}
}

var ErrClosed = errors.New("http2: connection has been closed")
