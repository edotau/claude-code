package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/gateway/translate"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/tokenledger"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// routeState uses the real auth-mark store under a scratch config directory.
type routeState struct{ t *testing.T }

func useGate(t *testing.T) *routeState {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	return &routeState{t}
}

func (g *routeState) marked(provider string) (string, bool) {
	return router.LoadState().AuthMarked(router.ProviderAccount(provider), time.Now())
}

func (g *routeState) premark(provider string) {
	g.t.Helper()
	if err := router.MarkAuth(router.ProviderAccount(provider), "auth rejected"); err != nil {
		g.t.Fatal(err)
	}
}

func (g *routeState) count() int { return len(router.LoadState()) }

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
	t.Cleanup(func() { srv.Close(); r.bg.Wait() }) // a late state write must not race TempDir removal
	return srv
}

// u2mRow is a sealed-auth row with no sealed refresh token: minting yields providers.ErrNeedsLogin.
func u2mRow(base string) string {
	return fmt.Sprintf(
		`{"protocol":"anthropic","base_url":%q,"auth":{"type":"oauth"},"models":{"opus":"m1-opus","haiku":"m1-haiku"}}`,
		base)
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
	srv := setupRouter(t,
		`{"default":"up1","fallback":["up2"],"providers":{"databricks":{"up1":`+u2mRow(up1.URL)+`},"anthropic":{"up2":`+
			anthropicProvider(up2.URL, "UP2_KEY", `{"opus":"m2-opus","haiku":"m2-haiku"}`)+`}}}`,
		nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-haiku"}`, nil)
	if resp.StatusCode != 200 || up1.hits.Load() != 0 || up2.hits.Load() != 1 {
		t.Fatalf("status %d hits up1=%d up2=%d, want 200 served by up2 only", resp.StatusCode, up1.hits.Load(),
			up2.hits.Load())
	}
	if reason, ok := g.marked("up1"); !ok || !strings.Contains(reason, "needs_login") ||
		!strings.Contains(reason, "databricks login --profile up1") {
		t.Fatalf("up1 mark = %q, %v; want a needs_login mark with the login hint", reason, ok)
	}
	if _, ok := g.marked("up2"); ok {
		t.Fatal("the healthy fallback was marked")
	}
	// attempts() now skips the marked provider: the next request goes straight to up2.
	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-haiku"}`, nil); resp.StatusCode != 200 ||
		up2.hits.Load() != 2 {
		t.Fatalf("second status %d up2 hits %d", resp.StatusCode, up2.hits.Load())
	}
}

func TestTransientRefreshFailureMovesOnWithoutMarking(t *testing.T) {
	g := useGate(t)
	up2 := newUpstream(t, ok)
	t.Setenv("UP2_KEY", "k2")
	srv := setupRouter(t,
		`{"default":"up1","fallback":["up2"],"providers":{"anthropic":{"up1":{"protocol":"anthropic","base_url":"`+up2.URL+
			`","auth":{"type":"key","command":"exit 1"},"models":{"opus":"m1"}},"up2":`+
			anthropicProvider(up2.URL, "UP2_KEY", `{"opus":"m2-opus"}`)+`}}}`,
		nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil)
	if resp.StatusCode != 200 || up2.hits.Load() != 1 {
		t.Fatalf("status %d up2 hits %d, want a plain failover", resp.StatusCode, up2.hits.Load())
	}
	if g.count() != 0 {
		t.Fatalf("a transient failure condemned an account: %d", g.count())
	}
}

func TestLadderExhaustedReturns401WithLoginHint(t *testing.T) {
	g := useGate(t)
	up1 := newUpstream(t, ok)
	srv := setupRouter(t, `{"default":"up1","providers":{"databricks":{"up1":`+u2mRow(up1.URL)+`}}}`, nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil)
	b := body(t, resp)
	if resp.StatusCode != 401 || !strings.Contains(b, `"authentication_error"`) ||
		!strings.Contains(b, "run `claude-code databricks login --profile up1`") {
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

	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil); resp.StatusCode != 200 ||
		up1.hits.Load() != 1 {
		t.Fatalf("status %d up1 hits %d, want the first target tried even though every target is marked",
			resp.StatusCode, up1.hits.Load())
	}
	// With only the first marked, the router skips it and uses the unmarked fallback.
	srv2 := setupRouter(t, failoverOverlay(up1.URL, up2.URL), nil) // a fresh config dir: re-mark there
	g.premark("up1")
	if resp := post(t, srv2.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil); resp.StatusCode != 200 ||
		up1.hits.Load() != 1 ||
		up2.hits.Load() != 1 {
		t.Fatalf("status %d hits up1=%d up2=%d, want the marked up1 skipped", resp.StatusCode, up1.hits.Load(),
			up2.hits.Load())
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
	srv := setupRouter(t,
		`{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}}`,
		nil)

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
	// the handler clears after streaming the reply, so the client can return first: wait for it
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, ok := g.marked("up1")
		if !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mark not cleared on success")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSuccessNeverClearsAMarkThisProcessDidNotSet(t *testing.T) {
	g := useGate(t)
	up := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	srv := setupRouter(t,
		`{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}}`,
		nil)
	g.premark("up1")
	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if _, ok := g.marked("up1"); !ok {
		t.Fatal("a foreign mark was cleared")
	}
}

// sealedPAT is a Databricks-shaped row (sealed authType): the gateway's 400 "Invalid Token" is an auth rejection.
func sealedPAT(t *testing.T, base string) string {
	t.Helper()
	if err := vault.Store(providers.SecretName(providers.GroupDatabricks, "patrow", providers.AuthPAT), "pat-value"); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"protocol":"anthropic","base_url":%q,"auth":{"type":"pat"},"models":{"opus":"m1"}}`, base)
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
	srv := setupRouter2(t, `{"default":"patrow","providers":{"databricks":{"patrow":`+row+`}}}`)

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
	srv := setupRouter(t,
		`{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}}`,
		nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil)
	if resp.StatusCode != 400 || !strings.Contains(body(t, resp), "Invalid Token") {
		t.Fatalf("status %d, want the upstream 400 relayed", resp.StatusCode)
	}
	if up.hits.Load() != 1 || g.count() != 0 {
		t.Fatalf("hits %d marks %d: a non-Databricks 400 must not remint or condemn", up.hits.Load(), g.count())
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
	t.Cleanup(func() { srv.Close(); r.bg.Wait() }) // a late state write must not race TempDir removal
	return srv
}

// A condemned refresh on the passthrough path (GET and POST) answers 401 and records the mark.
func TestPassthroughCondemnedRefreshAnswers401AndMarks(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			g := useGate(t)
			up := newRawUp(t, okRaw)
			srv := setupRouter(t,
				fmt.Sprintf(`{"default":"up1","providers":{"databricks":{"up1":{"protocol":"openai","base_url":%q,`+
					`"auth":{"type":"oauth"},"models":{"opus":"m"}}}}}`, up.URL+"/v1"),
				nil)
			resp := ptDo(t, method, srv.URL+"/p/up1/openai/models", "{}", nil)
			b := body(t, resp)
			if resp.StatusCode != http.StatusUnauthorized ||
				!strings.Contains(b, "authentication failed; run `claude-code databricks login --profile up1`") {
				t.Fatalf("status %d body %s, want 401 authentication failed with login hint", resp.StatusCode, b)
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
	return fmt.Sprintf(
		`{"default":"sub","providers":{"anthropic":{"sub":{"protocol":"anthropic","base_url":%q,"auth":{"type":"cli"},"models":{"opus":"m"}}}}}`,
		base)
}

func allHeaderValues(h http.Header) string {
	var sb strings.Builder
	for k, vs := range h {
		fmt.Fprintf(&sb, "%s=%s\n", k, strings.Join(vs, ","))
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
		t.Fatalf("status %d hits %d, want 400 and no upstream call without a client login or vault token",
			resp.StatusCode, len(up.got()))
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
	srv := setupRouter2(t, `{"default":"patrow","providers":{"databricks":{"patrow":`+row+`}}}`)

	start := time.Now()
	req := testRequest(t, http.MethodPost, srv.URL+"/p/patrow/v1/messages", strings.NewReader(`{"model":"m1"}`))
	req.Header.Set("X-Api-Key", secret)
	if resp, err := http.DefaultClient.Do(req); err == nil { // the cut body aborts the relay: a broken stream is fine
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("request took %v: the stalled body was not cut by idleTimeout", d)
	}
	if up.hits.Load() != 1 {
		t.Fatalf("upstream hits %d: a body that never arrived must not read as Invalid Token", up.hits.Load())
	}
}

// A rejected static key names the vault remedy and the mark, never a Databricks login.
func TestRejectedKeyHintsVaultSetNotDatabricksLogin(t *testing.T) {
	g := useGate(t)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusUnauthorized) })
	t.Setenv("UP1_KEY", "k1")
	srv := setupRouter(t,
		`{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}}`,
		nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil)
	b := body(t, resp)
	if resp.StatusCode != 401 || !strings.Contains(b, "run `claude-code vault set UP1_KEY`") ||
		strings.Contains(b, "databricks") {
		t.Fatalf("status %d body %s, want 401 with the vault set hint", resp.StatusCode, b)
	}
	if reason, ok := g.marked("up1"); !ok || !strings.Contains(reason, "vault set UP1_KEY") {
		t.Fatalf("mark = %q, %v; want the vault set hint", reason, ok)
	}
}

// A bare 403 that survives the re-mint is a policy/WAF block: relayed to the caller as-is, never a mark.
func TestBareForbiddenAfterRemintIsRelayedNotCondemned(t *testing.T) {
	g := useGate(t)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"model not permitted"}`)
	})
	t.Setenv("UP1_KEY", "k1")
	srv := setupRouter(t,
		`{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}}`,
		nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil)
	b := body(t, resp)
	if resp.StatusCode != 403 || !strings.Contains(b, "model not permitted") {
		t.Fatalf("status %d body %s, want the upstream 403 relayed", resp.StatusCode, b)
	}
	if _, ok := g.marked("up1"); ok {
		t.Fatal("a bare 403 after the re-mint must not mark the provider")
	}
}

