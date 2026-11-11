package router

import (
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/nonet"
)

func TestMarkCooling(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const acct = "databricks·dev"
	for _, step := range []struct {
		name       string
		until, now time.Time
		want       time.Time // CoolingUntil after the step; zero = no hold stored
	}{
		{"first hold", now.Add(30 * time.Second), now, now.Add(30 * time.Second)},
		{"shorter ignored", now.Add(10 * time.Second), now, now.Add(30 * time.Second)},
		{"longer wins", now.Add(time.Minute), now, now.Add(time.Minute)},
		{"expired pruned", now.Add(time.Minute), now.Add(2 * time.Minute), time.Time{}},
	} {
		ran, err := MarkCooling(acct, step.until, step.now)
		if err != nil || !ran {
			t.Fatalf("%s: MarkCooling = %v, %v", step.name, ran, err)
		}
		if got := LoadState().CoolingUntil(acct); !got.Equal(step.want) {
			t.Errorf("%s: CoolingUntil = %v, want %v", step.name, got, step.want)
		}
	}
	if e := LoadState()[acct]; e.Cooling != nil {
		t.Errorf("after prune: cooling = %v, want none", e.Cooling)
	}
	if ran, err := MarkCooling("codex·own", now.Add(time.Minute), now); ran || err != nil || len(LoadState()["codex·own"].Cooling) != 0 {
		t.Errorf("MarkCooling(·own) = %v, %v; want nothing written", ran, err)
	}
}
