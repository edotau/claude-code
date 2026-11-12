package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/gateway"
)

func okMsg(w http.ResponseWriter, r *http.Request, _ int) {
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/chat/completions") {
		io.WriteString(w,
			`{"id":"c","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`)
		return
	}
	io.WriteString(w, `{"type":"message","content":[]}`)
}

func headerHolds(h http.Header, needle string) bool {
	for _, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, needle) {
				return true
			}
		}
	}
	return false
}

// Every upstream hop carries our UA with the client's agent, and never the client's agent/session/UA headers.
func TestUpstreamUserAgentOnEveryRoute(t *testing.T) {
	up := newRawUp(t, okMsg)
	t.Setenv("PT_KEY", "prov-cred")
	t.Setenv("AN_KEY", "an-cred")
	t.Setenv("HARNESS_REQUEST_TAGS", "")
	srv := setup(t, `{"default":"pt","providers":{`+
		`"openai":{"pt":{"protocol":"openai","base_url":"`+up.URL+`/v1","auth":{"type":"key","env":"PT_KEY"},`+
		`"routes":{"responses":"`+up.URL+`/resp"},"models":{"opus":"m"}}},`+
		`"anthropic":{"an":`+anthropicProvider(up.URL, "AN_KEY", `{"opus":"m"}`)+`}}}`)

	cases := []struct{ name, url, body string }{
		{"anthropic", "/p/an/v1/messages", `{"model":"m","max_tokens":5}`},
		{"chat", "/p/pt/v1/messages", `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`},
		{"passthrough-chat", "/p/pt/openai/chat/completions", `{}`},
		{"passthrough-responses", "/p/pt/responses/responses", `{}`},
	}
	for _, agent := range []string{"codex", ""} {
		for _, tc := range cases {
			t.Run(tc.name+"/"+agent, func(t *testing.T) {
				before := len(up.got())
				hdr := map[string]string{"User-Agent": "client-ua/9"}
				if agent != "" {
					hdr[gateway.AgentHeader] = agent
				}
				resp := ptDo(t, http.MethodPost, srv.URL+tc.url, tc.body, hdr)
				if resp.StatusCode != 200 {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("status %d: %s", resp.StatusCode, b)
				}
				reqs := up.got()
				if len(reqs) != before+1 {
					t.Fatalf("upstream hits %d, want %d", len(reqs), before+1)
				}
				checkAttribution(t, reqs[before].hdr, agent)
			})
		}
	}
}

// Request tags reach only an ai-gateway row, and invalid HARNESS_REQUEST_TAGS turns the feature off.
func TestRequestTagsOnlyOnAIGatewayRow(t *testing.T) {
	gw := func(up string) string {
		return `{"preset":"databricks","host":"https://example.cloud.databricks.com","base_url":"` + up +
			`","auth":{"type":"key","env":"DBX_KEY"},"options":{"wire":"ai-gateway"}}`
	}
	const tagMark = "tagmark-7f3a"
	valid := `{"team":"` + tagMark + `"}`
	cases := []struct {
		name, tags string
		gateway    bool
		want       bool
	}{
		{"valid-gateway", valid, true, true},
		{"valid-anthropic-row", valid, false, false},
		{"multiline", "{\n\"team\":\"" + tagMark + "\"}", true, false},
		{"control-char", "{\"team\":\"" + tagMark + "\x01\"}", true, false},
		{"not-object", `["` + tagMark + `"]`, true, false},
		{"too-big", `{"team":"` + tagMark + strings.Repeat("a", 4097) + `"}`, true, false},
		{"unset", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newRawUp(t, okMsg)
			t.Setenv("DBX_KEY", "dbx-token")
			t.Setenv("AN_KEY", "an-cred")
			t.Setenv("HARNESS_REQUEST_TAGS", tc.tags)
			row, path, def := anthropicProvider(up.URL, "AN_KEY", `{"opus":"m"}`), "/p/an/v1/messages", "an"
			prov := `"anthropic":{"an":` + row + `}`
			if tc.gateway {
				prov, path = `"databricks":{"dbx":`+gw(up.URL)+`}`, "/p/dbx/v1/messages"
				def = "dbx"
			}
			srv := setup(t, `{"default":"`+def+`","providers":{`+prov+`}}`)
			resp := ptDo(t, http.MethodPost, srv.URL+path, `{"model":"system.ai.claude-opus-5-5","max_tokens":5}`, nil)
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status %d: %s", resp.StatusCode, b)
			}
			reqs := up.got()
			if len(reqs) != 1 {
				t.Fatalf("upstream hits = %d", len(reqs))
			}
			if got := headerHolds(reqs[0].hdr, tagMark); got != tc.want {
				t.Errorf("tags upstream = %v, want %v; headers %v", got, tc.want, reqs[0].hdr)
			}
		})
	}
}

