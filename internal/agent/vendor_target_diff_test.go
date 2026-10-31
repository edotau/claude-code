package agent

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// vendorTargetBase honors the request's model tier; ResolveVendorTarget always returns the session's opus slot.
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
	rt, _, err := ResolveVendorTarget(reg, "codex", sel, false)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	bt, _, _, _, err := vendorTargetBase(RunOptions{Prompt: "p", Model: "haiku"}, "codex")
	if err != nil {
		t.Fatalf("vendorTargetBase: %v", err)
	}
	if rt.Model == bt.Model {
		t.Errorf("expected the tier to differ, both %q", rt.Model)
	}
	if want := bt.Provider.Models["haiku"]; bt.Model != want {
		t.Errorf("base model = %q, want the haiku slot %q", bt.Model, want)
	}
}
