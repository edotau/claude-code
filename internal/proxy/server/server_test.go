package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const secret = "test-secret"

// upstream is a fake provider that records what it received and answers via handle.
type upstream struct {
	*httptest.Server
	hits   atomic.Int32
	last   atomic.Pointer[http.Request]
	body   atomic.Pointer[map[string]any]
	handle func(w http.ResponseWriter, r *http.Request, n int)
}

func newUpstream(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, n int)) *upstream {
	u := &upstream{handle: handle}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(u.hits.Add(1))
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, "decode upstream request: "+err.Error(), http.StatusBadRequest)
			return
		}
		u.last.Store(r)
		u.body.Store(&m)
		u.handle(w, r, n)
	}))
	t.Cleanup(u.Close)
	return u
}

func ok(w http.ResponseWriter, _ *http.Request, _ int) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"type":"message","content":[]}`)
}

// setup seeds vault overlay/providers in a temp config dir and serves a router over it.
func setup(t *testing.T, overlay string) *httptest.Server {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	r := newRouter(secret)
	r.log = io.Discard
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func testRequest(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func post(t *testing.T, url, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req := testRequest(t, http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", secret)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func anthropicProvider(url, env string, models string) string {
	return fmt.Sprintf(`{"protocol":"anthropic","base_url":%q,"auth":{"type":"key","header":"x-api-key","env":%q},"models":%s}`, url, env, models)
}

func TestAuthAndHostRejection(t *testing.T) {
	up := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	srv := setup(t, `{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}}`)
	url := srv.URL + "/p/up1/v1/messages"

	if resp := post(t, url, `{"model":"m1"}`, map[string]string{"X-Api-Key": ""}); resp.StatusCode != 401 {
		t.Fatalf("no secret: got %d", resp.StatusCode)
	}
	if resp := post(t, url, `{"model":"m1"}`, map[string]string{"X-Api-Key": "wrong"}); resp.StatusCode != 401 {
		t.Fatalf("wrong secret: got %d", resp.StatusCode)
	}
	if resp := post(t, url, `{"model":"m1"}`, map[string]string{"X-Api-Key": "", "Authorization": "Bearer " + secret}); resp.StatusCode != 200 {
		t.Fatalf("bearer secret: got %d", resp.StatusCode)
	}
	req := testRequest(t, http.MethodPost, url, strings.NewReader(`{"model":"m1"}`))
	req.Host = "evil.example:80"
	req.Header.Set("X-Api-Key", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("rebinding Host: got %d", resp.StatusCode)
	}
	if up.hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", up.hits.Load())
	}
	h, err := http.DefaultClient.Do(testRequest(t, http.MethodGet, srv.URL+"/healthz", nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Body.Close()
	if h.StatusCode != 200 {
		t.Fatalf("healthz: %d", h.StatusCode)
	}
}

func TestAnthropicPassthrough(t *testing.T) {
	up := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "provider-key")
	srv := setup(t, `{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"claude-opus-5"}`)+`}}}`)

	resp := post(t, srv.URL+"/p/up1/v1/messages?beta=true", `{"model":"claude-opus-5[1m]","max_tokens":5,"messages":[{"role":"user","content":"<hi>"}]}`,
		map[string]string{"Anthropic-Beta": "context-1m-2025-08-07", "Anthropic-Version": "2023-06-01"})
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	got, body := up.last.Load(), *up.body.Load()
	if got.URL.Path != "/v1/messages" || got.URL.RawQuery != "beta=true" {
		t.Errorf("upstream url = %s", got.URL)
	}
	if got.Header.Get("X-Api-Key") != "provider-key" || got.Header.Get("Authorization") != "" {
		t.Errorf("auth headers: x-api-key=%q authorization=%q", got.Header.Get("X-Api-Key"), got.Header.Get("Authorization"))
	}
	if got.Header.Get("Anthropic-Beta") != "context-1m-2025-08-07" || got.Header.Get("Anthropic-Version") == "" {
		t.Errorf("anthropic headers not forwarded: %v", got.Header)
	}
	if body["model"] != "claude-opus-5" || body["max_tokens"] != float64(5) {
		t.Errorf("body = %v", body)
	}

	resp = post(t, srv.URL+"/p/up1/v1/messages/count_tokens", `{"model":"claude-opus-5"}`, nil)
	if resp.StatusCode != 200 || up.last.Load().URL.Path != "/v1/messages/count_tokens" {
		t.Errorf("count_tokens: %d %s", resp.StatusCode, up.last.Load().URL.Path)
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

func TestPassthroughRefused(t *testing.T) {
	up := newUpstream(t, ok)
	srv := setup(t, `{"default":"sub","providers":{"anthropic":{"sub":{"protocol":"anthropic","base_url":"`+up.URL+`","auth":{"type":"cli"},"models":{"opus":"m"}}}}}`)
	resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "passthrough") || up.hits.Load() != 0 {
		t.Fatalf("got %d %s hits=%d", resp.StatusCode, b, up.hits.Load())
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

func failoverOverlay(up1, up2 string) string {
	return `{"default":"up1","fallback":["up2"],"providers":{"anthropic":{
		"up1":` + anthropicProvider(up1, "UP1_KEY", `{"opus":"m1-opus","haiku":"m1-haiku"}`) + `,
		"up2":` + anthropicProvider(up2, "UP2_KEY", `{"opus":"m2-opus","haiku":"m2-haiku"}`) + `}}}`
}

func TestFailoverOn429BeforeBytes(t *testing.T) {
	up1 := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	})
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, failoverOverlay(up1.URL, up2.URL))

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-haiku"}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if m := (*up2.body.Load())["model"]; m != "m2-haiku" {
		t.Errorf("failover model = %v, want same slot on up2", m)
	}
	// up1 is cooling down: the next request starts at up2.
	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("second status %d", resp.StatusCode)
	}
	if up1.hits.Load() != 1 || up2.hits.Load() != 2 {
		t.Fatalf("hits up1=%d up2=%d, want 1 and 2", up1.hits.Load(), up2.hits.Load())
	}
}

func TestLastAttemptRelaysUpstreamError(t *testing.T) {
	fail := func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(529)
		io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
	}
	up1, up2 := newUpstream(t, fail), newUpstream(t, fail)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, failoverOverlay(up1.URL, up2.URL))
	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 529 || !strings.Contains(string(b), "busy") || up1.hits.Load() != 1 || up2.hits.Load() != 1 {
		t.Fatalf("got %d %s hits %d/%d", resp.StatusCode, b, up1.hits.Load(), up2.hits.Load())
	}
}

func TestNoFailoverAfterStreamStarted(t *testing.T) {
	up1 := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	})
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, failoverOverlay(up1.URL, up2.URL))

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus","stream":true}`, nil)
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if resp.StatusCode != 200 || line != "event: message_start\n" {
		t.Fatalf("status %d first line %q", resp.StatusCode, line)
	}
	io.Copy(io.Discard, resp.Body)
	if up2.hits.Load() != 0 {
		t.Fatalf("failed over after bytes were written")
	}
}

