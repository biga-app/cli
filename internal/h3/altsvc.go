package h3

import (
	"net"
	"strconv"
	"strings"
	"time"
)

// defaultMaxAge is how long an Alt-Svc entry lasts when it names no max age
// (RFC 7838).
const defaultMaxAge = 24 * time.Hour

// alternative is where a server said it also speaks HTTP/3.
type alternative struct {
	// host is empty when the alternative is on the origin's own host.
	host string
	port string
	// maxAge is how long to remember it.
	maxAge time.Duration
}

// parseAltSvc reads an Alt-Svc header and returns its HTTP/3 alternative, if
// any. "clear" and headers with no h3 entry report false. Alternatives for
// other protocols are ignored.
func parseAltSvc(value string) (alternative, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "clear") {
		return alternative{}, false
	}
	for _, entry := range splitOutsideQuotes(value, ',') {
		params := splitOutsideQuotes(entry, ';')
		proto, authority, ok := strings.Cut(strings.TrimSpace(params[0]), "=")
		if !ok || proto != "h3" {
			continue
		}
		authority = strings.TrimSpace(authority)
		if len(authority) < 2 || authority[0] != '"' || authority[len(authority)-1] != '"' {
			continue
		}
		authority = authority[1 : len(authority)-1]
		host, port, err := net.SplitHostPort(authority)
		if err != nil || port == "" {
			continue
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			continue
		}
		alt := alternative{host: host, port: port, maxAge: defaultMaxAge}
		for _, p := range params[1:] {
			key, val, _ := strings.Cut(strings.TrimSpace(p), "=")
			if key != "ma" {
				continue
			}
			if secs, err := strconv.Atoi(strings.Trim(val, `"`)); err == nil && secs >= 0 {
				alt.maxAge = time.Duration(secs) * time.Second
			}
		}
		return alt, true
	}
	return alternative{}, false
}

func splitOutsideQuotes(s string, sep byte) []string {
	var (
		out     []string
		start   int
		inQuote bool
	)
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"':
			inQuote = !inQuote
		case s[i] == sep && !inQuote:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
