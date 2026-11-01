package opencode

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// isolateOpencodeRouteEnv clears every credential/login surface DescribeRoute's opencode/copilot chain
// reads, so a test controls precisely which dispatch (if any) resolves, never a real machine's own key.
func isolateOpencodeRouteEnv(t *testing.T) (dataHome, cfgHome string) {
	t.Helper()
	dataHome, cfgHome = t.TempDir(), t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())          // no real `gh`: CopilotLoginPresent's shell-out dispatch can't fire by accident
	t.Setenv("COPILOT_HOME", t.TempDir())  // no real copilot config.json: CopilotStoredLogin reads nothing
	t.Setenv("GH_CONFIG_DIR", t.TempDir()) // no real gh hosts.yml: GHActiveAccount reads nothing
	for _, k := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	return dataHome, cfgHome
}

// pinCopilotLogin overlays the built-in github·personal profile with login and pins COPILOT_PROFILE=personal.
func pinCopilotLogin(t *testing.T, login string) {
	t.Helper()
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"personal":{"protocol":"openai",`+
		`"base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},"login":"`+login+`",`+
		`"models":{"opus":"auto","sonnet":"auto","haiku":"auto"},"opencode_fallback":"claude-opus-5.5",`+
		`"opencode_variant":"medium","opencodeID":"github-copilot"}}}}`)
	vaulttest.SetRecord(t, "copilot", "profile", "personal")
}

func subscriptionSelection(t *testing.T, reg *providers.Registry) providers.Selection {
	t.Helper()
	sub, err := reg.Get("max")
	if err != nil {
		t.Fatal(err)
	}
	return providers.Selection{Provider: sub, Aliases: map[string]providers.Target{"opus": {Provider: sub, Model: "claude-opus-5-5"}}}
}





// OpencodeCopilotModelIDs swaps the registry's "auto" haiku default for the provider's OpencodeFallback,
// while the sonnet alias's own (non-"auto") id passes through unchanged. The production registry's
// github-copilot entry no longer defaults haiku to "auto" (it now pins a concrete id), so this overlays
// a synthetic one to keep exercising the swap itself, independent of that literal.
func TestOpencodeCopilotModelIDsSwapsAutoForFallback(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for _, k := range []string{"HARNESS_MODEL", "HARNESS_SONNET_MODEL", "HARNESS_HAIKU_MODEL"} {
		t.Setenv(k, "")
	}
	overlay := `{"providers":{"github":{"personal":{"protocol":"openai","base_url":"https://api.githubcopilot.com",` +
		`"auth":{"type":"cli","login":"copilot"},` +
		`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"auto"},` +
		`"opencode_fallback":"mai-code-1.1-flash"}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	main, small, ok := OpencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("OpencodeCopilotModelIDs: ok = false")
	}
	if main != "claude-sonnet-5.5" {
		t.Errorf("main = %q, want the registry's own sonnet id unchanged", main)
	}
	if small != "mai-code-1.1-flash" {
		t.Errorf("small = %q, want the haiku alias's \"auto\" swapped for OpencodeFallback", small)
	}
}

// A pin that resolves the haiku alias to a real (non-"auto") id passes through untouched, even though
// the registry's own haiku default is "auto".
func TestOpencodeCopilotModelIDsLeavesNonAutoPinUnchanged(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.SetRecord(t, "claude", "aliases.haiku", "claude-haiku-4.5")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, small, ok := OpencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("OpencodeCopilotModelIDs: ok = false")
	}
	if small != "claude-haiku-4.5" {
		t.Errorf("small = %q, want the pinned id unchanged (not swapped for OpencodeFallback)", small)
	}
}



