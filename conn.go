package http2

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
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

	flowControlWriter flowControlWriter

	connStream *stream

	streamL sync.RWMutex
	streams map[uint32]*stream

	closing int32
	closed  int32
	closeCh chan struct{}

	settingsCh chan Settings
	*connState
	remote *connState

	idCh    chan struct{}
	idState int32
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
	conn.flowControlWriter = &streamWriter{}
	conn.connStream = &stream{conn: conn, id: 0, weight: defaultWeight}
	w := int(defaultInitialWindowSize)
	conn.connStream.recvFlow = &flowController{s: conn.connStream, win: w, winUpperBound: w, processedWin: w}
	conn.connStream.sendFlow = &remoteFlowController{s: conn.connStream, winCh: make(chan int, 1)}
	conn.connStream.sendFlow.incrementInitialWindow(w)
	conn.streams = make(map[uint32]*stream)
	conn.closeCh = make(chan struct{})
	conn.settingsCh = make(chan Settings, 4)
	conn.connState = &connState{conn: conn, server: server}
	conn.remote = &connState{conn: conn, server: !server}
	if server {
		conn.nextStreamID = 2
		conn.settings.Store(Settings{setting{SettingEnablePush, 0}})
		conn.remote.nextStreamID = 3
		conn.remote.settings.Store(Settings{})
	} else {
		conn.nextStreamID = 3
		conn.settings.Store(Settings{})
		conn.remote.nextStreamID = 2
		conn.remote.settings.Store(Settings{setting{SettingEnablePush, 0}})
	}
	conn.idCh = make(chan struct{}, 1)
	conn.idCh <- struct{}{}

	go conn.writeLoop()

	return conn
}

func (c *Conn) ServerConn() bool {
	return c.server
}

func (c *Conn) NumActiveStreams() uint32 {
	return atomic.LoadUint32(&c.numStreams) + atomic.LoadUint32(&c.remote.numStreams)
}

func (c *Conn) NextStreamID() (uint32, error) {
	select {
	case <-c.idCh:
		atomic.StoreInt32(&c.idState, 1)
		return c.nextStreamID, nil
	case <-c.closeCh:
		return 0, ErrClosed
	}
}

func (c *Conn) LastStreamID() uint32 {
	return atomic.LoadUint32(&c.remote.lastStreamID)
}

func (c *Conn) Settings() Settings {
	return c.settings.Load().(Settings)
}

func (c *Conn) RemoteSettings() Settings {
	return c.remote.settings.Load().(Settings)
}

func (c *Conn) GoAwayReceived() (bool, *GoAwayFrame) {
	goAway, received := c.goAway.Load().(*GoAwayFrame)
	return received, goAway
}

func (c *Conn) GoAwaySent() (bool, *GoAwayFrame) {
	goAway, sent := c.remote.goAway.Load().(*GoAwayFrame)
	return sent, goAway
}

func (c *Conn) goingAway() bool {
	return c.remote.goAway.Load() != nil || c.goAway.Load() != nil
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

	if c.goingAway() && c.NumActiveStreams() == 0 {
		c.Flush()
	}
}

type connState struct {
	conn   *Conn
	server bool
	numStreams,
	nextStreamID,
	lastStreamID uint32
	settings,
	goAway atomic.Value
}

var errClosedStream = ConnError{errors.New("closed stream"), ErrCodeProtocol}

