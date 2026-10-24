package router

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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
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
	return fmt.Sprintf(`{"kind":"anthropic","base_url":%q,"auth":{"type":"x-api-key","env":%q},"models":%s}`, url, env, models)
}

func TestAuthAndHostRejection(t *testing.T) {
	up := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	srv := setup(t, `{"default":"up1","providers":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}`)
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
	srv := setup(t, `{"default":"up1","providers":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"claude-opus-5"}`)+`}}`)

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
	srv := setup(t, `{"default":"up1","providers":{
		"up1":`+anthropicProvider(up1.URL, "UP1_KEY", `{"opus":"m1"}`)+`,
		"up2":{"kind":"anthropic","base_url":"`+up2.URL+`","auth":{"type":"bearer","env":"UP2_KEY"},"models":{"opus":"m2"}}}}`)

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
	srv := setup(t, `{"default":"sub","providers":{"sub":{"kind":"anthropic","base_url":"`+up.URL+`","auth":{"type":"passthrough"},"models":{"opus":"m"}}}}`)
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
	gem := newUpstream(t, ok)
	t.Setenv("GEM_KEY", "gem-key")
	srv := setup(t, `{"default":"sub","fallback":["gem"],"providers":{
		"sub":{"kind":"anthropic","base_url":"`+sub.URL+`","auth":{"type":"passthrough"},"models":{"opus":"m"}},
		"gem":{"kind":"anthropic","base_url":"`+gem.URL+`","auth":{"type":"bearer","env":"GEM_KEY"},"models":{"opus":"g"}}}}`)
	login := map[string]string{"X-Api-Key": "", ClientHeader: secret, "Authorization": "Bearer oauth-tok"}

	if resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, login); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h := sub.last.Load().Header
	if h.Get("Authorization") != "Bearer oauth-tok" || h.Get(ClientHeader) != "" || h.Get("X-Api-Key") != "" {
		t.Errorf("passthrough upstream headers: %v", h)
	}
	// Failover to an API-key provider sends that provider's key, never the client's login.
	if resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, login); resp.StatusCode != 200 {
		t.Fatalf("failover status %d", resp.StatusCode)
	}
	if a := gem.last.Load().Header.Get("Authorization"); a != "Bearer gem-key" {
		t.Errorf("fallback authorization = %q", a)
	}
	if resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, map[string]string{"X-Api-Key": "", ClientHeader: "wrong"}); resp.StatusCode != 401 {
		t.Errorf("wrong ClientHeader secret: status %d", resp.StatusCode)
	}
}

func failoverOverlay(up1, up2 string) string {
	return `{"default":"up1","fallback":["up2"],"providers":{
		"up1":` + anthropicProvider(up1, "UP1_KEY", `{"opus":"m1-opus","haiku":"m1-haiku"}`) + `,
		"up2":` + anthropicProvider(up2, "UP2_KEY", `{"opus":"m2-opus","haiku":"m2-haiku"}`) + `}}`
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
	overlay, _ := json.Marshal(map[string]any{"default": name, "providers": map[string]any{name: map[string]any{
		"kind": "anthropic", "base_url": up.URL, "auth": map[string]any{"type": "bearer", "command": cmd, "ttl": 3600},
		"models": map[string]string{"opus": "m"}}}})
	srv := setup(t, string(overlay))

	resp := post(t, srv.URL+"/p/"+name+"/v1/messages", `{"model":"m"}`, nil)
	if resp.StatusCode != 200 || up.hits.Load() != 2 {
		t.Fatalf("status %d hits %d, want 200 after one replay", resp.StatusCode, up.hits.Load())
	}
}

// A Refresh failure (e.g. a revoked env-var credential with no auth.command to re-run) must still drop the
// cached presence: the router must re-probe the provider on the very next call, not keep believing for up
// to credTTL that a now-broken credential is present.
func TestAuthRefreshFailureInvalidatesCachedCredential(t *testing.T) {
	const envVar = "ROUTER_TEST_REFRESH_FAILURE_CRED"
	t.Setenv(envVar, "v1")
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		os.Unsetenv(envVar) // simulate the credential being revoked between the first 401 and the Refresh replay
		w.WriteHeader(http.StatusUnauthorized)
	})
	p := &providers.Provider{Name: "refreshfail", BaseURL: up.URL,
		Auth: providers.Auth{Type: providers.AuthAPIKey, Env: envVar}}

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

// storeCredentialPresent must drop a probe's result once its generation is stale: an invalidation that
// landed after the probe started (and before it stores) must win, so the cache stays empty.
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
	srv := setup(t, `{"default":"oa","providers":{"oa":{"kind":"openai","base_url":"`+up.URL+`/v1","auth":{"type":"bearer","env":"OA_KEY"},"models":{"opus":"gpt-x"}}}}`)

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

// A slot table like opus=A sonnet=B haiku=A demoted A->B->A->B... forever before demotion tracked
// models it had already visited; with every slot dead, it must give up instead of spinning.
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

	// With only A dead, B's one live rung is A's neighbor, haiku; demoting from A must land on B instead
	// of being blocked by the earlier cycle guard.
	r2 := newRouter("s")
	r2.markDead(providers.Target{Provider: p, Model: "A"})
	d, ok := r2.demotion(providers.Target{Provider: p, Model: "A"})
	if !ok || d.Model != "B" {
		t.Errorf("demotion(A) = %+v, %v; want B, true", d, ok)
	}
}

