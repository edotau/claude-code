package cli

import (
	"context"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/router"
)

// stubRefresh swaps routeRefresh for fn (restoring the prior value) and returns a call counter.
func stubRefresh(t *testing.T, fn func(key string) bool) *int {
	t.Helper()
	prev := routeRefresh
	calls := 0
	routeRefresh = func(_ context.Context, key string) bool { calls++; return fn(key) }
	t.Cleanup(func() { routeRefresh = prev })
	return &calls
}

func TestCopilotUsedPct(t *testing.T) {
	const key = "github·alice"
	now := time.Now()
	fresh := func(t *testing.T, pct float64, age time.Duration) {
		t.Helper()
		if err := router.Record(key, pct, "month", now.Add(24*time.Hour), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("fresh entry returns its used pct without refreshing", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		fresh(t, 42.5, time.Minute)
		calls := stubRefresh(t, func(string) bool { return true })
		if pct, ok := copilotUsedPct(context.Background(), "alice"); !ok || pct != 42.5 || *calls != 0 {
			t.Errorf("got (%v, %v), refreshes %d; want (42.5, true), 0", pct, ok, *calls)
		}
	})

	t.Run("active exhausted mark counts as 100", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		fresh(t, 10, time.Minute)
		if err := router.MarkExhausted(key, now.Add(time.Hour), now, "402"); err != nil {
			t.Fatal(err)
		}
		calls := stubRefresh(t, func(string) bool { return true })
		if pct, ok := copilotUsedPct(context.Background(), "alice"); !ok || pct != router.ExhaustedPct || *calls != 0 {
			t.Errorf("got (%v, %v), refreshes %d; want (100, true), 0", pct, ok, *calls)
		}
	})

	t.Run("expired exhausted mark is ignored", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		if err := router.MarkExhausted(key, now.Add(-time.Hour), now.Add(-2*time.Hour), "402"); err != nil {
			t.Fatal(err)
		}
		fresh(t, 7, time.Minute)
		stubRefresh(t, func(string) bool { return true })
		if pct, ok := copilotUsedPct(context.Background(), "alice"); !ok || pct != 7 {
			t.Errorf("got (%v, %v), want (7, true)", pct, ok)
		}
	})

	t.Run("stale entry refreshes then reloads the new value", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		fresh(t, 10, router.StaleAfter+time.Minute)
		calls := stubRefresh(t, func(k string) bool {
			if k != key {
				t.Errorf("refresh key = %q, want %q", k, key)
			}
			return router.Record(key, 61, "month", now.Add(24*time.Hour), time.Now()) == nil
		})
		if pct, ok := copilotUsedPct(context.Background(), "alice"); !ok || pct != 61 || *calls != 1 {
			t.Errorf("got (%v, %v), refreshes %d; want (61, true), 1", pct, ok, *calls)
		}
	})

	t.Run("stale entry whose refresh fails is unverified", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		fresh(t, 10, router.StaleAfter+time.Minute)
		stubRefresh(t, func(string) bool { return false })
		if pct, ok := copilotUsedPct(context.Background(), "alice"); ok || pct != 0 {
			t.Errorf("got (%v, %v), want (0, false)", pct, ok)
		}
	})

	t.Run("no entry and refresh fails is unverified", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		calls := stubRefresh(t, func(string) bool { return false })
		if _, ok := copilotUsedPct(context.Background(), "alice"); ok || *calls != 1 {
			t.Errorf("ok=%v refreshes %d; want false, 1", ok, *calls)
		}
	})

	t.Run("no entry and refresh succeeds without recording is unverified", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		stubRefresh(t, func(string) bool { return true })
		if _, ok := copilotUsedPct(context.Background(), "alice"); ok {
			t.Error("a never-seen account must stay unverified")
		}
	})
}
