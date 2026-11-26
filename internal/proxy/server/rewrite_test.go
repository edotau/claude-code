package server

import (
	"encoding/json"
	"errors"
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

	"github.com/edotau/claude-code/internal/driver/googletools"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/routing/inflight"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/router"
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

	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"up2:other-model"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: a prefixed id is never cast to another profile", resp.StatusCode)
	}
	if up1.hits.Load() != 0 || up2.hits.Load() != 0 {
		t.Fatalf("hits up1=%d up2=%d, want none", up1.hits.Load(), up2.hits.Load())
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
	if r := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"up2:x"}`, hdr); r.StatusCode != http.StatusBadRequest {
		t.Errorf("out-of-scope prefixed model = %d, want 400", r.StatusCode)
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

func TestFableRefusal(t *testing.T) {
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
		{"databricks fable refused naming opus", "claude-fable-5-2", dev, "databricks-claude-opus-5-5"},
		{"databricks opus untouched", "databricks-claude-opus-5-5", dev, ""},
		{"other route fable untouched", "claude-fable-5-2", sub, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := fableRefusal(reg, providers.Target{Provider: c.p, Model: c.model})
			if (err == nil) != (c.want == "") || (err != nil && !strings.Contains(err.Error(), c.want)) {
				t.Errorf("fableRefusal(%s on %s) = %v, want mention of %q", c.model, c.p.Name, err, c.want)
			}
		})
	}
}

func TestFableRefusalHTTP(t *testing.T) {
	up := newRawUp(t, okRaw)
	t.Setenv("DEV_TOKEN", "dev-workspace-token")
	srv := setupRouter(t, clientTokenOverlay(up.URL), nil)
	resp := post(t, srv.URL+"/p/dev/v1/messages", `{"model":"claude-fable-5-2","max_tokens":5}`, nil)
	b := body(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, `"type":"invalid_request_error"`) ||
		!strings.Contains(b, "databricks-claude-opus-5-5") || len(up.got()) != 0 {
		t.Fatalf("status %d body %s upstream hits %d", resp.StatusCode, b, len(up.got()))
	}
}

// A profile Select cannot resolve, or one with no Opus slot, still gets a refusal naming an Opus id.
func TestFableRefusalNamesOpusWhenSelectFails(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", clientTokenOverlay("http://127.0.0.1:1"))
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	ghost := *reg.Row("dev")
	ghost.Name = "ghost"
	bare := ghost
	bare.Models = nil
	for name, p := range map[string]*providers.Provider{"not in registry": &ghost, "no opus slot": &bare} {
		err := fableRefusal(reg, providers.Target{Provider: p, Model: "claude-fable-5-2"})
		if err == nil || !strings.Contains(err.Error(), "its Opus id is databricks-claude-opus-5-5") {
			t.Errorf("%s: fableRefusal = %v, want it to name databricks-claude-opus-5-5", name, err)
		}
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

// A plain id and a local tag stay on the session profile; any profile, type or pool prefix is refused.
func TestClientTargetNamesRequestPath(t *testing.T) {
	vaulttest.Use(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	name := reg.Names()[0]
	_, _, err = clientTarget(reg, reg.Providers[name], name+":opus", "openai/v1/chat/completions")
	if err == nil || !strings.Contains(err.Error(), "/p/"+name+"/openai/v1/chat/completions") {
		t.Errorf("err = %v; want the request's own path", err)
	}
}

func TestClientTarget(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"an","providers":{"anthropic":{`+
		`"an":`+anthropicProvider("http://127.0.0.1:1", "AN_KEY", `{"opus":"m"}`)+`,`+
		`"other":`+anthropicProvider("http://127.0.0.1:2", "AN_KEY", `{"opus":"o"}`)+`}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sess := reg.Providers["an"]
	for _, model := range []string{"m", "qwen3:30b"} {
		if got, plain, err := clientTarget(reg, sess, model, "v1/messages"); err != nil || got.Provider != sess || got.Model != model || plain != model {
			t.Errorf("clientTarget(%q) = %+v %q, %v; want it on an as typed", model, got, plain, err)
		}
	}
	for model, want := range map[string]string{"other:x": "/p/other/v1/messages", "An:x": "/p/an/", "auto:opus": "/p/auto/", "typo:opus": "/p/<profile>/"} {
		if _, _, err := clientTarget(reg, sess, model, "v1/messages"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("clientTarget(%q) = %v; want a refusal naming %s", model, err, want)
		}
	}
}

func TestBuiltinSonnetSwap(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", clientTokenOverlay("http://127.0.0.1:1"))
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	dev, sub := reg.Row("dev"), reg.Row("sub")
	r := &Router{log: io.Discard}
	for _, c := range []struct {
		name, model string
		p           *providers.Provider
		swap        bool
	}{
		{"built-in marked id swaps", "databricks-claude-sonnet-5[1m]", dev, true},
		{"typed unmarked id stays", "databricks-claude-sonnet-5", dev, false},
		{"current sonnet stays", "databricks-claude-sonnet-5-5[1m]", dev, false},
		{"other route stays", "claude-sonnet-5[1m]", sub, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tgt := providers.PlainTarget(c.p, models.Strip1M(c.model))
			got, ok := r.builtinSonnetSwap(reg, tgt, c.model)
			if ok != c.swap || (ok && !strings.Contains(got.Model, "sonnet-5-5")) {
				t.Errorf("swap(%s on %s) = %v %s, want swap=%v", c.model, c.p.Name, ok, got.Model, c.swap)
			}
		})
	}
}

// The ids Claude Code sends by itself reach their Databricks endpoint, with no slot env needed.
func TestClaudeCodeOwnIDsReachDatabricksEndpoints(t *testing.T) {
	up := newRawUp(t, okRaw)
	t.Setenv("DEV_TOKEN", "dev-workspace-token")
	srv := setupRouter(t, clientTokenOverlay(up.URL), nil)
	for _, c := range []struct{ sent, want string }{
		{"claude-haiku-5-5", "databricks-claude-haiku-5-5"}, {"claude-haiku-5-5-20261001", "databricks-claude-haiku-5-5"},
		{"claude-sonnet-5-5", "databricks-claude-sonnet-5-5"}, {"claude-sonnet-5-5[1m]", "databricks-claude-sonnet-5-5"},
		{"claude-opus-5-5", "databricks-claude-opus-5-5"}, {"claude-opus-5-5[1m]", "databricks-claude-opus-5-5"},
		{"opus", "databricks-claude-opus-5-5"}, {"sonnet", "databricks-claude-sonnet-5-5"}, {"haiku", "databricks-claude-haiku-5-5"},
	} {
		before := len(up.got())
		resp := post(t, srv.URL+"/p/dev/v1/messages", `{"model":"`+c.sent+`","max_tokens":5}`, nil)
		body(t, resp)
		got := up.got()
		if resp.StatusCode != http.StatusOK || len(got) != before+1 || !strings.Contains(got[before].body, `"model":"`+c.want+`"`) {
			t.Errorf("%s: status %d, upstream %d requests, want model %s (last body %v)", c.sent, resp.StatusCode, len(got)-before, c.want, got)
		}
	}
}

func googleToolsSetup(t *testing.T, options string) (*rawUp, string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newRawUp(t, okRaw)
	t.Setenv("DBX_KEY", "dbx-token")
	row := `{"preset":"databricks","host":"https://example.cloud.databricks.com","base_url":"` + up.URL +
		`/serving-endpoints/anthropic","routes":{"gemini":"` + up.URL + `/serving-endpoints/gemini"},` + options +
		`"auth":{"type":"key","env":"DBX_KEY"}}`
	srv := setup(t, `{"default":"dbx","providers":{"databricks":{"dbx":`+row+`}}}`)
	return up, srv.URL + "/p/dbx/gemini/v1beta/models/gemini-3.8-flash:generateContent"
}

