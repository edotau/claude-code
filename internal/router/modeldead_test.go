package router

import (
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/nonet"
)

func TestModelKeySharesOneMarker(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"claude-opus-4-7[1m]", "claude-opus-4-7"},
		{"unknown-model[1m]", "unknown-model"},
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
		dead           map[string]bool // model -> ModelDead at step.at
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