var authRefreshReplayRun atomic.Uint64

func TestAuthRefreshReplay(t *testing.T) {
	// Isolate each run from the process-wide command credential cache.
	name := fmt.Sprintf("%s-%d", t.Name(), authRefreshReplayRun.Add(1))
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Header.Get("Authorization") != "Bearer key2" {
			w.WriteHeader(401)
			return
		}
		ok(w, r, 0)
	})
	cmd := fmt.Sprintf(`n=$(cat %q 2>/dev/null || echo 0); n=$((n+1)); echo $n > %q; echo key$n`, counter, counter)
	overlay, _ := json.Marshal(map[string]any{"default": name, "providers": map[string]any{"anthropic": map[string]any{name: map[string]any{
		"protocol": "anthropic", "base_url": up.URL, "auth": map[string]any{"type": "key", "header": "bearer", "command": cmd, "ttl": 3600},
		"models": map[string]string{"opus": "m"}}}}})
	srv := setup(t, string(overlay))

	resp := post(t, srv.URL+"/p/"+name+"/v1/messages", `{"model":"m"}`, nil)
	if resp.StatusCode != 200 || up.hits.Load() != 2 {
		t.Fatalf("status %d hits %d, want 200 after one replay", resp.StatusCode, up.hits.Load())
	}
}

