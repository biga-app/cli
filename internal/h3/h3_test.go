package h3

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

func TestParseAltSvc(t *testing.T) {
	cases := []struct {
		in   string
		want alternative
		ok   bool
	}{
		{`h3=":443"; ma=86400`, alternative{port: "443", maxAge: 86400 * time.Second}, true},
		{`h3=":8443"`, alternative{port: "8443", maxAge: defaultMaxAge}, true},
		{`h2=":443"; ma=60, h3=":4433"; ma=120; persist=1`, alternative{port: "4433", maxAge: 120 * time.Second}, true},
		{`h3="alt.example:443"; ma=5`, alternative{host: "alt.example", port: "443", maxAge: 5 * time.Second}, true},
		{`h3-29=":443"`, alternative{}, false},
		{`h2=":443"`, alternative{}, false},
		{`clear`, alternative{}, false},
		{``, alternative{}, false},
		{`h3=":notaport"`, alternative{}, false},
		{`h3=":99999"`, alternative{}, false},
		{`h3=":443`, alternative{}, false},
	}
	for _, c := range cases {
		got, ok := parseAltSvc(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseAltSvc(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseMode(t *testing.T) {
	if got := ParseMode("  off "); got != Off {
		t.Errorf("surrounding space is ignored, got %v", got)
	}
	for in, want := range map[string]Mode{"": Auto, "auto": Auto, "garbage": Auto, "on": On, "TRUE": On, "1": On, "off": Off, "0": Off} {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %v, want %v", in, got, want)
		}
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// site is one origin reachable over TCP and, unless udp is false, over QUIC
// on the same port number.
type site struct {
	url      string
	tcpHits  atomic.Int32
	h3Hits   atomic.Int32
	lastBody atomic.Value
}

func startSite(t *testing.T, udp bool, altSvc func(port int) string) *site {
	t.Helper()
	cert := selfSigned(t)
	s := &site{}
	handler := func(counter *atomic.Int32) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			body, _ := io.ReadAll(r.Body)
			s.lastBody.Store(string(body))
			if altSvc != nil && counter == &s.tcpHits {
				w.Header().Set("Alt-Svc", altSvc(portOf(t, s.url)))
			}
			_, _ = io.WriteString(w, r.Proto)
		})
	}
	tcp := httptest.NewUnstartedServer(handler(&s.tcpHits))
	tcp.EnableHTTP2 = true
	tcp.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	tcp.StartTLS()
	t.Cleanup(tcp.Close)
	s.url = tcp.URL
	if !udp {
		return s
	}
	conn, err := net.ListenPacket("udp4", "127.0.0.1:"+strconv.Itoa(portOf(t, tcp.URL)))
	if err != nil {
		t.Skipf("the UDP port matching the TCP one is taken: %v", err)
	}
	srv := &http3.Server{
		Handler:   handler(&s.h3Hits),
		TLSConfig: http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}),
	}
	go func() { _ = srv.Serve(conn) }()
	t.Cleanup(func() { _ = srv.Close(); _ = conn.Close() })
	return s
}

func portOf(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Port() == "" {
		return 0
	}
	n, _ := strconv.Atoi(u.Port())
	return n
}

func newTransport(mode Mode, now func() time.Time) *Transport {
	base := &http.Transport{ //nolint:gosec // throwaway certificate on loopback
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true,
	}
	return New(base, Options{
		Mode: mode, TLS: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // throwaway certificate on loopback
		HandshakeTimeout: 400 * time.Millisecond, Now: now,
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
	})
}

