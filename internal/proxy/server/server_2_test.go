package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