// HARNESS_HEADERS reaches upstream; auth and attribution headers win; bad lines drop with one warning.
func TestHarnessHeadersForwarded(t *testing.T) {
	up := newRawUp(t, okMsg)
	t.Setenv("AN_KEY", "an-cred")
	t.Setenv("HARNESS_REQUEST_TAGS", "")
	t.Setenv("HARNESS_HEADERS", "X-Team: alpha\nx-team: beta\nAuthorization: Bearer evil\nUser-Agent: evil/1\n"+
		"Bad Name: x\nX-Inj: a\rX-Evil: b\n:novalue\nX-Ok:  spaced ")
	srv := setup(t,
		`{"default":"an","providers":{"anthropic":{"an":`+anthropicProvider(up.URL, "AN_KEY", `{"opus":"m"}`)+`}}}`)
	resp := ptDo(t, http.MethodPost, srv.URL+"/p/an/v1/messages", `{"model":"m","max_tokens":5}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h := up.got()[0].hdr
	if got := h.Values("X-Team"); len(got) != 1 || got[0] != "beta" {
		t.Errorf("X-Team = %q, want single beta (case-insensitive replace)", got)
	}
	if h.Get("X-Ok") != "spaced" {
		t.Errorf("X-Ok = %q", h.Get("X-Ok"))
	}
	if headerHolds(h, "evil") || h.Get("X-Evil") != "" || h.Get("X-Inj") != "" {
		t.Errorf("protected or injected header reached upstream: %v", h)
	}
	if !strings.HasPrefix(h.Get("User-Agent"), "claude-code/") {
		t.Errorf("User-Agent overridden: %q", h.Get("User-Agent"))
	}
}

func checkAttribution(t *testing.T, h http.Header, agent string) {
	t.Helper()
	checkUpstreamUserAgent(t, h.Values("User-Agent"), agent)
	for _, k := range []string{gateway.AgentHeader, gateway.ClientHeader} {
		if v := h.Get(k); v != "" {
			t.Errorf("%s reached upstream: %q", k, v)
		}
	}
	if headerHolds(h, "client-ua/9") || headerHolds(h, "sess-leak") || headerHolds(h, secret) {
		t.Errorf("client-side value leaked upstream: %v", h)
	}
}

// checkUpstreamUserAgent asserts a single claude-code/<version> User-Agent that carries agent only when one was sent.
func checkUpstreamUserAgent(t *testing.T, ua []string, agent string) {
	t.Helper()
	if len(ua) != 1 || !strings.HasPrefix(ua[0], "claude-code/") {
		t.Fatalf("User-Agent = %q, want one claude-code/<version> value", ua)
	}
	if agent != "" && !strings.HasSuffix(ua[0], " "+agent) {
		t.Errorf("User-Agent %q lacks agent %q", ua[0], agent)
	}
	if agent == "" && strings.Contains(strings.TrimSpace(ua[0]), " ") {
		t.Errorf("User-Agent %q has an agent part without the header", ua[0])
	}
}

func TestWithModelRejectsNonObject(t *testing.T) {
	for _, body := range []string{"null", "[]", "3"} {
		if _, err := withModel([]byte(body), "m"); err == nil {
			t.Errorf("withModel(%s) = nil error, want one", body)
		}
	}
}

func TestCrossProviderTarget(t *testing.T) {
	up1 := newUpstream(t, ok)
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, `{"default":"up1","providers":{"anthropic":{
		"up1":`+anthropicProvider(up1.URL, "UP1_KEY", `{"opus":"m1"}`)+`,
		"up2":{"protocol":"anthropic","base_url":"`+up2.URL+`","auth":{"type":"key","header":"bearer","env":"UP2_KEY"},"models":{"opus":"m2"}}}}}`)

	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"up2:other-model"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if up1.hits.Load() != 0 || up2.hits.Load() != 1 {
		t.Fatalf("hits up1=%d up2=%d", up1.hits.Load(), up2.hits.Load())
	}
	if m := (*up2.body.Load())["model"]; m != "other-model" {
		t.Errorf("model = %v", m)
	}
	if a := up2.last.Load().Header.Get("Authorization"); a != "Bearer k2" {
		t.Errorf("authorization = %q", a)
	}
}

func TestSessionTokenCannotReachOtherProfiles(t *testing.T) {
	up1 := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(429) })
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, failoverOverlay(up1.URL, up2.URL))
	req := testRequest(t, http.MethodPost, srv.URL+gateway.SessionPath,
		strings.NewReader(`{"profiles":["up1"],"pid":`+strconv.Itoa(os.Getpid())+`}`))
	req.Header.Set("X-Api-Key", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out gateway.SessionResponse
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.Token == "" {
		t.Fatalf("mint = %d", resp.StatusCode)
	}
	hdr := map[string]string{"X-Api-Key": out.Token}
	if r := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"up2:x"}`, hdr); r.StatusCode != http.StatusForbidden {
		t.Errorf("cross-provider model = %d, want 403", r.StatusCode)
	}
	if r := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-haiku"}`, hdr); r.StatusCode == 200 {
		t.Errorf("failover escaped to up2 under a session token")
	}
	if up2.hits.Load() != 0 {
		t.Errorf("up2 hits = %d, want 0", up2.hits.Load())
	}
}

