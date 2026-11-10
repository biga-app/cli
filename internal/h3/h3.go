// Package h3 sends requests over HTTP/3 when the server offers it, and falls
// back to the ordinary transport when it does not or QUIC cannot get through.
//
// Many networks drop UDP, so HTTP/3 is never required: the first request goes
// over TCP, an Alt-Svc header in its response says whether the server also
// speaks HTTP/3, and a failure to reach it over UDP sends the request, and the
// ones after it for a while, back to TCP.
package h3

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// Mode says when HTTP/3 is tried.
type Mode int

const (
	// Auto tries HTTP/3 after the server advertises it.
	Auto Mode = iota
	// On tries HTTP/3 first, without waiting for an advertisement.
	On
	// Off never uses HTTP/3.
	Off
)

// ParseMode reads a BIGA_HTTP3 value. Anything unrecognized is Auto.
func ParseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "1", "true", "yes":
		return On
	case "off", "0", "false", "no":
		return Off
	}
	return Auto
}

const (
	// handshakeTimeout is how long to wait for QUIC before assuming UDP is
	// blocked, short enough that a blocked network costs little.
	handshakeTimeout = 4 * time.Second
	// avoidFor is how long HTTP/3 is skipped for a server after it failed.
	avoidFor = 10 * time.Minute
	// quietBufferEnv is quic-go's switch for its UDP buffer warning.
	quietBufferEnv = "QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING"
)

// Options tune a [Transport]. The zero value is right for real use.
type Options struct {
	Mode Mode
	// TLS is the client TLS configuration for QUIC, for tests.
	TLS *tls.Config
	// HandshakeTimeout overrides how long QUIC may take to connect.
	HandshakeTimeout time.Duration
	// AvoidFor overrides how long HTTP/3 is skipped after a failure.
	AvoidFor time.Duration
	// Now reads the clock, for tests.
	Now func() time.Time
	// Proxy decides whether a request goes through a proxy, which QUIC cannot
	// do. Defaults to the environment's.
	Proxy func(*http.Request) (*url.URL, error)
}

// Transport is an [http.RoundTripper].
type Transport struct {
	base  http.RoundTripper
	h3    *http3.Transport
	opts  Options
	mu    sync.Mutex
	hosts map[string]*hostState
}

type hostState struct {
	alt       alternative
	altUntil  time.Time
	avoidTill time.Time
}

// New wraps base, which carries everything that does not go over HTTP/3.
func New(base http.RoundTripper, opts Options) *Transport {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = handshakeTimeout
	}
	if opts.AvoidFor <= 0 {
		opts.AvoidFor = avoidFor
	}
	if opts.Proxy == nil {
		opts.Proxy = http.ProxyFromEnvironment
	}
	// quic-go grumbles on stderr when the OS caps the UDP buffer, which a
	// CLI cannot raise and would only print into the user's terminal.
	if _, set := os.LookupEnv(quietBufferEnv); !set {
		_ = os.Setenv(quietBufferEnv, "true")
	}
	t := &Transport{base: base, opts: opts, hosts: make(map[string]*hostState)}
	t.h3 = &http3.Transport{
		TLSClientConfig: opts.TLS,
		QUICConfig: &quic.Config{
			HandshakeIdleTimeout: opts.HandshakeTimeout,
			MaxIdleTimeout:       30 * time.Second,
			KeepAlivePeriod:      10 * time.Second,
		},
		Dial: t.dial,
	}
	return t
}

// CloseIdleConnections closes idle connections on both transports.
func (t *Transport) CloseIdleConnections() {
	t.h3.CloseIdleConnections()
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// RoundTrip sends req over HTTP/3 when that is on offer, otherwise over the
// base transport.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.wants(req) {
		return t.viaBase(req)
	}
	probe := &probeBody{orig: req.Body}
	h3req := req.Clone(req.Context())
	if req.Body != nil {
		h3req.Body = probe
	}
	resp, err := t.h3.RoundTrip(h3req)
	if err == nil {
		return resp, nil
	}
	if req.Context().Err() != nil {
		return nil, err
	}
	t.avoid(authority(req.URL))
	switch {
	case req.Body == nil || req.Body == http.NoBody:
	case !probe.started():
		req.Body = probe.orig
	case req.GetBody != nil:
		body, gerr := req.GetBody()
		if gerr != nil {
			return nil, err
		}
		req.Body = body
	default:
		_ = probe.orig.Close()
		return nil, err
	}
	return t.viaBase(req)
}

// viaBase sends req over the base transport and learns from the response
// whether the server speaks HTTP/3.
func (t *Transport) viaBase(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil && req.URL.Scheme == "https" && t.opts.Mode != Off {
		if values, ok := resp.Header["Alt-Svc"]; ok {
			t.learn(authority(req.URL), values)
		}
	}
	return resp, err
}

func (t *Transport) wants(req *http.Request) bool {
	if t.opts.Mode == Off || req.URL.Scheme != "https" {
		return false
	}
	if proxy, err := t.opts.Proxy(req); err != nil || proxy != nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.hosts[authority(req.URL)]
	now := t.opts.Now()
	if st != nil && now.Before(st.avoidTill) {
		return false
	}
	if t.opts.Mode == On {
		return true
	}
	return st != nil && now.Before(st.altUntil)
}

func (t *Transport) learn(host string, values []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.hosts[host]
	if st == nil {
		st = &hostState{}
		t.hosts[host] = st
	}
	for _, v := range values {
		if alt, ok := parseAltSvc(v); ok && (alt.host == "" || sameHost(alt.host, host)) {
			st.alt, st.altUntil = alt, t.opts.Now().Add(alt.maxAge)
			return
		}
	}
	st.altUntil = time.Time{}
}

func (t *Transport) avoid(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.hosts[host]
	if st == nil {
		st = &hostState{}
		t.hosts[host] = st
	}
	st.avoidTill = t.opts.Now().Add(t.opts.AvoidFor)
}

// dial opens the QUIC connection, to the port the server advertised if there
// is one.
func (t *Transport) dial(ctx context.Context, addr string, tlsConf *tls.Config, conf *quic.Config) (*quic.Conn, error) {
	t.mu.Lock()
	if st := t.hosts[addr]; st != nil && st.alt.port != "" {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			addr = net.JoinHostPort(host, st.alt.port)
		}
	}
	t.mu.Unlock()
	return quic.DialAddr(ctx, addr, tlsConf, conf)
}

// authority is host:port of the request's server, with the port https uses
// when the URL names none. It matches the address QUIC is asked to dial.
func authority(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func sameHost(a, authority string) bool {
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		host = authority
	}
	return strings.EqualFold(a, host)
}

// probeBody hands the request body to HTTP/3 while remembering whether any of
// it was read, so a request that failed before sending can go again as it is.
type probeBody struct {
	orig io.ReadCloser
	mu   sync.Mutex
	read bool
}

func (b *probeBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.read = true
	b.mu.Unlock()
	return b.orig.Read(p)
}

// Close leaves the body open: the fallback may still need it.
func (b *probeBody) Close() error { return nil }

func (b *probeBody) started() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.read
}
