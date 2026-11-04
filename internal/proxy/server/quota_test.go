package server

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

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
