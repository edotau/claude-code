package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
)

// csrfHeader carries the per-process token every mutating request must echo back.
const csrfHeader = "X-CSRF-Token"

// maxTaskBytes caps the dispatch task field; a phone keyboard has no business sending more.
const maxTaskBytes = 8 << 10

// newCSRFToken returns 32 random bytes, hex-encoded, generated once per server process.
func newCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// constantTimeEqual compares two tokens without leaking timing information about where they differ.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// hostAllowed reports whether the Host header hostHeader (as net/http presents it, already stripped
// of a leading scheme) names this server: the exact bound host:port, "localhost" (with or without a
// port), or one of this node's tailnet MagicDNS names (full or short, with or without a port) — the
// DNS-rebinding defence: an attacker's page cannot point a browser's Host header at a tailnet IP by
// registering a public DNS name that resolves there, because that name is never on this allowlist.
func (s *Server) hostAllowed(hostHeader string) bool {
	h := strings.ToLower(hostHeader)
	if h == strings.ToLower(s.boundAddr) {
		return true
	}
	hostOnly := h
	if host, _, err := net.SplitHostPort(h); err == nil {
		hostOnly = host
	}
	if hostOnly == "localhost" {
		return true
	}
	for _, n := range s.dnsNames {
		if hostOnly == n {
			return true
		}
	}
	return false
}

// checkOrigin rejects a cross-origin request when the browser sent Origin or Sec-Fetch-Site: same
// trust boundary as the Host check, but the only line of defence available cross-scheme (http vs
// https) or when a browser extension spoofs Host but can't spoof these.
func (s *Server) checkOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // no browser-set origin info at all (curl, same-origin fetch omitting it): allow
	}
	return s.hostAllowed(strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://"))
}

// checkCSRF requires the per-process token on every mutating request, via header only (never a form
// field an attacker's cross-site form could also send blind).
func (s *Server) checkCSRF(r *http.Request) bool {
	return constantTimeEqual(r.Header.Get(csrfHeader), s.csrfToken)
}
