package router

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// fakeGate is an in-memory route store behind router.Gate; the real quota.json is never touched.
type fakeGate struct {
	mu      sync.Mutex
	marks   map[string]string
	cleared []string
}

func useGate(t *testing.T) *fakeGate {
	t.Helper()
	g := &fakeGate{marks: map[string]string{}}
	prev := Gate
	Gate = AuthGate{
		Account: func(p string) string { return "provider:" + p },
		Mark: func(a, reason string) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.marks[a] = reason
			return nil
		},
		Clear: func(a string) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			delete(g.marks, a)
			g.cleared = append(g.cleared, a)
			return nil
		},
		Marked: func() func(string, time.Time) (string, bool) {
			g.mu.Lock()
			snap := map[string]string{}
			for k, v := range g.marks {
				snap[k] = v
			}
			g.mu.Unlock()
			return func(a string, _ time.Time) (string, bool) { r, ok := snap[a]; return r, ok }
		},
	}
	t.Cleanup(func() { Gate = prev })
	return g
}

func (g *fakeGate) marked(provider string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.marks["provider:"+provider]
	return r, ok
}

func (g *fakeGate) premark(provider string) {
	g.mu.Lock()
	g.marks["provider:"+provider] = "auth rejected"
	g.mu.Unlock()
}

// setupRouter is setup with a keyring-backed vault (sealed secrets work) and a hook to tweak the router.
func setupRouter(t *testing.T, overlay string, tweak func(*Router)) *httptest.Server {
	t.Helper()
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	r := newRouter(secret)
	r.log = io.Discard
	r.oauth = func() string { return "" }
	if tweak != nil {
		tweak(r)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// u2mRow is a sealed-auth row with no sealed refresh token: minting yields providers.ErrNeedsLogin.
func u2mRow(base string) string {
	return fmt.Sprintf(`{"kind":"anthropic","base_url":%q,"auth":{"type":"bearer","authType":"u2m"},"models":{"opus":"m1-opus","haiku":"m1-haiku"}}`, base)
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func TestCondemnedRefreshMovesOnToNextTarget(t *testing.T) {
	g := useGate(t)
	up1 := newUpstream(t, ok)
	up2 := newUpstream(t, ok)
	t.Setenv("UP2_KEY", "k2")
	srv := setupRouter(t, `{"default":"up1","fallback":["up2"],"providers":{"up1":`+u2mRow(up1.URL)+`,"up2":`+
		anthropicProvider(up2.URL, "UP2_KEY", `{"opus":"m2-opus","haiku":"m2-haiku"}`)+`}}`, nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-haiku"}`, nil)
	if resp.StatusCode != 200 || up1.hits.Load() != 0 || up2.hits.Load() != 1 {
		t.Fatalf("status %d hits up1=%d up2=%d, want 200 served by up2 only", resp.StatusCode, up1.hits.Load(), up2.hits.Load())
	}
	if reason, ok := g.marked("up1"); !ok || !strings.Contains(reason, "needs_login") || !strings.Contains(reason, "databricks login up1") {
		t.Fatalf("up1 mark = %q, %v; want a needs_login mark with the login hint", reason, ok)
	}
	if _, ok := g.marked("up2"); ok {
		t.Fatal("the healthy fallback was marked")
	}
	// attempts() now skips the marked provider: the next request goes straight to up2.
	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-haiku"}`, nil); resp.StatusCode != 200 || up2.hits.Load() != 2 {
		t.Fatalf("second status %d up2 hits %d", resp.StatusCode, up2.hits.Load())
	}
}

func TestTransientRefreshFailureMovesOnWithoutMarking(t *testing.T) {
	g := useGate(t)
	up2 := newUpstream(t, ok)
	t.Setenv("UP2_KEY", "k2")
	srv := setupRouter(t, `{"default":"up1","fallback":["up2"],"providers":{"up1":{"kind":"anthropic","base_url":"`+up2.URL+
		`","auth":{"type":"bearer","command":"exit 1"},"models":{"opus":"m1"}},"up2":`+
		anthropicProvider(up2.URL, "UP2_KEY", `{"opus":"m2-opus"}`)+`}}`, nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil)
	if resp.StatusCode != 200 || up2.hits.Load() != 1 {
		t.Fatalf("status %d up2 hits %d, want a plain failover", resp.StatusCode, up2.hits.Load())
	}
	if len(g.marks) != 0 {
		t.Fatalf("a transient failure condemned an account: %v", g.marks)
	}
}

func TestLadderExhaustedReturns401WithLoginHint(t *testing.T) {
	g := useGate(t)
	up1 := newUpstream(t, ok)
	srv := setupRouter(t, `{"default":"up1","providers":{"up1":`+u2mRow(up1.URL)+`}}`, nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil)
	b := body(t, resp)
	if resp.StatusCode != 401 || !strings.Contains(b, `"authentication_error"`) ||
		!strings.Contains(b, "run `claude-code databricks login up1`") {
		t.Fatalf("status %d body %s, want 401 authentication_error with the login hint", resp.StatusCode, b)
	}
	if up1.hits.Load() != 0 {
		t.Fatalf("upstream reached %d times without a credential", up1.hits.Load())
	}
	if _, ok := g.marked("up1"); !ok {
		t.Fatal("exhausting the ladder must still record the mark")
	}
}

func TestAllTargetsMarkedKeepsTheLadder(t *testing.T) {
	g := useGate(t)
	up1 := newUpstream(t, ok)
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setupRouter(t, failoverOverlay(up1.URL, up2.URL), nil)
	g.premark("up1")
	g.premark("up2")

	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil); resp.StatusCode != 200 || up1.hits.Load() != 1 {
		t.Fatalf("status %d up1 hits %d, want the first target tried even though every target is marked", resp.StatusCode, up1.hits.Load())
	}
	// With only the first marked, the router skips it and uses the unmarked fallback.
	g.mu.Lock()
	delete(g.marks, "provider:up2")
	g.mu.Unlock()
	srv2 := setupRouter(t, failoverOverlay(up1.URL, up2.URL), nil)
	if resp := post(t, srv2.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil); resp.StatusCode != 200 || up1.hits.Load() != 1 || up2.hits.Load() != 1 {
		t.Fatalf("status %d hits up1=%d up2=%d, want the marked up1 skipped", resp.StatusCode, up1.hits.Load(), up2.hits.Load())
	}
}

func TestClearOnSuccessLiftsOwnMark(t *testing.T) {
	g := useGate(t)
	rejecting := true
	var mu sync.Mutex
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		mu.Lock()
		rej := rejecting
		mu.Unlock()
		if rej {
			w.WriteHeader(401)
			return
		}
		ok(w, r, n)
	})
	t.Setenv("UP1_KEY", "k1")
	srv := setupRouter(t, `{"default":"up1","providers":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}`, nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("status %d, want 401 after a re-mint that is still rejected", resp.StatusCode)
	}
	if reason, ok := g.marked("up1"); !ok || !strings.Contains(reason, "rejected") {
		t.Fatalf("mark = %q, %v; want rejected", reason, ok)
	}
	mu.Lock()
	rejecting = false
	mu.Unlock()
	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("status %d after recovery", resp.StatusCode)
	}
	if _, ok := g.marked("up1"); ok || len(g.cleared) != 1 || g.cleared[0] != "provider:up1" {
		t.Fatalf("mark not cleared on success: marks=%v cleared=%v", g.marks, g.cleared)
	}
}

