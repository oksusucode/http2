package http2

type Handler func(*Conn)

type Server struct {
	Addr    string
	Handler Handler
}

func (s *Server) ListenAndServe() error {
	return nil
}

func (s *Server) ListenAndServeTLS(certFile, keyFile string) error {
	return nil
}
