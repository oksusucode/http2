package http2

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

func (c *Conn) InitialRecvWindow(streamID uint32) uint32 {
	if streamID == 0 {
		return c.connStream.recvFlow.initialWindow()
	}
	if stream := c.stream(streamID); stream != nil {
		return stream.recvFlow.initialWindow()
	}
	return 0
}

func (c *Conn) RecvWindow(streamID uint32) uint32 {
	var stream *stream
	if streamID == 0 {
		stream = c.connStream
	} else {
		stream = c.stream(streamID)
	}
	if stream != nil {
		if w := stream.recvFlow.window(); w > 0 {
			return uint32(w)
		}
	}
	return 0
}

func (c *Conn) setInitialRecvWindow(n uint32) error {
	o := atomic.LoadUint32(&c.initialRecvWindow)
	delta := int(n) - int(o)
	if delta == 0 {
		return nil
	}
	atomic.StoreUint32(&c.initialRecvWindow, n)

	var errors StreamErrorList

	c.streamL.RLock()
	defer c.streamL.RUnlock()

	for _, stream := range c.streams {
		if stream.active() {
			if err := stream.recvFlow.incrementInitialWindow(delta); err != nil {
				errors.add(stream.id, ErrCodeFlowControl, err)
			}
		}
	}

	return errors.Err()
}

type flowController struct {
	sync.RWMutex
	s *stream
	win,
	winLowerBound,
	winUpperBound,
	processedWin int
}

func (c *flowController) initialWindow() uint32 {
	c.RLock()
	win := c.winUpperBound
	c.RUnlock()

	return uint32(win)
}

func (c *flowController) incrementInitialWindow(delta int) error {
	if c.s.id == 0 {
		return nil
	}

	c.Lock()
	defer c.Unlock()

	err := c.updateWindow(delta)
	if err == nil {
		c.updateInitialWindow(delta)
	}
	return err
}

func (c *flowController) window() int {
	c.RLock()
	win := c.win
	c.RUnlock()

	return win
}

func (c *flowController) incrementWindow(delta int) error {
	c.Lock()
	defer c.Unlock()

	c.updateInitialWindow(delta)

	return c.windowUpdate()
}

func (c *flowController) consumedBytes() int {
	c.Lock()
	defer c.Unlock()

	return c.processedWin - c.win
}

func (c *flowController) consumeBytes(n int) error {
	if n <= 0 {
		return nil
	}

	c.Lock()
	defer c.Unlock()

	if c.s.id != 0 {
		if err := c.s.conn.connStream.recvFlow.consumeBytes(n); err != nil {
			return err
		}
	}
	c.win -= n
	if c.win < c.winLowerBound {
		if c.s.id == 0 {
			return ConnError{errors.New("window size limit exceeded"), ErrCodeFlowControl}
		}
		return StreamError{errors.New("window size limit exceeded"), ErrCodeFlowControl, c.s.id}
	}
	return nil
}

func (c *flowController) returnBytes(delta int) error {
	c.Lock()
	defer c.Unlock()

	if c.s.id != 0 {
		if err := c.s.conn.connStream.recvFlow.returnBytes(delta); err != nil {
			return err
		}
	}
	if c.processedWin-delta < c.win {
		if c.s.id == 0 {
			return ConnError{errors.New("attempting to return too many bytes"), ErrCodeInternal}
		}
		return StreamError{errors.New("attempting to return too many bytes"), ErrCodeInternal, c.s.id}
	}
	c.processedWin -= delta
	return c.windowUpdate()
}

func (c *flowController) updateWindow(delta int) error {
	if delta > 0 && maxInitialWindowSize-delta < c.win {
		return errors.New("window size overflow")
	}
	c.win += delta
	c.processedWin += delta
	c.winLowerBound = 0
	if delta < 0 {
		c.winLowerBound = delta
	}
	return nil
}

func (c *flowController) updateInitialWindow(delta int) {
	n := c.winUpperBound + delta
	if n < defaultInitialWindowSize {
		n = defaultInitialWindowSize
	}
	if n > maxInitialWindowSize {
		n = maxInitialWindowSize
	}
	delta = n - c.winUpperBound
	c.winUpperBound += delta
}

