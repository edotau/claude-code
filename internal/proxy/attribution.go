package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/buildver"
	"github.com/edotau/claude-code/internal/providers"
)

// RequestTagsEnv holds a single-line JSON object of request tags sent to Databricks AI Gateway rows.
const RequestTagsEnv = "HARNESS_REQUEST_TAGS"

// HeadersEnv holds extra upstream request headers, one "Name: value" per line.
const HeadersEnv = "HARNESS_HEADERS"

const (
	maxHeadersBytes   = 4 << 10
	requestTagsHeader = "Databricks-Ai-Gateway-Request-Tags" // docs.databricks.com/aws/en/ai-gateway/usage-tracking
	maxRequestTags    = 4 << 10
	maxAgentToken     = 64
)

// launcherHeaders are the router's own request headers; they never go upstream.
var launcherHeaders = []string{AgentHeader, SessionHeader, "X-Claude-Code-Session-Id", ClientHeader}

// attribute stamps the claude-code User-Agent (and tags on an AI Gateway row) and drops launcher headers.
func (r *Router) attribute(h http.Header, in *http.Request, p *providers.Provider) {
	for _, k := range launcherHeaders {
		h.Del(k)
	}
	for k, v := range r.headers {
		h.Set(k, v)
	}
	h.Set("User-Agent", userAgent(in.Header.Get(AgentHeader)))
	if r.tags != "" && p.IsAIGateway() {
		h.Set(requestTagsHeader, r.tags)
	}
}

// userAgent is "claude-code/<version>" plus the sanitized agent name when the client sent one.
func userAgent(agent string) string {
	v := buildver.Version()
	if v == "(devel)" {
		v = "devel"
	}
	ua := "claude-code/" + v
	if a := agentToken(agent); a != "" {
		ua += " " + a
	}
	return ua
}

// agentToken keeps [A-Za-z0-9._-] from the client-controlled agent name, capped; "" when nothing survives.
func agentToken(s string) string {
	var b strings.Builder
	for _, c := range s {
		if b.Len() >= maxAgentToken {
			break
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// parseRequestTags returns raw when it is a single-line JSON object of strings within the cap; else logs once and "".
func parseRequestTags(raw string, log io.Writer) string {
	if raw == "" {
		return ""
	}
	reason := ""
	var obj map[string]string
	switch {
	case len(raw) > maxRequestTags:
		reason = "over 4 KiB"
	case strings.ContainsFunc(raw, func(c rune) bool { return c < 0x20 || c == 0x7f }):
		reason = "contains a newline or control character"
	case json.Unmarshal([]byte(raw), &obj) != nil || obj == nil:
		reason = "not a JSON object of string values"
	}
	if reason != "" {
		fmt.Fprintf(log, "proxy: %s ignored: %s\n", RequestTagsEnv, reason)
		return ""
	}
	return raw
}

// protectedHeaders can never be set from HARNESS_HEADERS: credentials, attribution, and framing.
var protectedHeaders = map[string]bool{
	"Authorization": true, "Proxy-Authorization": true, "X-Api-Key": true, "X-Goog-Api-Key": true, "Host": true,
	"User-Agent": true, "Content-Length": true, "Content-Type": true, "Transfer-Encoding": true, "Connection": true,
	"Cookie": true, textproto.CanonicalMIMEHeaderKey(requestTagsHeader): true,
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
	protected := map[string]bool{}
	for k := range protectedHeaders {
		protected[k] = true
	}
	for _, k := range launcherHeaders {
		protected[textproto.CanonicalMIMEHeaderKey(k)] = true
	}
	bad := 0
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, val, ok := strings.Cut(line, ":")
		name, val = strings.TrimSpace(name), strings.TrimSpace(val)
		key := textproto.CanonicalMIMEHeaderKey(name)
		if !ok || !validHeaderName(name) || !validHeaderValue(val) || protected[key] {
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

// HarnessHeaders is the validated HARNESS_HEADERS set from the environment (warnings go to stderr).
func HarnessHeaders() map[string]string { return ParseHeaders(os.Getenv(HeadersEnv), os.Stderr) }

// validHeaderName reports whether s is a non-empty RFC 7230 token.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			continue
		}
		return false
	}
	return true
}

// validHeaderValue rejects CR, LF, NUL and other control characters (tab allowed).
func validHeaderValue(s string) bool {
	return !strings.ContainsFunc(s, func(c rune) bool { return c != '\t' && (c < 0x20 || c == 0x7f) })
}
