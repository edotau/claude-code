package agent

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestVendorIsolationCaps(t *testing.T) {
	for name, want := range map[string][3]Level{
		"codex": {Native, Native, None}, "opencode": {None, Native, None}, "gemini": {None, None, Native}} {
		c := newCLIEngine(name).Caps()
		if got := [3]Level{c.NoPersist, c.SafeMode, c.DebugFile}; got != want {
			t.Errorf("%s caps = %v, want %v", name, got, want)
		}
	}
}

// vendorTargetBase honors the request's model alias; ResolveVendorTarget always returns the session's opus alias.
func TestVendorTargetBaseDiffersFromResolveTargetOnTier(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Use(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	rt, _, err := schemaFor("codex").ResolveTarget(reg, sel, false)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	bt, _, _, _, err := schemaFor("codex").targetBase(RunOptions{Prompt: "p", Model: "haiku"})
	if err != nil {
		t.Fatalf("vendorTargetBase: %v", err)
	}
	if rt.Model == bt.Model {
		t.Errorf("expected the alias to differ, both %q", rt.Model)
	}
	if want := bt.Provider.Models["haiku"]; bt.Model != want {
		t.Errorf("base model = %q, want the haiku alias %q", bt.Model, want)
	}
}