func TestAnthropicOriginIsExact(t *testing.T) {
	prev := anthropicHost
	anthropicHost = "api.anthropic.com"
	t.Cleanup(func() { anthropicHost = prev })
	for route, want := range map[string]bool{
		"https://api.anthropic.com":                 true,
		"https://api.anthropic.com/v1":              true,
		"http://api.anthropic.com":                  false,
		"https://api.anthropic.com.evil.com":        false,
		"https://evil.com/api.anthropic.com":        false,
		"https://user@api.anthropic.com":            false,
		"https://api.anthropic.com@evil.com":        false,
		"https://xapi.anthropic.com":                false,
		"https://api.anthropic.com:8443":            false,
		"":                                          false,
		"://bad":                                    false,
		"https://API.ANTHROPIC.COM.attacker.test/v": false,
	} {
		if got := anthropicOrigin(route); got != want {
			t.Errorf("anthropicOrigin(%q) = %v, want %v", route, got, want)
		}
	}
}

func TestPassthroughForwardsClientLogin(t *testing.T) {
	sub := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n > 1 {
			w.WriteHeader(529)
			return
		}
		ok(w, r, n)
	})
	sub.Close() // the client login reaches only the https anthropicHost origin
	sub.Server = httptest.NewTLSServer(sub.Config.Handler)
	t.Cleanup(sub.Close)
	pointAnthropicHostAt(t, sub.URL)
	gem := newUpstream(t, ok)
	t.Setenv("GEM_KEY", "gem-key")
	srv := setupRouter(t, `{"default":"sub","fallback":["gem"],"providers":{"anthropic":{
		"sub":{"protocol":"anthropic","base_url":"`+sub.URL+`","auth":{"type":"cli"},"models":{"opus":"m"}},
		"gem":{"protocol":"anthropic","base_url":"`+gem.URL+`","auth":{"type":"key","header":"bearer","env":"GEM_KEY"},"models":{"opus":"g"}}}}}`,
		func(r *Router) { r.client = sub.Client() })
	login := map[string]string{"X-Api-Key": "", gateway.ClientHeader: secret, "Authorization": "Bearer oauth-tok"}

	if resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, login); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h := sub.last.Load().Header
	if h.Get("Authorization") != "Bearer oauth-tok" || h.Get(gateway.ClientHeader) != "" || h.Get("X-Api-Key") != "" {
		t.Errorf("passthrough upstream headers: %v", h)
	}
	// Failover to an API-key provider sends that provider's key, never the client's login.
	if resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, login); resp.StatusCode != 200 {
		t.Fatalf("failover status %d", resp.StatusCode)
	}
	if a := gem.last.Load().Header.Get("Authorization"); a != "Bearer gem-key" {
		t.Errorf("fallback authorization = %q", a)
	}
	if resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, map[string]string{"X-Api-Key": "", gateway.ClientHeader: "wrong"}); resp.StatusCode != 401 {
		t.Errorf("wrong gateway.ClientHeader secret: status %d", resp.StatusCode)
	}
}

