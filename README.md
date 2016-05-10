
A Go implementation of the HTTP/2 protocol.

It is useful for client/server application development using the HTTP/2 connection/stream directly.

> currently under heavy development.

## Features

- [x] Server/Client Connection
- [x] Negotiation (ALPN, Upgrade)
- [x] Flow Control
- [x] Multiplexing without head-of-line blocking
- [x] Graceful Shutdown

## Requirements

- Golang 1.5+

## Installation

    go get github.com/nekolunar/http2

## Documentation

- [API Reference](https://godoc.org/github.com/nekolunar/http2)
- [Example](https://github.com/nekolunar/http2/blob/master/conn_test.go#L389-L427)

## Benchmarks

- 2.2 GHz Intel Core i7
- 16 GB 1600 MHz DDR3
- Concurrency: C(1|8|64|512)
- Request/Response Data Length: 1024 Bytes

#### HTTP/2 over TLS (ALPN)

    go test -bench BenchmarkConnReadWriteTLS -benchmem

    BenchmarkConnReadWriteTLS_1K_C1-8      50000         33666 ns/op        4083 B/op         32 allocs/op
    BenchmarkConnReadWriteTLS_1K_C8-8      50000         34544 ns/op        4080 B/op         32 allocs/op
    BenchmarkConnReadWriteTLS_1K_C64-8     50000         34277 ns/op        4058 B/op         32 allocs/op
    BenchmarkConnReadWriteTLS_1K_C512-8    50000         33966 ns/op        3880 B/op         30 allocs/op

#### HTTP/2 over TCP (Upgrade)

    go test -bench BenchmarkConnReadWriteTCP -benchmem

    BenchmarkConnReadWriteTCP_1K_C1-8      50000         30090 ns/op        4065 B/op         32 allocs/op
    BenchmarkConnReadWriteTCP_1K_C8-8      50000         30610 ns/op        4062 B/op         32 allocs/op
    BenchmarkConnReadWriteTCP_1K_C64-8     50000         30673 ns/op        4040 B/op         32 allocs/op
    BenchmarkConnReadWriteTCP_1K_C512-8   100000         30715 ns/op        3965 B/op         31 allocs/op

## License

MIT license. See [LICENSE](https://github.com/nekolunar/http2/blob/master/LICENSE) for details.