// The header is stripped, served tools are injected once, an unserved tool is a 400, an unprobed row sends as asked.
func TestWithGoogleTools(t *testing.T) {
	if http.CanonicalHeaderKey(googletools.Header) != googletools.Header {
		t.Fatalf("%s is not canonical", googletools.Header)
	}
	served := `"options":{"google_tools":["url","code"]},`
	t.Run("injects", func(t *testing.T) {
		up, url := googleToolsSetup(t, served)
		hdr := map[string]string{googletools.Header: "url,code"}
		resp := ptDo(t, http.MethodPost, url, `{"contents":[],"tools":[{"urlContext":{}}]}`, hdr)
		g := up.got()
		if resp.StatusCode != 200 || len(g) != 1 || g[0].hdr.Get(googletools.Header) != "" {
			t.Fatalf("status %d hits %+v, want 200 and one hit with the header stripped", resp.StatusCode, g)
		}
		if want := `"tools":[{"urlContext":{}},{"codeExecution":{}}]`; !strings.Contains(g[0].body, want) {
			t.Errorf("body = %s, want %s", g[0].body, want)
		}
	})
	t.Run("refusals", func(t *testing.T) { googleToolsRefusals(t, served) })
	t.Run("repeated header lines", func(t *testing.T) {
		up, url := googleToolsSetup(t, served)
		req := testRequest(t, http.MethodPost, url, strings.NewReader(`{"contents":[]}`))
		req.Header.Set("X-Api-Key", secret)
		req.Header.Add(googletools.Header, "url")
		req.Header.Add(googletools.Header, "code")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		g := up.got()
		if resp.StatusCode != 200 || len(g) != 1 || !strings.Contains(g[0].body, `{"urlContext":{}},{"codeExecution":{}}`) {
			t.Errorf("status %d hits %+v, want both tools injected", resp.StatusCode, g)
		}
	})
	t.Run("failover skips an unserving sibling", googleToolsFailover)
}

