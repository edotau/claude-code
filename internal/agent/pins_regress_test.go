package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// DescribeRoute honors COPILOT_PROFILE/COPILOT_MODEL: the pinned usable Databricks row, not the first fallback.
func TestDescribeCopilotRouteHonorsProfileAndModelPins(t *testing.T) {
	reg := gateReg(t, map[string]bool{"dev": true, "qa": true})
	gateSetup(t, 99, true, reg) // GitHub over quota, so only the pin can explain a qa pick
	setPin(t, providers.PinCopilotProfile, "qa")
	setPin(t, providers.PinCopilotModel, "kimi-k3")
	ri := describeCopilotRoute(reg)
	if ri.Leg != LegDatabricks || ri.Provider != "qa" || ri.Model != "databricks-kimi-k3" || !ri.OK {
		t.Fatalf("describe = %+v, want databricks qa databricks-kimi-k3", ri)
	}
	if strings.Contains(ri.Reason, "no pinned GitHub account") {
		t.Errorf("reason %q must not claim an unpinned account", ri.Reason)
	}
}

// Without a model pin the pinned profile's opus slot is the model.
func TestDescribeCopilotRoutePinnedProfileDefaultsToOpusSlot(t *testing.T) {
	reg := gateReg(t, map[string]bool{"dev": true, "qa": true})
	gateSetup(t, 99, true, reg)
	setPin(t, providers.PinCopilotProfile, "qa")
	if ri := describeCopilotRoute(reg); ri.Provider != "qa" || ri.Model != "databricks-claude-opus-5-5" {
		t.Fatalf("describe = %+v, want qa with its opus slot", ri)
	}
}

// Interactive codex (nil Headless) carries the CODEX_EFFORT pin; headless Effort still wins.
func TestCodexInteractiveCarriesEffortPin(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	setPin(t, providers.PinCodexEffort, "medium")
	_, argv, err := Configure(context.Background(), "codex", tg, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="medium"`) {
		t.Errorf("interactive argv = %q, want the effort pin", argv)
	}
	_, argv, err = configureHeadless(context.Background(), "codex", tg, Headless{Prompt: "hi", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="low"`) || slices.Contains(argv, `model_reasoning_effort="medium"`) {
		t.Errorf("headless argv = %q, want explicit low over the pin", argv)
	}
}
