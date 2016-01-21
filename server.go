package http2

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
)

type Handler func(*Conn)

type Server struct {
	Addr    string
	Handler Handler
	Config  *Config
}

func (c *Conn) serverHandshake() error {
	if tlsConn, ok := c.rwc.(*tls.Conn); ok {
		if !tlsConn.ConnectionState().HandshakeComplete {
			if err := tlsConn.Handshake(); err != nil {
				return err
			}
		}

		state := tlsConn.ConnectionState()

		if state.NegotiatedProtocol != VersionTLS {
			return fmt.Errorf("bad protocol %s", state.NegotiatedProtocol)
		}

		// Due to deployment limitations, it might not
		// be possible to fail TLS negotiation when these restrictions are not
		// met.  An endpoint MAY immediately terminate an HTTP/2 connection that
		// does not meet these TLS requirements with a connection error
		// (Section 5.4.1) of type INADEQUATE_SECURITY.
		if !c.config.AllowLowTLSVersion && state.Version < tls.VersionTLS12 {
			return ConnError{fmt.Errorf("bad TLS version %x", state.Version), ErrCodeInadequateSecurity}
		}

		// A deployment of HTTP/2 over TLS 1.2 SHOULD NOT use any of the cipher
		// suites that are listed in the cipher suite black list (Appendix A).
		//
		// Endpoints MAY choose to generate a connection error (Section 5.4.1)
		// of type INADEQUATE_SECURITY if one of the cipher suites from the
		// black list is negotiated.
		if state.Version >= tls.VersionTLS12 && badCipher(state.CipherSuite) {
			return ConnError{fmt.Errorf("prohibited TLS 1.2 cipher type %x", state.CipherSuite), ErrCodeInadequateSecurity}
		}
	} else {
		// TODO: server upgrade
		return nil
	}

	// The server connection preface consists of a potentially empty
	// SETTINGS frame (Section 6.5) that MUST be the first frame the server
	// sends in the HTTP/2 connection.
	if err := c.writeFrame(&SettingsFrame{false, c.config.InitialSettings}); err != nil {
		return err
	}

	// The client connection preface starts with a sequence of 24 octets.
	// This sequence MUST be followed by a
	// SETTINGS frame (Section 6.5), which MAY be empty.
	if preface, err := c.buf.Peek(len(ClientPreface)); err == nil {
		if !bytes.Equal(preface, clientPreface) {
			return errBadConnPreface
		}
	} else {
		return err
	}
	c.buf.Discard(len(ClientPreface))
	if firstFrame, err := c.readFrame(); err == nil {
		if settings, ok := firstFrame.(*SettingsFrame); !ok || settings.Ack {
			return errors.New("first received frame was not SETTINGS")
		}
	} else {
		return err
	}

	return nil
}

func badCipher(cipher uint16) bool {
	switch cipher {
	case
		tls.TLS_RSA_WITH_RC4_128_SHA,
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA,
		tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA:

		return true
	default:
		return false
	}
}
