package http2

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func Test(t *testing.T) {
}

func BenchmarkConnReadWriteC1_1K(b *testing.B) {
	benchmark(b, 1, 1024)
}

func BenchmarkConnReadWriteC8_1K(b *testing.B) {
	benchmark(b, 8, 1024)
}

func BenchmarkConnReadWriteC64_1K(b *testing.B) {
	benchmark(b, 64, 1024)
}

func BenchmarkConnReadWriteC512_1K(b *testing.B) {
	benchmark(b, 512, 1024)
}

func benchmark(b *testing.B, c, n int) {
	sc, cc := pipe(true)
	server, client := &conn{Conn: sc, pending: map[uint32]int64{}}, &conn{Conn: cc, pending: map[uint32]int64{}}
	go server.serve()
	go client.serve()
	ch := make(chan int, c*4)
	var wg sync.WaitGroup
	for i := 0; i < c; i++ {
		wg.Add(1)
		go func() {
			for range ch {
				streamID, err := client.NextStreamID()
				if err != nil {
					b.Fatal(err)
				}
				err = client.writeBytes(streamID, n)
				if err != nil {
					b.Fatal(err)
				}
			}
			wg.Done()
		}()
	}
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		ch <- i
	}
	b.StopTimer()
	close(ch)
	wg.Wait()
	time.Sleep(100 * time.Millisecond)
	if err := client.Close(); err != nil {
		b.Fatal(err)
	}
	if err := server.Close(); err != nil {
		b.Fatal(err)
	}
	if atomic.LoadInt64(&client.tx) != server.rx {
		b.Fatal("lost data")
	}
	if atomic.LoadInt64(&server.tx) != client.rx {
		b.Fatal("lost data")
	}
	if int64(b.N*n) != client.rx {
		b.Fatal("lost data")
	}
}

type conn struct {
	*Conn
	rx, tx  int64
	rb      bytes.Buffer
	pending map[uint32]int64
}

func (c *conn) serve() {
	for !c.Closed() {
		frame, err := c.ReadFrame()
		if err != nil {
			return
		}
		var endStream bool
		switch v := frame.(type) {
		case *DataFrame:
			c.rb.Reset()
			var n int64
			n, err = c.rb.ReadFrom(v.Data)
			c.rx += n
			c.pending[v.StreamID] += n
			if err != nil {
				return
			}
			endStream = v.EndStream
		case *HeadersFrame:
			endStream = v.EndStream
		}
		if endStream && c.server {
			go c.writeBytes(frame.streamID(), int(c.pending[frame.streamID()]))
		}
	}
}

func (c *conn) writeBytes(streamID uint32, n int) (err error) {
	if streamID == 0 {
		if streamID, err = c.NextStreamID(); err != nil {
			return
		}
	}
	err = c.WriteFrame(&HeadersFrame{streamID, nil, Priority{}, 0, n == 0})
	if n > 0 && err == nil {
		if err = c.WriteFrame(&DataFrame{streamID, bytes.NewBuffer(make([]byte, n)), n, 0, true}); err == nil {
			atomic.AddInt64(&c.tx, int64(n))
		}
	}
	return
}

func pipe(tcp bool) (server *Conn, client *Conn) {
	if tcp {
		done := make(chan struct{})
		addr := &net.TCPAddr{Port: 8989}
		for {
			lis, err := net.ListenTCP("tcp", addr)
			if err != nil {
				if addr.Port > 65535 {
					panic(err)
				}
				addr.Port++
				continue
			}
			lis.SetDeadline(time.Now().Add(300 * time.Millisecond))
			go func() {
				s, err := lis.AcceptTCP()
				if err != nil {
					panic(err)
				}
				s.SetNoDelay(true)
				server = NewConn(s, true)
				lis.Close()
				close(done)
			}()
			break
		}
		c, err := net.DialTCP("tcp", nil, addr)
		if err != nil {
			panic(err)
		}
		c.SetNoDelay(true)
		client = NewConn(c, false)
		<-done
	} else {
		type rwc struct {
			io.Reader
			io.Writer
			io.Closer
		}
		sr, cw := io.Pipe()
		cr, sw := io.Pipe()
		server = NewConn(&rwc{sr, sw, sw}, true)
		client = NewConn(&rwc{cr, cw, cw}, false)
	}
	return
}
