package server

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/textutil"
)

// scopeKey carries the profiles a session or client token is confined to; absent for the router secret.
type scopeKey struct{}

// sessionScope is the profile set a session- or client-token request is confined to.
func sessionScope(in *http.Request) ([]string, bool) {
	scope, ok := in.Context().Value(scopeKey{}).([]string)
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
	target = fableToOpus(reg, target)
	// a resolved target outside a token's profiles is a 403
	if scope, scoped := sessionScope(in); scoped && !slices.Contains(scope, target.Provider.Name) {
		writeError(w, http.StatusForbidden, "proxy: this token is limited to profiles "+strings.Join(scope, ", "))
		return target, false
	}
	return target, true
}

// fableToOpus maps a Fable target on a Databricks serving route to the session's opus slot; Databricks serves no Fable.
func fableToOpus(reg *providers.Registry, t providers.Target) providers.Target {
	if t.Provider.SpellingRule() != "databricks-serving" {
		return t
	}
	if canonical, _, ok := models.Resolve(t.Model); !ok || models.FamilyOf(canonical) != models.Fable {
		return t
	}
	if sel, err := providers.Select(reg, t.Provider.Name); err == nil && sel.Aliases[models.Opus].Provider == t.Provider {
		return sel.Aliases[models.Opus]
	}
	return t
}

// confineLadder drops fallback providers outside a token's profiles; they hold other credentials.
func confineLadder(in *http.Request, ladder []providers.Target) []providers.Target {
	scope, scoped := sessionScope(in)
	if !scoped {
		return ladder
	}
	return slices.DeleteFunc(ladder, func(t providers.Target) bool { return !slices.Contains(scope, t.Provider.Name) })
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

type proxyBody struct {
	raw   []byte
	top   map[string]json.RawMessage
	model string
	lazy  *pendingMarshal // set: stages only mutate top and finish marshals once
}

// pendingMarshal records which stage kinds changed top: the last marshaler's escaping decides the bytes.
type pendingMarshal struct{ dirty, model bool }

// marshalTop marshals top for a stage, or defers to finish when the body is lazy.
func (b proxyBody) marshalTop() []byte {
	if b.lazy != nil {
		b.lazy.dirty = true
		return b.raw
	}
	return marshalOr(b.top, b.raw)
}

// finish marshals a lazy body once, with the encoder the last firing stage used; orig is the pre-stage body.
func (m *pendingMarshal) finish(b proxyBody, orig []byte) ([]byte, error) {
	if !m.dirty && !m.model {
		return orig, nil
	}
	if m.dirty {
		if out, err := json.Marshal(b.top); err == nil {
			return out, nil
		}
		if !m.model {
			return orig, nil
		}
	}
	return textutil.MarshalNoEscape(b.top)
}

func decodeProxyBody(raw []byte) proxyBody {
	d := jsontext.NewDecoder(bytes.NewBuffer(raw), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if d.PeekKind() != '{' {
		return proxyBody{raw: raw}
	}
	if _, err := d.ReadToken(); err != nil {
		return proxyBody{raw: raw}
	}
	b := proxyBody{raw: raw, top: make(map[string]json.RawMessage)}
	for d.PeekKind() == '"' {
		if !b.readField(d) {
			return proxyBody{raw: raw}
		}
	}
	if end, err := d.ReadToken(); err != nil || end.Kind() != '}' {
		return proxyBody{raw: raw}
	}
	if _, err := d.ReadToken(); err != io.EOF {
		return proxyBody{raw: raw}
	}
	return b
}

func (b *proxyBody) readField(d *jsontext.Decoder) bool {
	key, err := d.ReadToken()
	if err != nil {
		return false
	}
	name := key.String()
	value, err := d.ReadValue()
	if err != nil {
		return false
	}
	end := d.InputOffset()
	b.top[name] = b.raw[end-int64(len(value)) : end]
	// Routing uses the last string across case variants, while rewrites use the exact key.
	if strings.EqualFold(name, "model") && value.Kind() == '"' {
		_ = json.Unmarshal(value, &b.model)
	}
	return true
}

func withModel(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m == nil { // a literal null body unmarshals without error
		return nil, errors.New("request body is not a JSON object")
	}
	out, err := (proxyBody{raw: body, top: m}).withModel(model)
	return out.raw, err
}

func (b proxyBody) withModel(model string) (proxyBody, error) {
	var cur string
	if json.Unmarshal(b.top["model"], &cur) == nil && cur == model {
		return b, nil
	}
	top := b.top
	if b.lazy == nil {
		top = maps.Clone(top)
	}
	top["model"], _ = json.Marshal(model)
	if b.lazy != nil {
		b.top, b.model, b.lazy.model = top, model, true
		return b, nil
	}
	raw, err := textutil.MarshalNoEscape(top)
	b.top, b.raw, b.model = top, raw, model
	return b, err
}

func (b proxyBody) stripWireMarker() ([]byte, error) {
	if !models.Has1M(b.model) {
		return b.raw, nil
	}
	model := models.Strip1M(b.model)
	if b.top == nil {
		return withModel(b.raw, model)
	}
	out, err := b.withModel(model)
	return out.raw, err
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

const maxAgentToken = 64

// attribute stamps the claude-code User-Agent and drops launcher headers.
func (r *Router) attribute(h http.Header, in *http.Request) {
	stripLauncherHeaders(h)
	for k, v := range r.headers {
		h.Set(k, v)
	}
	h.Set("User-Agent", userAgent(in.Header.Get(proxy.AgentHeader)))
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

// headersEnv holds extra upstream request headers, one "Name: value" per line.
const headersEnv = "HARNESS_HEADERS"

const maxHeadersBytes = 4 << 10

// launcherHeaders are the proxy's own request headers; they never go upstream.
var launcherHeaders = []string{
	proxy.AgentHeader, proxy.IdentityHeader, "X-Claude-Code-Session-Id", proxy.ClientHeader,
}

// protectedHeaders can never be set from HARNESS_HEADERS: credentials, attribution, and framing.
var protectedHeaders = map[string]bool{
	"Authorization": true, "Proxy-Authorization": true, "X-Api-Key": true, "X-Goog-Api-Key": true, "Host": true,
	"User-Agent": true, "Content-Length": true, "Content-Type": true, "Transfer-Encoding": true, "Connection": true,
	"Cookie": true,
}

// parseHeaders parses "Name: value" lines; invalid or protected lines are dropped with one warning, never fatal.
func parseHeaders(raw string, log io.Writer) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	if len(raw) > maxHeadersBytes {
		fmt.Fprintf(log, "proxy: %s ignored: over 4 KiB\n", headersEnv)
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
		fmt.Fprintf(log, "proxy: %s: dropped %d invalid or protected line(s)\n", headersEnv, bad)
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

// stripLauncherHeaders removes client-only attribution and authentication headers.
func stripLauncherHeaders(h http.Header) {
	for _, k := range launcherHeaders {
		h.Del(k)
	}
}
