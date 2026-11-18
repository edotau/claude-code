package router

import (
	"slices"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/nonet"
	"github.com/edotau/claude-code/internal/providers"
)

func TestModelKeySharesOneMarker(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"claude-opus-4-7[1m]", "claude-opus-4-7"},
		{"unknown-model[1m]", "unknown-model"},
		{"databricks-gpt-6-1-terra", "gpt-6-1-terra"},
	} {
		if ModelKey(tc.a) != ModelKey(tc.b) {
			t.Errorf("ModelKey(%q) = %q, ModelKey(%q) = %q; want one key", tc.a, ModelKey(tc.a), tc.b, ModelKey(tc.b))
		}
	}
}

func TestMarkModelDead(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const acct = "databricks·dev"
	for _, step := range []struct {
		name, model    string
		until, now, at time.Time
		// model -> ModelDead at step.at
		dead map[string]bool
	}{
		{"first mark", "m-a", now.Add(30 * time.Minute), now, now, map[string]bool{"m-a": true, "m-b": false}},
		{"shorter ignored", "m-a", now.Add(time.Minute), now, now.Add(10 * time.Minute), map[string]bool{"m-a": true}},
		{"longer wins", "m-a[1m]", now.Add(time.Hour), now, now.Add(45 * time.Minute), map[string]bool{"m-a": true}},
		{"expired pruned", "m-b", now.Add(2 * time.Hour), now.Add(90 * time.Minute), now.Add(90 * time.Minute),
			map[string]bool{"m-a": false, "m-b": true}},
	} {
		ran, err := MarkModelDead(acct, step.model, step.until, step.now)
		if err != nil || !ran {
			t.Fatalf("%s: MarkModelDead = %v, %v", step.name, ran, err)
		}
		s := LoadState()
		for model, want := range step.dead {
			if got := s.ModelDead(acct, model, step.at); got != want {
				t.Errorf("%s: ModelDead(%q) = %v, want %v", step.name, model, got, want)
			}
		}
	}
	got := LoadState()[acct].ModelDead
	if len(got) != 1 || !got["m-b"].Equal(now.Add(2*time.Hour)) {
		t.Errorf("after prune: model_dead = %v, want only m-b", got)
	}
	if s := LoadState(); !s.ModelDead(acct, "m-b", now) || s.ModelDead("other", "m-b", now) {
		t.Error("ModelDead must be scoped to its account")
	}
}

func familyDeadFixture(t *testing.T, reads *int) *providers.Registry {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := GithubCatalogIDs
	GithubCatalogIDs = func(*providers.Provider) []string {
		*reads++
		return []string{"gpt-6.1-sol", "gpt-6.2-sol", "gpt-6-astra"}
	}
	t.Cleanup(func() { GithubCatalogIDs = old })
	return &providers.Registry{Providers: map[string]*providers.Provider{"octo": {Name: "octo", Provider: "github"}}}
}

// A family rung is judged by the id it resolves to: a mark on that id demotes it, a mark on another version does not.
func TestDeadLastResolvesFamilyRung(t *testing.T) {
	var reads int
	reg := familyDeadFixture(t, &reads)
	now := time.Now()
	const acct = "github·octo"
	cs := []Candidate{{Agent: "copilot", Account: acct, Model: "sol"}, {Agent: "copilot", Account: acct, Model: "astra"}}
	order := func(st State) string {
		var res PickResult
		out := deadLast(slices.Clone(cs), st, reg, now, &res)
		return out[0].Model
	}
	if got := order(State{}); got != "sol" || reads != 0 {
		t.Fatalf("clean state: first %q after %d list reads, want sol and 0", got, reads)
	}
	for model, want := range map[string]string{"gpt-6.2-sol": "astra", "gpt-6.1-sol": "sol"} {
		if _, err := MarkModelDead(acct, model, now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		if got := order(LoadState()); got != want {
			t.Errorf("mark on %s: first %q, want %q", model, got, want)
		}
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	}
}

func TestMarkModelDeadIgnoresFamilyWord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	if ran, err := MarkModelDead("github·octo", "sol", now.Add(time.Hour), now); ran || err != nil {
		t.Fatalf("MarkModelDead(sol) = %v, %v, want false, nil", ran, err)
	}
	if LoadState().ModelDead("github·octo", "sol", now) {
		t.Error("a family word was recorded as dead")
	}
}
