package configure

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

// copilot present via a raw env token override offers no rotate CLI (`copilot login` would rotate
// copilot's own session, not the override); present via copilot's/gh's own login does.
func TestVendorKeyTargetCopilotRotateWiring(t *testing.T) {
	t.Setenv("PATH", t.TempDir())              // no real gh/secret-tool on PATH
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // CLAUDE_CONFIG_DIR isolated so nothing real leaks in
	t.Setenv("COPILOT_HOME", t.TempDir())      // no real copilot config.json: CopilotStoredLogin reads nothing
	t.Setenv("GH_CONFIG_DIR", t.TempDir())     // no real gh hosts.yml: GHActiveAccount reads nothing
	t.Setenv("GH_TOKEN", "ghp_env_override")
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	kt := vendorKeyTarget(reg, "copilot")
	if !kt.present || len(kt.loginArgv) != 0 {
		t.Errorf("env-token copilot = %+v, want present with no loginArgv", kt)
	}
}

// codex present via its own ChatGPT login (file-based, no codex binary needed) offers `codex login` as the
// rotate CLI.
func TestVendorKeyTargetCodexRotateWiring(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no codex binary: forces the auth.json file check
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	kt := vendorKeyTarget(reg, "codex")
	if !kt.present {
		t.Fatalf("codex = %+v, want present via the ChatGPT login file", kt)
	}
	if len(kt.loginArgv) != 2 || kt.loginArgv[0] != "codex" || kt.loginArgv[1] != "login" {
		t.Errorf("codex loginArgv = %v, want [codex login]", kt.loginArgv)
	}
}

// gemini present via agy's cached Google login (~/.gemini/antigravity-cli/antigravity-oauth-token) offers
// `agy` as the rotate CLI.
func TestVendorKeyTargetGeminiGoogleLoginRotateWiring(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := agent.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	kt := vendorKeyTarget(reg, "gemini")
	if !kt.present {
		t.Fatalf("gemini = %+v, want present via the agy login marker", kt)
	}
	if len(kt.loginArgv) != 1 || kt.loginArgv[0] != "agy" {
		t.Errorf("gemini loginArgv = %v, want [agy]", kt.loginArgv)
	}
}
