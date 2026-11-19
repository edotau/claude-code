package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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
					hdr[proxy.AgentHeader] = agent
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

// HARNESS_HEADERS reaches upstream; auth and attribution headers win; bad lines drop with one warning.
func TestHarnessHeadersForwarded(t *testing.T) {
	up := newRawUp(t, okMsg)
	t.Setenv("AN_KEY", "an-cred")
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
	for _, k := range []string{proxy.AgentHeader, proxy.ClientHeader} {
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

func TestDropDatabricksBetas(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"advanced-tool-use-2025-11-20", ""},
		{"context-1m-2025-08-07,advanced-tool-use-2025-11-20", "context-1m-2025-08-07"},
		{"context-1m-2025-08-07", "context-1m-2025-08-07"},
	} {
		h := http.Header{"Anthropic-Beta": {c.in}}
		dropDatabricksBetas(&providers.Provider{Preset: providers.PresetDatabricks}, h)
		if got := h.Get("Anthropic-Beta"); got != c.want {
			t.Errorf("%q -> %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWithModelRejectsNonObject(t *testing.T) {
	for _, body := range []string{"null", "[]", "3"} {
		if _, err := withModel([]byte(body), "m"); err == nil {
			t.Errorf("withModel(%s) = nil error, want one", body)
		}
	}
}

func TestPassthroughBodyByteIdentity(t *testing.T) {
	up := newRawUp(t, okMsg)
	t.Setenv("PT_KEY", "test-key")
	srv := setup(t, `{"default":"pt","providers":{"openai":{"pt":{"protocol":"openai","base_url":"`+
		up.URL+`/v1","auth":{"type":"key","env":"PT_KEY"},"models":{"opus":"m"}}}}}`)

	body := " \n" + `{ "z" : "\u0061", "model" : "m", "n" : 900719925474099312345, "z" : "\u263a" }` + "\r\n"
	resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/openai/chat/completions", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	got := up.got()
	if len(got) != 1 || got[0].body != body {
		t.Fatalf("upstream request did not preserve the original JSON bytes")
	}
}

func TestAnthropicRouteDoesNotRescanModel(t *testing.T) {
	up := newRawUp(t, okMsg)
	t.Setenv("AN_KEY", "test-key")
	srv := setup(t, `{"default":"an","providers":{"anthropic":{"an":`+
		anthropicProvider(up.URL, "AN_KEY", `{"opus":"m"}`)+`}}}`)
	decode := passthroughModel
	var rescans atomic.Int32
	passthroughModel = func(body []byte) string {
		rescans.Add(1)
		return decode(body)
	}
	t.Cleanup(func() { passthroughModel = decode })

	resp := post(t, srv.URL+"/p/an/v1/messages", `{"model":"m[1m]"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := rescans.Load(); got != 0 {
		t.Errorf("extra body decodes = %d, want 0", got)
	}
	if got := up.got(); len(got) != 1 || got[0].body != `{"model":"m"}` {
		t.Error("upstream request did not use the stripped model")
	}
}

func BenchmarkRouteBody(b *testing.B) {
	vaulttest.Use(b)
	b.Setenv("AN_KEY", "test-key")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		okMsg(w, req, 0)
	}))
	b.Cleanup(up.Close)
	vaulttest.Put(b, vault.Overlay, "providers",
		`{"default":"an","providers":{"anthropic":{"an":`+anthropicProvider(up.URL, "AN_KEY", `{"opus":"m"}`)+`}}}`)
	r := newRouter(secret)
	r.log = io.Discard
	b.Cleanup(r.bg.Wait)

	for _, size := range []int{256, 1 << 20, 8 << 20, 24 << 20} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			const start, end = `{"model":"m","messages":[{"role":"user","content":"`, `"}]}`
			body := start + strings.Repeat("x", size-len(start)-len(end)) + end
			b.SetBytes(int64(size))
			for b.Loop() {
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
				rec := httptest.NewRecorder()
				w := &statusWriter{ResponseWriter: rec, rc: http.NewResponseController(rec)}
				r.route(w, req, "an", false)
				if w.status != http.StatusOK {
					b.Fatalf("status %d, want 200", w.status)
				}
			}
		})
	}
}

func routeStampFile() string { return filepath.Join(paths.StateDir(), "router", "route-auto.json") }

func seedDatabricksLoad(t testing.TB) {
	t.Helper()
	for i, name := range []string{"qa", "prod", "up1"} {
		account := router.AccountOf(&providers.Provider{Name: name, Preset: providers.PresetDatabricks})
		want := 3 - i
		ok, _ := inflight.TryClaim(inflight.Claim{Owner: os.Getpid(), ID: name, Account: account, N: want},
			inflight.Limits{Total: 20, Override: map[string]int{account: want}})
		if got := inflight.Count().Per[account]; !ok || got != want {
			t.Fatalf("in-flight count for %s = %d, want %d", name, got, want)
		}
	}
}

func BenchmarkDatabricksFailoverRank(b *testing.B) {
	vaulttest.Use(b)
	seedDatabricksLoad(b)
	now := time.Now()
	picks := map[string]any{"schema": 1, "picked": map[string]time.Time{"qa": now}}
	if err := paths.WriteJSON(routeStampFile(), picks); err != nil {
		b.Fatal(err)
	}
	var rows []providers.Target
	for _, name := range []string{"dev", "qa", "prod", "up1"} {
		p := &providers.Provider{Name: name, Preset: providers.PresetDatabricks}
		rows = append(rows, providers.Target{Provider: p, Model: "databricks-claude-opus-5"})
	}
	ladder := attemptLadder{primary: rows[0], rows: slices.Clone(rows), coolingFrom: len(rows)}
	for b.Loop() {
		copy(ladder.rows, rows)
		ladder.rankSiblings(1, now)
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

// A family name a client sends is resolved on the row the request routes to, never forwarded as typed.
func TestProxyResolvesFamilyOnItsRow(t *testing.T) {
	up := newUpstream(t, ok)
	t.Setenv("UP_KEY", "k")
	srv := setup(t, `{"default":"up","providers":{"anthropic":{"up":`+anthropicProvider(up.URL, "UP_KEY", `{"opus":"m1"}`)+`}}}`)
	if resp := post(t, srv.URL+"/p/up/v1/messages", `{"model":"sol"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	m, _ := (*up.body.Load())["model"].(string)
	if f, _, ok := providers.FamilyVersion(m); !ok || f != "sol" {
		t.Errorf("upstream model = %q, want a concrete sol id", m)
	}
}

func TestSessionTokenCannotReachOtherProfiles(t *testing.T) {
	up1 := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(429) })
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, failoverOverlay(up1.URL, up2.URL))
	req := testRequest(t, http.MethodPost, srv.URL+proxy.SessionPath,
		strings.NewReader(`{"profiles":["up1"],"pid":`+strconv.Itoa(os.Getpid())+`}`))
	req.Header.Set("X-Api-Key", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out proxy.SessionResponse
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
	login := map[string]string{"X-Api-Key": "", proxy.ClientHeader: secret, "Authorization": "Bearer oauth-tok"}

	if resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, login); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h := sub.last.Load().Header
	if h.Get("Authorization") != "Bearer oauth-tok" || h.Get(proxy.ClientHeader) != "" || h.Get("X-Api-Key") != "" {
		t.Errorf("passthrough upstream headers: %v", h)
	}
	// Failover to an API-key provider sends that provider's key, never the client's login.
	if resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, login); resp.StatusCode != 200 {
		t.Fatalf("failover status %d", resp.StatusCode)
	}
	if a := gem.last.Load().Header.Get("Authorization"); a != "Bearer gem-key" {
		t.Errorf("fallback authorization = %q", a)
	}
	if resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, map[string]string{"X-Api-Key": "", proxy.ClientHeader: "wrong"}); resp.StatusCode != 401 {
		t.Errorf("wrong proxy.ClientHeader secret: status %d", resp.StatusCode)
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
	if h.Get("X-Api-Key") != "" || h.Get(proxy.ClientHeader) != "" {
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
	login := map[string]string{"X-Api-Key": "", proxy.ClientHeader: secret, "Authorization": "Bearer clients-own"}
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

func TestFableToOpus(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", clientTokenOverlay("http://127.0.0.1:1"))
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	dev, sub := reg.Row("dev"), reg.Row("sub")
	for _, c := range []struct {
		name, model string
		p           *providers.Provider
		want        string
	}{
		{"databricks fable maps to opus", "claude-fable-5-2", dev, "databricks-claude-opus-5-5"},
		{"databricks opus untouched", "databricks-claude-opus-5-5", dev, "databricks-claude-opus-5-5"},
		{"other route fable untouched", "claude-fable-5-2", sub, "claude-fable-5-2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := fableToOpus(reg, providers.Target{Provider: c.p, Model: c.model})
			if got.Provider != c.p || got.Model != c.want {
				t.Errorf("fableToOpus(%s on %s) = %s on %s, want %s", c.model, c.p.Name, got.Model, got.Provider.Name, c.want)
			}
		})
	}
}

func BenchmarkWithModel(b *testing.B) {
	b.SetBytes(int64(len(benchChatBody)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := withModel(benchChatBody, "wire-model"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUserAgent(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		userAgent("opencode-agent/1.2")
	}
}
