package http2

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHandshake(t *testing.T) {
	for _, overTLS := range []bool{true, false} {
		client, server := pipe(true, overTLS, true)
		done := make(chan struct{})

		go func() {
			defer close(done)

			if err := client.Handshake(); err != nil {
				t.Errorf("error from client handshake: %s", err)
				return
			}

			frame, err := client.ReadFrame()
			if err != nil {
				t.Errorf("error from client read: %s", err)
				return
			}

			if settings, ok := frame.(*SettingsFrame); !ok || !settings.Ack {
				t.Error("client handshake expected ACK settings frame")
			}
		}()

		if err := server.Handshake(); err != nil {
			t.Fatalf("error from server handshake: %s", err)
		}

		frame, err := server.ReadFrame()
		if err != nil {
			t.Fatalf("error from server read: %s", err)
		}

		if !overTLS {
			if frame.Type() != FrameHeaders {
				t.Fatalf("server handshake expected headers frame, got %v", frame.Type())
			}
			if !frame.EndOfStream() {
				if frame, err = server.ReadFrame(); err != nil {
					t.Fatalf("error from server read: %s", err)
				}
				if frame.Type() != FrameData {
					t.Fatalf("server handshake expected data frame, got %v", frame.Type())
				}
			}
			if frame, err = server.ReadFrame(); err != nil {
				t.Fatalf("error from server read: %s", err)
			}
		}

		if settings, ok := frame.(*SettingsFrame); !ok || !settings.Ack {
			t.Fatal("server handshake expected ACK settings frame")
		}

		<-done
	}
}

func TestHeaders(t *testing.T) {
	for _, overTLS := range []bool{true, false} {
		client, server := pipe(true, overTLS, false)

		streamID, err := client.NextStreamID()
		if err != nil {
			t.Fatalf("error creating new stream", err)
		}
		if streamID != 3 {
			t.Fatalf("%d", streamID)
		}

		expected := new(HeadersFrame)
		expected.StreamID = streamID
		expected.Header = make(Header)
		expected.Header.Set("Test-A", "a")
		expected.Header.Set("test-B", "b")
		expected.EndStream = true

		if err = client.WriteFrame(expected); err != nil {
			t.Fatalf("error writing frame: %s", err)
		}

		frame, err := server.ReadFrame()
		if err != nil {
			t.Fatalf("error reading frame: %s", err)
		}
		got, ok := frame.(*HeadersFrame)
		if !ok {
			t.Fatalf("expected headers frame, got %s", frame.Type())
		}
		if !reflect.DeepEqual(expected, got) {
			t.Fatalf("expected %v, got %v", expected, got)
		}
		if client.NumActiveStreams() != 1 {
			t.Fatalf("expected number of streams: 1, got: %v", client.NumActiveStreams())
		}
		if server.NumActiveStreams() != 1 {
			t.Fatalf("expected number of streams: 1, got: %v", server.NumActiveStreams())
		}

		expected.EndStream = true
		if err = server.WriteFrame(expected); err != nil {
			t.Fatalf("error writing frame: %s", err)
		}
		frame, err = client.ReadFrame()
		if err != nil {
			t.Fatalf("error reading frame: %s", err)
		}
		got, ok = frame.(*HeadersFrame)
		if !ok {
			t.Fatalf("expected headers frame, got %s", frame.Type())
		}
		if !reflect.DeepEqual(expected, got) {
			t.Fatalf("expected %v, got %v", expected, got)
		}
		if client.NumActiveStreams() != 0 {
			t.Fatalf("expected number of streams: 0, got: %v", client.NumActiveStreams())
		}
		if server.NumActiveStreams() != 0 {
			t.Fatalf("expected number of streams: 0, got: %v", server.NumActiveStreams())
		}
	}
}

