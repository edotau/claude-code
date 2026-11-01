package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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
	st := router.LoadState()
	if e := st["anthropic·max"]; e.UsedPct != 92 || e.Window != "weekly" || e.Status(now) != router.StateLow {
		t.Errorf("anthropic·max = %+v, want the weekly 92%% window, low", e)
	}
	if e := st["github·work-user"]; e.Status(now) != router.StateExhausted || e.Until(now).Day() != 1 {
		t.Errorf("copilot = %+v, want exhausted until the 1st", e)
	}
	if _, ok := st["openai·chatgpt"]; ok {
		t.Error("a failure row must not record")
	}
}

func personalVault(t *testing.T) {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"personal":{"protocol":"openai",`+
		`"base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},"login":"edotau",`+
		`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"auto"}}}}}`)
}

func TestAccountKeyMapsLoginToProfile(t *testing.T) {
	personalVault(t)
	for login, want := range map[string]string{"edotau": "github·personal", "octocat": "github·octocat", "": ""} {
		if got := accountKey(quota{Provider: "github", Account: login}); got != want {
			t.Errorf("accountKey(%q) = %q, want %q", login, got, want)
		}
	}
}

// RefreshAccount maps github·<profile> back to the login before gh sees it: the round trip login -> profile -> login.
func TestRefreshAccountPassesLoginToGh(t *testing.T) {
	personalVault(t)
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for key, want := range map[string]string{"github·personal": "--user edotau", "github·octocat": "--user octocat"} {
		_ = os.Remove(log)
		RefreshAccount(context.Background(), key)
		raw, _ := os.ReadFile(log)
		if !strings.Contains(string(raw), want) || strings.Contains(string(raw), "--user personal") {
			t.Errorf("RefreshAccount(%q): gh argv %q, want %q", key, raw, want)
		}
	}
}