// dbxGeminiSetup is a Databricks serving row whose derived gemini route lands on a recording upstream.
func dbxGeminiSetup(t *testing.T) (*rawUp, string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newRawUp(t, okRaw)
	t.Setenv("DBX_KEY", "dbx-token")
	row := `{"preset":"databricks","host":"https://example.cloud.databricks.com","base_url":"` + up.URL +
		`/serving-endpoints/anthropic","auth":{"type":"key","env":"DBX_KEY"}}`
	srv := setup(t, `{"default":"dbx","providers":{"databricks":{"dbx":`+row+`}}}`)
	return up, srv.URL + "/p/dbx/gemini/"
}

// A Databricks row answers count_tokens locally: the serving endpoint has no such path and would return 400.
func TestDatabricksCountTokensAnsweredLocally(t *testing.T) {
	up, base := dbxGeminiSetup(t)
	resp := post(t, strings.TrimSuffix(base, "gemini/")+"v1/messages/count_tokens",
		`{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "input_tokens") || len(up.got()) != 0 {
		t.Fatalf("count_tokens = %d %s with %d upstream hits, want a local 200 estimate", resp.StatusCode, b, len(up.got()))
	}
}

// agy's call reaches /serving-endpoints/gemini with the wire model, alt=sse only, fresh token, thoughts-off fixed.
func TestDatabricksGeminiRoute(t *testing.T) {
	up, base := dbxGeminiSetup(t)
	body := `{"contents":[],"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":0}}}`
	resp := ptDo(t, http.MethodPost, base+"v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse&key=x", body,
		nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	g := up.got()
	if len(g) != 1 {
		t.Fatalf("upstream hits %d", len(g))
	}
	if want := "/serving-endpoints/gemini/v1beta/models/databricks-gemini-3-8-flash:streamGenerateContent?alt=sse"; g[0].uri != want {
		t.Errorf("uri = %s, want %s", g[0].uri, want)
	}
	if strings.Contains(g[0].body, "includeThoughts") || !strings.Contains(g[0].body, `"thinkingBudget":0`) {
		t.Errorf("body = %s", g[0].body)
	}
	if a := g[0].hdr.Get("Authorization"); a != "Bearer dbx-token" {
		t.Errorf("authorization = %q", a)
	}
}

// Regression: an encoded or literal climb, or any call but (stream)GenerateContent, never reaches the workspace.
func TestDatabricksGeminiRouteRejectsTraversal(t *testing.T) {
	up, base := dbxGeminiSetup(t)
	for _, rest := range []string{
		"v1beta/models/%2e%2e/%2e%2e/api/2.0/secrets:generateContent", "v1beta/../../api/2.0/clusters/list",
		"v1beta/models/m%252e:generateContent", "v1beta//models/m:generateContent", "v1beta/models/m:countTokens",
		"v1beta/models/m%5c:generateContent",
	} {
		resp := ptDo(t, http.MethodPost, base+rest, `{}`, nil)
		if resp.StatusCode < 400 {
			t.Errorf("%s: status %d", rest, resp.StatusCode)
		}
	}
	if g := up.got(); len(g) != 0 {
		t.Fatalf("upstream reached: %v", g[0].uri)
	}
}

// Regression: every passthrough dialect refuses a dot-segment tail before building the upstream URL.
func TestPassthroughRejectsDotSegments(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newRawUp(t, okRaw)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	for _, rest := range []string{"openai/%2e%2e/admin", "gemini/v1beta/%2e%2e/x", "responses/a%252e%252e/b"} {
		if resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/"+rest, `{}`, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", rest, resp.StatusCode)
		}
	}
	if g := up.got(); len(g) != 0 {
		t.Fatalf("upstream reached: %v", g[0].uri)
	}
}

const geminiChat = `{"model":"databricks-gemini-3-8-flash","stream":true,"stream_options":{"include_usage":true},"messages":[` +
	`{"role":"user","content":"hi"},` +
	`{"role":"assistant","tool_calls":[` +
	`{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},` +
	`{"id":"b","type":"function","thoughtSignature":"real-sig","function":{"name":"f","arguments":"{}"}},` +
	`{"id":"c","type":"function","extra_content":{"google":{"thought_signature":"x"}},"function":{"name":"f","arguments":"{}"}}]},` +
	`{"role":"tool","tool_call_id":"a","content":"ok"}]}`

func sendChat(t *testing.T, body string, h func(http.ResponseWriter, *http.Request, int)) (*rawUp, *http.Response) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newRawUp(t, h)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	return up, ptDo(t, http.MethodPost, srv.URL+"/p/pt/openai/chat/completions", body, nil)
}

// Passthrough to a Gemini model drops stream_options and signs unsigned assistant tool_calls only.
func TestPassthroughGeminiChatSanitized(t *testing.T) {
	up, resp := sendChat(t, geminiChat, okRaw)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	g := up.got()
	if len(g) != 1 {
		t.Fatalf("upstream hits %d", len(g))
	}
	var sent struct {
		StreamOptions *json.RawMessage `json:"stream_options"`
		Messages      []struct {
			ToolCalls []map[string]json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(g[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.StreamOptions != nil || strings.Contains(g[0].body, "stream_options") {
		t.Errorf("stream_options reached upstream: %s", g[0].body)
	}
	calls := sent.Messages[1].ToolCalls
	if string(calls[0]["thoughtSignature"]) != `"skip_thought_signature_validator"` {
		t.Errorf("unsigned call = %v, want skip signature", calls[0])
	}
	if string(calls[1]["thoughtSignature"]) != `"real-sig"` {
		t.Errorf("signed call changed: %v", calls[1])
	}
	if _, has := calls[2]["thoughtSignature"]; has || calls[2]["extra_content"] == nil {
		t.Errorf("extra_content call touched: %v", calls[2])
	}
}

// A non-Gemini chat body is forwarded byte-identical.
func TestPassthroughNonGeminiChatUntouched(t *testing.T) {
	body := strings.Replace(geminiChat, "databricks-gemini-3-8-flash", "databricks-llama", 1)
	up, resp := sendChat(t, body, okRaw)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if g := up.got(); len(g) != 1 || g[0].body != body {
		t.Fatalf("upstream body %+v, want unchanged", g)
	}
}

func ledgerSettled(t *testing.T, want int) []tokenledger.Entry {
	t.Helper()
	var got []tokenledger.Entry
	for i := range 100 {
		got, _ = tokenledger.Read()
		if len(got) >= want && want > 0 {
			return got
		}
		time.Sleep(20 * time.Millisecond)
		if want == 0 && i >= 10 {
			break
		}
	}
	return got
}

// Passthrough chat usage (plain JSON and the SSE closing chunk) lands in the ledger.
func TestPassthroughMetersChatUsage(t *testing.T) {
	for name, tc := range map[string]struct{ ct, out string }{
		"json": {"application/json", `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`},
		"sse": {"text/event-stream", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":4,\"prompt_tokens\":11,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"},
	} {
		t.Run(name, func(t *testing.T) {
			_, resp := sendChat(t, `{"model":"m"}`, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", tc.ct)
				io.WriteString(w, tc.out)
			})
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || string(b) != tc.out {
				t.Fatalf("status %d body %q, want relayed unchanged", resp.StatusCode, b)
			}
			got := ledgerSettled(t, 1)
			if len(got) != 1 || got[0].Profile != "pt" || got[0].Model != "m" || got[0].Input != 11 ||
				got[0].Output != 4 {
				t.Fatalf("ledger = %+v, want pt/m 11 in 4 out", got)
			}
		})
	}
}

func chatChunk(content, usage string) string {
	return fmt.Sprintf("data: {\"id\":\"c\",\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]%s}\n\n",
		content, usage)
}

// Translated (/v1/messages -> chat/completions) replies land in the ledger: JSON, SSE, SSE past the tail cap, cut SSE.
func TestTranslatedChatMetersUsage(t *testing.T) {
	const usage = `,"usage":{"prompt_tokens":11,"completion_tokens":4}`
	pad := strings.Repeat("x", 1<<10)
	big := strings.Repeat(chatChunk(pad, ""), 3*tailCap/len(pad)) + chatChunk("end", usage) + "data: [DONE]\n\n"
	for name, tc := range map[string]struct {
		stream  bool
		out     string
		cut     bool
		wantOut string
	}{
		"json":                {false, `{"id":"c","model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]` + usage + `}`, false, "hi"},
		"sse":                 {true, chatChunk("hi", usage) + "data: [DONE]\n\n", false, "hi"},
		"sse-over-tail-cap":   {true, big, false, "end"},
		"sse-cut-after-usage": {true, chatChunk("hi", usage), true, "hi"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			up := newRawUp(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				io.WriteString(w, tc.out)
				if tc.cut {
					w.(http.Flusher).Flush()
					panic(http.ErrAbortHandler)
				}
			})
			t.Setenv("OA_KEY", "oa")
			srv := setup(t, `{"default":"oa","providers":{"openai":{"oa":{"protocol":"openai","base_url":"`+up.URL+
				`/v1","auth":{"type":"key","env":"OA_KEY"},"models":{"opus":"gpt-x"}}}}}`)
			req := fmt.Sprintf(`{"model":"gpt-x","max_tokens":10,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, tc.stream)
			resp := post(t, srv.URL+"/p/oa/v1/messages", req, map[string]string{
				"X-Claude-Code-Session-Id": "sess-1", gateway.AgentHeader: "copilot"})
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if g := up.got(); resp.StatusCode != 200 || len(g) != 1 || !strings.HasSuffix(g[0].uri, "/v1/chat/completions") ||
				!strings.Contains(string(b), tc.wantOut) {
				t.Fatalf("status %d upstream %+v body %.200s", resp.StatusCode, g, b)
			}
			checkTranslatedLedger(t)
		})
	}
}

func checkTranslatedLedger(t *testing.T) {
	t.Helper()
	got := ledgerSettled(t, 1)
	if len(got) != 1 {
		t.Fatalf("ledger = %+v, want one translated response", got)
	}
	entry := got[0]
	entry.Ts, entry.Schema = time.Time{}, 0
	want := tokenledger.Entry{Profile: "oa", Provider: "openai", Model: "gpt-x", Session: "sess-1",
		Agent: "copilot", Input: 11, Output: 4}
	if entry != want {
		t.Fatalf("ledger = %+v, want %+v", entry, want)
	}
}

// A non-200 upstream answer records nothing.
func TestPassthroughNon200NotMetered(t *testing.T) {
	_, resp := sendChat(t, `{"model":"m"}`, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"usage":{"prompt_tokens":9,"completion_tokens":9}}`)
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if got := ledgerSettled(t, 0); len(got) != 0 {
		t.Fatalf("ledger = %+v, want empty", got)
	}
}