func TestData(t *testing.T) {
	for _, overTLS := range []bool{true, false} {
		client, server := pipe(false, overTLS, false)
		streamID, _ := client.NextStreamID()

		client.WriteFrame(&HeadersFrame{StreamID: streamID, EndStream: false})
		server.ReadFrame()

		expected := make([]byte, 1024*1024)
		for i := range expected {
			expected[i] = byte(i % 256)
		}

		done0 := make(chan struct{})
		done1 := make(chan struct{})

		go func() {
			defer close(done0)

			buf := bytes.NewBuffer(nil)

			for {
				cw := server.RecvWindow(0)
				sw := server.RecvWindow(streamID)

				frame, err := server.ReadFrame()
				if err != nil {
					t.Errorf("error from server read: %s", err)
					return
				}
				if streamID != frame.Stream() {
					t.Errorf("expected streamID: %d, got %d", streamID, frame.Stream())
					return
				}
				data, ok := frame.(*DataFrame)
				if !ok {
					t.Errorf("expected data frame, got %s", frame.Type())
					return
				}

				dataLen := uint32(data.DataLen) + uint32(data.PadLen)

				if server.RecvWindow(0) != cw-dataLen {
					t.Errorf("server stream 0 expected recv win: %d, got %d", cw-dataLen, server.RecvWindow(0))
					return
				}
				if server.RecvWindow(streamID) != sw-dataLen {
					t.Errorf("server stream %d expected recv win: %d, got %d", streamID, sw-dataLen, server.RecvWindow(streamID))
					return
				}

				if _, err = buf.ReadFrom(data.Data); err != nil {
					t.Errorf("error from server read: %s", err)
					return
				}

				for _, i := range []uint32{0, streamID} {
					recvFlow := server.connStream.recvFlow
					if i > 0 {
						recvFlow = server.streams[i].recvFlow
					}
					if recvFlow.processedWin <= int(float32(recvFlow.winUpperBound)*0.5) {
						if server.RecvWindow(i) != server.InitialRecvWindow(i) {
							t.Errorf("server stream %d expected recv win: %d, got %d",
								i,
								server.InitialRecvWindow(i),
								server.RecvWindow(i))
							return
						}
					}
				}

				if frame.EndOfStream() {
					if !reflect.DeepEqual(expected, buf.Bytes()) {
						t.Errorf("expected data: %s, got %s", string(expected), buf.String())
						return
					}
					err = server.WriteFrame(&HeadersFrame{StreamID: streamID, EndStream: true})
					if err != nil {
						t.Errorf("error writing frame: %s", err)
						return
					}
					if server.NumActiveStreams() != 0 {
						t.Errorf("expected number of streams: 0, got: %v", server.NumActiveStreams())
					}
					return
				}
			}
		}()

		go func() {
			defer close(done1)

			for {
				frame, err := client.ReadFrame()
				if err != nil {
					t.Errorf("error reading frame: %s", err)
					return
				}
				if frame.EndOfStream() {
					if client.NumActiveStreams() != 0 {
						t.Errorf("expected number of streams: 0, got: %v", client.NumActiveStreams())
					}
					return
				}
			}
		}()

		err := client.WriteFrame(&DataFrame{
			StreamID:  streamID,
			Data:      bytes.NewBuffer(expected),
			DataLen:   len(expected),
			PadLen:    128,
			EndStream: true})
		if err != nil {
			t.Fatalf("error writing frame: %s", err)
		}

		<-done0
		<-done1

		client.connStream.sendFlow.cancel()
		if client.SendWindow(0) != server.RecvWindow(0) {
			t.Fatalf("client stream expected send win: %d, got %d", server.RecvWindow(0), client.SendWindow(0))
		}
	}
}

