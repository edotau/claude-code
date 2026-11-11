package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// scopeKey carries the profile a session token is confined to; absent for the router secret.
type scopeKey struct{}

// sessionScope is the profile a session-token request is confined to.
func sessionScope(in *http.Request) (string, bool) {
	scope, ok := in.Context().Value(scopeKey{}).(string)
	return scope, ok
}

// resolveTarget maps the path profile and requested model to a target, writing the error when it cannot.
func resolveTarget(w http.ResponseWriter, in *http.Request, reg *providers.Registry,
	session, model string) (providers.Target, bool) {
	sess, err := reg.Get(session)
	if err != nil {
		writeError(w, http.StatusBadRequest, "proxy: "+err.Error())
		return providers.Target{}, false
	}
	target := providers.ParseTarget(reg, model, sess)
	// a resolved target outside a session token's profile is a 403
	if scope, scoped := sessionScope(in); scoped && target.Provider.Name != scope {
		writeError(w, http.StatusForbidden, "proxy: session token is limited to profile "+scope)
		return target, false
	}
	return target, true
}

// confineLadder drops fallback providers outside a session token's profile; they hold other credentials.
func confineLadder(in *http.Request, ladder []providers.Target) []providers.Target {
	scope, scoped := sessionScope(in)
	if !scoped {
		return ladder
	}
	return slices.DeleteFunc(ladder, func(t providers.Target) bool { return t.Provider.Name != scope })
}

// anthropicHost is the only host the sealed subscription token is sent to; a var so tests can point it at httptest.
var anthropicHost = "api.anthropic.com"

// anthropicOrigin reports whether route is https on anthropicHost exactly (no suffix or userinfo tricks).
func anthropicOrigin(route string) bool {
	u, err := url.Parse(route)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Host == anthropicHost
}

// parseRoute splits /p/<provider>/<rest>; rest is "" when the path has no second segment.
func parseRoute(path string) (provider, rest string, ok bool) {
	tail, ok := strings.CutPrefix(path, "/p/")
	if !ok {
		return "", "", false
	}
	provider, rest, _ = strings.Cut(tail, "/")
	return provider, rest, true
}

// parsePath splits /p/<provider>/v1/messages[/count_tokens].
func parsePath(path string) (provider string, count, ok bool) {
	provider, rest, ok := parseRoute(path)
	if !ok {
		return "", false, false
	}
	switch "/" + rest {
	case "/v1/messages":
		return provider, false, provider != ""
	case "/v1/messages/count_tokens":
		return provider, true, provider != ""
	}
	return "", false, false
}

// withModel rewrites the top-level model; an unchanged id forwards the client's bytes verbatim.
func withModel(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m == nil { // a literal null body unmarshals without error
		return nil, errors.New("request body is not a JSON object")
	}
	var cur string
	if json.Unmarshal(m["model"], &cur) == nil && cur == model {
		return body, nil
	}
	m["model"], _ = json.Marshal(model)
	return textutil.MarshalNoEscape(m)
}

// copyRequestHeaders forwards only wire-relevant headers; client auth is added back for passthrough providers only.
func copyRequestHeaders(dst, src http.Header) {
	for k, vs := range src {
		ck := http.CanonicalHeaderKey(k)
		if strings.HasPrefix(ck, "Anthropic-") || ck == "Content-Type" || ck == "Accept" {
			dst[ck] = append([]string(nil), vs...)
		}
	}
}

var hopHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true,
	"Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Content-Length": true,
}

// passthroughAuth sets the client's login on h, else the sealed subscription token, only for anthropicHost routes.
func (r *Router) passthroughAuth(h http.Header, in *http.Request, route string) {
	if !anthropicOrigin(route) {
		return
	}
	if a := in.Header.Get("Authorization"); a != "" {
		h.Set("Authorization", a)
		return
	}
	tok := r.oauthToken()
	if tok == "" {
		return
	}
	h.Set("Authorization", "Bearer "+tok)
	if beta := h.Get("Anthropic-Beta"); !strings.Contains(beta, oauthBeta) {
		h.Set("Anthropic-Beta", strings.Trim(beta+","+oauthBeta, ","))
	}
}

const (
	maxRequestTags = 4 << 10
	maxAgentToken  = 64
)

// attribute stamps the claude-code User-Agent (and tags on an AI Gateway row) and drops launcher headers.
func (r *Router) attribute(h http.Header, in *http.Request, p *providers.Provider) {
	stripLauncherHeaders(h)
	for k, v := range r.headers {
		h.Set(k, v)
	}
	h.Set("User-Agent", userAgent(in.Header.Get(gateway.AgentHeader)))
	if r.tags != "" && p.IsAIGateway() {
		h.Set(requestTagsHeader, r.tags)
	}
}

// userAgent is "claude-code/<version>" plus the sanitized agent name when the client sent one.
func userAgent(agent string) string {
	v := textutil.BuildVersion()
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
		if isTokenRune(c) {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// isTokenRune reports whether c is in [A-Za-z0-9._-].
func isTokenRune(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
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
		fmt.Fprintf(log, "proxy: %s ignored: %s\n", requestTagsEnv, reason)
		return ""
	}
	return raw
}
