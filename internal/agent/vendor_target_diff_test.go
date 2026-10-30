package agent

import (
	"testing"

	"github.com/edotau/claude-code/internal/agent/vendors"
	"github.com/edotau/claude-code/internal/providers"
)

// vendorTargetBase honors the request's model tier; vendors.ResolveTarget always returns the session's opus slot.
func TestVendorTargetBaseDiffersFromResolveTargetOnTier(t *testing.T) {
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	rt, _, err := vendors.ResolveTarget(reg, "codex", sel, false)
	if err != nil {
		t.Skipf("ResolveTarget: %v", err)
	}
	bt, _, _, _, err := vendorTargetBase(Request{Prompt: "p", Model: "haiku"}, "codex")
	t.Logf("ResolveTarget model=%q; base model=%q err=%v", rt.Model, bt.Model, err)
	if err == nil && rt.Model == bt.Model {
		t.Errorf("expected the tier to differ, both %q", rt.Model)
	}
}
