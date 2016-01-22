package http2

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
)

type Dialer struct {
	Config *Config
}

func ClientConn(rawConn net.Conn, config *Config, req *http.Request) *Conn {
	conn := newConn(rawConn, false, config)
	conn.upgradeFunc = func() error {
		return conn.clientUpgrade(req)
	}
	return conn
}

var clientPreface = []byte(ClientPreface)

func (c *Conn) clientHandshake() error {
	if tlsConn, ok := c.rwc.(*tls.Conn); ok {
		if !tlsConn.ConnectionState().HandshakeComplete {
			if err := tlsConn.Handshake(); err != nil {
				return HandshakeError(err.Error())
			}
		}

		state := tlsConn.ConnectionState()

		if !state.NegotiatedProtocolIsMutual || state.NegotiatedProtocol != VersionTLS {
			return HandshakeError(fmt.Sprintf("bad protocol %s", state.NegotiatedProtocol))
		}
	} else {
		upgradeFunc := c.upgradeFunc
		if upgradeFunc == nil {
			upgradeFunc = func() error {
				return c.clientUpgrade(nil)
			}
		}
		if err := upgradeFunc(); err != nil {
			return err
		}
		c.upgradeFunc = nil
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

func (c *Conn) clientUpgrade(req *http.Request) (err error) {
	if req == nil {
		if req, err = http.NewRequest("GET", fmt.Sprintf("http://%s/", c.RemoteAddr().String()), nil); err != nil {
			return HandshakeError(err.Error())
		}
	}

	if req.Method != "GET" {
		return HandshakeError(http.StatusText(http.StatusMethodNotAllowed))
	}

	// The client does so by
	// making an HTTP/1.1 request that includes an Upgrade header field with
	// the "h2c" token.
	req.Header.Set("Upgrade", VersionTCP)

	// Since the upgrade is only intended to apply to the immediate
	// connection, a client sending the HTTP2-Settings header field MUST
	// also send "HTTP2-Settings" as a connection option in the Connection
	// header field to prevent it from being forwarded (see Section 6.1 of
	// [RFC7230]).
	if !containsValue(req.Header, "Connection", "Upgrade") {
		req.Header.Add("Connection", "Upgrade")
	}
	if !containsValue(req.Header, "Connection", "HTTP2-Settings") {
		req.Header["Connection"] = append(req.Header["Connection"], "HTTP2-Settings")
	}

	// A request that upgrades from HTTP/1.1 to HTTP/2 MUST include exactly
	// one "HTTP2-Settings" header field.
	if values := splitHeader(req.Header, "HTTP2-Settings"); len(values) == 0 {
		// The content of the HTTP2-Settings header field is the payload of a
		// SETTINGS frame (Section 6.5), encoded as a base64url string (that is,
		// the URL- and filename-safe Base64 encoding described in Section 5 of
		// [RFC4648], with any trailing '=' characters omitted).
		var encodedSettings string

		if settings := c.config.InitialSettings; len(settings) > 0 {
			payload := make([]byte, settingLen*len(settings))
			for i, setting := range settings {
				i *= settingLen
				binary.BigEndian.PutUint16(payload[i:i+2], uint16(setting.ID))
				binary.BigEndian.PutUint32(payload[i+2:i+6], setting.Value)
			}
			encodedSettings = base64.URLEncoding.EncodeToString(payload)
		}

		req.Header["HTTP2-Settings"] = []string{encodedSettings}
	} else if len(values) > 1 {
		return HandshakeError(http.StatusText(http.StatusBadRequest))
	}

	if err = req.Write(c.rwc); err != nil {
		return HandshakeError(err.Error())
	}

	// The HTTP/1.1 request that is sent prior to upgrade is assigned a
	// stream identifier of 1 (see Section 5.1.1) with default priority
	// values (Section 5.3.5).  Stream 1 is implicitly "half-closed" from
	// the client toward the server (see Section 5.1), since the request is
	// completed as an HTTP/1.1 request.  After commencing the HTTP/2
	// connection, stream 1 is used for the response.
	stream, _ := c.idleStream(1)
	stream.transition(false, FrameHeaders, true)

	var res *http.Response
	if res, err = http.ReadResponse(c.buf.Reader, req); err != nil {
		return HandshakeError(err.Error())
	}

	if res.StatusCode != http.StatusSwitchingProtocols ||
		!containsValue(res.Header, "Upgrade", VersionTCP) ||
		!containsValue(res.Header, "Connection", "Upgrade") {
		var reason string
		if res.Body != nil {
			buf := new(bytes.Buffer)
			buf.ReadFrom(res.Body)
			res.Body.Close()
			reason = buf.String()
		}
		if reason == "" {
			reason = "upgrade failed"
		}
		return HandshakeError(reason)
	}

	return
}

type RoundTripper struct{}

func (RoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}

var (
	_ http.RoundTripper = (*RoundTripper)(nil)
)