// A refresh failure drops cached presence so the next call probes the provider again.
func TestAuthRefreshFailureInvalidatesCachedCredential(t *testing.T) {
	const envVar = "ROUTER_TEST_REFRESH_FAILURE_CRED"
	t.Setenv(envVar, "v1")
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		os.Unsetenv(envVar) // simulate the credential being revoked between the first 401 and the Refresh replay
		w.WriteHeader(http.StatusUnauthorized)
	})
	p := &providers.Provider{Name: "refreshfail", BaseURL: up.URL,
		Auth: providers.Auth{Type: providers.AuthKey, Header: providers.HeaderXAPIKey, Env: envVar}}

	r := newRouter("s")
	r.log = io.Discard
	if !r.credentialPresent(p) {
		t.Fatal("credential must be reported present with the env var set")
	}

	req := testRequest(t, http.MethodPost, up.URL, strings.NewReader("{}"))
	if _, err := r.send(req, p, up.URL, []byte("{}"), func(http.Header) {}); err == nil {
		t.Fatal("expected send to surface the Refresh failure")
	}

	if r.credentialPresent(p) {
		t.Fatal("a failed Refresh must invalidate the cached presence, not leave a stale \"present\" cached")
	}
}

// storeCredentialPresent rejects a probe whose generation was invalidated before it stored its result.
func TestStoreCredentialPresentDropsStaleGeneration(t *testing.T) {
	r := newRouter("s")
	const name = "genprov"
	startGen := r.credGen[name]
	r.invalidateCredential(name) // races ahead of the in-flight probe below, bumping the generation
	r.storeCredentialPresent(name, startGen, r.now().Add(credTTL))
	if _, cached := r.cred[name]; cached {
		t.Fatal("a probe that started before an invalidation must not resurrect the cache after it")
	}

	// A probe that starts (captures its generation) after the invalidation must still store normally.
	freshGen := r.credGen[name]
	r.storeCredentialPresent(name, freshGen, r.now().Add(credTTL))
	if _, cached := r.cred[name]; !cached {
		t.Fatal("a probe matching the current generation must still populate the cache")
	}
}

func TestOpenAITranslation(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	})
	t.Setenv("OA_KEY", "oa")
	srv := setup(t, `{"default":"oa","providers":{"openai":{"oa":{"protocol":"openai","base_url":"`+up.URL+`/v1","auth":{"type":"key","env":"OA_KEY"},"models":{"opus":"gpt-x"}}}}}`)

	resp := post(t, srv.URL+"/p/oa/v1/messages", `{"model":"gpt-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || up.last.Load().URL.Path != "/v1/chat/completions" || up.last.Load().Header.Get("Authorization") != "Bearer oa" {
		t.Fatalf("status %d body %s path %s", resp.StatusCode, b, up.last.Load().URL.Path)
	}
	var msg struct {
		Type    string `json:"type"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &msg); err != nil || msg.Type != "message" || len(msg.Content) == 0 || msg.Content[0].Text != "hello" {
		t.Fatalf("translated body %s", b)
	}

	up.handle = func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"c2\",\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"streamed\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"c2\",\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}
	resp = post(t, srv.URL+"/p/oa/v1/messages", `{"model":"gpt-x","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "message_stop") || !strings.Contains(string(b), "streamed") {
		t.Fatalf("stream %d %s", resp.StatusCode, b)
	}

	resp = post(t, srv.URL+"/p/oa/v1/messages/count_tokens", `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "input_tokens") || up.hits.Load() != 2 {
		t.Fatalf("count_tokens %d %s hits %d", resp.StatusCode, b, up.hits.Load())
	}
}

// Demotion terminates when model slots form a cycle and every candidate is dead.
func TestDemotionCycleTerminates(t *testing.T) {
	p := &providers.Provider{Name: "g", Models: map[string]string{"opus": "A", "sonnet": "B", "haiku": "A"}}
	r := newRouter("s")
	r.markDead(providers.Target{Provider: p, Model: "A"})
	r.markDead(providers.Target{Provider: p, Model: "B"})
	done := make(chan struct{})
	var ok bool
	go func() { _, ok = r.demotion(providers.Target{Provider: p, Model: "B"}); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("demotion never returned: the cyclic slot table spins forever")
	}
	if ok {
		t.Error("every slot is dead: demotion must report no usable target, not loop back to a dead one")
	}

	// With only A dead, demotion must reach the live B slot despite the earlier cycle.
	r2 := newRouter("s")
	r2.markDead(providers.Target{Provider: p, Model: "A"})
	d, ok := r2.demotion(providers.Target{Provider: p, Model: "A"})
	if !ok || d.Model != "B" {
		t.Errorf("demotion(A) = %+v, %v; want B, true", d, ok)
	}
}