func (s *connState) idleStream(streamID uint32) (*stream, error) {
	// Receivers of a GOAWAY frame MUST NOT open
	// additional streams on the connection, although a new connection can
	// be established for new streams.
	if goAway, received := s.conn.goAway.Load().(*GoAwayFrame); received {
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

	if atomic.LoadUint32(&s.numStreams)+1 > s.settings.Load().(Settings).MaxConcurrentStreams() {
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
	stream.wio <- struct{}{}
	s.nextStreamID = streamID + 2
	atomic.StoreUint32(&s.lastStreamID, streamID)

	return stream, nil
}

func (s *connState) validStreamID(streamID uint32) bool {
	return s.server == ((streamID&1) == 0) && streamID > 0
}

func (c *Conn) WriteFrame(frame Frame) (err error) {
	if c.Closed() {
		return ErrClosed
	}
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
				if atomic.CompareAndSwapInt32(&c.idState, 1, 0) {
					c.idCh <- struct{}{}
				}
			}()

			if stream, err = c.idleStream(frame.streamID()); err != nil {
				break
			}
		}
		if _, err = stream.transition(false, FrameHeaders, false); err == nil {
			return c.flowControlWriter.Write(stream, frame)
		}
	case FramePriority:
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
		if c.goAway.Load() != nil {
			err = ConnError{errors.New("sending PUSH_PROMISE after GO_AWAY received"), ErrCodeProtocol}
			break
		}
		stream := c.stream(frame.streamID())
		if stream == nil {
			err = ConnError{fmt.Errorf("stream %d does not exist", frame.streamID()), ErrCodeProtocol}
			break
		}
		if !stream.writable() {
			err = ConnError{fmt.Errorf("stream %d is not active", frame.streamID()), ErrCodeProtocol}
			break
		}
		if !c.RemoteSettings().PushEnabled() {
			err = ConnError{errors.New("server push not allowed"), ErrCodeProtocol}
			break
		}

		defer func() {
			if atomic.CompareAndSwapInt32(&c.idState, 1, 0) {
				c.idCh <- struct{}{}
			}
		}()

		if stream, err = c.idleStream(frame.(*PushPromiseFrame).PromisedStreamID); err == nil {
			_, err = stream.transition(false, FramePushPromise, false)
		}
		if err == nil {
			c.writeQueue.add(frame, false)
		}
	case FramePing:
		if frame.(*PingFrame).Ack {
			return errors.New("not allowed to send ACK ping frame")
		}
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
		if goAway, sent := c.remote.goAway.Load().(*GoAwayFrame); sent {
			if frame.(*GoAwayFrame).LastStreamID > goAway.LastStreamID {
				return fmt.Errorf("last stream ID must be <= %d", goAway.LastStreamID)
			}
		}

		c.remote.goAway.Store(frame)

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
		} else if stream := c.stream(frame.streamID()); stream != nil {
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
	err := c.flowControlWriter.Flush()
	c.writeQueue.add(nil, false)
	return err
}

func (c *Conn) Closed() bool {
	return atomic.LoadInt32(&c.closed) == 1
}

var defaultCloseTimeout = 3 * time.Second

func (c *Conn) Close() error {
	return c.CloseTimeout(defaultCloseTimeout)
}

