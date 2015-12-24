package http2

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

type Conn struct {
	rwc io.ReadWriteCloser
	buf *bufio.ReadWriter

	rio         sync.Mutex
	frameReader *frameReader

	wio         sync.Mutex
	frameWriter *frameWriter

	settingsCh chan Settings

	flowCond sync.Cond
	initialRecvWindow,
	initialSendWindow uint32

	lastData *data

	mu         sync.RWMutex
	connStream *stream
	streams    map[uint32]*stream

	*connState
	remote *connState

	goAwaySent,
	goAwayReceived atomic.Value
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
	conn.buf = bufio.NewReadWriter(bufio.NewReaderSize(rwc, readBufSize),
		bufio.NewWriterSize(rwc, writeBufSize))
	conn.frameReader = newFrameReader(conn.buf.Reader, readBufSize)
	conn.frameWriter = newFrameWriter(conn.buf.Writer)
	conn.settingsCh = make(chan Settings, 2)
	conn.flowCond.L = new(sync.Mutex)
	conn.initialRecvWindow = defaultInitialWindowSize
	conn.initialSendWindow = defaultInitialWindowSize
	conn.connStream = &stream{conn: conn, id: 0, weight: defaultWeight}
	conn.setRecvWindow(conn.connStream)
	conn.setSendWindow(conn.connStream)
	conn.streams = map[uint32]*stream{}
	conn.connState = &connState{conn: conn, server: server, maxStreams: defaultMaxConcurrentStreams}
	conn.remote = &connState{conn: conn, server: !server, maxStreams: defaultMaxConcurrentStreams}
	if server {
		conn.nextStreamID = 2
		conn.remote.nextStreamID = 1
		conn.remote.pushEnabled = defaultEnablePush
		go conn.flushQueuedFramesLoop()
	} else {
		conn.nextStreamID = 1
		conn.remote.nextStreamID = 2
		conn.pushEnabled = defaultEnablePush
	}

	return conn
}

func (c *Conn) ServerConn() bool {
	return c.server
}

func (c *Conn) NumActiveStreams() uint32 {
	return atomic.LoadUint32(&c.numStreams) + atomic.LoadUint32(&c.remote.numStreams)
}

func (c *Conn) NextStreamID() uint32 {
	return c.nextStreamID
}

func (c *Conn) LastStreamID() uint32 {
	return c.remote.lastStreamID
}

func (c *Conn) Settings() Settings {
	var settings Settings
	settings.SetPushEnabled(c.pushEnabled)
	settings.SetMaxConcurrentStreams(c.remote.maxStreams)
	settings.SetInitialWindowSize(c.initialRecvWindow)
	settings.SetHeaderTableSize(c.frameReader.MaxHeaderTableSize())
	settings.SetMaxHeaderListSize(c.frameReader.maxHeaderListSize)
	settings.SetMaxFrameSize(c.frameReader.maxFrameSize)
	return settings
}

func (c *Conn) Close() error {
	return nil
}

func (c *Conn) stream(streamID uint32) *stream {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.streams[streamID]
}

func (c *Conn) verifyStreamID(streamID uint32) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if (c.remote.validStreamID(streamID) && streamID <= c.remote.lastStreamID) ||
		(c.validStreamID(streamID) && streamID <= c.lastStreamID) {
		return ignoreFrame
	}
	return ConnError{fmt.Errorf("stream %d does not exist", streamID), ErrCodeProtocol}
}

func (c *Conn) handleStreams(fn func(*stream) error) (stream *stream, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, stream = range c.streams {
		if err = fn(stream); err != nil {
			break
		}
	}
	return
}

func (c *Conn) removeStream(stream *stream) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// TODO: verify stream dependency

	delete(c.streams, stream.id)
}

type connState struct {
	conn         *Conn
	server       bool
	pushEnabled  bool
	maxStreams   uint32
	numStreams   uint32
	nextStreamID uint32
	lastStreamID uint32
}

var ErrConnClosing = errors.New("http2: use of closed network connection")

func (state *connState) newStream(streamID uint32) (*stream, error) {
	// Receivers of a GOAWAY frame MUST NOT open
	// additional streams on the connection, although a new connection can
	// be established for new streams.
	if _, ok := state.conn.goAwayReceived.Load().(*GoAwayFrame); ok {
		return nil, ErrConnClosing
	}

	state.conn.mu.Lock()
	defer state.conn.mu.Unlock()

	if streamID == 0 {
		streamID = state.nextStreamID
	}

	if !validStreamID(streamID) || streamID < state.nextStreamID {
		return nil, ConnError{fmt.Errorf("bad stream id %d", streamID), ErrCodeProtocol}
	}

	if atomic.LoadUint32(&state.numStreams)+1 > state.maxStreams {
		return nil, ConnError{errors.New("maximum streams exceeded"), ErrCodeRefusedStream}
	}

	stream := &stream{conn: state.conn, id: streamID, state: StateIdle, weight: defaultWeight}
	state.conn.streams[stream.id] = stream
	state.nextStreamID = streamID + 2
	state.lastStreamID = streamID

	return stream, nil
}

func (state *connState) validStreamID(streamID uint32) bool {
	return state.server == ((streamID&1) == 0) && streamID > 0
}