func okMsg(w http.ResponseWriter, r *http.Request, _ int) {
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/chat/completions") {
		io.WriteString(w, `{"id":"c","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`)
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
				hdr := map[string]string{"User-Agent": "client-ua/9", proxy.SessionHeader: "sess-leak"}
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
	srv := setup(t, `{"default":"an","providers":{"anthropic":{"an":`+anthropicProvider(up.URL, "AN_KEY", `{"opus":"m"}`)+`}}}`)
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

func TestParseHeadersWarnsOnce(t *testing.T) {
	var log strings.Builder
	got := proxy.ParseHeaders("A: 1\nbad name: x\nAuthorization: y\nB: v\x00\n", &log)
	if len(got) != 1 || got["A"] != "1" {
		t.Errorf("got %v", got)
	}
	if strings.Count(log.String(), "\n") != 1 {
		t.Errorf("want one warning, got %q", log.String())
	}
}

func checkAttribution(t *testing.T, h http.Header, agent string) {
	ua := h.Values("User-Agent")
	if len(ua) != 1 || !strings.HasPrefix(ua[0], "claude-code/") {
		t.Fatalf("User-Agent = %q, want one claude-code/<version> value", ua)
	}
	if agent != "" && !strings.HasSuffix(ua[0], " "+agent) {
		t.Errorf("User-Agent %q lacks agent %q", ua[0], agent)
	}
	if agent == "" && strings.Contains(strings.TrimSpace(ua[0]), " ") {
		t.Errorf("User-Agent %q has an agent part without the header", ua[0])
	}
	for _, k := range []string{proxy.AgentHeader, proxy.SessionHeader, proxy.ClientHeader} {
		if v := h.Get(k); v != "" {
			t.Errorf("%s reached upstream: %q", k, v)
		}
	}
	if headerHolds(h, "client-ua/9") || headerHolds(h, "sess-leak") || headerHolds(h, secret) {
		t.Errorf("client-side value leaked upstream: %v", h)
	}
}

// A stream read failure before any output must permit failover instead of committing an empty 200.
func TestStreamReadErrorBeforeOutputFailsOver(t *testing.T) {
	up1 := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	})
	up2 := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"from-up2\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, `{"default":"up1","fallback":["up2"],"providers":{"openai":{`+
		`"up1":{"protocol":"openai","base_url":"`+up1.URL+`","auth":{"type":"key","env":"UP1_KEY"},"models":{"opus":"m1"}},`+
		`"up2":{"protocol":"openai","base_url":"`+up2.URL+`","auth":{"type":"key","env":"UP2_KEY"},"models":{"opus":"m2"}}}}}`)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "from-up2") || up2.hits.Load() != 1 {
		t.Fatalf("status %d body %q up2 hits %d: want 200 with from-up2 after failover, up2 hit once", resp.StatusCode, b, up2.hits.Load())
	}

	// up1 alone, with no fallback: the same early read error must surface as a 502, not an empty 200.
	srv2 := setup(t, `{"default":"up1","providers":{"openai":{`+
		`"up1":{"protocol":"openai","base_url":"`+up1.URL+`","auth":{"type":"key","env":"UP1_KEY"},"models":{"opus":"m1"}}}}}`)
	resp2 := post(t, srv2.URL+"/p/up1/v1/messages", `{"model":"m1","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusBadGateway || len(b2) == 0 {
		t.Fatalf("status %d body %q: want 502 with an error body, not an empty 200", resp2.StatusCode, b2)
	}
}

func TestIdleBodyCutsWedgedStream(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	b := newIdleBody(pr, 20*time.Millisecond)
	if _, err := b.Read(make([]byte, 1)); err == nil {
		t.Fatal("read on a silent body should fail after the idle cutoff")
	}
}

func TestServeCanceledContextDoesNotPublishState(t *testing.T) {
	vaulttest.Use(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Serve(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve with a canceled context = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir(), "router.json")); !os.IsNotExist(err) {
		t.Fatalf("router.json must not be published for a canceled context: %v", err)
	}
}

func TestServeStateAndEnsure(t *testing.T) {
	vaulttest.Use(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, 0) }()
	var st proxy.State
	for range 100 {
		if s, ok := proxy.Running(context.Background()); ok {
			st = s
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Port == 0 || st.PID != os.Getpid() {
		t.Fatalf("router never became healthy: %+v", st)
	}
	base, err := proxy.Ensure(context.Background())
	if err != nil || base != proxy.Base(st.Port) {
		t.Fatalf("proxy.Ensure = %q, %v", base, err)
	}
	s1, _ := proxy.ClientSecret()
	s2, _ := proxy.ClientSecret()
	_, err = os.Stat(filepath.Join(paths.StateDir(), "router.secret"))
	if !os.IsNotExist(err) || s1 == "" || s1 != s2 || len(s1) != 64 {
		t.Fatalf("secret %q/%q (vault-sealed, no file) stat err %v", s1, s2, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir(), "router.json")); !os.IsNotExist(err) {
		t.Fatalf("router.json not removed: %v", err)
	}
}

// A stream past the drain is closed and Serve still returns nil: exiting 1 there reported every busy stop as a failure.
func TestServeClosesStreamsPastTheDrain(t *testing.T) {
	old := proxy.ShutdownDrain
	proxy.ShutdownDrain = 100 * time.Millisecond
	t.Cleanup(func() { proxy.ShutdownDrain = old })
	release := make(chan struct{})
	defer close(release)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: ping\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	vaulttest.Use(t)
	overlay := fmt.Sprintf(`{"default":"up1","providers":{"anthropic":{"up1":{"protocol":"anthropic","base_url":%q,"auth":{"type":"none"},"models":{"opus":"m"}}}}}`, up.URL)
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, 0) }()
	var st proxy.State
	for i := 0; i < 100 && st.Port == 0; i++ {
		st, _ = proxy.Running(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
	s, _ := proxy.ClientSecret()
	req := testRequest(t, http.MethodPost, proxy.Base(st.Port)+"/p/up1/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("x-api-key", s)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve with a stream past the drain = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve waited on the stream past the drain budget")
	}
}

func TestUnroutableLogin(t *testing.T) {
	p := &providers.Provider{Name: "personal", Provider: "github", Protocol: providers.ProtocolOpenAI, BaseURL: "https://x", Auth: providers.Auth{Type: providers.AuthCLI, Login: "copilot"}}
	if why := unroutable(p, true); !strings.Contains(why, "copilot sign-in") {
		t.Errorf("a vendor login must never receive a forwarded client credential: %q", why)
	}
}

// A hard-quota 429 demotes within the provider and skips the denied model on subsequent requests.
func TestQuotaDeadModelDemotesWithinProvider(t *testing.T) {
	var up *upstream
	up = newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if (*up.body.Load())["model"] == "m1-opus" {
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"code":429,"message":"You exceeded your current quota. Quota exceeded for metric: generate_content_free_tier_requests, limit: 0, model: m1-opus"}}`)
			return
		}
		ok(w, r, n)
	})
	t.Setenv("UP1_KEY", "k1")
	srv := setup(t, `{"default":"up1","fallback":[],"providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1-opus","sonnet":"m1-sonnet","haiku":"m1-haiku"}`)+`}}}`)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d %s", resp.StatusCode, b)
	}
	if m := (*up.body.Load())["model"]; m != "m1-sonnet" || up.hits.Load() != 2 {
		t.Fatalf("demoted model = %v after %d hits, want m1-sonnet after 2", m, up.hits.Load())
	}
	// The dead model is remembered: the second request goes straight to the demoted slot.
	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("second status %d", resp.StatusCode)
	}
	if m := (*up.body.Load())["model"]; m != "m1-sonnet" || up.hits.Load() != 3 {
		t.Fatalf("second request model = %v after %d hits, want m1-sonnet after 3 (no retry of the dead model)", m, up.hits.Load())
	}
}

