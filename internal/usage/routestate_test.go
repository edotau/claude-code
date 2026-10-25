package usage

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/route"
)

// TestRecordQuotas: each account keeps its tightest window; a 100% Copilot seat is exhausted until its reset day.
func TestRecordQuotas(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 10, 25, 2, 0, 0, 0, time.Local)
	qs := []quota{
		{Provider: "anthropic", Window: "5h", UsedPct: 40, ResetsAt: "2026-10-25T08:00:00Z"},
		{Provider: "anthropic", Window: "weekly", UsedPct: 92, ResetsAt: "2026-10-30T08:00:00Z"},
		{Provider: "anthropic", Window: "weekly", Group: "Fable", UsedPct: 100},
		{Provider: "github", Account: "work-user", Window: "monthly", UsedPct: 100, Resets: "2026-11-01"},
		{Provider: "openai", Note: "unavailable: codex not signed in to ChatGPT"},
	}
	if n := recordQuotas(qs, now); n != 2 {
		t.Fatalf("recorded %d accounts, want 2", n)
	}
	st := route.LoadState()
	if e := st["claude:max"]; e.UsedPct != 92 || e.Window != "weekly" || e.Status(now) != route.StateLow {
		t.Errorf("claude:max = %+v, want the weekly 92%% window, low", e)
	}
	if e := st["copilot:work-user"]; e.Status(now) != route.StateExhausted || e.Until(now).Day() != 1 {
		t.Errorf("copilot = %+v, want exhausted until the 1st", e)
	}
	if _, ok := st["codex:chatgpt"]; ok {
		t.Error("a failure row must not record")
	}
}
