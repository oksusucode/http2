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

// A Conn represents a HTTP/2 connection.
type Conn struct {
	config *Config

	rwc io.ReadWriteCloser
	buf *bufio.ReadWriter

	handshakeL        sync.Mutex
	handshakeComplete bool
	handshakeErr      error

	upgradeFunc   func() error
	upgradeFrames []Frame

	rio         sync.Mutex
	frameReader *frameReader
	lastData    *data
	data        data

	frameWriter *frameWriter
	writeQueue  *writeQueue

	connStream *stream

	streamL sync.RWMutex
	streams map[uint32]*stream

	closing int32
	closed  int32
	closeCh chan struct{}

	settingsCh chan Settings
	*connState
	remote *connState

	idCh        chan struct{}
	idState     int32
	idTimer     *time.Timer
	idTimeoutCh <-chan time.Time
}

// A Config structure is used to configure a HTTP/2 client or server connection.
type Config struct {
	// InitialSettings specifies the Http2Settings to use for the initial
	// connection settings exchange. If nil, empty settings is used.
	InitialSettings Settings

	// HandshakeTimeout specifies the duration for the handshake to complete.
	HandshakeTimeout time.Duration

	// AllowLowTLSVersion controls whether a server allows the client's
	// TLSVersion is lower than TLS 1.2.
	AllowLowTLSVersion bool

	// ReadBufSize and WriteBufSize specify I/O buffer sizes. If the buffer
	// size is zero, then a default value of 4096 is used. The I/O buffer sizes
	// do not limit the size of the frames that can be sent or received.
	ReadBufSize, WriteBufSize int
}

var defaultConfig = Config{}