// A null tool_calls entry passes through instead of panicking the handler.
func TestFixGeminiChatNullToolCall(t *testing.T) {
	in := `{"model":"databricks-gemini-3-8-flash","messages":[{"role":"assistant","tool_calls":[null,{"id":"a","type":"function"}]}]}`
	out := string(fixGeminiChat([]byte(in)))
	if !strings.Contains(out, "null") || !strings.Contains(out, translate.SkipThoughtSignature) {
		t.Fatalf("null entry or signature lost: %s", out)
	}
}

// An Anthropic-style tool_choice object becomes the chat string Databricks Gemini accepts; other forms stay.
func TestFixGeminiChatToolChoice(t *testing.T) {
	for in, want := range map[string]string{
		`{"type":"auto"}`: `"auto"`, `{"type":"any"}`: `"required"`, `"auto"`: `"auto"`, `"validated"`: `"auto"`,
		`{"type":"function","function":{"name":"p"}}`: `{"type":"function","function":{"name":"p"}}`,
	} {
		out := string(fixGeminiChat([]byte(`{"model":"databricks-gemini-3-8-flash","tool_choice":` + in + `}`)))
		if !strings.Contains(out, `"tool_choice":`+want) {
			t.Errorf("tool_choice %s -> %s, want %s", in, out, want)
		}
	}
}

