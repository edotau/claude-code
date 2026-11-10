package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strings"
)

// RequestTagsEnv holds a single-line JSON object of request tags sent to Databricks AI Gateway rows.
const RequestTagsEnv = "HARNESS_REQUEST_TAGS"

// HeadersEnv holds extra upstream request headers, one "Name: value" per line.
const HeadersEnv = "HARNESS_HEADERS"

const (
	maxHeadersBytes   = 4 << 10
	RequestTagsHeader = "Databricks-Ai-Gateway-Request-Tags" // docs.databricks.com/aws/en/ai-gateway/usage-tracking
)

// launcherHeaders are the proxy's own request headers; they never go upstream.
var launcherHeaders = []string{AgentHeader, IdentityHeader, "X-Claude-Code-Session-Id", ClientHeader}

// protectedHeaders can never be set from HARNESS_HEADERS: credentials, attribution, and framing.
var protectedHeaders = map[string]bool{
	"Authorization": true, "Proxy-Authorization": true, "X-Api-Key": true, "X-Goog-Api-Key": true, "Host": true,
	"User-Agent": true, "Content-Length": true, "Content-Type": true, "Transfer-Encoding": true, "Connection": true,
	"Cookie": true, textproto.CanonicalMIMEHeaderKey(RequestTagsHeader): true,
}

// ParseHeaders parses "Name: value" lines; invalid or protected lines are dropped with one warning, never fatal.
func ParseHeaders(raw string, log io.Writer) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	if len(raw) > maxHeadersBytes {
		fmt.Fprintf(log, "proxy: %s ignored: over 4 KiB\n", HeadersEnv)
		return out
	}
	protected := protectedKeys()
	bad := 0
	for line := range strings.SplitSeq(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, val, ok := parseHeaderLine(line, protected)
		if !ok {
			bad++
			continue
		}
		out[key] = val
	}
	if bad > 0 {
		fmt.Fprintf(log, "proxy: %s: dropped %d invalid or protected line(s)\n", HeadersEnv, bad)
	}
	return out
}

// protectedKeys is the canonical set of header names HARNESS_HEADERS may not set.
func protectedKeys() map[string]bool {
	protected := map[string]bool{}
	for k := range protectedHeaders {
		protected[k] = true
	}
	for _, k := range launcherHeaders {
		protected[textproto.CanonicalMIMEHeaderKey(k)] = true
	}
	return protected
}

// parseHeaderLine splits one "Name: value" line; ok is false when it is malformed or protected.
func parseHeaderLine(line string, protected map[string]bool) (key, val string, ok bool) {
	name, val, ok := strings.Cut(line, ":")
	name, val = strings.TrimSpace(name), strings.TrimSpace(val)
	key = textproto.CanonicalMIMEHeaderKey(name)
	if !ok || !validHeaderName(name) || !validHeaderValue(val) || protected[key] {
		return "", "", false
	}
	return key, val, true
}

// validHeaderName reports whether s is a non-empty RFC 7230 token.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !isTokenChar(c) {
			return false
		}
	}
	return true
}

// isTokenChar reports whether c is an RFC 7230 tchar.
func isTokenChar(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		strings.ContainsRune("!#$%&'*+-.^_`|~", c)
}

// validHeaderValue rejects CR, LF, NUL and other control characters (tab allowed).
func validHeaderValue(s string) bool {
	return !strings.ContainsFunc(s, func(c rune) bool { return c != '\t' && (c < 0x20 || c == 0x7f) })
}

// StripLauncherHeaders removes client-only attribution and authentication headers.
func StripLauncherHeaders(h http.Header) {
	for _, k := range launcherHeaders {
		h.Del(k)
	}
}
