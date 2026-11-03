package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

// scopeKey carries the profile a session token is confined to; absent for the router secret.
type scopeKey struct{}

// sessionScope is the profile a session-token request is confined to.
func sessionScope(in *http.Request) (string, bool) {
	scope, ok := in.Context().Value(scopeKey{}).(string)
	return scope, ok
}

// resolveTarget maps the path profile and requested model to a target, writing the error when it cannot.
func resolveTarget(w http.ResponseWriter, in *http.Request, reg *providers.Registry, session, model string) (providers.Target, bool) {
	sess, err := reg.Get(session)
	if err != nil {
		writeError(w, http.StatusBadRequest, "proxy: "+err.Error())
		return providers.Target{}, false
	}
	target := providers.ParseTarget(reg, model, sess)
	return target, scopeAllows(w, in, target)
}

// scopeAllows rejects with 403 a resolved target outside a session token's profile.
func scopeAllows(w http.ResponseWriter, in *http.Request, t providers.Target) bool {
	scope, scoped := sessionScope(in)
	if scoped && t.Provider.Name != scope {
		writeError(w, http.StatusForbidden, "proxy: session token is limited to profile "+scope)
		return false
	}
	return true
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

// parsePath splits /p/<provider>/v1/messages[/count_tokens].
func parsePath(path string) (provider string, count, ok bool) {
	rest, ok := strings.CutPrefix(path, "/p/")
	if !ok {
		return "", false, false
	}
	provider, rest, _ = strings.Cut(rest, "/")
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
	var cur string
	if json.Unmarshal(m["model"], &cur) == nil && cur == model {
		return body, nil
	}
	m["model"], _ = json.Marshal(model)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
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
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
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