// tryOpenAI used to write the 200 status before translating the stream; a read error on the very
// first chunk (upstream hangs up before any SSE event) then left a committed empty 200, which the
// client treats as a successful but silent response instead of a failure the router can fail over.
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
	srv := setup(t, `{"default":"up1","fallback":["up2"],"providers":{`+
		`"up1":{"kind":"openai","base_url":"`+up1.URL+`","auth":{"type":"bearer","env":"UP1_KEY"},"models":{"opus":"m1"}},`+
		`"up2":{"kind":"openai","base_url":"`+up2.URL+`","auth":{"type":"bearer","env":"UP2_KEY"},"models":{"opus":"m2"}}}}`)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "from-up2") || up2.hits.Load() != 1 {
		t.Fatalf("status %d body %q up2 hits %d: want 200 with from-up2 after failover, up2 hit once", resp.StatusCode, b, up2.hits.Load())
	}

	// up1 alone, with no fallback: the same early read error must surface as a 502, not an empty 200.
	srv2 := setup(t, `{"default":"up1","providers":{`+
		`"up1":{"kind":"openai","base_url":"`+up1.URL+`","auth":{"type":"bearer","env":"UP1_KEY"},"models":{"opus":"m1"}}}}`)
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Serve(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve with a canceled context = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(stateFile()); !os.IsNotExist(err) {
		t.Fatalf("router.json must not be published for a canceled context: %v", err)
	}
}

func TestServeStateAndEnsure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, 0) }()
	var st State
	for range 100 {
		if s, ok := Running(context.Background()); ok {
			st = s
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Port == 0 || st.PID != os.Getpid() {
		t.Fatalf("router never became healthy: %+v", st)
	}
	base, err := Ensure(context.Background())
	if err != nil || base != Base(st.Port) {
		t.Fatalf("Ensure = %q, %v", base, err)
	}
	s1, _ := ClientSecret()
	s2, _ := ClientSecret()
	fi, err := os.Stat(secretFile())
	if err != nil || s1 == "" || s1 != s2 || len(s1) != 64 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret %q/%q mode %v err %v", s1, s2, fi.Mode(), err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateFile()); !os.IsNotExist(err) {
		t.Fatalf("router.json not removed: %v", err)
	}
}

// A stream past the drain is closed and Serve still returns nil: exiting 1 there reported every busy stop as a failure.
func TestServeClosesStreamsPastTheDrain(t *testing.T) {
	old := shutdownDrain
	shutdownDrain = 100 * time.Millisecond
	t.Cleanup(func() { shutdownDrain = old })
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
	overlay := fmt.Sprintf(`{"default":"up1","providers":{"up1":{"kind":"anthropic","base_url":%q,"auth":{"type":"none"},"models":{"opus":"m"}}}}`, up.URL)
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, 0) }()
	var st State
	for i := 0; i < 100 && st.Port == 0; i++ {
		st, _ = Running(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
	s, _ := ClientSecret()
	req := testRequest(t, http.MethodPost, Base(st.Port)+"/p/up1/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
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
	p := &providers.Provider{Name: "github-copilot", Kind: providers.DialectOpenAI, BaseURL: "https://x", Auth: providers.Auth{Type: providers.AuthPassthrough, Login: "copilot"}}
	if why := unroutable(p, true); !strings.Contains(why, "copilot sign-in") {
		t.Errorf("a vendor login must never receive a forwarded client credential: %q", why)
	}
}

// A hard-quota 429 (the key cannot use that model at all) demotes to the provider's next slot instead of failing
// the ladder, and the dead model is skipped up front on the next request.
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
	srv := setup(t, `{"default":"up1","fallback":[],"providers":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1-opus","sonnet":"m1-sonnet","haiku":"m1-haiku"}`)+`}}`)

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

// An absent credential must never be cached: a fallback candidate's credential added mid-session must be
// seen on the very next call, not hidden until credTTL (60s) expires.
func TestCredentialPresentDoesNotCacheAbsence(t *testing.T) {
	r := newRouter("s")
	const envVar = "ROUTER_TEST_CRED_ABSENT_THEN_PRESENT"
	os.Unsetenv(envVar)
	p := &providers.Provider{Name: "envprov", Auth: providers.Auth{Type: providers.AuthAPIKey, Env: envVar}}
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
	srv := setup(t, `{"default":"up1","fallback":[],"providers":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1-opus","sonnet":"m1-sonnet"}`)+`}}`)
	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || !strings.Contains(string(b), "slow down") || up.hits.Load() != 1 {
		t.Fatalf("transient 429 must be relayed untouched: %d %s hits=%d", resp.StatusCode, b, up.hits.Load())
	}
}

// bumpMtime forces path's mtime to an exact, test-controlled value instead of relying on wall-clock
// drift between calls (which registry()'s second-granularity mtime comparison could mask).
func bumpMtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// TestRegistryRetriesLoadAfterAFailureRatherThanCachingIt: regStamp must advance only on a successful
// Load. If a failed Load's mtime were cached into regStamp anyway, a later overlay fix landing at that
// same (second-granularity) mtime would be mistaken for "already seen" and never retried.
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

	// Restore a good overlay at the SAME (bad) mtime the failed attempt left behind: with the fix,
	// regStamp never advanced past stamp1, so this call still differs from regStamp and retries Load.
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

// preFixRegistry reproduces registry() as it stood at HEAD before this session's fix (regStamp
// advances unconditionally, even on a failed Load) — a direct edit of router.go to prove teeth was
// blocked by the sandbox's destructive-edit guard on this shared live tree, so it is reproduced here.
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

// TestRegistryPreFixLogicCachedTheBadMtime is the teeth proof for the test above: replay the exact
// same good/bad/good-at-same-mtime sequence through the pre-fix body and show it serves the stale
// registry at the third call — i.e. TestRegistryRetriesLoadAfterAFailureRatherThanCachingIt would
// have failed against it.
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