func TestSuccessNeverClearsAMarkThisProcessDidNotSet(t *testing.T) {
	g := useGate(t)
	up := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	srv := setupRouter(t, `{"default":"up1","providers":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}`, nil)
	g.premark("up1")
	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if _, ok := g.marked("up1"); !ok || len(g.cleared) != 0 {
		t.Fatalf("a foreign mark was cleared: marks=%v cleared=%v", g.marks, g.cleared)
	}
}

// sealedPAT is a Databricks-shaped row (sealed authType): the gateway's 400 "Invalid Token" is an auth rejection.
func sealedPAT(t *testing.T, base string) string {
	t.Helper()
	if err := providers.SealSecret(providers.SecretNameFor("patrow", providers.AuthPAT), "pat-value"); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"kind":"anthropic","base_url":%q,"auth":{"type":"bearer","authType":"pat"},"models":{"opus":"m1"}}`, base)
}

func invalidTokenUp(t *testing.T) *upstream {
	return newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(400)
		io.WriteString(w, `{"message":"Invalid Token"}`)
	})
}

func TestInvalidToken400OnDatabricksRowIsAnAuthRejection(t *testing.T) {
	g := useGate(t)
	up := invalidTokenUp(t)
	vaulttest.UseKeyring(t)
	row := sealedPAT(t, up.URL)
	srv := setupRouter2(t, `{"default":"patrow","providers":{"patrow":`+row+`}}`)

	resp := post(t, srv.URL+"/p/patrow/v1/messages", `{"model":"m1"}`, nil)
	b := body(t, resp)
	if resp.StatusCode != 401 || !strings.Contains(b, "authentication_error") {
		t.Fatalf("status %d body %s, want 401 authentication_error", resp.StatusCode, b)
	}
	if up.hits.Load() != 2 {
		t.Fatalf("upstream hits %d, want the request plus one replay after the re-mint", up.hits.Load())
	}
	if _, ok := g.marked("patrow"); !ok {
		t.Fatal("a persistent Invalid Token must mark the provider")
	}
}

func TestInvalidToken400OnOtherProviderIsRelayedAsIs(t *testing.T) {
	g := useGate(t)
	up := invalidTokenUp(t)
	t.Setenv("UP1_KEY", "k1")
	srv := setupRouter(t, `{"default":"up1","providers":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}`, nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil)
	if resp.StatusCode != 400 || !strings.Contains(body(t, resp), "Invalid Token") {
		t.Fatalf("status %d, want the upstream 400 relayed", resp.StatusCode)
	}
	if up.hits.Load() != 1 || len(g.marks) != 0 {
		t.Fatalf("hits %d marks %v: a non-Databricks 400 must not remint or condemn", up.hits.Load(), g.marks)
	}
}

// setupRouter2 reuses the keyring/vault the caller already created (sealed secrets must survive into the router).
func setupRouter2(t *testing.T, overlay string) *httptest.Server {
	t.Helper()
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	r := newRouter(secret)
	r.log = io.Discard
	r.oauth = func() string { return "" }
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// A condemned refresh on the passthrough path (GET and POST) answers 502 and records the mark.
func TestPassthroughCondemnedRefreshAnswers502AndMarks(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			g := useGate(t)
			up := newRawUp(t, okRaw)
			srv := setupRouter(t, fmt.Sprintf(`{"default":"up1","providers":{"up1":{"kind":"openai","base_url":%q,`+
				`"auth":{"type":"bearer","authType":"u2m"},"models":{"opus":"m"}}}}`, up.URL+"/v1"), nil)
			resp := ptDo(t, method, srv.URL+"/p/up1/openai/models", "{}", nil)
			b := body(t, resp)
			if resp.StatusCode != http.StatusBadGateway || !strings.Contains(b, "authentication failed") {
				t.Fatalf("status %d body %s, want 502 authentication failed", resp.StatusCode, b)
			}
			if len(up.got()) != 0 {
				t.Fatal("upstream reached without a credential")
			}
			if _, ok := g.marked("up1"); !ok {
				t.Fatal("passthrough path did not record the mark")
			}
		})
	}
}

// ---- B4: the sealed Max token on the subscription passthrough -------------------------------------------------

func tlsUpstream(t *testing.T) (*httptest.Server, *rawUp) {
	t.Helper()
	up := &rawUp{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.reqs = append(up.reqs, recorded{r.Method, r.RequestURI, string(b), r.Header.Clone()})
		up.mu.Unlock()
		ok(w, r, 0)
	}))
	t.Cleanup(srv.Close)
	return srv, up
}

func subOverlay(base string) string {
	return fmt.Sprintf(`{"default":"sub","providers":{"sub":{"kind":"anthropic","base_url":%q,"auth":{"type":"passthrough"},"models":{"opus":"m"}}}}`, base)
}

func allHeaderValues(h http.Header) string {
	var sb strings.Builder
	for k, vs := range h {
		sb.WriteString(k + "=" + strings.Join(vs, ",") + "\n")
	}
	return sb.String()
}

func pointAnthropicHostAt(t *testing.T, base string) {
	t.Helper()
	u, _ := url.Parse(base)
	prev := anthropicHost
	anthropicHost = u.Host
	t.Cleanup(func() { anthropicHost = prev })
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
	if h.Get("X-Api-Key") != "" || h.Get(ClientHeader) != "" {
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
	login := map[string]string{"X-Api-Key": "", ClientHeader: secret, "Authorization": "Bearer clients-own"}
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
			if h.Get("Authorization") != "" || strings.Contains(allHeaderValues(h), "vault-max-token") || strings.Contains(allHeaderValues(h), secret) {
				t.Fatalf("credential sent to a non-Anthropic origin: %v", h)
			}
		})
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

func TestPassthroughWithoutAnyCredentialIsRefused(t *testing.T) {
	tsrv, up := tlsUpstream(t)
	pointAnthropicHostAt(t, tsrv.URL)
	srv := setupRouter(t, subOverlay(tsrv.URL), func(r *Router) { r.client = tsrv.Client() })
	resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, nil)
	if resp.StatusCode != 400 || len(up.got()) != 0 {
		t.Fatalf("status %d hits %d, want 400 and no upstream call without a client login or vault token", resp.StatusCode, len(up.got()))
	}
}

// The idle timer wraps the body BEFORE the 256-byte Invalid Token peek: a stalled 400 body must be cut, not hang.
func TestStalledInvalidTokenBodyIsCutByIdleTimeout(t *testing.T) {
	useGate(t)
	prev := idleTimeout
	idleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { idleTimeout = prev })
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.WriteHeader(400)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never sends a body byte
	})
	vaulttest.UseKeyring(t)
	row := sealedPAT(t, up.URL)
	srv := setupRouter2(t, `{"default":"patrow","providers":{"patrow":`+row+`}}`)

	start := time.Now()
	resp := post(t, srv.URL+"/p/patrow/v1/messages", `{"model":"m1"}`, nil)
	_, _ = io.ReadAll(resp.Body)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("request took %v: the stalled body was not cut by idleTimeout", d)
	}
	if up.hits.Load() != 1 {
		t.Fatalf("upstream hits %d: a body that never arrived must not read as Invalid Token", up.hits.Load())
	}
}