func googleToolsFailover(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	first := newRawUp(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(429) })
	second, third := newRawUp(t, okRaw), newRawUp(t, okRaw)
	for _, k := range []string{"GT_FIRST", "GT_SECOND", "GT_THIRD"} {
		t.Setenv(k, "fake-"+k)
	}
	row := func(up *rawUp, key, tools string) string {
		return `{"preset":"databricks","host":"https://workspace.example.test","base_url":"` + up.URL +
			`/serving-endpoints/anthropic","routes":{"gemini":"` + up.URL + `/serving-endpoints/gemini"},` +
			`"options":{"google_tools":` + tools + `},"auth":{"type":"key","env":"` + key + `"},"models":{"opus":"databricks-test-model"}}`
	}
	srv := setup(t, `{"default":"first","fallback":["second","third"],"providers":{"databricks":{"first":`+
		row(first, "GT_FIRST", `["code"]`)+`,"second":`+row(second, "GT_SECOND", `["url"]`)+`,"third":`+
		row(third, "GT_THIRD", `["url","code"]`)+`}}}`)
	hdr := map[string]string{googletools.Header: "code"}
	resp := ptDo(t, http.MethodPost, srv.URL+"/p/first/gemini/v1beta/models/databricks-test-model:generateContent",
		`{"contents":[]}`, hdr)
	if resp.StatusCode != 200 || len(second.got()) != 0 || len(third.got()) != 1 {
		t.Errorf("status %d hits second=%d third=%d, want 200 via third only", resp.StatusCode,
			len(second.got()), len(third.got()))
	}
	for _, up := range []*rawUp{first, third} {
		for _, hit := range up.got() {
			if strings.Count(hit.body, `"codeExecution"`) != 1 || hit.hdr.Get(googletools.Header) != "" {
				t.Errorf("hop lost or duplicated tools or forwarded their header: %+v", hit)
			}
		}
	}
}

func googleToolsRefusals(t *testing.T, served string) {
	hdr := map[string]string{googletools.Header: "search"}
	t.Run("unserved", func(t *testing.T) {
		up, url := googleToolsSetup(t, served)
		if resp := ptDo(t, http.MethodPost, url, `{"contents":[]}`, hdr); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status %d, want 400", resp.StatusCode)
		}
		if len(up.got()) != 0 {
			t.Error("an unserved tool reached the upstream")
		}
	})
	t.Run("unprobed", func(t *testing.T) {
		up, url := googleToolsSetup(t, "")
		resp := ptDo(t, http.MethodPost, url, `{"contents":[]}`, hdr)
		g := up.got()
		if resp.StatusCode != 200 || len(g) != 1 || !strings.Contains(g[0].body, `"tools":[{"googleSearch":{}}]`) {
			t.Errorf("status %d hits %+v", resp.StatusCode, g)
		}
	})
}

func TestWithGoogleToolsRefusesUnparsableBody(t *testing.T) {
	for _, body := range []string{`[1]`, `not json`, `{"tools":"x"}`, `{"tools":{"a":1}}`} {
		if _, err := decodeProxyBody([]byte(body)).withGoogleTools("url", nil); !errors.Is(err, errGoogleToolsBody) {
			t.Errorf("body %s: err = %v, want errGoogleToolsBody", body, err)
		}
	}
	if _, err := decodeProxyBody([]byte(`{"tools":[]}`)).withGoogleTools("url", nil); err != nil {
		t.Errorf("valid body refused: %v", err)
	}
}