// An absent credential is rechecked immediately so a newly added fallback becomes available.
func TestCredentialPresentDoesNotCacheAbsence(t *testing.T) {
	r := newRouter("s")
	const envVar = "ROUTER_TEST_CRED_ABSENT_THEN_PRESENT"
	os.Unsetenv(envVar)
	p := &providers.Provider{Name: "envprov", Auth: providers.Auth{Type: providers.AuthKey, Header: providers.HeaderXAPIKey, Env: envVar}}
	if r.credentialPresent(p) {
		t.Fatal("credential reported present with no env var set")
	}
	t.Setenv(envVar, "v")
	if !r.credentialPresent(p) {
		t.Fatal("newly set credential must be seen on the next call, not cached as absent")
	}
}

// A transient 429 (no quota marker) on a provider with lower slots must not demote: same model, provider cooled.
func TestTransientRateLimitDoesNotDemote(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	})
	t.Setenv("UP1_KEY", "k1")
	srv := setup(t, `{"default":"up1","fallback":[],"providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1-opus","sonnet":"m1-sonnet"}`)+`}}}`)
	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || !strings.Contains(string(b), "slow down") || up.hits.Load() != 1 {
		t.Fatalf("transient 429 must be relayed untouched: %d %s hits=%d", resp.StatusCode, b, up.hits.Load())
	}
}

