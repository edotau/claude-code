package agent

import (
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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
	sel := providers.Selection{Provider: sub, Aliases: map[string]providers.Target{"opus": {Provider: sub, Model: "claude-opus-5-5"}}}

	// codex: subscription can't serve it, no ChatGPT login present -> target unchanged, no note.
	target, note, err := schemaFor("codex").ResolveTarget(reg, sel, false)
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
	target, note, err = schemaFor("codex").ResolveTarget(reg, sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Provider == nil || target.Provider.Name != "chatgpt" {
		t.Fatalf("expected fallback to chatgpt, got %v", target.Provider)
	}
	if !strings.Contains(note, "chatgpt") {
		t.Fatalf("expected a note naming chatgpt, got %q", note)
	}

	// The CODEX_MODEL pin beats both the chatgpt provider's own opus alias and codexOwnModel's
	// config.toml (unset here), once the ChatGPT login fallback applies.
	vaulttest.SetRecord(t, "codex", "model", "")
	vaulttest.SetRecord(t, "codex", "model", "gpt-6.1-sol")
	target, _, err = schemaFor("codex").ResolveTarget(reg, sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Model != "gpt-6.1-sol" {
		t.Fatalf("CODEX_MODEL pin = gpt-6.1-sol, want it to win the fallback target's model, got %q", target.Model)
	}

	// An explicit provider choice is never overridden by a fallback.
	target, note, err = schemaFor("codex").ResolveTarget(reg, sel, true)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" || target.Provider != sub {
		t.Fatalf("explicit provider must not fall back, got target=%v note=%q", target.Provider, note)
	}
}
