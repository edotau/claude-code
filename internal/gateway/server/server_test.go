package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/tokenledger"
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
	t.Cleanup(func() { srv.Close(); r.bg.Wait() }) // a late state write must not race TempDir removal
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

func failoverOverlay(up1, up2 string) string {
	return `{"default":"up1","fallback":["up2"],"providers":{"anthropic":{
		"up1":` + anthropicProvider(up1, "UP1_KEY", `{"opus":"m1-opus","haiku":"m1-haiku"}`) + `,
		"up2":` + anthropicProvider(up2, "UP2_KEY", `{"opus":"m2-opus","haiku":"m2-haiku"}`) + `}}}`
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

func TestLaunchIdentityReachesMintNotUpstream(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/p/prod/v1/messages", nil)
	req.Header.Set(gateway.IdentityHeader, providers.IdentitySP)
	if got := providers.IdentityFrom(withLaunchIdentity(req).Context()); got != providers.IdentitySP {
		t.Fatalf("identity on ctx = %q; want sp", got)
	}
	if got := providers.IdentityFrom(withLaunchIdentity(httptest.NewRequest(http.MethodPost, "/", nil)).Context()); got != "" {
		t.Errorf("no header set identity %q", got)
	}
	stripLauncherHeaders(req.Header)
	if v := req.Header.Get(gateway.IdentityHeader); v != "" {
		t.Errorf("identity header %q would go upstream", v)
	}
}

// TestMain runs hermetically (hosttest) and treats this test binary as the proxy process.
func TestMain(m *testing.M) {
	gateway.HolderComm = func(int) string { return "claude-code" }
	os.Exit(hosttest.Main(m))
}

// A request refused before routing leaves a "refused" log line that never carries the secret it sent.
func TestRefusedRequestIsLogged(t *testing.T) {
	vaulttest.Use(t)
	r := newRouter(secret)
	var log bytes.Buffer
	r.log = &log
	srv := httptest.NewServer(r)
	t.Cleanup(func() { srv.Close(); r.bg.Wait() })
	resp := post(t, srv.URL+"/p/qa/v1/messages", `{"model":"m1"}`, map[string]string{"X-Api-Key": "wrong-secret-value"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	if got := log.String(); !strings.Contains(got, "refused 401 POST /p/qa/v1/messages") || strings.Contains(got, "wrong-secret-value") {
		t.Errorf("log %q, want a refused 401 line without the secret", got)
	}
}

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

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// sealedPAT is a Databricks-shaped row (sealed authType): the gateway's 400 "Invalid Token" is an auth rejection.
func sealedPAT(t *testing.T, base string) string {
	t.Helper()
	if err := vault.Store(providers.SecretName(providers.GroupDatabricks, "patrow", providers.AuthPAT), "pat-value"); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"protocol":"anthropic","base_url":%q,"auth":{"type":"pat"},"models":{"opus":"m1"}}`, base)
}

func pointAnthropicHostAt(t *testing.T, base string) {
	t.Helper()
	u, _ := url.Parse(base)
	prev := anthropicHost
	anthropicHost = u.Host
	t.Cleanup(func() { anthropicHost = prev })
}

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

// rawUp is an upstream that records raw requests (the shared newUpstream insists on a JSON body).
type rawUp struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recorded
}

type recorded struct {
	method, uri, body string
	hdr               http.Header
}

func newRawUp(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int)) *rawUp {
	u := &rawUp{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, recorded{r.Method, r.RequestURI, string(b), r.Header.Clone()})
		n := len(u.reqs)
		u.mu.Unlock()
		h(w, r, n)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *rawUp) got() []recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recorded(nil), u.reqs...)
}

func okRaw(w http.ResponseWriter, _ *http.Request, _ int) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"ok":true}`)
}

func ptDo(t *testing.T, method, url, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req := testRequest(t, method, url, strings.NewReader(body))
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

// lockedBuf is a log sink the handler goroutine writes after the reply is already on the wire.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

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

// The 401 hint names the real helper flag (resolve --proxy; --router was removed).
func TestUnauthorizedHintNamesTokenProxy(t *testing.T) {
	srv := setup(t, `{"default":"prod","providers":{}}`)
	resp := post(t, srv.URL+"/v1/messages", `{}`, map[string]string{"X-Api-Key": "wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	if b := body(t, resp); !strings.Contains(b, "resolve --proxy") || strings.Contains(b, "--router") {
		t.Errorf("401 body = %s, want resolve --proxy", b)
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
