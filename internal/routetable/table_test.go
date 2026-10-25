package routetable

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestLoadEveryCellResolves exercises Load() directly (no registry overlay, an isolated empty vault):
// every runner × tier cell has a model, account and billing, and TierOf round-trips each cell's own
// model back to its tier.
func TestLoadEveryCellResolves(t *testing.T) {
	vaulttest.Use(t)
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range Runners {
		for _, tier := range Tiers {
			c, ok := tb.Cell(r, tier)
			if !ok || c.Model == "" || c.Account == "" || tb.Billing[c.Account] == "" {
				t.Fatalf("%s/%s: cell %+v ok=%v billing=%q", r, tier, c, ok, tb.Billing[c.Account])
			}
			// codex and opencode serve one id at both opus and sonnet: the strongest tier claims a shared id.
			if got := tb.TierOf(c.Model); got != tier && !((r == "codex" || r == "opencode") && tier == "sonnet" && got == "opus") {
				t.Errorf("TierOf(%s) = %q, want %q", c.Model, got, tier)
			}
		}
	}
}

// TestLoadFromOverlayCellAndWeights checks LoadFrom applies a registry's per-cell Route override and
// replaces Weights whole, and rejects an overlay cell naming an unknown runner/tier.
func TestLoadFromOverlayCellAndWeights(t *testing.T) {
	reg := &providers.Registry{
		Route:        map[string]map[string]providers.RouteCell{"opencode": {"sonnet": {Model: "x/y"}}},
		RouteWeights: map[string]float64{"price": 0.2, "headroom": 0.8},
	}
	tb, err := LoadFrom(reg, "u")
	if err != nil {
		t.Fatal(err)
	}
	if c := tb.Rows["opencode"]["sonnet"]; c.Model != "x/y" || c.Effort != "high" {
		t.Errorf("overlay cell = %+v, want model replaced and effort kept", c)
	}
	if tb.Weights["headroom"] != 0.8 {
		t.Errorf("weights = %v", tb.Weights)
	}
	bad := &providers.Registry{Route: map[string]map[string]providers.RouteCell{"ghost": {"opus": {Model: "m"}}}}
	if _, err := LoadFrom(bad, "u"); err == nil {
		t.Error("unknown overlay cell must fail")
	}
}

// TestLoadFromCopilotAccountKey checks the pinned gh user replaces every copilot cell's account key.
func TestLoadFromCopilotAccountKey(t *testing.T) {
	tb, err := LoadFrom(nil, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	if a := tb.Rows["copilot"]["sonnet"].Account; a != "copilot:work-user" {
		t.Errorf("copilot account = %q, want the pinned user only", a)
	}
	tb, err = LoadFrom(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if a := tb.Rows["copilot"]["sonnet"].Account; a != CopilotUnpinned {
		t.Errorf("copilot account = %q, want %q when no user is pinned", a, CopilotUnpinned)
	}
}

// TestTierOfSlotNamesAndFamilies checks TierOf's other paths: a bare slot name, a provider:model id, and an
// unknown model (none of the table's cells, no slot name, no family match).
func TestTierOfSlotNamesAndFamilies(t *testing.T) {
	vaulttest.Use(t)
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := tb.TierOf("opus"); got != "opus" {
		t.Errorf("TierOf(opus) = %q", got)
	}
	if got := tb.TierOf("claude-opus-5-5[1m]"); got != "opus" {
		t.Errorf("TierOf(claude id) = %q", got)
	}
	if got := tb.TierOf(""); got != "" {
		t.Errorf("TierOf(empty) = %q, want empty", got)
	}
	if got := tb.TierOf("no-such-model-anywhere"); got != "" {
		t.Errorf("TierOf(unknown) = %q, want empty", got)
	}
}
