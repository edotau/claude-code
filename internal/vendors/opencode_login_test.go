package vendors

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// opencode's own `auth login` store is a fallback leg: a non-copilot login present → launch plain
// (ErrOwnLogin); absent → keyed provider. github-copilot is its own leg (see
// TestOpencodeCopilotLoginTakesPrecedence): it resolves to a real target, not ErrOwnLogin.
func TestOpencodeOwnLoginIsAFallbackLeg(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "subscription")
	if got := OpencodeLogins(); got != nil {
		t.Fatalf("no auth.json yet, got %v", got)
	}
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{"anthropic":{"type":"api"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := OpencodeLogins(); len(got) != 1 || got[0] != "anthropic" {
		t.Fatalf("logins = %v", got)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	target, note, err := ResolveTarget(reg, "opencode", sel, false)
	if !errors.Is(err, ErrOwnLogin) || target.Provider != nil || note == "" {
		t.Errorf("want ErrOwnLogin with a note and no provider; got target=%+v note=%q err=%v", target, note, err)
	}
	// --provider is explicit: the session provider is used as-is, own login or not.
	if _, _, err := ResolveTarget(reg, "opencode", sel, true); errors.Is(err, ErrOwnLogin) {
		t.Error("an explicit provider must not be overridden by the own-login leg")
	}
}

// A github-copilot entry in opencode's auth.json is its own leg, ahead of both the session provider
// and the generic own-login fallback above: it resolves to a real github-copilot target (no
// ErrOwnLogin), since the harness, not opencode, now owns writing that route's config.
func TestOpencodeCopilotLoginTakesPrecedence(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "subscription")
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A second, unrelated login (anthropic) is present too: github-copilot still wins.
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{"github-copilot":{"type":"oauth"},"anthropic":{"type":"api"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !OpencodeCopilotLoginPresent() {
		t.Fatal("OpencodeCopilotLoginPresent() = false, want true")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	target, note, err := ResolveTarget(reg, "opencode", sel, false)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if target.Provider == nil || target.Provider.Name != "github-copilot" {
		t.Fatalf("target.Provider = %+v, want github-copilot", target.Provider)
	}
	if !strings.Contains(note, "opencode auth login") {
		t.Errorf("note = %q, want it to name opencode auth login", note)
	}
	// --provider is explicit: the github-copilot login leg must not override an explicit choice.
	explicitTarget, _, err := ResolveTarget(reg, "opencode", sel, true)
	if err != nil {
		t.Fatalf("ResolveTarget (explicit): %v", err)
	}
	if explicitTarget.Provider != nil && explicitTarget.Provider.Name == "github-copilot" && sel.Provider.Name != "github-copilot" {
		t.Errorf("an explicit provider was overridden by the github-copilot login leg: %+v", explicitTarget)
	}
}

// OpencodeCopilotModelIDs swaps the registry's "auto" haiku default for the provider's OpencodeFallback,
// while the sonnet slot's own (non-"auto") id passes through unchanged.
func TestOpencodeCopilotModelIDsSwapsAutoForFallback(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, k := range []string{"HARNESS_MODEL", "HARNESS_SONNET_MODEL", "HARNESS_HAIKU_MODEL"} {
		t.Setenv(k, "")
	}
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
		t.Errorf("small = %q, want the haiku slot's \"auto\" swapped for OpencodeFallback", small)
	}
}

// A pin that resolves the haiku slot to a real (non-"auto") id passes through untouched, even though
// the registry's own haiku default is "auto".
func TestOpencodeCopilotModelIDsLeavesNonAutoPinUnchanged(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_MODEL", "")
	t.Setenv("HARNESS_SONNET_MODEL", "")
	t.Setenv("HARNESS_HAIKU_MODEL", "claude-haiku-4.5")
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

// A model set in the user's own opencode.jsonc is the same leg: opencode runs on its own config, untouched.
func TestOpencodeOwnConfigModelIsAFallbackLeg(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "subscription")
	if err := os.MkdirAll(filepath.Join(cfg, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "opencode", "opencode.jsonc"), []byte(`{"model":"google/gemini-3.8-flash"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	target, note, err := ResolveTarget(reg, "opencode", sel, false)
	if !errors.Is(err, ErrOwnLogin) || target.Provider != nil || !strings.Contains(note, "google/gemini-3.8-flash") {
		t.Errorf("want ErrOwnLogin naming the model; got target=%+v note=%q err=%v", target, note, err)
	}
}