func (c *flowController) windowUpdate() error {
	if c.winUpperBound <= 0 {
		return nil
	}

	const windowUpdateRatio = 0.5

	threshold := int(float32(c.winUpperBound) * windowUpdateRatio)
	if c.processedWin > threshold {
		return nil
	}

	delta := c.winUpperBound - c.processedWin
	if err := c.updateWindow(delta); err != nil {
		return ConnError{errors.New("attempting to return too many bytes"), ErrCodeInternal}
	}

	c.s.conn.writeQueue.add(&WindowUpdateFrame{c.s.id, uint32(delta)}, true)

	return nil
}

func (c *Conn) InitialSendWindow(uint32) uint32 {
	return atomic.LoadUint32(&c.initialSendWindow)
}

func (c *Conn) SendWindow(streamID uint32) uint32 {
	var stream *stream
	if streamID == 0 {
		stream = c.connStream
	} else {
		stream = c.stream(streamID)
	}
	if stream != nil {
		if w := stream.sendFlow.window(); w > 0 {
			return uint32(w)
		}
	}
	return 0
}

func (c *Conn) setInitialSendWindow(n uint32) error {
	o := atomic.LoadUint32(&c.initialSendWindow)
	delta := int(n) - int(o)
	if delta == 0 {
		return nil
	}
	atomic.StoreUint32(&c.initialSendWindow, n)

	var errors StreamErrorList

	c.streamL.RLock()
	defer c.streamL.RUnlock()

	for _, stream := range c.streams {
		if stream.writable() {
			if err := stream.sendFlow.incrementInitialWindow(delta); err != nil {
				if stream.id == 0 {
					c.streamL.RUnlock()
					return err
				}
				se := err.(StreamError)
				errors = append(errors, &se)
			}
		}
	}

	return errors.Err()
}

type remoteFlowController struct {
	sync.RWMutex
	s     *stream
	win   int
	winCh chan int
}

func (c *remoteFlowController) initialWindow() uint32 {
	return atomic.LoadUint32(&c.s.conn.initialSendWindow)
}

func (c *remoteFlowController) incrementInitialWindow(delta int) error {
	return c.updateWindow(delta, true)
}

func (c *remoteFlowController) window() int {
	c.RLock()
	win := c.win
	c.RUnlock()

	return win
}

func (c *remoteFlowController) incrementWindow(delta int) error {
	return c.updateWindow(delta, false)
}

func (c *remoteFlowController) updateWindow(delta int, reset bool) error {
	c.Lock()
	defer c.Unlock()

	if delta > 0 && maxInitialWindowSize-delta < c.win {
		if c.s.id == 0 {
			return ConnError{errors.New("window size overflow"), ErrCodeFlowControl}
		}
		return StreamError{errors.New("window size overflow"), ErrCodeFlowControl, c.s.id}
	}

	if reset {
		select {
		case n := <-c.winCh:
			c.win += n
		default:
		}
	}

	c.win += delta

	if c.win <= 0 {
		return nil
	}

	select {
	case c.winCh <- c.win:
		c.win = 0
	default:
	}

	return nil
}

func (c *remoteFlowController) windowCh() <-chan int {
	return c.winCh
}

func (c *remoteFlowController) cancel() {
	c.Lock()
	defer c.Unlock()

	select {
	case n := <-c.winCh:
		c.win += n
	default:
	}
}

type flowControlWriter interface {
	Write(*stream, Frame) error
	Flush() error
	Close() error
	Cancel(*stream) error
}

type streamWriter struct {
	// pending,
	// written int64
}

