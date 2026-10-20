package providers

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
)

func TestClaudeLoginStatusLoggedIn(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	pathstest.FakeVendor(t, "claude", `[ "$#" -eq 2 ] && [ "$1" = auth ] && [ "$2" = status ] || exit 1
echo '{"loggedIn":true,"subscriptionType":"max"}'`)
	loggedIn, plan := ClaudeLoginStatus()
	if !loggedIn || plan != " (max)" {
		t.Errorf("ClaudeLoginStatus() = %v, %q; want true, \" (max)\"", loggedIn, plan)
	}
}

func TestClaudeLoginStatusNoBinary(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	loggedIn, plan := ClaudeLoginStatus()
	if loggedIn || plan != "" {
		t.Errorf("ClaudeLoginStatus() with no claude binary = %v, %q; want false, \"\"", loggedIn, plan)
	}
}

// TestHeadlessOAuthToken covers the three sources in priority order: env, keyring, secrets.env.
func TestHeadlessOAuthToken(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(OAuthTokenEnv, "")
	stubKeyring(t, nil)
	if got := HeadlessOAuthToken(); got != "" {
		t.Errorf("HeadlessOAuthToken() with nothing set = %q, want \"\"", got)
	}

	os.MkdirAll(filepath.Dir(paths.SecretsFile()), 0o700)
	if err := os.WriteFile(paths.SecretsFile(), []byte(OAuthTokenEnv+"=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := HeadlessOAuthToken(); got != "from-file" {
		t.Errorf("HeadlessOAuthToken() (secrets.env) = %q, want from-file", got)
	}

	stubKeyring(t, map[string]string{OAuthTokenEnv: "from-keyring"})
	if got := HeadlessOAuthToken(); got != "from-keyring" {
		t.Errorf("HeadlessOAuthToken() (keyring beats file) = %q, want from-keyring", got)
	}

	t.Setenv(OAuthTokenEnv, "from-env")
	if got := HeadlessOAuthToken(); got != "from-env" {
		t.Errorf("HeadlessOAuthToken() (env beats keyring) = %q, want from-env", got)
	}
}