func newConn(rwc io.ReadWriteCloser, server bool, config *Config) *Conn {
	conn := new(Conn)
	conn.config = config
	if conn.config == nil {
		conn.config = &defaultConfig
	}
	conn.rwc = rwc

	const (
		defaultBufSize = 4096
		minReadBufSize = len(ClientPreface)
	)

	readBufSize := conn.config.ReadBufSize

	if readBufSize <= 0 {
		readBufSize = defaultBufSize
	}
	if readBufSize < minReadBufSize {
		readBufSize = minReadBufSize
	}

	conn.buf = bufio.NewReadWriter(bufio.NewReaderSize(rwc, readBufSize), bufio.NewWriterSize(rwc, conn.config.WriteBufSize))
	conn.frameReader = newFrameReader(conn.buf.Reader, readBufSize)
	conn.frameWriter = newFrameWriter(conn.buf.Writer)
	conn.writeQueue = &writeQueue{ch: make(chan Frame, 1)}
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
		conn.remote.nextStreamID = 1
		conn.remote.settings.Store(Settings{})
	} else {
		conn.nextStreamID = 1
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
again:
	select {
	case <-c.closeCh:
		return 0, ErrClosed
	case <-c.idCh:
		const cancelTimeout = 1 * time.Second

		if c.idTimer == nil {
			c.idTimer = time.NewTimer(cancelTimeout)
			c.idTimeoutCh = c.idTimer.C
		} else {
			c.idTimer.Reset(cancelTimeout)
		}

		atomic.StoreInt32(&c.idState, 1)

		if c.nextStreamID > 1 {
			return c.nextStreamID, nil
		}
		return c.nextStreamID + 2, nil
	case <-c.idTimeoutCh:
		select {
		case <-c.closeCh:
			return 0, ErrClosed
		default:
			if atomic.CompareAndSwapInt32(&c.idState, 1, 0) {
				c.idCh <- struct{}{}
			}
			goto again
		}
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

func (s *connState) applySettings(settings Settings) (err error) {
	cur := s.settings.Load().(Settings)
	local := s.conn.connState == s
	for _, setting := range settings {
		switch setting.ID {
		case SettingEnablePush:
			if setting.Value == 1 {
				switch {
				case local && s.server:
					return ConnError{
						errors.New("server sent SETTINGS frame with ENABLE_PUSH specified"),
						ErrCodeProtocol,
					}
				case !local && !s.server:
					return ConnError{
						errors.New("client received SETTINGS frame with ENABLE_PUSH specified for remote server"),
						ErrCodeProtocol,
					}
				}
			}
		case SettingMaxConcurrentStreams:
		case SettingInitialWindowSize:
			delta := int(setting.Value) - int(cur.InitialWindowSize())
			if local {
				err = s.conn.setInitialRecvWindow(delta)
			} else {
				err = s.conn.setInitialSendWindow(delta)
			}
			if err != nil {
				return
			}
		case SettingHeaderTableSize:
			if local {
				s.conn.frameReader.SetMaxHeaderTableSize(setting.Value)
			} else {
				s.conn.frameWriter.SetMaxHeaderTableSize(setting.Value)
			}
		case SettingMaxHeaderListSize:
			if local {
				s.conn.frameReader.maxHeaderListSize = setting.Value
			} else {
				s.conn.frameWriter.maxHeaderListSize = setting.Value
			}
		case SettingMaxFrameSize:
			if local {
				s.conn.frameReader.maxFrameSize = setting.Value
			} else {
				s.conn.frameWriter.maxFrameSize = setting.Value
			}
		default:
			// An endpoint that receives a SETTINGS frame with any unknown or
			// unsupported identifier MUST ignore that setting.
			continue
		}
		cur.SetValue(setting.ID, setting.Value)
	}
	s.settings.Store(cur)
	return
}

// WriteFrame writes a frame to the connection.
func (c *Conn) WriteFrame(frame Frame) error {
	if c.Closed() {
		return ErrClosed
	}

	if err := c.Handshake(); err != nil {
		return err
	}

	if frame == nil {
		return errors.New("frame must be non-nil")
	}

	return c.writeFrame(frame)
}

func (c *Conn) writeFrame(frame Frame) (err error) {
	switch frame.Type() {
	case FrameData:
		stream := c.stream(frame.Stream())
		if stream == nil {
			return fmt.Errorf("stream %d does not exist", frame.Stream())
		}
		if _, err = stream.transition(false, FrameData, false); err == nil {
			return stream.write(frame)
		}
	case FrameHeaders:
		stream := c.stream(frame.Stream())
		if stream == nil {
			defer func() {
				if atomic.CompareAndSwapInt32(&c.idState, 1, 0) {
					c.idCh <- struct{}{}
				}
			}()

			if stream, err = c.idleStream(frame.Stream()); err != nil {
				break
			}
		}
		if _, err = stream.transition(false, FrameHeaders, false); err == nil {
			return stream.write(frame)
		}
	case FramePriority:
		//
	case FrameRSTStream:
		stream := c.stream(frame.Stream())
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

		// If the sender of a SETTINGS frame does not receive an acknowledgement
		// within a reasonable amount of time, it MAY issue a connection error
		// (Section 5.4.1) of type SETTINGS_TIMEOUT.

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
		stream := c.stream(frame.Stream())
		if stream == nil {
			err = ConnError{fmt.Errorf("stream %d does not exist", frame.Stream()), ErrCodeProtocol}
			break
		}
		if !stream.writable() {
			err = ConnError{fmt.Errorf("stream %d is not active", frame.Stream()), ErrCodeProtocol}
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
		if frame.Stream() == 0 {
			err = c.connStream.recvFlow.incrementWindow(int(frame.(*WindowUpdateFrame).WindowSizeIncrement))
		} else if stream := c.stream(frame.Stream()); stream != nil {
			err = stream.recvFlow.incrementWindow(int(frame.(*WindowUpdateFrame).WindowSizeIncrement))
		}
	default:
		c.writeQueue.add(frame, frame == nil)
	}

	if err != nil {
		c.handleErr(err)
	}

	return
}

func (c *Conn) Flush() error {
	c.writeQueue.add(nil, false)
	return nil
}

func (c *Conn) Closed() bool {
	return atomic.LoadInt32(&c.closed) == 1
}

func (c *Conn) Close() error {
	const defaultCloseTimeout = 3 * time.Second

	return c.CloseTimeout(defaultCloseTimeout)
}

var ErrClosed = errors.New("http2: connection has been closed")

func (c *Conn) CloseTimeout(timeout time.Duration) error {
	if atomic.CompareAndSwapInt32(&c.closing, 0, 1) {
		c.handshakeL.Lock()
		if !c.handshakeComplete {
			c.handshakeL.Unlock()
			return c.close()
		}
		c.handshakeL.Unlock()

		// Endpoints SHOULD send a GOAWAY frame when ending a connection,
		// providing that circumstances permit it.
		c.writeFrame(&GoAwayFrame{LastStreamID: c.LastStreamID(), ErrCode: ErrCodeNo})

		c.Flush()

		if timeout <= 0 {
			return c.close()
		}

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
	if c.idTimer != nil {
		c.idTimer.Stop()
	}
	return c.rwc.Close()
}

// LocalAddr returns the local network address.
func (c *Conn) LocalAddr() net.Addr {
	if nc, ok := c.rwc.(net.Conn); ok {
		return nc.LocalAddr()
	}
	return nil
}

// RemoteAddr returns the remote network address.
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
					if err = c.remote.applySettings(v.Settings); err != nil {
						c.handleErr(err)
						continue loop
					}
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

// ReadFrame reads a frame from the connection.
func (c *Conn) ReadFrame() (Frame, error) {
	if err := c.Handshake(); err != nil {
		return nil, err
	}

	c.rio.Lock()
	defer c.rio.Unlock()

	if len(c.upgradeFrames) > 0 {
		frame := c.upgradeFrames[0]
		c.upgradeFrames = c.upgradeFrames[1:]
		return frame, nil
	}

	return c.readFrame()
}

func (c *Conn) readFrame() (frame Frame, err error) {
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
		if c.remote.validStreamID(frame.Stream()) && frame.Stream() > goAway.LastStreamID {
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
		c.data.stream = stream
		c.data.src = v.Data
		c.data.endStream = v.EndStream
		c.data.processed = 0
		c.data.sawEOF = false
		c.data.err = nil
		c.lastData = &c.data
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
				if err = c.applySettings(settings); err != nil {
					goto exit
				}
			default:
			}
		} else {
			if enablePush, exists := v.Settings.value(SettingEnablePush); exists {
				if enablePush == 1 && !c.server {
					err = ConnError{
						errors.New("client received SETTINGS frame with ENABLE_PUSH specified for remote server"),
						ErrCodeProtocol,
					}
					goto exit
				}
			}
			c.writeQueue.add(&SettingsFrame{true, v.Settings}, true)
		}
	case *PushPromiseFrame:
		stream := c.stream(frame.Stream())
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

var errBadConnPreface = errors.New("http2: bad connection preface")

// HandshakeError represents connection handshake error.
type HandshakeError string

func (e HandshakeError) Error() string {
	return fmt.Sprintf("http2: %s", string(e))
}

// Handshake runs the client or server handshake
// protocol if it has not yet been run.
// Most uses of this package need not call Handshake
// explicitly: the first Read or Write will call it automatically.
func (c *Conn) Handshake() error {
	c.handshakeL.Lock()
	defer c.handshakeL.Unlock()

	if err := c.handshakeErr; err != nil {
		return err
	}

	if c.handshakeComplete {
		return nil
	}

	if timeout := c.config.HandshakeTimeout; timeout > 0 {
		done := make(chan struct{})
		go func() {
			if c.server {
				c.handshakeErr = c.serverHandshake()
			} else {
				c.handshakeErr = c.clientHandshake()
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(timeout):
			c.handshakeErr = HandshakeError("handshake timed out")
		}
	} else {
		if c.server {
			c.handshakeErr = c.serverHandshake()
		} else {
			c.handshakeErr = c.clientHandshake()
		}
	}

	if err := c.handshakeErr; err != nil {
		switch err.(type) {
		case HandshakeError:
			c.close()
			return err
		}

		// Clients and servers MUST treat an invalid connection preface as a
		// connection error (Section 5.4.1) of type PROTOCOL_ERROR.
		if err == errBadConnPreface {
			err = ConnError{err, ErrCodeProtocol}
		}
		c.handleErr(err)
	} else {
		c.handshakeComplete = true
	}

	return c.handshakeErr
}

func (c *Conn) handleErr(err error) {
	if err == nil || err == ErrClosed {
		return
	}

	switch e := err.(type) {
	case StreamError:
		c.writeFrame(&RSTStreamFrame{e.StreamID, e.ErrCode})
	case StreamErrorList:
		for _, se := range e {
			c.writeFrame(&RSTStreamFrame{se.StreamID, se.ErrCode})
		}
	case ConnError:
		c.writeFrame(&GoAwayFrame{c.LastStreamID(), e.ErrCode, []byte(e.Error())})
	default:
		c.writeFrame(&GoAwayFrame{c.LastStreamID(), ErrCodeInternal, []byte(e.Error())})
	}
}
