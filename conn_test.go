package http2

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func test(t *testing.T) {
	lis, err := net.Listen("tcp", ":8989")
	if err != nil {
		t.Fatal(err)
	}
	for {
		s, err := lis.Accept()
		if err != nil {
			t.Fatal(err)
		}
		ss := ServerConn(s, nil)
		if err = ss.Handshake(); err != nil {
			t.Fatal(err)
		}
		for !ss.Closed() {
			var frame Frame
			if frame, err = ss.ReadFrame(); err != nil {
				t.Fatal(err)
			} else {
				es := false
				switch v := frame.(type) {
				case *DataFrame:
					es = v.EndStream
				case *HeadersFrame:
					es = v.EndStream
				}
				if es {
					headers := &HeadersFrame{
						frame.streamID(),
						make(Header),
						Priority{},
						0, true,
					}
					headers.SetStatus("200")
					headers.Set("content-length", "0")
					if err = ss.WriteFrame(headers); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
}

func BenchmarkConnReadWriteTCP_1K_C1(b *testing.B) {
	benchmarkConnReadWrite(b, false, 1024, 1)
}

func BenchmarkConnReadWriteTCP_1K_C8(b *testing.B) {
	benchmarkConnReadWrite(b, false, 1024, 8)
}

func BenchmarkConnReadWriteTCP_1K_C64(b *testing.B) {
	benchmarkConnReadWrite(b, false, 1024, 64)
}

func BenchmarkConnReadWriteTCP_1K_C512(b *testing.B) {
	benchmarkConnReadWrite(b, false, 1024, 512)
}

func BenchmarkConnReadWriteTCP_1M_C1(b *testing.B) {
	benchmarkConnReadWrite(b, false, 1024*1024, 1)
}

func BenchmarkConnReadWriteTCP_1M_C8(b *testing.B) {
	benchmarkConnReadWrite(b, false, 1024*1024, 8)
}

func BenchmarkConnReadWriteTCP_1M_C64(b *testing.B) {
	benchmarkConnReadWrite(b, false, 1024*1024, 64)
}

func BenchmarkConnReadWriteTCP_1M_C512(b *testing.B) {
	benchmarkConnReadWrite(b, false, 1024*1024, 512)
}

func BenchmarkConnReadWriteTLS_1K_C1(b *testing.B) {
	benchmarkConnReadWrite(b, true, 1024, 1)
}

func BenchmarkConnReadWriteTLS_1K_C8(b *testing.B) {
	benchmarkConnReadWrite(b, true, 1024, 8)
}

func BenchmarkConnReadWriteTLS_1K_C64(b *testing.B) {
	benchmarkConnReadWrite(b, true, 1024, 64)
}

func BenchmarkConnReadWriteTLS_1K_C512(b *testing.B) {
	benchmarkConnReadWrite(b, true, 1024, 512)
}

func BenchmarkConnReadWriteTLS_1M_C1(b *testing.B) {
	benchmarkConnReadWrite(b, true, 1024*1024, 1)
}

func BenchmarkConnReadWriteTLS_1M_C8(b *testing.B) {
	benchmarkConnReadWrite(b, true, 1024*1024, 8)
}

func BenchmarkConnReadWriteTLS_1M_C64(b *testing.B) {
	benchmarkConnReadWrite(b, true, 1024*1024, 64)
}

func BenchmarkConnReadWriteTLS_1M_C512(b *testing.B) {
	benchmarkConnReadWrite(b, true, 1024*1024, 512)
}

func benchmarkConnReadWrite(b *testing.B, overTLS bool, n, c int) {
	sc, cc := pipe(overTLS)
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
	if err := client.Close(); err != nil {
		b.Log(err)
	}
	if err := server.Close(); err != nil {
		b.Log(err)
	}
	time.Sleep(100 * time.Millisecond)
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
		if endStream && c.ServerConn() {
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

func pipe(overTLS bool) (server *Conn, client *Conn) {
	done := make(chan struct{})
	addr := &net.TCPAddr{Port: 8989}
	sc := &Config{}
	cc := &Config{}
	for {
		lis, err := net.Listen("tcp", addr.String())
		if err != nil {
			if addr.Port > 65535 {
				panic(err)
			}
			addr.Port++
			continue
		}
		go func() {
			s, err := lis.Accept()
			if err != nil {
				panic(err)
			}
			s.(*net.TCPConn).SetNoDelay(true)
			if overTLS {
				cert, err := tls.LoadX509KeyPair("testdata/server.pem", "testdata/server.key")
				if err != nil {
					panic(err)
				}
				sc.TLSConfig = &tls.Config{
					Certificates:             []tls.Certificate{cert},
					Rand:                     rand.Reader,
					NextProtos:               []string{VersionTLS},
					PreferServerCipherSuites: true,
				}
				s = tls.Server(s, sc.TLSConfig)
			}
			server = newConn(s, true, sc)
			if err = server.Handshake(); err != nil {
				panic(err)
			}
			lis.Close()
			close(done)
		}()
		break
	}
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		panic(err)
	}
	c.(*net.TCPConn).SetNoDelay(true)
	if overTLS {
		cc.TLSConfig = &tls.Config{
			Rand:               rand.Reader,
			NextProtos:         []string{VersionTLS},
			InsecureSkipVerify: true,
		}
		c = tls.Client(c, cc.TLSConfig)
	}
	client = newConn(c, false, cc)
	if err = client.Handshake(); err != nil {
		panic(err)
	}
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		panic("pipe: timed out")
	}
	return
}
