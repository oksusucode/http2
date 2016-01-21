
A Go implementation of the HTTP/2 protocol.

It is useful for client/server application development using the HTTP/2 connection/stream directly. (e.g. gRPC)

> currently under heavy development.

## Example

### Server

```go
    server := &http2.Server{
        Addr:       ":http",
        Handler:    sayHello,
    }

    server.ListenAndServe()

    ---

    func sayHello(conn *http2.Conn) {
        for !conn.Closed() {
            frame, _ := conn.ReadFrame()

            log.Printf("received %s frame\n", frame.Type())

            var (
                streamID  uint32
                endStream bool
            )

            switch v := frame.(type) {
            case *http2.DataFrame:
                // read flow-controlled data
                buf := new(bytes.Buffer)
                buf.ReadFrom(v.Data)

                streamID = v.StreamID
                endStream = v.EndStream
            case *http2.HeadersFrame:
                streamID = v.StreamID
                endStream = v.EndStream
            }

            if endStream {
                go func(streamID uint32) {

                    headers := new(http2.HeadersFrame)
                    headers.StreamID = streamID
                    headers.SetStatus("200")
                    conn.WriteFrame(headers)

                    data := new(http2.DataFrame)
                    data.StreamID = streamID
                    data.Data = bytes.NewBuffer([]byte("hello"))
                    data.DataLen = 5
                    data.PadLen = 128
                    data.EndStream = true
                    conn.WriteFrame(data)

                }(streamID)
            }
        }
    }
```

### Client

```go
    conn, _ := http2.Dial(":http")

    go readLoop(conn)

    newStreamID, _ := conn.NextStreamID()

    headers := new(http2.HeadersFrame)
    headers.StreamID = newStreamID
    headers.SetMethod("GET")
    headers.SetPath("/")
    headers.EndStream = true

    conn.WriteFrame(headers)

    ---

    var pending = map[uint32]*bytes.Buffer{}

    func readLoop(conn *Conn) {
        for !conn.Closed() {
            switch v := frame.(type) {
            case *http2.DataFrame:
                streamBuf := pending(v.StreamID)

                if streamBuf == nil {
                    streamBuf = new(bytes.Buffer)
                    pending[v.StreamID] = streamBuf
                }

                streamBuf.ReadFrom(v.Data)

                if v.EndStream {
                    log.Printf("stream %d; received data: %v\n", v.StreamID, streamBuf.String())

                    delete(pending, v.StreamID)
                }
            case *http2.HeadersFrame:
                if v.EndStream {
                    log.Printf("stream %d; received headers: %v\n", v.StreamID, v.Headers)
                }
            }
        }
    }
```

## Benchmarks

### HTTP/2 over TCP (Upgrade)

    BenchmarkConnReadWriteTCP_1K_C1-8      50000         31527 ns/op
    BenchmarkConnReadWriteTCP_1K_C8-8      50000         32138 ns/op
    BenchmarkConnReadWriteTCP_1K_C64-8     50000         32881 ns/op
    BenchmarkConnReadWriteTCP_1K_C512-8    50000         32448 ns/op
    BenchmarkConnReadWriteTCP_1M_C1-8        500       4113463 ns/op
    BenchmarkConnReadWriteTCP_1M_C8-8       1000       3585480 ns/op
    BenchmarkConnReadWriteTCP_1M_C64-8     10000       3668482 ns/op
    BenchmarkConnReadWriteTCP_1M_C512-8    10000       2893772 ns/op

### HTTP/2 over TLS (ALPN)

    BenchmarkConnReadWriteTLS_1K_C1-8      30000         51752 ns/op
    BenchmarkConnReadWriteTLS_1K_C8-8      30000         55804 ns/op
    BenchmarkConnReadWriteTLS_1K_C64-8     30000         50793 ns/op
    BenchmarkConnReadWriteTLS_1K_C512-8    30000         46517 ns/op
    BenchmarkConnReadWriteTLS_1M_C1-8        100      16708173 ns/op
    BenchmarkConnReadWriteTLS_1M_C8-8        100      10880645 ns/op
    BenchmarkConnReadWriteTLS_1M_C64-8     10000      18013781 ns/op
    BenchmarkConnReadWriteTLS_1M_C512-8    10000      16912103 ns/op