// A Gemini chat reply whose content is an array of parts reaches the client as one string; other models' replies stay.
func TestPassthroughGeminiChatReplyFlattened(t *testing.T) {
	parts := func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w,
			`{"choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"O","thoughtSignature":"s"},{"type":"text","text":"K"}]}}]}`)
	}
	_, resp := sendChat(t, geminiChat, parts)
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"content":"OK"`) {
		t.Errorf("gemini reply = %s, want content OK string", b)
	}
	_, resp = sendChat(t, strings.Replace(geminiChat, "databricks-gemini-3-8-flash", "databricks-llama", 1), parts)
	b, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"thoughtSignature"`) {
		t.Errorf("non-gemini reply rewritten: %s", b)
	}
}

// Tool schemas sent to Gemini lose the keys Google rejects; a clean schema and other models' schemas are untouched.
func TestPassthroughGeminiChatToolSchemaCleaned(t *testing.T) {
	body := `{"model":"databricks-gemini-3-8-flash","tools":[{"type":"function","function":{"name":"ls","parameters":` +
		`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"n":{"type":"integer","exclusiveMinimum":0},` +
		`"k":{"const":"a"}},"additionalProperties":false}}}]}`
	up, _ := sendChat(t, body, okRaw)
	got := up.got()[0].body
	for _, bad := range []string{"$schema", "exclusiveMinimum", "const"} {
		if strings.Contains(got, bad) {
			t.Errorf("%s reached gemini: %s", bad, got)
		}
	}
	if !strings.Contains(got, `"enum":["a"]`) || !strings.Contains(got, `"additionalProperties":false`) {
		t.Errorf("valid keys lost: %s", got)
	}
	other := strings.Replace(body, "databricks-gemini-3-8-flash", "databricks-llama", 1)
	if up, _ := sendChat(t, other, okRaw); up.got()[0].body != other {
		t.Errorf("non-gemini tools rewritten: %s", up.got()[0].body)
	}
}
