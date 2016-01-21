
A Go implementation of the HTTP/2 protocol.

It is useful for client/server application development using the HTTP/2 connection/stream directly. (eg: gRPC)

> currently under heavy development.

## Example

### Server

```go
    func sayHello(conn *http2.Conn) {
        for !conn.Closed() {
            frame, err := conn.ReadFrame()

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

                streamID, endStream = v.StreamID, v.EndStream
            case *http2.HeadersFrame:
                streamID, endStream = v.StreamID, v.EndStream

            // case *http2.PriorityFrame:
            // case *http2.RSTStreamFrame:
            // case *http2.SettingsFrame:
            // case *http2.PushPromiseFrame:
            // case *http2.PingFrame:
            // case *http2.GoAwayFrame:
            // case *http2.WindowUpdateFrame:
            // case *http2.UnknownFrame:

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

    server := &http2.Server{
        Addr:       ":http",
        Handler:    sayHello,
    }

    server.ListenAndServe()
```

### Client

```go
    conn, err := http2.Dial(":http")

    go readLoop(conn)

    newStreamID, _ := conn.NextStreamID()

    headers := new(http2.HeadersFrame)
    headers.StreamID = newStreamID
    headers.SetMethod("GET")
    headers.SetPath("/")
    headers.EndStream = true

    conn.WriteFrame(headers)

    ---

    var pending = map[uint32]*byte.Buffer{}

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
