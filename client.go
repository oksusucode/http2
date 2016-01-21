package http2

import (
	"crypto/tls"
	"fmt"
)

type Dialer struct {
	Config *Config
}

var clientPreface = []byte(ClientPreface)

func (c *Conn) clientHandshake() error {
	if tlsConn, ok := c.rwc.(*tls.Conn); ok {
		if !tlsConn.ConnectionState().HandshakeComplete {
			if err := tlsConn.Handshake(); err != nil {
				return err
			}
		}

		state := tlsConn.ConnectionState()

		if !state.NegotiatedProtocolIsMutual || state.NegotiatedProtocol != VersionTLS {
			return fmt.Errorf("bad protocol %s", state.NegotiatedProtocol)
		}
	} else {
		// TODO: client upgrade
		return nil
	}

	// The client connection preface starts with a sequence of 24 octets.
	// This sequence MUST be followed by a
	// SETTINGS frame (Section 6.5), which MAY be empty.
	if _, err := c.buf.Write(clientPreface); err != nil {
		return err
	}
	if err := c.writeFrame(&SettingsFrame{false, c.config.InitialSettings}); err != nil {
		return err
	}

	// The server connection preface consists of a potentially empty
	// SETTINGS frame (Section 6.5) that MUST be the first frame the server
	// sends in the HTTP/2 connection.
	if firstFrame, err := c.readFrame(); err == nil {
		if settings, ok := firstFrame.(*SettingsFrame); !ok || settings.Ack {
			return errBadConnPreface
		}
	} else {
		return err
	}

	return nil
}
