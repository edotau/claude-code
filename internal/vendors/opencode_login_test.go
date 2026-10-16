package vendors

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// opencode's own `auth login` store is a fallback leg: present → launch plain (ErrOwnLogin); absent → keyed provider.
func TestOpencodeOwnLoginIsAFallbackLeg(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "subscription")
	if got := OpencodeLogins(); got != nil {
		t.Fatalf("no auth.json yet, got %v", got)
	}
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{"github-copilot":{"type":"oauth"},"anthropic":{"type":"api"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := OpencodeLogins(); len(got) != 2 || got[0] != "anthropic" || got[1] != "github-copilot" {
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
