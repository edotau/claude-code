package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestClaudeLoginProbeNativeInstall(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	resetClaudeProbe()
	t.Cleanup(resetClaudeProbe)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	versions := filepath.Join(os.Getenv("HOME"), ".local", "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" + `[ "$#" -eq 2 ] && [ "$1" = auth ] && [ "$2" = status ] || exit 1
 echo '{"loggedIn":true,"subscriptionType":"max"}'
`
	if err := os.WriteFile(filepath.Join(versions, "2.0.14"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := claudeAgent.Available(); err != nil {
		t.Fatalf("native install unavailable: %v", err)
	}
	loggedIn, plan, err := claudeLoginProbe()
	if err != nil || !loggedIn || plan != " (max)" {
		t.Fatalf("native auth probe = %v, %q, %v; want true, max, nil", loggedIn, plan, err)
	}
}

// `claude auth status` keeps claude's own login but no registry credential and no other agent's login.
func TestClaudeLoginProbeScrubsCredentials(t *testing.T) {
	resetClaudeProbe()
	t.Cleanup(resetClaudeProbe)
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.cloud.databricks.com","auth":{"type":"key","env":"DEV_WS_TOKEN"}}}}}`)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, k := range []string{"DEV_WS_TOKEN", "OPENCODE_API_KEY", "GH_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		t.Setenv(k, "SENTINEL")
	}
	dump := filepath.Join(t.TempDir(), "env")
	pathstest.FakeNativeClaude(t, "/usr/bin/env > "+dump+"\necho '{\"loggedIn\":true,\"subscriptionType\":\"max\"}'")
	if in, plan, err := claudeLoginProbe(); !in || plan != " (max)" || err != nil {
		t.Fatalf("probe = %v %q %v; want logged in (max)", in, plan, err)
	}
	env, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"DEV_WS_TOKEN", "OPENCODE_API_KEY", "GH_TOKEN", "OPENAI_API_KEY"} {
		if strings.Contains("\n"+string(env), "\n"+k+"=") {
			t.Errorf("claude auth status inherited %s", k)
		}
	}
	if !strings.Contains(string(env), "CLAUDE_CODE_OAUTH_TOKEN=SENTINEL") {
		t.Error("claude auth status lost its own login CLAUDE_CODE_OAUTH_TOKEN")
	}
}

// resetClaudeProbe drops the memo so the next claudeLoginProbe runs the binary again.
func resetClaudeProbe() {
	claudeProbeMemo.Drop()
}
