package server

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A hard-quota 429 demotes within the provider and skips the denied model on subsequent requests.
func TestQuotaDeadModelDemotesWithinProvider(t *testing.T) {
	var up *upstream
	up = newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if (*up.body.Load())["model"] == "m1-opus" {
			w.WriteHeader(429)
			io.WriteString(w,
				`{"error":{"code":429,"message":"You exceeded your current quota. Quota exceeded for metric: generate_content_free_tier_requests, limit: 0, model: m1-opus"}}`)
			return
		}
		ok(w, r, n)
	})
	t.Setenv("UP1_KEY", "k1")
	srv := setup(
		t,
		`{"default":"up1","fallback":[],"providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY",
			`{"opus":"m1-opus","sonnet":"m1-sonnet","haiku":"m1-haiku"}`)+`}}}`,
	)

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
		t.Fatalf("second request model = %v after %d hits, want m1-sonnet after 3 (no retry of the dead model)", m,
			up.hits.Load())
	}
}

// An absent credential is rechecked immediately so a newly added fallback becomes available.
func TestCredentialPresentDoesNotCacheAbsence(t *testing.T) {
	r := newRouter("s")
	const envVar = "ROUTER_TEST_CRED_ABSENT_THEN_PRESENT"
	os.Unsetenv(envVar)
	p := &providers.Provider{
		Name: "envprov",
		Auth: providers.Auth{Type: providers.AuthKey, Header: providers.HeaderXAPIKey, Env: envVar},
	}
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
	srv := setup(
		t,
		`{"default":"up1","fallback":[],"providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY",
			`{"opus":"m1-opus","sonnet":"m1-sonnet"}`)+`}}}`,
	)
	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || !strings.Contains(string(b), "slow down") || up.hits.Load() != 1 {
		t.Fatalf("transient 429 must be relayed untouched: %d %s hits=%d", resp.StatusCode, b, up.hits.Load())
	}
}

func TestDeadKeyCanonicalizesOneMarker(t *testing.T) {
	p := &providers.Provider{Name: "p"}
	a := deadKey(providers.Target{Provider: p, Model: "claude-opus-4-7[1m]"})
	b := deadKey(providers.Target{Provider: p, Model: "claude-opus-4-7"})
	if a != b {
		t.Errorf("deadKey marked %q != unmarked %q", a, b)
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

// Demotion terminates when model slots form a cycle and every candidate is dead.
func TestDemotionCycleTerminates(t *testing.T) {
	p := &providers.Provider{Name: "g", Models: map[string]string{"opus": "A", "sonnet": "B", "haiku": "A"}}
	vaulttest.Use(t) // markDead persists quota.json: keep it out of the shared home and later tests' dirs
	r := newRouter("s")
	t.Cleanup(r.bg.Wait)
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
	r.bg.Wait()
	vaulttest.Use(t) // a fresh quota.json: r's persisted B mark must not reach r2
	r2 := newRouter("s")
	t.Cleanup(r2.bg.Wait)
	r2.markDead(providers.Target{Provider: p, Model: "A"})
	d, ok := r2.demotion(providers.Target{Provider: p, Model: "A"})
	if !ok || d.Model != "B" {
		t.Errorf("demotion(A) = %+v, %v; want B, true", d, ok)
	}
}
