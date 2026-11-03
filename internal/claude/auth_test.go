package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
)

func writeClaudeJSON(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if body == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeTier(t *testing.T) {
	cases := []struct{ name, json, plan, want string }{
		{"user 20x", `{"oauthAccount":{"userRateLimitTier":"default_claude_max_20x"}}`, "max", "Ultra"},
		{"org 5x", `{"oauthAccount":{"organizationRateLimitTier":"default_claude_max_5x"}}`, "max", "Max 5x"},
		{"user beats org", `{"oauthAccount":{"userRateLimitTier":"default_claude_max_5x","organizationRateLimitTier":"default_claude_max_20x"}}`, "max", "Max 5x"},
		{"no tier, Max plan", `{"oauthAccount":{"userRateLimitTier":"default"}}`, "max", "Max"},
		{"no tier, Pro plan in parens", `{}`, " (pro) ", "Pro"},
		{"no file", ``, "pro", "Pro"},
		{"malformed file", `{`, "pro", "Pro"},
		{"nothing known", ``, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeClaudeJSON(t, c.json)
			if got := claudeAgent.Plan(context.Background(), providers.AuthState{Present: true, PlanHint: strings.Trim(c.plan, " ()")}).Name; got != c.want {
				t.Errorf("claudeTier(%q) = %q, want %q", c.plan, got, c.want)
			}
		})
	}
}

func TestClaudeLoginProbeLoggedIn(t *testing.T) {
	resetClaudeProbe()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	pathstest.FakeVendor(t, "claude", `[ "$#" -eq 2 ] && [ "$1" = auth ] && [ "$2" = status ] || exit 1
echo '{"loggedIn":true,"subscriptionType":"max"}'`)
	loggedIn, plan, _ := ClaudeLoginProbe()
	if !loggedIn || plan != " (max)" {
		t.Errorf("ClaudeLoginProbe() = %v, %q; want true, \" (max)\"", loggedIn, plan)
	}
}

func TestClaudeLoginProbeNoBinary(t *testing.T) {
	resetClaudeProbe()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	loggedIn, plan, _ := ClaudeLoginProbe()
	if loggedIn || plan != "" {
		t.Errorf("ClaudeLoginProbe() with no claude binary = %v, %q; want false, \"\"", loggedIn, plan)
	}
}

// TestHeadlessOAuthToken covers the two sources in priority order: env, then the vault.

// The probe is tri-state: a failed probe is an error (unknown), never "logged out".
func TestClaudeLoginProbeTriState(t *testing.T) {
	for _, c := range []struct {
		name, body string
		noBin      bool
		wantIn     bool
		wantPlan   string
		wantErr    bool
	}{
		{"logged in", `echo '{"loggedIn":true,"subscriptionType":"max"}'`, false, true, " (max)", false},
		{"logged out json on nonzero exit", `echo '{"loggedIn":false}'; exit 1`, false, false, "", false},
		{"bad json", `echo 'not json'`, false, false, "", true},
		{"nonzero exit no output", `exit 3`, false, false, "", true},
		{"missing binary", "", true, false, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetClaudeProbe()
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			if !c.noBin {
				pathstest.FakeVendor(t, "claude", c.body)
			}
			in, plan, err := ClaudeLoginProbe()
			if in != c.wantIn || plan != c.wantPlan || (err != nil) != c.wantErr {
				t.Errorf("probe = %v %q %v; want %v %q err=%v", in, plan, err, c.wantIn, c.wantPlan, c.wantErr)
			}
		})
	}
}
