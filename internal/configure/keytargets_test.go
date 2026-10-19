package configure

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendors"
)

func TestCopilotEnvTokenSet(t *testing.T) {
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	if copilotEnvTokenSet() {
		t.Error("copilotEnvTokenSet must be false with none of the three vars set")
	}
	t.Setenv("GH_TOKEN", "ghp_fake")
	if !copilotEnvTokenSet() {
		t.Error("copilotEnvTokenSet must be true once GH_TOKEN is set")
	}
}

// copilot present via a raw env token override offers no rotate CLI (`gh auth refresh` would rotate gh's
// own session, not the override); present via gh's own session does.
func TestVendorKeyTargetCopilotRotateWiring(t *testing.T) {
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
	marker := vendors.AgyLoginMarker(home)
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

// gemini present via a plain API key offers "API key" (its envVar+provider are wired), never a login CLI.
func TestVendorKeyTargetGeminiAPIKeyRotateWiring(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GEMINI_CLI_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GEMINI_API_KEY", "sk-test-key")
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"gemini": {Name: "gemini", Auth: providers.Auth{Type: providers.AuthAPIKey, Env: "GEMINI_API_KEY"}},
	}}
	kt := vendorKeyTarget(reg, "gemini")
	if !kt.present || kt.envVar != "GEMINI_API_KEY" || kt.provider == nil {
		t.Fatalf("gemini API key target = %+v, want present+envVar+provider", kt)
	}
	opts := rotateOptions(kt)
	if len(opts) != 1 || opts[0].action != "API key" {
		t.Errorf("rotateOptions(gemini key target) = %v, want [Replace key]", opts)
	}
}
