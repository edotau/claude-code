package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
