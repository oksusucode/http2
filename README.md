
A Go implementation of the HTTP/2 protocol.

It is useful for client/server application development using the HTTP/2 connection/stream directly. (e.g. gRPC)

> currently under heavy development.

## Features

- [x] Server/Client Connection
- [x] Negotiation (ALPN, Upgrade)
- [x] Flow Control
- [x] Multiplexing without head-of-line blocking
- [x] Graceful Shutdown

## Benchmarks

- 2.2 GHz Intel Core i7
- 16 GB 1600 MHz DDR3
- Concurrency: C(1|8|64|512)
- Request/Response Data Length: 1024 Bytes

#### HTTP/2 over TLS (ALPN)

    go test -bench BenchmarkConnReadWriteTLS -benchmem

    BenchmarkConnReadWriteTLS_1K_C1-8      30000         49149 ns/op        4594 B/op         60 allocs/op
    BenchmarkConnReadWriteTLS_1K_C8-8      30000         49578 ns/op        4584 B/op         60 allocs/op
    BenchmarkConnReadWriteTLS_1K_C64-8     30000         49078 ns/op        4545 B/op         59 allocs/op
    BenchmarkConnReadWriteTLS_1K_C512-8    50000         48160 ns/op        4307 B/op         57 allocs/op

#### HTTP/2 over TCP (Upgrade)

    go test -bench BenchmarkConnReadWriteTCP -benchmem

    BenchmarkConnReadWriteTCP_1K_C1-8      50000         31044 ns/op        4064 B/op         32 allocs/op
    BenchmarkConnReadWriteTCP_1K_C8-8      50000         31911 ns/op        4062 B/op         32 allocs/op
    BenchmarkConnReadWriteTCP_1K_C64-8     50000         31298 ns/op        4039 B/op         32 allocs/op
    BenchmarkConnReadWriteTCP_1K_C512-8    50000         31233 ns/op        3862 B/op         30 allocs/op

