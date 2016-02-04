package http2

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// A Dialer contains options for connecting to HTTP/2 server.
type Dialer struct {
	// DialTCP specifies the dial function for creating unencrypted
	// TCP connections.
	// If DialTCP is nil, net.Dial is used.
	DialTCP func(network, addr string) (net.Conn, error)

	// DialTLS specifies an optional dial function for creating
	// TLS connections for non-proxied HTTPS requests.
	//
	// If DialTLS is nil, DialTCP and TLSClientConfig are used.
	//
	// If DialTLS is set, the TLSClientConfig are ignored.
	DialTLS func(network, addr string) (net.Conn, error)

	// Config specifies the connection configuration to use with
	// ClientConn. If nil, the default configuration is used.
	Config *Config

	// TLSClientConfig specifies the TLS configuration to use with
	// tls.Client. If nil, the default configuration is used.
	TLSClientConfig *tls.Config
}

// Dial connects to the address on the named protocol and
// then initiates a HTTP/2 negotiation, returning the
// resulting HTTP/2 connection.
//
// Allowed protocols are "h2" (ALPN) and "h2c" (Upgrade).
//
// The request parameter specifies an HTTP/1.1 request
// to send for client's upgrade.
//
// If request is nil, default upgrade request are used.
//
// If protocol is "h2", request are ignored.
//
// Examples:
//	d.Dial("h2", "google.com:https", nil)
//	d.Dial("h2c", "12.34.56.78:http", nil)
//	d.Dial(http2.ProtocolTCP, request.Host, request)
//
func (d *Dialer) Dial(protocol, address string, request *http.Request) (*Conn, error) {
	if d == nil {
		d = &Dialer{}
	}
	switch protocol {
	case ProtocolTCP:
		if address == "" && request != nil {
			if address = request.Host; address == "" {
				address = request.URL.Host
			}
		}
		address = joinHostPort(address, "http")
		c, err := d.dialTCP("tcp", address)
		if err != nil {
			return nil, err
		}
		conn := ClientConn(c, d.Config, request)
		return conn, conn.Handshake()
	case ProtocolTLS:
		address = joinHostPort(address, "https")
		if dialTLS := d.DialTLS; dialTLS != nil {
			c, err := dialTLS("tcp", address)
			if err != nil {
				return nil, err
			}
			conn := ClientConn(c, d.Config, nil)
			return conn, conn.Handshake()
		}
		config := cloneTLSClientConfig(d.TLSClientConfig)
		haveNPN := false
		for _, p := range config.NextProtos {
			if p == ProtocolTLS {
				haveNPN = true
				break
			}
		}
		if !haveNPN {
			config.NextProtos = append(config.NextProtos, ProtocolTLS)
		}
		if config.ServerName == "" {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			config.ServerName = host
		}
		c, err := d.dialTCP("tcp", address)
		if err != nil {
			return nil, err
		}
		tc := tls.Client(c, config)
		conn := ClientConn(tc, d.Config, nil)
		if err = conn.Handshake(); err == nil && !config.InsecureSkipVerify {
			err = tc.VerifyHostname(config.ServerName)
		}
		if err != nil {
			conn.close()
		}
		return conn, err
	default:
		return nil, fmt.Errorf("http2: bad protocol %s", protocol)
	}
}

func (d *Dialer) dialTCP(network, addr string) (net.Conn, error) {
	dial := d.DialTCP
	if dial != nil {
		return dial(network, addr)
	}
	dial = (&net.Dialer{Timeout: 3 * time.Second}).Dial
	c, err := dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	c.(*net.TCPConn).SetNoDelay(true)
	return c, nil
}

func joinHostPort(host, port string) string {
	if i := strings.LastIndexByte(host, ':'); i < 0 || i < strings.LastIndexByte(host, ']') {
		return host + ":" + port
	}
	return host
}

// ClientConn returns a new HTTP/2 client side connection
// using rawConn as the underlying transport.
// If config is nil, the default configuration is used.
// The req parameter optionally specifies the request to
// send for client's upgrade.
func ClientConn(rawConn net.Conn, config *Config, req *http.Request) *Conn {
	conn := newConn(rawConn, false, config)
	if req != nil {
		conn.upgradeFunc = func() error {
			return conn.clientUpgrade(req)
		}
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

		if !state.NegotiatedProtocolIsMutual || state.NegotiatedProtocol != ProtocolTLS {
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
	req.Header.Set("Upgrade", ProtocolTCP)

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
		!containsValue(res.Header, "Upgrade", ProtocolTCP) ||
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

func cloneTLSClientConfig(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return &tls.Config{}
	}
	return &tls.Config{
		Rand:                     cfg.Rand,
		Time:                     cfg.Time,
		Certificates:             cfg.Certificates,
		NameToCertificate:        cfg.NameToCertificate,
		GetCertificate:           cfg.GetCertificate,
		RootCAs:                  cfg.RootCAs,
		NextProtos:               cfg.NextProtos,
		ServerName:               cfg.ServerName,
		ClientAuth:               cfg.ClientAuth,
		ClientCAs:                cfg.ClientCAs,
		InsecureSkipVerify:       cfg.InsecureSkipVerify,
		CipherSuites:             cfg.CipherSuites,
		PreferServerCipherSuites: cfg.PreferServerCipherSuites,
		ClientSessionCache:       cfg.ClientSessionCache,
		MinVersion:               cfg.MinVersion,
		MaxVersion:               cfg.MaxVersion,
		CurvePreferences:         cfg.CurvePreferences,
	}
}

type RoundTripper struct {
}

func (RoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}

var (
	_ http.RoundTripper = (*RoundTripper)(nil)
)