func (w *streamWriter) Write(stream *stream, frame Frame) error {
	select {
	case <-stream.conn.closeCh:
		return ErrClosed
	case <-stream.closeCh:
		return errors.New("stream closed")
	case <-stream.wio:
		defer func() { stream.wio <- struct{}{} }()

		if frame.Type() == FrameHeaders {
			headers := frame.(*HeadersFrame)
			stream.conn.writeQueue.add(&flowControlled{stream, headers, headers.EndStream}, false)
			select {
			case err := <-stream.werr:
				return err
			case <-stream.conn.closeCh:
				return ErrClosed
			case <-stream.closeCh:
				return errors.New("stream closed x")
			}
		}

		data, ok := frame.(*DataFrame)
		if !ok {
			return fmt.Errorf("bad flow control frame type %s", frame.Type())
		}
		dataLen := data.DataLen
		padLen := int(data.PadLen)
		allowed, err := w.allocateBytes(stream, dataLen+padLen)
		if err != nil {
			return err
		}
		if allowed == dataLen+padLen {
			stream.conn.writeQueue.add(&flowControlled{stream, data, data.EndStream}, false)
			select {
			case err = <-stream.werr:
				return err
			case <-stream.conn.closeCh:
				return ErrClosed
			case <-stream.closeCh:
				return errors.New("stream closed x")
			}
		}

		cdata := &flowControlled{s: stream}
		chunk := new(DataFrame)

		*chunk = *data

		var lastFrame bool

	again:
		chunk.DataLen = dataLen
		if chunk.DataLen > allowed {
			chunk.DataLen = allowed
		}
		padding := allowed - chunk.DataLen
		if padding > padLen {
			padding = padLen
		}

		dataLen -= chunk.DataLen
		padLen -= padding
		lastFrame = dataLen+padLen == 0

		chunk.PadLen = uint8(padding)
		chunk.EndStream = data.EndStream && lastFrame

		cdata.frameWriterTo = chunk
		cdata.endStream = chunk.EndStream
		stream.conn.writeQueue.add(cdata, false)
		select {
		case err = <-stream.werr:
			if lastFrame || err != nil {
				return err
			}
			allowed, err = w.allocateBytes(stream, dataLen+padLen)
			if err != nil {
				return err
			}
			goto again
			// case <-stream.conn.closeCh:
			// 	return ErrClosed
			// case <-stream.closeCh:
			// 	return errors.New("stream closed x")
		}
	}
}

type flowControlled struct {
	s *stream
	frameWriterTo
	endStream bool
}

func (f *flowControlled) writeTo(w *frameWriter) error {
	err := f.frameWriterTo.writeTo(w)
	select {
	case f.s.werr <- err:
	default:
	}
	if f.endStream && err == nil {
		_, err = f.s.transition(false, f.Type(), true)
	}
	return err
}

func (streamWriter) Flush() error {
	return nil
}

func (streamWriter) Close() error {
	return nil
}

func (w *streamWriter) Cancel(stream *stream) error {
	select {
	case stream.werr <- errors.New("stream canceled"):
	default:
	}
	return nil
}

func (w *streamWriter) allocateBytes(stream *stream, n int) (int, error) {
	if n <= 0 {
		return 0, nil
	}

	c, s := stream.conn.connStream.sendFlow, stream.sendFlow

	s.incrementWindow(0)

	var sw int
	select {
	case <-stream.closeCh:
		return 0, errors.New("stream closed xxx")
	case <-stream.conn.closeCh:
		return 0, ErrClosed
	case sw = <-s.windowCh():
	}

	c.incrementWindow(0)

	var cw int
	select {
	case <-stream.closeCh:
		c.cancel()
		return 0, errors.New("stream closed xxx")
	case <-stream.conn.closeCh:
		return 0, ErrClosed
	case cw = <-c.windowCh():
	}

	if sw < n {
		n = sw
	}
	if cw < n {
		n = cw
	}

	if n < sw {
		s.incrementWindow(sw - n)
	}
	if n < cw {
		c.incrementWindow(cw - n)
	}

	return n, nil
}

type flowControlQueue struct {
}

func (flowControlQueue) Write(*stream, Frame) error {
	return nil
}

func (flowControlQueue) Flush() error {
	return nil
}

func (flowControlQueue) Close() error {
	return nil
}

func (flowControlQueue) Cancel(*stream) error {
	return nil
}

var _ flowControlWriter = (*streamWriter)(nil)
var _ flowControlWriter = (*flowControlQueue)(nil)