// Window exhaustion cools the provider without marking a model dead.
func TestWindowExhaustionCoolsButDoesNotMarkDead(t *testing.T) {
	p := &providers.Provider{Name: "up1", Models: map[string]string{"opus": "m1-opus", "sonnet": "m1-sonnet"}}
	target := providers.Target{Provider: p, Model: "m1-opus"}
	r := newRouter("s")
	resp := &http.Response{
		StatusCode: 429,
		Header:     http.Header{"Retry-After": []string{"60"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"usage limit reached, try again in 60 seconds"}}`)),
	}
	moveOn, hard := r.failover(resp, target, false)
	if hard {
		t.Error("a window-exhaustion 429 must not be classified hard (ModelDenied), only IsQuota")
	}
	if !moveOn {
		t.Error("a non-last retryable 429 must still move on to the next ladder entry")
	}
	if r.isDead(target) {
		t.Error("failover must not mark the model dead on a window-exhaustion 429")
	}
	if _, cooling := r.cool[p.Name]; !cooling {
		t.Error("failover must cool the provider on a window-exhaustion 429")
	}
}

// Account exhaustion overrides model-denial markers and cools the provider without demotion.
func TestHardAccountExhaustion402CoolsButDoesNotMarkDead(t *testing.T) {
	p := &providers.Provider{Name: "up1", Models: map[string]string{"opus": "m1-opus", "sonnet": "m1-sonnet"}}
	target := providers.Target{Provider: p, Model: "m1-opus"}
	r := newRouter("s")
	resp := &http.Response{
		StatusCode: 429,
		Header:     http.Header{"Retry-After": []string{"60"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"402 payment required: insufficient_quota"}}`)),
	}
	moveOn, hard := r.failover(resp, target, false)
	if hard {
		t.Error("a 402+insufficient_quota body must not be classified hard (ModelDenied vetoed by hardAccountRE)")
	}
	if !moveOn {
		t.Error("a non-last retryable 429 must still move on to the next ladder entry")
	}
	if r.isDead(target) {
		t.Error("failover must not mark the model dead on account exhaustion")
	}
	if _, cooling := r.cool[p.Name]; !cooling {
		t.Error("failover must cool the provider on account exhaustion")
	}
}

