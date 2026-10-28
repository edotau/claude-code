package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/edotau/claude-code/internal/buildver"
	"github.com/edotau/claude-code/internal/providers"
)

// RequestTagsEnv holds a single-line JSON object of request tags sent to Databricks AI Gateway rows.
const RequestTagsEnv = "HARNESS_REQUEST_TAGS"

const (
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
