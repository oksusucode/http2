package http2

type Dialer struct {
}

func (*Dialer) Dial() (*Conn, error) {
	return nil, nil
}
