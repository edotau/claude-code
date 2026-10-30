package agent

import (
	"context"
	"github.com/edotau/claude-code/internal/providers"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveTargetFallsBackWhenSessionProviderCannotServe(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no real codex: login detection falls back to the auth.json fixture
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	t.Setenv("GH_TOKEN", "")
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "GATEWAY_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reg.Get("max")
	if err != nil {
		t.Fatal(err)
	}
	sel := providers.Selection{Provider: sub, Slots: map[string]providers.Target{"opus": {Provider: sub, Model: "claude-opus-5-5"}}}

	// codex: subscription can't serve it, no ChatGPT login present -> target unchanged, no note.
	target, note, err := ResolveVendorTarget(reg, "codex", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("expected no fallback without a codex login, got %q", note)
	}
	if target.Provider != sub {
		t.Fatalf("expected unchanged target, got %s", target.Provider.Name)
	}

	// With a ChatGPT login present, codex falls back to the chatgpt provider.
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, "codex", "auth.json")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex", "auth.json"), []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	target, note, err = ResolveVendorTarget(reg, "codex", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Provider == nil || target.Provider.Name != "chatgpt" {
		t.Fatalf("expected fallback to chatgpt, got %v", target.Provider)
	}
	if !strings.Contains(note, "chatgpt") {
		t.Fatalf("expected a note naming chatgpt, got %q", note)
	}

	// The CODEX_MODEL pin beats both the chatgpt provider's own opus slot and codexOwnModel's
	// config.toml (unset here), once the ChatGPT login fallback applies.
	setPin(t, providers.PinCodexModel, "")
	if err := providers.SetPins(map[string]string{providers.PinCodexModel: "gpt-6.1-sol"}); err != nil {
		t.Fatal(err)
	}
	target, _, err = ResolveVendorTarget(reg, "codex", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Model != "gpt-6.1-sol" {
		t.Fatalf("CODEX_MODEL pin = gpt-6.1-sol, want it to win the fallback target's model, got %q", target.Model)
	}

	// An explicit provider choice is never overridden by a fallback.
	target, note, err = ResolveVendorTarget(reg, "codex", sel, true)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" || target.Provider != sub {
		t.Fatalf("explicit provider must not fall back, got target=%v note=%q", target.Provider, note)
	}
}

// The GEMINI_AGENT_MODEL pin beats the gemini provider's own registry opus slot once agy's Google
// login fallback applies (subscription can't serve gemini directly).
func TestResolveTargetGeminiModelPinBeatsRegistryDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reg.Get("max")
	if err != nil {
		t.Fatal(err)
	}
	sel := providers.Selection{Provider: sub, Slots: map[string]providers.Target{"opus": {Provider: sub, Model: "claude-opus-5-5"}}}

	setPin(t, providers.PinGeminiModel, "")
	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: ""}); err != nil {
		t.Fatal(err)
	}
	target, _, err := ResolveVendorTarget(reg, "gemini", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	wantDefault := reg.Providers["antigravity"].Models["opus"]
	if target.Model != wantDefault {
		t.Fatalf("unpinned: target.Model = %q, want the registry's gemini opus slot %q", target.Model, wantDefault)
	}

	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: "gemini-2.5-pro"}); err != nil {
		t.Fatal(err)
	}
	target, _, err = ResolveVendorTarget(reg, "gemini", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Model != "gemini-2.5-pro" {
		t.Fatalf("GEMINI_AGENT_MODEL pin = gemini-2.5-pro, want it to win the fallback target's model, got %q", target.Model)
	}
}

func TestResolveTargetGeminiFallsBackToGoogleLogin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GEMINI_API_KEY", "")
	marker := AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reg.Get("max")
	if err != nil {
		t.Fatal(err)
	}
	sel := providers.Selection{Provider: sub, Slots: map[string]providers.Target{"opus": {Provider: sub, Model: "claude-opus-5-5"}}}
	target, note, err := ResolveVendorTarget(reg, "gemini", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Provider == nil || target.Provider.Provider != "google" || !strings.Contains(note, "Google login") {
		t.Fatalf("expected the Google-login gemini route, got %v / %q", target.Provider, note)
	}
	env, _, err := ConfigureHeadless(context.Background(), "gemini", target, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatalf("a login with no key must still configure: %v", err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "GEMINI_API_KEY=") {
			t.Fatalf("login run must not inject a key: %v", env)
		}
	}
}