// bumpMtime sets an exact timestamp to exercise second-granularity registry comparisons.
func bumpMtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// Failed registry loads must not advance the cached timestamp or hide a later fix at the same timestamp.
func TestRegistryRetriesLoadAfterAFailureRatherThanCachingIt(t *testing.T) {
	vaulttest.Use(t)
	const good = `{"providers":{}}`
	vaulttest.Put(t, vault.Overlay, "providers", good)
	r := newRouter(secret)
	r.log = io.Discard

	reg1, err := r.registry()
	if err != nil {
		t.Fatalf("first registry() = %v", err)
	}
	stamp1 := r.regStamp
	badStamp := stamp1.Add(time.Second)

	// Corrupt the overlay and force the vault file's mtime forward so registry() sees a change.
	vaulttest.Put(t, vault.Overlay, "providers", "not json")
	bumpMtime(t, vault.Path(), badStamp)

	reg2, err := r.registry()
	if err != nil {
		t.Fatalf("second registry() = %v, want it to keep the previous registry on a bad overlay", err)
	}
	if reg2 != reg1 {
		t.Error("a failed Load must keep serving the previous *Registry, not swap in a different one")
	}
	if r.regStamp.Equal(badStamp) {
		t.Fatal("regStamp advanced on a failed Load; a later fix landing at this same mtime would never be retried")
	}

	// Restore the overlay at the failed load timestamp; the unchanged cached timestamp must force a retry.
	vaulttest.Put(t, vault.Overlay, "providers", good)
	bumpMtime(t, vault.Path(), badStamp)

	reg3, err := r.registry()
	if err != nil {
		t.Fatalf("third registry() = %v, want the retried Load to succeed", err)
	}
	if reg3 == reg1 {
		t.Error("registry() served the stale cached registry instead of retrying Load at the same mtime")
	}
}

// preFixRegistry reproduces the old unconditional timestamp advance for the regression proof.
func preFixRegistry(r *Router) (*providers.Registry, error) {
	var stamp time.Time
	if fi, err := os.Stat(vault.Path()); err == nil {
		stamp = fi.ModTime()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reg != nil && stamp.Equal(r.regStamp) {
		return r.reg, nil
	}
	reg, err := providers.Load()
	if err != nil {
		if r.reg == nil {
			return nil, err
		}
		reg = r.reg
	}
	r.reg, r.regStamp = reg, stamp // pre-fix bug: stamp advances even when Load failed above
	return reg, nil
}

// The old registry implementation must serve stale data after the same-timestamp overlay repair.
func TestRegistryPreFixLogicCachedTheBadMtime(t *testing.T) {
	vaulttest.Use(t)
	const good = `{"providers":{}}`
	vaulttest.Put(t, vault.Overlay, "providers", good)
	r := newRouter(secret)
	r.log = io.Discard

	reg1, err := preFixRegistry(r)
	if err != nil {
		t.Fatalf("first call = %v", err)
	}
	badStamp := r.regStamp.Add(time.Second)

	vaulttest.Put(t, vault.Overlay, "providers", "not json")
	bumpMtime(t, vault.Path(), badStamp)
	if _, err := preFixRegistry(r); err != nil {
		t.Fatalf("second call = %v", err)
	}
	if !r.regStamp.Equal(badStamp) {
		t.Fatal("replica no longer reproduces the bug: regStamp did not advance on the failed Load")
	}

	vaulttest.Put(t, vault.Overlay, "providers", good)
	bumpMtime(t, vault.Path(), badStamp) // same mtime the failed attempt already claimed as "seen"
	reg3, err := preFixRegistry(r)
	if err != nil {
		t.Fatalf("third call = %v", err)
	}
	if reg3 != reg1 {
		t.Fatal("replica no longer reproduces the bug: it retried Load instead of serving the stale cache")
	}
}

func TestDatabricksGatewayWireReachesUpstream(t *testing.T) {
	up := newUpstream(t, ok)
	t.Setenv("DBX_KEY", "dbx-token")
	row := `{"preset":"databricks","host":"https://example.cloud.databricks.com","base_url":"` + up.URL +
		`","auth":{"type":"key","env":"DBX_KEY"},"options":{"wire":"ai-gateway"}}`
	srv := setup(t, `{"default":"dbx","providers":{"databricks":{"dbx":`+row+`}}}`)

	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	wire := reg.Providers["dbx"].Models["opus"]
	if wire != "system.ai.claude-opus-5-5" {
		t.Fatalf("registry opus slot = %q, want system.ai.claude-opus-5-5", wire)
	}
	resp := post(t, srv.URL+"/p/dbx/v1/messages", `{"model":"`+wire+`","max_tokens":5}`, nil)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	got, body := up.last.Load(), *up.body.Load()
	if h := got.Header.Get("x-databricks-use-coding-agent-mode"); h != "true" {
		t.Errorf("coding-agent header = %q, headers %v", h, got.Header)
	}
	if body["model"] != wire {
		t.Errorf("model = %v", body["model"])
	}
	if a := got.Header.Get("Authorization"); a != "Bearer dbx-token" {
		t.Errorf("authorization = %q", a)
	}
}

// A 400 "prompt is too long" is not retried and is rewritten to invalid_request_error quoting only the limit text.
func TestFailoverPromptTooLong(t *testing.T) {
	p := &providers.Provider{Name: "up1", Models: map[string]string{"opus": "m1"}}
	body := `{"message":"prompt is too long: 1177204 tokens > 1000000 maximum","secret":"leak-me"}`
	resp := &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	moveOn, _ := newRouter("s").failover(resp, providers.Target{Provider: p, Model: "m1"}, false)
	if moveOn {
		t.Fatal("prompt-too-long must not fail over")
	}
	got, _ := io.ReadAll(resp.Body)
	var e struct {
		Error struct{ Type, Message string }
	}
	_ = json.Unmarshal(got, &e)
	if e.Error.Type != "invalid_request_error" || e.Error.Message != "prompt is too long: 1177204 tokens > 1000000 maximum" ||
		strings.Contains(string(got), "leak-me") {
		t.Errorf("body = %s", got)
	}
}

// TestFailoverPromptTooLongNotOtherStatus pins that only a 400 is classified, and 400s without the text pass through.
func TestFailoverPromptTooLongNotOtherStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"500 with text":    {500, `{"message":"prompt is too long"}`},
		"400 other reason": {400, `{"message":"bad field"}`},
	} {
		t.Run(name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}
			if tooLong(resp) {
				t.Fatal("must not classify")
			}
			got, _ := io.ReadAll(resp.Body)
			if string(got) != tc.body {
				t.Errorf("body altered: %s", got)
			}
		})
	}
}