func TestPing(t *testing.T) {
	for _, overTLS := range []bool{true, false} {
		client, server := pipe(true, overTLS, false)

		expected := [8]byte{}
		copy(expected[:], "pingpong")

		if err := client.WriteFrame(&PingFrame{Data: expected}); err != nil {
			t.Fatalf("error writing frame: %s", err)
		}

		frame, err := server.ReadFrame()
		if err != nil {
			t.Fatalf("error reading frame: %s", err)
		}
		got, ok := frame.(*PingFrame)
		if !ok {
			t.Fatalf("expected ping frame, got %s", frame.Type())
		}
		if got.Ack {
			t.Fatal("expected SYN ping frame")
		}
		if !bytes.Equal(expected[:], got.Data[:]) {
			t.Fatalf("expected %v, got %v", expected, got.Data[:])
		}

		frame, err = client.ReadFrame()
		if err != nil {
			t.Fatalf("error reading frame: %s", err)
		}
		got, ok = frame.(*PingFrame)
		if !ok {
			t.Fatalf("expected ACK ping frame, got %s", frame.Type())
		}
		if !got.Ack {
			t.Fatal("expected ACK ping frame")
		}
		if !bytes.Equal(expected[:], got.Data[:]) {
			t.Fatalf("expected %v, got %v", expected, got.Data[:])
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

func benchmarkConnReadWrite(b *testing.B, overTLS bool, n, c int) {
	cc, sc := pipe(false, overTLS, false)
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
		switch frame.Type() {
		case FrameData:
			c.rb.Reset()
			var n int64
			n, err = c.rb.ReadFrom(frame.(*DataFrame).Data)
			c.rx += n
			c.pending[frame.Stream()] += n
			if err != nil {
				return
			}
		}
		if frame.EndOfStream() && c.ServerConn() {
			go c.writeBytes(frame.Stream(), int(c.pending[frame.Stream()]))
		}
	}
}

func (c *conn) writeBytes(streamID uint32, n int) (err error) {
	err = c.WriteFrame(&HeadersFrame{StreamID: streamID, EndStream: n == 0})
	if n > 0 && err == nil {
		if err = c.WriteFrame(&DataFrame{streamID, bytes.NewBuffer(make([]byte, n)), n, 0, true}); err == nil {
			atomic.AddInt64(&c.tx, int64(n))
		}
	}
	return
}

func pipe(fake, overTLS, skipHandshake bool) (client, server *Conn) {
	var c, s net.Conn

	if fake {
		c, s = net.Pipe()
	} else {
		var lis net.Listener
		var err error

		addr := &net.TCPAddr{Port: 8989}

		for {
			lis, err = net.Listen("tcp", addr.String())
			if err == nil {
				break
			}
			if addr.Port > 65535 {
				panic(err)
			}
			addr.Port++
		}

		done := make(chan struct{})

		go func() {
			if s, err = lis.Accept(); err != nil {
				panic(err)
			}
			s.(*net.TCPConn).SetNoDelay(true)
			lis.Close()
			close(done)
		}()

		c, err = (&net.Dialer{Timeout: 1 * time.Second}).Dial("tcp", addr.String())
		if err != nil {
			panic(err)
		}
		c.(*net.TCPConn).SetNoDelay(true)
		<-done
	}

	if overTLS {
		cert, err := tls.LoadX509KeyPair("testdata/server.pem", "testdata/server.key")
		if err != nil {
			panic(err)
		}
		s = tls.Server(s, &tls.Config{
			Certificates:             []tls.Certificate{cert},
			Rand:                     rand.Reader,
			NextProtos:               []string{ProtocolTLS},
			PreferServerCipherSuites: true,
		})
		c = tls.Client(c, &tls.Config{
			Rand:               rand.Reader,
			NextProtos:         []string{ProtocolTLS},
			InsecureSkipVerify: true,
		})
	}

	client = ClientConn(c, nil, nil)
	server = ServerConn(s, nil)

	if !skipHandshake {
		done := make(chan struct{})

		go func() {
			client.Handshake()
			client.ReadFrame()
			if !overTLS {
				client.ReadFrame()
			}
			close(done)
		}()

		server.Handshake()
		server.ReadFrame()

		if !overTLS {
			server.ReadFrame()
			server.WriteFrame(&HeadersFrame{StreamID: 1, EndStream: true})
		}

		select {
		case <-done:
		case <-time.After(1 * time.Second):
			panic("handshake failed: timeout")
		}
	}

	return
}