// ---- B4: the sealed Max token on the subscription passthrough -------------------------------------------------

func allHeaderValues(h http.Header) string {
	var sb strings.Builder
	for k, vs := range h {
		fmt.Fprintf(&sb, "%s=%s\n", k, strings.Join(vs, ","))
	}
	return sb.String()
}

func TestPassthroughFallbackUsesVaultTokenAndNeverTheRouterSecret(t *testing.T) {
	tsrv, up := tlsUpstream(t)
	pointAnthropicHostAt(t, tsrv.URL)
	srv := setupRouter(t, subOverlay(tsrv.URL), func(r *Router) {
		r.client = tsrv.Client()
		r.oauth = func() string { return "vault-max-token" }
	})
	// The Databricks-session shape: the router secret is the ONLY client credential (X-Api-Key, no Authorization).
	resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, map[string]string{"Anthropic-Beta": "client-beta-1"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, body(t, resp))
	}
	g := up.got()
	if len(g) != 1 {
		t.Fatalf("upstream hits %d", len(g))
	}
	h := g[0].hdr
	if h.Get("Authorization") != "Bearer vault-max-token" {
		t.Fatalf("Authorization = %q, want the vault OAuth token", h.Get("Authorization"))
	}
	if beta := h.Get("Anthropic-Beta"); !strings.Contains(beta, oauthBeta) || !strings.Contains(beta, "client-beta-1") {
		t.Errorf("Anthropic-Beta = %q, want the client's beta merged with %s", beta, oauthBeta)
	}
	if dump := allHeaderValues(h); strings.Contains(dump, secret) {
		t.Fatalf("the router secret leaked upstream:\n%s", dump)
	}
	if h.Get("X-Api-Key") != "" || h.Get(gateway.ClientHeader) != "" {
		t.Errorf("client credential headers forwarded: %v", h)
	}
}

func TestPassthroughForwardsClientLoginOverVaultToken(t *testing.T) {
	tsrv, up := tlsUpstream(t)
	pointAnthropicHostAt(t, tsrv.URL)
	srv := setupRouter(t, subOverlay(tsrv.URL), func(r *Router) {
		r.client = tsrv.Client()
		r.oauth = func() string { return "vault-max-token" }
	})
	login := map[string]string{"X-Api-Key": "", gateway.ClientHeader: secret, "Authorization": "Bearer clients-own"}
	if resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, login); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h := up.got()[0].hdr
	if h.Get("Authorization") != "Bearer clients-own" || strings.Contains(allHeaderValues(h), "vault-max-token") {
		t.Fatalf("upstream headers %v, want the client's own login only", h)
	}
}

func TestPassthroughVaultTokenWithheldFromNonAnthropicOrigins(t *testing.T) {
	tsrv, tlsUp := tlsUpstream(t)
	plain := newRawUp(t, okRaw)
	cases := []struct {
		name string
		base string
		host string // anthropicHost override; "" keeps the default api.anthropic.com
		up   *rawUp
	}{
		{"http route even on the matching host", plain.URL, strings.TrimPrefix(plain.URL, "http://"), plain},
		{"https route on some other host", tsrv.URL, "", tlsUp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prev := anthropicHost
			if c.host != "" {
				anthropicHost = c.host
			}
			t.Cleanup(func() { anthropicHost = prev })
			srv := setupRouter(t, subOverlay(c.base), func(r *Router) {
				r.client = tsrv.Client()
				r.oauth = func() string { return "vault-max-token" }
			})
			if resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, nil); resp.StatusCode != 200 {
				t.Fatalf("status %d %s", resp.StatusCode, body(t, resp))
			}
			g := c.up.got()
			h := g[len(g)-1].hdr
			if h.Get("Authorization") != "" || strings.Contains(allHeaderValues(h), "vault-max-token") ||
				strings.Contains(allHeaderValues(h), secret) {
				t.Fatalf("credential sent to a non-Anthropic origin: %v", h)
			}
		})
	}
}

func TestParseHeadersWarnsOnce(t *testing.T) {
	var log strings.Builder
	got := parseHeaders("A: 1\nbad name: x\nAuthorization: y\nB: v\x00\n", &log)
	if len(got) != 1 || got["A"] != "1" {
		t.Errorf("got %v", got)
	}
	if strings.Count(log.String(), "\n") != 1 {
		t.Errorf("want one warning, got %q", log.String())
	}
}