// TestChatPathPromptTooLong checks the classified limit text from chat translation.
func TestChatPathPromptTooLong(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"prompt is too long: 1177204 tokens > 1000000 maximum","secret":"leak-me"}}`)
	})
	t.Setenv("OA_KEY", "oa")
	srv := setup(t, `{"default":"oa","providers":{"openai":{"oa":{"protocol":"openai","base_url":"`+up.URL+`/v1","auth":{"type":"key","env":"OA_KEY"},"models":{"opus":"gpt-x"}}}}}`)
	resp := post(t, srv.URL+"/p/oa/v1/messages", `{"model":"gpt-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(resp.Body)
	var e struct {
		Error struct{ Type, Message string }
	}
	_ = json.Unmarshal(b, &e)
	if resp.StatusCode != 400 || e.Error.Type != "invalid_request_error" ||
		e.Error.Message != "prompt is too long: 1177204 tokens > 1000000 maximum" || strings.Contains(string(b), "leak-me") {
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
}

// Regression: a side proxy (--serve --port N) must neither overwrite nor delete a live daemon's router.json.
func TestServeSideProxyKeepsDaemonState(t *testing.T) {
	vaulttest.Use(t)
	daemon := proxy.State{PID: os.Getppid(), Port: 1, Started: time.Now().UTC()} // a live pid on another port
	data, _ := json.Marshal(daemon)
	if err := paths.AtomicWrite(filepath.Join(paths.StateDir(), "router.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	listening := make(chan struct{})
	orig := serveListening
	serveListening = func() { close(listening) }
	t.Cleanup(func() { serveListening = orig })
	go func() { done <- Serve(ctx, 0) }()
	select {
	case <-listening:
	case err := <-done:
		t.Fatalf("Serve exited before listening: %v", err)
	}
	if cur, err := proxy.ReadState(); err != nil || cur.PID != daemon.PID || cur.Port != daemon.Port {
		t.Fatalf("while serving, router.json = %+v, %v; want the daemon's %+v", cur, err, daemon)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if cur, err := proxy.ReadState(); err != nil || cur.PID != daemon.PID {
		t.Fatalf("after the side proxy exits, router.json = %+v, %v; want the daemon's kept", cur, err)
	}
}

func TestSessionTokenCannotReachOtherProfiles(t *testing.T) {
	up1 := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(429) })
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, failoverOverlay(up1.URL, up2.URL))
	req := testRequest(t, http.MethodPost, srv.URL+proxy.SessionPath, strings.NewReader(`{"profile":"up1","pid":`+strconv.Itoa(os.Getpid())+`}`))
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
