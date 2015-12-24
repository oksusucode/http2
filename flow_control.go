package http2

import (
	"errors"
)

func (c *Conn) setInitialRecvWindow(n uint32) error {
	delta := int(n) - int(c.initialRecvWindow)
	if delta == 0 {
		return nil
	}
	c.initialRecvWindow = n

	var errors StreamErrorList

	c.handleStreams(func(stream *stream) error {
		if stream.active() {
			if err := stream.updateRecvWindow(delta); err != nil {
				errors.add(stream.id, ErrCodeFlowControl, err)
			} else {
				stream.updateInitialRecvWindow(delta)
			}
		}
		return nil
	})

	return errors.Err()
}

func (c *Conn) setRecvWindow(stream *stream) {
	stream.recvWindow = int(c.initialRecvWindow)
	stream.recvWindowUpperBound = stream.recvWindow
	stream.processedBytes = stream.recvWindow
}

func (c *Conn) recvData(stream *stream, dataFrame *DataFrame) error {
	dataLen := dataFrame.DataLen + int(dataFrame.PadLen)
	if err := c.connStream.recvData(dataLen); err != nil {
		return err
	}
	if stream != nil && stream.active() {
		return stream.recvData(dataLen)
	} else if dataLen > 0 {
		return c.connStream.returnProcessedBytes(dataLen)
	}
	return nil
}

func (c *Conn) returnProcessedBytes(stream *stream, n int) error {
	if n <= 0 {
		return nil
	}
	if stream != nil && stream.active() {
		if err := c.connStream.returnProcessedBytes(n); err != nil {
			return err
		}
		return stream.returnProcessedBytes(n)
	}
	return nil
}

func (c *Conn) setInitialSendWindow(n uint32) error {
	c.flowCond.L.Lock()
	defer c.flowCond.L.Unlock()

	delta := int(n) - int(c.initialSendWindow)
	if delta == 0 {
		return nil
	}
	c.initialSendWindow = n

	var errors StreamErrorList

	c.handleStreams(func(stream *stream) error {
		if stream.writable() {
			if err := stream.updateSendWindow(delta); err != nil {
				errors.add(stream.id, ErrCodeFlowControl, err)
			}
		}
		return nil
	})

	if err := errors.Err(); err != nil {
		return err
	}

	if delta > 0 {
		c.flowCond.Broadcast()
	}

	return nil
}

func (c *Conn) setSendWindow(stream *stream) {
	c.flowCond.L.Lock()
	stream.sendWindow = int(c.initialSendWindow)
	c.flowCond.L.Unlock()
}

func (c *Conn) incrementWindow(stream *stream, delta int) error {
	c.flowCond.L.Lock()
	defer c.flowCond.L.Unlock()

	if err := stream.updateSendWindow(delta); err != nil {
		if stream.id == 0 {
			return ConnError{err, ErrCodeFlowControl}
		}
		return StreamError{err, ErrCodeFlowControl, stream.id}
	}

	c.flowCond.Broadcast()

	return nil
}

func (c *Conn) allocateBytes(stream *stream, n int) (int, error) {
	c.flowCond.L.Lock()
	defer c.flowCond.L.Unlock()

	for {
		// TODO: verify conn closing

		if !stream.writable() {
			return 0, errors.New("xxx")
		}

		allocated := stream.sendWindow
		if allocated > c.connStream.sendWindow {
			allocated = c.connStream.sendWindow
		}
		if allocated > 0 {
			if allocated > n {
				allocated = n
			}
			c.connStream.updateSendWindow(-allocated)
			stream.updateSendWindow(-allocated)

			return allocated, nil
		}

		c.flowCond.Wait()
	}
}

func (c *Conn) allocateBytesForTree() error {

	// TODO: allocate for priority tree

	return nil
}

func (c *Conn) enqueueFlowControlledFrame(stream *stream, frame Frame) error {
	if err := stream.enqueueFrame(frame); err != nil {
		return err
	}

	c.flowCond.L.Lock()
	c.flowCond.Signal()
	c.flowCond.L.Unlock()

	return nil
}

func (c *Conn) flushQueuedFramesLoop() {

	// TODO: server-side loop

}

func (c *Conn) cancelStream(stream *stream) {
	c.flowCond.L.Lock()
	defer c.flowCond.L.Unlock()

	if !stream.cancelled {

		// TODO: cleanup window

		stream.cancelled = true
		c.flowCond.Broadcast()
	}
}

func (s *stream) recvData(dataLen int) error {
	if dataLen < 0 {
		return errors.New("data length must be >= 0")
	}
	s.recvWindow -= dataLen
	if s.recvWindow < s.recvWindowLowerBound {
		if s.id == 0 {
			return ConnError{errors.New("window size exceeded"), ErrCodeFlowControl}
		}
		return StreamError{errors.New("window size exceeded"), ErrCodeFlowControl, s.id}
	}
	return nil
}

func (s *stream) returnProcessedBytes(delta int) error {
	if s.processedBytes-delta < s.recvWindow {
		if s.id == 0 {
			return ConnError{errors.New("attempting to return too many bytes"), ErrCodeInternal}
		}
		return StreamError{errors.New("attempting to return too many bytes"), ErrCodeInternal, s.id}
	}
	s.processedBytes -= delta

	if s.recvWindowUpperBound <= 0 {
		return nil
	}

	const windowUpdateRatio = 0.5
	threshold := int(float32(s.recvWindowUpperBound) * windowUpdateRatio)
	if s.processedBytes > threshold {
		return nil
	}

	delta = s.recvWindowUpperBound - s.processedBytes

	if err := s.updateRecvWindow(delta); err != nil {
		return ConnError{errors.New("attempting to return too many bytes"), ErrCodeInternal}
	}
	return s.conn.writeFrame(&WindowUpdateFrame{s.id, uint32(delta)}, true)
}

func (s *stream) updateInitialRecvWindow(delta int) {
	n := s.recvWindowUpperBound + delta
	if n < defaultInitialWindowSize {
		n = defaultInitialWindowSize
	}
	if n > maxInitialWindowSize {
		n = maxInitialWindowSize
	}
	delta = n - s.recvWindowUpperBound
	s.recvWindowUpperBound += delta
}

func (s *stream) updateRecvWindow(delta int) error {
	if delta > 0 && maxInitialWindowSize-delta < s.recvWindow {
		return errors.New("window size overflow")
	}
	s.recvWindow += delta
	s.processedBytes += delta
	s.recvWindowLowerBound = 0
	if delta < 0 {
		s.recvWindowLowerBound = delta
	}
	return nil
}

func (s *stream) enqueueFrame(frame Frame) error {
	s.Lock()
	defer s.Unlock()

	// TODO

	return nil
}

func (s *stream) updateSendWindow(delta int) error {
	if delta > 0 && maxInitialWindowSize-delta < s.sendWindow {
		return errors.New("window size overflow")
	}
	s.sendWindow += delta
	return nil
}