func get(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func advertise(port int) string { return `h3=":` + strconv.Itoa(port) + `"; ma=60` }

func TestSwitchesToHTTP3AfterTheServerAdvertisesIt(t *testing.T) {
	s := startSite(t, true, advertise)
	tr := newTransport(Auto, nil)
	c := &http.Client{Transport: tr}
	t.Cleanup(tr.CloseIdleConnections)

	if got := get(t, c, s.url); got != "HTTP/2.0" {
		t.Fatalf("the first request has nothing to go on and goes over TCP: %q", got)
	}
	if got := get(t, c, s.url); got != "HTTP/3.0" {
		t.Fatalf("after Alt-Svc the next request uses QUIC: %q", got)
	}
	if s.tcpHits.Load() != 1 || s.h3Hits.Load() != 1 {
		t.Errorf("hits tcp=%d h3=%d", s.tcpHits.Load(), s.h3Hits.Load())
	}
}

func TestAdvertisementExpires(t *testing.T) {
	s := startSite(t, true, advertise)
	clock := time.Now()
	tr := newTransport(Auto, func() time.Time { return clock })
	c := &http.Client{Transport: tr}
	t.Cleanup(tr.CloseIdleConnections)
	get(t, c, s.url)
	clock = clock.Add(2 * time.Minute)
	if got := get(t, c, s.url); got != "HTTP/2.0" {
		t.Errorf("after the advertised lifetime the request goes back to TCP: %q", got)
	}
}

func TestBlockedUDPFallsBackAndIsRemembered(t *testing.T) {
	s := startSite(t, false, advertise)
	clock := time.Now()
	tr := newTransport(Auto, func() time.Time { return clock })
	c := &http.Client{Transport: tr}
	t.Cleanup(tr.CloseIdleConnections)

	get(t, c, s.url)
	start := time.Now()
	if got := get(t, c, s.url); got != "HTTP/2.0" {
		t.Fatalf("with nothing answering on UDP the request still succeeds over TCP: %q", got)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("giving up on QUIC took %v", took)
	}
	start = time.Now()
	get(t, c, s.url)
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Errorf("the next request waited %v for QUIC again instead of remembering", took)
	}
	if s.tcpHits.Load() != 3 {
		t.Errorf("tcp hits = %d, want 3", s.tcpHits.Load())
	}

	clock = clock.Add(avoidFor + time.Minute)
	get(t, c, s.url) // the advertisement lapsed too; this one over TCP renews it
	start = time.Now()
	get(t, c, s.url)
	if took := time.Since(start); took < 200*time.Millisecond {
		t.Errorf("after the pause QUIC should be tried again, took only %v", took)
	}
}

func TestRequestBodiesSurviveTheFallback(t *testing.T) {
	s := startSite(t, false, advertise)
	tr := newTransport(On, nil)
	c := &http.Client{Transport: tr}
	t.Cleanup(tr.CloseIdleConnections)

	payload := bytes.Repeat([]byte("checkpoint "), 5000)
	for name, body := range map[string]io.Reader{
		"replayable": bytes.NewReader(payload),
		"one shot":   io.NopCloser(io.MultiReader(bytes.NewReader(payload))),
	} {
		tr.mu.Lock()
		clear(tr.hosts)
		tr.mu.Unlock()
		req, err := http.NewRequest(http.MethodPost, s.url, body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if got, _ := s.lastBody.Load().(string); got != string(payload) {
			t.Errorf("%s: the server received %d bytes, want %d", name, len(got), len(payload))
		}
	}
}

func TestModes(t *testing.T) {
	t.Run("on uses QUIC before any advertisement", func(t *testing.T) {
		s := startSite(t, true, nil)
		tr := newTransport(On, nil)
		t.Cleanup(tr.CloseIdleConnections)
		if got := get(t, &http.Client{Transport: tr}, s.url); got != "HTTP/3.0" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("off never does, whatever the server says", func(t *testing.T) {
		s := startSite(t, true, advertise)
		tr := newTransport(Off, nil)
		c := &http.Client{Transport: tr}
		t.Cleanup(tr.CloseIdleConnections)
		get(t, c, s.url)
		if got := get(t, c, s.url); got == "HTTP/3.0" || s.h3Hits.Load() != 0 {
			t.Errorf("got %q with %d QUIC hits", got, s.h3Hits.Load())
		}
	})
	t.Run("a proxy keeps traffic on TCP", func(t *testing.T) {
		s := startSite(t, true, advertise)
		tr := newTransport(On, nil)
		tr.opts.Proxy = func(*http.Request) (*url.URL, error) { return &url.URL{Host: "proxy:3128"}, nil }
		t.Cleanup(tr.CloseIdleConnections)
		if tr.wants(mustRequest(t, s.url)) {
			t.Error("QUIC cannot go through an HTTP proxy")
		}
	})
	t.Run("plain http is left alone", func(t *testing.T) {
		tr := newTransport(On, nil)
		if tr.wants(mustRequest(t, "http://example.com/")) {
			t.Error("HTTP/3 is TLS only")
		}
	})
}

func mustRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
