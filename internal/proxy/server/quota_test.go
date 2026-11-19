package server

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
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

func TestCredentialLadderCachesMissesUntilSignIn(t *testing.T) {
	vaulttest.Use(t)
	const route = `"protocol":"anthropic","base_url":"https://example.test",`
	const overlay = `{"default":"lead","fallback":["signed-out-a","signed-out-b"],"providers":{"anthropic":{"lead":{` +
		route + `"auth":{"type":"none"},"models":{"opus":"lead-model"}},"signed-out-a":{` + route +
		`"auth":{"type":"pat","header":"x-api-key"},"models":{"opus":"a-model"}},"signed-out-b":{` + route +
		`"auth":{"type":"pat","header":"x-api-key"},"models":{"opus":"b-model"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	r := newRouter("s")
	r.now = func() time.Time { return time.Unix(1, 0) }
	reg, err := r.registry()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range reg.Fallback {
		t.Setenv(providers.AuthSecretName(reg.Providers[name]), "")
	}
	opens, probe := map[string]*atomic.Int32{"signed-out-a": {}, "signed-out-b": {}}, probeCredential
	t.Cleanup(func() { probeCredential = probe })
	probeCredential = func(p *providers.Provider) bool {
		opens[p.Name].Add(1) // Each of these PAT rows has exactly one vault-backed presence check.
		return probe(p)
	}
	var requests sync.WaitGroup
	for range 99 {
		requests.Go(func() {
			ladder := r.failoverLadder(reg, providers.Target{Provider: reg.Providers["lead"], Model: "lead-model"}, false)
			if len(ladder) != 1 {
				t.Errorf("signed-out rows reached ladder: %d targets", len(ladder))
			}
		})
	}
	requests.Wait()
	r.failoverLadder(reg, providers.Target{Provider: reg.Providers["lead"], Model: "lead-model"}, false)
	a, b := opens["signed-out-a"].Load(), opens["signed-out-b"].Load()
	t.Logf("presence vault opens over 100 ladder builds: %d", a+b)
	if a != 1 || b != 1 {
		t.Errorf("opens per missing row = %d and %d, want one per row", a, b)
	}
	vaulttest.Secret(t, providers.AuthSecretName(reg.Providers["signed-out-a"]), "fake")
	bumpMtime(t, vault.Path(), r.regStamp.Add(time.Second))
	reg, err = r.registry()
	if err != nil {
		t.Fatal(err)
	}
	ladder := r.failoverLadder(reg, providers.Target{Provider: reg.Providers["lead"], Model: "lead-model"}, false)
	if len(ladder) != 2 || ladder[1].Provider.Name != "signed-out-a" {
		t.Fatal("the vault sign-in did not reach the next ladder inside the miss window")
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

func TestStoreCredentialProbeDropsStaleGeneration(t *testing.T) {
	r := newRouter("s")
	p := &providers.Provider{Name: "genprov"}
	startGen := r.credGen[p.Name]
	r.invalidateCredential(p.Name)
	r.storeCredentialProbe(p, startGen, time.Time{}, r.now(), true)
	if cached, _, _, _ := r.cachedCredential(p.Name, r.now()); cached {
		t.Fatal("a probe that started before an invalidation must not resurrect the cache after it")
	}

	freshGen := r.credGen[p.Name]
	r.storeCredentialProbe(p, freshGen, time.Time{}, r.now(), true)
	if cached, _, _, _ := r.cachedCredential(p.Name, r.now()); !cached {
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

func TestStateWritesContainPanic(t *testing.T) {
	oldCooling, oldDead := persistCooling, persistModelDead
	t.Cleanup(func() { persistCooling, persistModelDead = oldCooling, oldDead })
	persistCooling = func(string, time.Time, time.Time) (bool, error) { panic("cooling exploded") }
	persistModelDead = func(string, string, time.Time, time.Time) (bool, error) { panic("quota exploded") }
	for _, kind := range []string{"cooling", "quota"} {
		t.Run(kind, func(t *testing.T) {
			r := newRouter("s")
			var logs strings.Builder
			r.log = &logs
			p := &providers.Provider{Name: "test"}
			if kind == "cooling" {
				r.coolDown(p, &http.Response{Header: http.Header{"Retry-After": {"10"}}})
			} else {
				r.markDead(providers.Target{Provider: p, Model: "test"})
			}
			r.bg.Wait()
			if strings.Count(logs.String(), "state write panicked") != 1 {
				t.Fatalf("log = %q", logs.String())
			}
		})
	}
}