func TestNativeGeminiGoogleTools(t *testing.T) {
	for _, tc := range []struct {
		name, options, header string
		status, tools         int
	}{
		{"unprobed", "", "url,code", 200, 1},
		{"served", `"options":{"google_tools":["url","code"]},`, "url,code", 200, 1},
		{"unserved", `"options":{"google_tools":["url"]},`, "code", 400, 0},
		{"title model", "", "code,model=other-model", 200, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newRawUp(t, okRaw)
			t.Setenv("PT_KEY", "provider-credential")
			overlay := strings.Replace(ptOverlay(up.URL), `"auth":`, tc.options+`"auth":`, 1)
			srv := setup(t, overlay)
			resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/gemini/v1beta/models/gemini-3.8-flash:generateContent",
				`{"contents":[],"tools":[{"urlContext":{}},{"functionDeclarations":[{"name":"keep"}]}]}`,
				map[string]string{googletools.Header: tc.header})
			if resp.StatusCode != tc.status {
				t.Fatalf("status=%d, want %d", resp.StatusCode, tc.status)
			}
			for _, hit := range up.got() {
				if strings.Count(hit.body, `"codeExecution"`) != tc.tools || strings.Count(hit.body, `"urlContext"`) != 1 ||
					!strings.Contains(hit.body, `"name":"keep"`) || hit.hdr.Get(googletools.Header) != "" {
					t.Errorf("tools lost, duplicated, or header leaked: %+v", hit)
				}
			}
			if tc.status == 400 && len(up.got()) != 0 {
				t.Fatal("unserved tools reached upstream")
			}
		})
	}
}

func TestGoogleToolsRefusesMalformedAndOversizedRequests(t *testing.T) {
	up, url := googleToolsSetup(t, "")
	for _, bad := range []string{`null`, `[1]`, `not json`, `{"tools":"x"}`, `{} {}`} {
		resp := ptDo(t, http.MethodPost, url, bad, map[string]string{googletools.Header: "code"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status=%d, want 400", bad, resp.StatusCode)
		}
	}
	req := testRequest(t, http.MethodPost, url, io.LimitReader(zeroReader{}, maxBody+1))
	req.Header.Set("X-Api-Key", secret)
	req.Header.Set(googletools.Header, "code")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge || len(up.got()) != 0 {
		t.Fatalf("oversized status=%d upstream hits=%d, want 413 with no upstream calls", resp.StatusCode, len(up.got()))
	}
}

func TestEffortRetryRefusesNullBody(t *testing.T) {
	up := newRawUp(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "function tools with reasoning_effort")
	})
	t.Setenv("PT_KEY", "provider-credential")
	srv := setup(t, ptOverlay(up.URL))
	resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/openai/chat/completions", `null`, nil)
	if resp.StatusCode != http.StatusBadRequest || len(up.got()) != 1 {
		t.Fatalf("status=%d hits=%d, want the original 400 without retry", resp.StatusCode, len(up.got()))
	}
}

func TestProxyHeaderFilteringAcrossFallbacks(t *testing.T) {
	srv, tok, _, qa, _ := setupAutoFailoverRig(t)
	for _, path := range []string{"v1/messages", "openai/chat/completions"} {
		resp := ptDo(t, http.MethodPost, srv.URL+"/p/dev/"+path, `{"model":"databricks-claude-opus-5-5"}`,
			map[string]string{proxy.ClientHeader: tok, "Authorization": "Bearer inbound-credential",
				"X-Goog-Api-Key": "inbound-google-key", "Accept-Encoding": "inbound-encoding", googletools.Header: "code"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status=%d", path, resp.StatusCode)
		}
	}
	for _, hit := range qa.got() {
		if hit.hdr.Get("Authorization") != "Bearer qa-tok" {
			t.Error("fallback did not use its own credential")
		}
		for _, key := range []string{googletools.Header, proxy.ClientHeader, "X-Api-Key", "X-Goog-Api-Key"} {
			if hit.hdr.Get(key) != "" {
				t.Errorf("%s forwarded on fallback", key)
			}
		}
		if hit.hdr.Get("Accept-Encoding") == "inbound-encoding" {
			t.Error("client transport header forwarded on fallback")
		}
	}
}

func TestCopyClientHeadersStripsCredentialVariants(t *testing.T) {
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Host", "Accept-Encoding", googletools.Header} {
		for _, key := range []string{name, strings.ToLower(name)} {
			in := httptest.NewRequest(http.MethodPost, "/", nil)
			in.Header[key] = []string{"inbound-only"}
			in.Header.Set("Content-Type", "application/json")
			for _, copyHeaders := range []func(http.Header){copyClientHeaders(in), func(dst http.Header) {
				copyRequestHeaders(dst, in.Header)
			}} {
				out := http.Header{}
				copyHeaders(out)
				if out.Get(name) != "" || out.Get("Content-Type") != "application/json" {
					t.Errorf("copying %s leaked a blocked header or dropped Content-Type", key)
				}
			}
		}
	}
}
