package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/hosttest"
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
	return fmt.Sprintf(
		`{"protocol":"anthropic","base_url":%q,"auth":{"type":"key","header":"x-api-key","env":%q},"models":%s}`, url,
		env, models)
}

func TestAuthAndHostRejection(t *testing.T) {
	up := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	srv := setup(t,
		`{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}}`)
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
	srv := setup(t,
		`{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY",
			`{"opus":"claude-opus-5"}`)+`}}}`,
	)

	resp := post(t, srv.URL+"/p/up1/v1/messages?beta=true",
		`{"model":"claude-opus-5[1m]","max_tokens":5,"messages":[{"role":"user","content":"<hi>"}]}`,
		map[string]string{"Anthropic-Beta": "context-1m-2025-08-07", "Anthropic-Version": "2023-06-01"})
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	checkAnthropicUpstream(t, up.last.Load(), *up.body.Load())

	resp = post(t, srv.URL+"/p/up1/v1/messages/count_tokens", `{"model":"claude-opus-5"}`, nil)
	if resp.StatusCode != 200 || up.last.Load().URL.Path != "/v1/messages/count_tokens" {
		t.Errorf("count_tokens: %d %s", resp.StatusCode, up.last.Load().URL.Path)
	}
}

// checkAnthropicUpstream asserts what the upstream saw for the /v1/messages passthrough request.
func checkAnthropicUpstream(t *testing.T, got *http.Request, body map[string]any) {
	t.Helper()
	if got.URL.Path != "/v1/messages" || got.URL.RawQuery != "beta=true" {
		t.Errorf("upstream url = %s", got.URL)
	}
	if got.Header.Get("X-Api-Key") != "provider-key" || got.Header.Get("Authorization") != "" {
		t.Errorf("auth headers: x-api-key=%q authorization=%q", got.Header.Get("X-Api-Key"),
			got.Header.Get("Authorization"))
	}
	if got.Header.Get("Anthropic-Beta") != "context-1m-2025-08-07" || got.Header.Get("Anthropic-Version") == "" {
		t.Errorf("anthropic headers not forwarded: %v", got.Header)
	}
	if body["model"] != "claude-opus-5" || body["max_tokens"] != float64(5) {
		t.Errorf("body = %v", body)
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
	srv := setup(t,
		`{"default":"sub","providers":{"anthropic":{"sub":{"protocol":"anthropic","base_url":"`+up.URL+`","auth":{"type":"cli"},"models":{"opus":"m"}}}}}`)
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
	overlay, _ := json.Marshal(
		map[string]any{"default": name, "providers": map[string]any{"anthropic": map[string]any{name: map[string]any{
			"protocol": "anthropic", "base_url": up.URL, "auth": map[string]any{"type": "key", "header": "bearer", "command": cmd, "ttl": 3600},
			"models": map[string]string{"opus": "m"}}}}},
	)
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
		io.WriteString(w,
			`{"id":"c1","object":"chat.completion","model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	})
	t.Setenv("OA_KEY", "oa")
	srv := setup(t,
		`{"default":"oa","providers":{"openai":{"oa":{"protocol":"openai","base_url":"`+up.URL+`/v1","auth":{"type":"key","env":"OA_KEY"},"models":{"opus":"gpt-x"}}}}}`)

	checkChatTranslation(t, srv.URL, up)

	up.handle = func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w,
			"data: {\"id\":\"c2\",\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"streamed\"}}]}\n\n")
		io.WriteString(w,
			"data: {\"id\":\"c2\",\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}
	checkChatStream(t, srv.URL)

	resp := post(t, srv.URL+"/p/oa/v1/messages/count_tokens",
		`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "input_tokens") || up.hits.Load() != 2 {
		t.Fatalf("count_tokens %d %s hits %d", resp.StatusCode, b, up.hits.Load())
	}
}

// checkChatTranslation posts a non-stream message and asserts the upstream chat call and the translated reply.
func checkChatTranslation(t *testing.T, base string, up *upstream) {
	t.Helper()
	resp := post(t, base+"/p/oa/v1/messages",
		`{"model":"gpt-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || up.last.Load().URL.Path != "/v1/chat/completions" ||
		up.last.Load().Header.Get("Authorization") != "Bearer oa" {
		t.Fatalf("status %d body %s path %s", resp.StatusCode, b, up.last.Load().URL.Path)
	}
	var msg struct {
		Type    string `json:"type"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &msg); err != nil || msg.Type != "message" || len(msg.Content) == 0 ||
		msg.Content[0].Text != "hello" {
		t.Fatalf("translated body %s", b)
	}
}

// checkChatStream posts a streaming message and asserts the translated event stream.
func checkChatStream(t *testing.T, base string) {
	t.Helper()
	resp := post(t, base+"/p/oa/v1/messages",
		`{"model":"gpt-x","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "message_stop") ||
		!strings.Contains(string(b), "streamed") {
		t.Fatalf("stream %d %s", resp.StatusCode, b)
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
		io.WriteString(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"from-up2\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, `{"default":"up1","fallback":["up2"],"providers":{"openai":{`+
		`"up1":{"protocol":"openai","base_url":"`+up1.URL+`","auth":{"type":"key","env":"UP1_KEY"},"models":{"opus":"m1"}},`+
		`"up2":{"protocol":"openai","base_url":"`+up2.URL+`","auth":{"type":"key","env":"UP2_KEY"},"models":{"opus":"m2"}}}}}`)

	resp := post(t, srv.URL+"/p/up1/v1/messages",
		`{"model":"m1","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "from-up2") || up2.hits.Load() != 1 {
		t.Fatalf("status %d body %q up2 hits %d: want 200 with from-up2 after failover, up2 hit once", resp.StatusCode,
			b, up2.hits.Load())
	}

	// up1 alone, with no fallback: the same early read error must surface as a 502, not an empty 200.
	srv2 := setup(t, `{"default":"up1","providers":{"openai":{`+
		`"up1":{"protocol":"openai","base_url":"`+up1.URL+`","auth":{"type":"key","env":"UP1_KEY"},"models":{"opus":"m1"}}}}}`)
	resp2 := post(t, srv2.URL+"/p/up1/v1/messages",
		`{"model":"m1","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
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

func TestUnroutableLogin(t *testing.T) {
	p := &providers.Provider{
		Name:     "personal",
		Provider: "github",
		Protocol: providers.ProtocolOpenAI,
		BaseURL:  "https://x",
		Auth:     providers.Auth{Type: providers.AuthCLI, Login: "copilot"},
	}
	if why := unroutable(p, true); !strings.Contains(why, "copilot sign-in") {
		t.Errorf("a vendor login must never receive a forwarded client credential: %q", why)
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
	if e.Error.Type != "invalid_request_error" ||
		e.Error.Message != "prompt is too long: 1177204 tokens > 1000000 maximum" ||
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
			resp := &http.Response{
				StatusCode: tc.status,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}
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
		io.WriteString(w,
			`{"error":{"message":"prompt is too long: 1177204 tokens > 1000000 maximum","secret":"leak-me"}}`)
	})
	t.Setenv("OA_KEY", "oa")
	srv := setup(t,
		`{"default":"oa","providers":{"openai":{"oa":{"protocol":"openai","base_url":"`+up.URL+`/v1","auth":{"type":"key","env":"OA_KEY"},"models":{"opus":"gpt-x"}}}}}`)
	resp := post(t, srv.URL+"/p/oa/v1/messages",
		`{"model":"gpt-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, nil)
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

func TestSessionTokenCannotReachOtherProfiles(t *testing.T) {
	up1 := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(429) })
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, failoverOverlay(up1.URL, up2.URL))
	req := testRequest(t, http.MethodPost, srv.URL+gateway.SessionPath,
		strings.NewReader(`{"profile":"up1","pid":`+strconv.Itoa(os.Getpid())+`}`))
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

func TestLaunchIdentityReachesMintNotUpstream(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/p/prod/v1/messages", nil)
	req.Header.Set(gateway.IdentityHeader, providers.IdentitySP)
	if got := providers.IdentityFrom(withLaunchIdentity(req).Context()); got != providers.IdentitySP {
		t.Fatalf("identity on ctx = %q; want sp", got)
	}
	if got := providers.IdentityFrom(withLaunchIdentity(httptest.NewRequest(http.MethodPost, "/", nil)).Context()); got != "" {
		t.Errorf("no header set identity %q", got)
	}
	gateway.StripLauncherHeaders(req.Header)
	if v := req.Header.Get(gateway.IdentityHeader); v != "" {
		t.Errorf("identity header %q would go upstream", v)
	}
}

func TestScopedAuthDeadProviderReturnsForbidden(t *testing.T) {
	gate := useGate(t)
	up1 := newUpstream(t, ok)
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	var routed *Router
	srv := setupRouter(t, failoverOverlay(up1.URL, up2.URL), func(r *Router) { routed = r })
	token, err := routed.sessions.mint("up1", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	gate.premark("up1") // attempts drops up1; session confinement then removes up2.

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, map[string]string{"X-Api-Key": token})
	got := body(t, resp)
	if resp.StatusCode != http.StatusForbidden ||
		!strings.Contains(got, "no provider in this session's scope is usable") {
		t.Fatalf("auth-dead scoped request = %d %s, want 403 without a panic", resp.StatusCode, got)
	}
	if up1.hits.Load() != 0 || up2.hits.Load() != 0 {
		t.Fatalf("scoped request escaped to upstreams: up1=%d up2=%d", up1.hits.Load(), up2.hits.Load())
	}
}

// stalledUploadUpstream accepts headers but never reads the body. A small receive
// buffer keeps the large request in the write phase, before ResponseHeaderTimeout.
func stalledUploadUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	up.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew {
			if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
				t.Errorf("set stalled upstream receive buffer: %v", err)
			}
		}
	}
	up.Start()
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) }) // unblock handlers before httptest waits for them.
	return up
}

func TestStalledLargeUploadFailsOverWithinAttemptBound(t *testing.T) {
	previous := uploadTimeout
	uploadTimeout = 2 * time.Second // covers the healthy leg too: proxy and upstream each parse the 16 MiB body
	t.Cleanup(func() { uploadTimeout = previous })
	stalled := stalledUploadUpstream(t)
	healthy := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	var routed *Router
	setupRouter(t, failoverOverlay(stalled.URL, healthy.URL), func(r *Router) { routed = r })

	payload := `{"model":"m1-opus","messages":[{"role":"user","content":"` + strings.Repeat("x", 16<<20) + `"}]}`
	req := testRequest(t, http.MethodPost, "http://127.0.0.1/p/up1/v1/messages", strings.NewReader(payload))
	req.Header.Set("X-Api-Key", secret)
	var interruptedWrite atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err != nil {
				interruptedWrite.Store(true)
			}
		},
	}))
	reply := httptest.NewRecorder()
	routed.ServeHTTP(reply, req)
	if healthy.hits.Load() != 1 || reply.Code != http.StatusOK ||
		!strings.Contains(reply.Body.String(), `"type":"message"`) {
		t.Fatalf("stalled upload did not fail over within attempt bound: healthy hits=%d status=%d body=%s",
			healthy.hits.Load(), reply.Code, reply.Body.String())
	}
	if !interruptedWrite.Load() {
		t.Fatal("upload completed before cancellation; regression did not exercise a stalled write")
	}
}

// TestMain runs hermetically (hosttest) and treats this test binary as the proxy process.
func TestMain(m *testing.M) {
	gateway.HolderComm = func(int) string { return "claude-code" }
	os.Exit(hosttest.Main(m))
}
