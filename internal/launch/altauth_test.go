package launch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// altAuthScratch isolates the machine state altAuth reads: CLAUDE_CONFIG_DIR (keyring/secrets.env),
// HOME (the gemini agy login marker, and ClaudeLoginStatus has nowhere real to run), and a PATH with
// neither `claude` nor `security`/`secret-tool` on it, so the real Keychain and a real claude login
// can never answer for this test by accident.
func altAuthScratch(t *testing.T) (home string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", t.TempDir())
	home = filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(providers.OAuthTokenEnv, "")
	return home
}

func TestAltAuthAnthropicHeadlessToken(t *testing.T) {
	altAuthScratch(t)
	t.Setenv(providers.OAuthTokenEnv, "sk-ant-oat-test")
	if got := altAuth(nil, "anthropic"); got != "claude uses the OAuth token instead" {
		t.Errorf("altAuth(anthropic) with a headless token = %q", got)
	}
}

func TestAltAuthAnthropicNone(t *testing.T) {
	altAuthScratch(t) // no headless token, no claude binary to report a login
	if got := altAuth(nil, "anthropic"); got != "" {
		t.Errorf("altAuth(anthropic) with neither = %q, want \"\"", got)
	}
}

// gemini has no altAuth case: GeminiLoginPresent (not altAuth) now reports the agy Google login.
func TestAltAuthGemini(t *testing.T) {
	home := altAuthScratch(t)
	if got := altAuth(nil, "gemini"); got != "" {
		t.Errorf("altAuth(gemini) without the agy marker = %q, want \"\"", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "jetski-standalone-oauth-token"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := altAuth(nil, "gemini"); got != "" {
		t.Errorf("altAuth(gemini) with the agy marker = %q, want \"\" (altAuth has no gemini case)", got)
	}
}

func TestAltAuthUnknownProvider(t *testing.T) {
	altAuthScratch(t)
	if got := altAuth(nil, "openai"); got != "" {
		t.Errorf("altAuth(openai) = %q, want \"\" (no alt auth path for this provider)", got)
	}
}