func (c *Conn) CloseTimeout(timeout time.Duration) error {
	if atomic.CompareAndSwapInt32(&c.closing, 0, 1) {
		// Endpoints SHOULD send a GOAWAY frame when ending a connection,
		// providing that circumstances permit it.
		c.WriteFrame(&GoAwayFrame{LastStreamID: c.LastStreamID(), ErrCode: ErrCodeNo})

		c.flowControlWriter.Flush()

		select {
		case <-c.closeCh:
			return nil
		case <-time.After(timeout):
			return fmt.Errorf("http2: close timeout; %v", c.close())
		}
	}
	<-c.closeCh
	return ErrClosed
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

func (c *Conn) LocalAddr() net.Addr {
	if nc, ok := c.rwc.(net.Conn); ok {
		return nc.LocalAddr()
	}
	return nil
}

func (c *Conn) RemoteAddr() net.Addr {
	if nc, ok := c.rwc.(net.Conn); ok {
		return nc.RemoteAddr()
	}
	return nil
}

func (c *Conn) writeLoop() {
	var (
		frame Frame
		err   error
	)

loop:
	for {
		select {
		case frame = <-c.writeQueue.get():
			flush := !c.writeQueue.set()

			if frame == nil {
				err = c.buf.Flush()
				if flush && c.goingAway() && c.NumActiveStreams() == 0 && !c.writeQueue.set() {
					c.close()
					return
				}
				if err != nil {
					c.handleErr(err)
				}
				continue loop
			}

			var settingsSyn bool

			switch frame.Type() {
			case FrameSettings:
				v := frame.(*SettingsFrame)
				settingsSyn = !v.Ack
				if v.Ack {
					remote := c.remote.settings.Load().(Settings)
				set:
					for _, setting := range v.Settings {
						switch setting.ID {
						case SettingEnablePush:
						case SettingMaxConcurrentStreams:
						case SettingInitialWindowSize:
							delta := int(setting.Value) - int(remote.InitialWindowSize())
							if err = c.setInitialSendWindow(delta); err != nil {
								c.handleErr(err)
								continue loop
							}
						case SettingHeaderTableSize:
							c.frameWriter.SetMaxHeaderTableSize(setting.Value)
						case SettingMaxHeaderListSize:
							c.frameWriter.maxHeaderListSize = setting.Value
						case SettingMaxFrameSize:
							c.frameWriter.maxFrameSize = setting.Value
						default:
							continue set
						}

						remote.SetValue(setting.ID, setting.Value)
					}
					c.remote.settings.Store(remote)
					v.Settings = nil
				}
			}

			err = c.frameWriter.WriteFrame(frame)

			if flush {
				if err == nil {
					err = c.buf.Flush()
				}
				if c.goingAway() && c.NumActiveStreams() == 0 && !c.writeQueue.set() {
					c.close()
					return
				}
			}

			if err != nil {
				if settingsSyn {
					select {
					case <-c.settingsCh:
					default:
					}
				}

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
	if goAway, sent := c.remote.goAway.Load().(*GoAwayFrame); sent {
		if c.remote.validStreamID(frame.streamID()) && frame.streamID() > goAway.LastStreamID {
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
				if ce, ok := stream.recvFlow.consumeBytes(dataLen).(ConnError); ok {
					err = ce
					break
				}
				if ce, ok := stream.recvFlow.returnBytes(dataLen).(ConnError); ok {
					err = ce
				}
			}
			break
		}

		if err = stream.recvFlow.consumeBytes(dataLen); err != nil {
			switch err.(type) {
			case ConnError:
			default:
				if ce, ok := stream.recvFlow.returnBytes(dataLen).(ConnError); ok {
					err = ce
				}
			}
			break
		}
		c.lastData = &data{stream: stream, src: v.Data, endStream: v.EndStream}
		v.Data = c.lastData
	case *HeadersFrame:
		stream := c.stream(v.StreamID)
		if stream == nil {
			if stream, err = c.remote.idleStream(v.StreamID); err != nil {
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
				if settings.PushEnabled() && c.server {
					err = ConnError{
						errors.New("server sent SETTINGS frame with ENABLE_PUSH specified"),
						ErrCodeProtocol,
					}
					goto exit
				}

				local := c.settings.Load().(Settings)

				for _, setting := range settings {
					switch setting.ID {
					case SettingEnablePush:
					case SettingMaxConcurrentStreams:
					case SettingInitialWindowSize:
						delta := int(setting.Value) - int(local.InitialWindowSize())
						if err = c.setInitialRecvWindow(delta); err != nil {
							goto exit
						}
					case SettingHeaderTableSize:
						c.frameReader.SetMaxHeaderTableSize(setting.Value)
					case SettingMaxHeaderListSize:
						c.frameReader.maxHeaderListSize = setting.Value
					case SettingMaxFrameSize:
						c.frameReader.maxFrameSize = setting.Value
					default:
						continue
					}

					local.SetValue(setting.ID, setting.Value)
				}

				c.settings.Store(local)
			default:
				err = errors.New("received SETTINGS frame but empty pool")
			}
			break
		}

		if v.PushEnabled() && !c.server {
			err = ConnError{
				errors.New("client received SETTINGS frame with ENABLE_PUSH specified for remote server"),
				ErrCodeProtocol,
			}
			break
		}
		c.writeQueue.add(&SettingsFrame{true, v.Settings}, true)
	case *PushPromiseFrame:
		stream := c.stream(frame.streamID())
		if stream == nil {
			err = ConnError{fmt.Errorf("stream %d does not exist", v.StreamID), ErrCodeProtocol}
			break
		}
		if !stream.readable() {
			err = ConnError{fmt.Errorf("stream %d is not active", v.StreamID), ErrCodeProtocol}
			break
		}
		if !c.Settings().PushEnabled() {
			err = ConnError{errors.New("server push not allowed"), ErrCodeProtocol}
			break
		}
		if stream, err = c.idleStream(v.PromisedStreamID); err == nil {
			_, err = stream.transition(true, FramePushPromise, false)
		}
	case *PingFrame:
		if !v.Ack {
			c.writeQueue.add(&PingFrame{true, v.Data}, true)
		}
	case *GoAwayFrame:
		c.goAway.Store(v)

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
		} else if stream := c.stream(v.StreamID); stream != nil {
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
		if ne, ok := err.(net.Error); ok {

			// TODO: handle temporary, write deadline

			if ne.Temporary() {
				goto again
			}
			return nil, c.close()
		}

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
		c.WriteFrame(&GoAwayFrame{c.LastStreamID(), e.ErrCode, []byte(e.Error())})
	default:
		c.WriteFrame(&GoAwayFrame{c.LastStreamID(), ErrCodeInternal, []byte(e.Error())})
	}
}

var ErrClosed = errors.New("http2: connection has been closed")
