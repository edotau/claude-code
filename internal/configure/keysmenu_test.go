package configure

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// TestKeyRows: one row per credential, absent ones say how to fix, and allOK tracks a missing credential.
func TestKeyRows(t *testing.T) {
	rows, ok := keyRows([]keyTarget{
		{label: "codex", present: true, source: "ChatGPT login (codex login)"},
		{label: "gemini", present: false},
	})
	if len(rows) != 2 || rows[0][1] != "subscription  verified" {
		t.Fatalf("rows = %v", rows)
	}
	if rows[1][1] != "not set up    not verified" || ok {
		t.Fatalf("absent row = %q ok=%v, want the set-up hint and ok=false", rows[1][1], ok)
	}
}

// TestKeysMenuBackChangesNothing: leaving the menu at once runs no action and reports nothing changed.
func TestKeysMenuBackChangesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no real vendor CLIs: building key rows must not exec opencode/codex/gh
	orig := pickKey
	t.Cleanup(func() { pickKey = orig })
	calls := 0
	pickKey = func(string, [][2]string, int) (int, error) { calls++; return -1, nil }
	reg, err := providers.Load()
	if err != nil {
		t.Skip(err)
	}
	if _, changed := keysMenu(&dialog{reg: reg}); changed || calls != 1 {
		t.Fatalf("changed=%v calls=%d, want unchanged after one Back", changed, calls)
	}
}

// TestLoginLine: every login row's mode is "subscription", with the tier when the CLI reports one.
func TestLoginLine(t *testing.T) {
	cases := []struct {
		kt   keyTarget
		want string
	}{
		{keyTarget{label: "claude", source: "claude.ai login (max)", headless: true}, "subscription (max)"},
		{keyTarget{label: "codex", source: "ChatGPT login (codex login)"}, "subscription"},
		{keyTarget{label: "opencode", source: "opencode auth login: github-copilot"}, "subscription"},
		{keyTarget{label: "copilot", source: "GH_TOKEN / gh auth status"}, "subscription"},
	}
	for _, c := range cases {
		if got := loginLine(c.kt); got != c.want {
			t.Errorf("loginLine(%s) = %q, want %q", c.kt.label, got, c.want)
		}
	}
}
