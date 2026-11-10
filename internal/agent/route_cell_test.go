package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A pinned caller's own profile:model runs the bare model in its cell; buildDispatch adds the profile once.
func TestPinnedCellStripsOwnProfile(t *testing.T) {
	rc := routeCtx{want: "codex", explicitModel: true, callerModel: "dev:databricks-gpt-6-1-sol",
		pins: map[string]router.PinSpec{"codex": {Profile: "dev"}}}
	cell, skip := rc.candidateCell(router.Candidate{Agent: "codex", Profile: "dev"})
	if skip != "" || cell.Model != "databricks-gpt-6-1-sol" {
		t.Errorf("cell model %q skip %q, want databricks-gpt-6-1-sol", cell.Model, skip)
	}
}

func TestClaudeCellModelIsTheAlias(t *testing.T) {
	if got := claudeCellModel("claude-opus-5-5"); got != "opus" {
		t.Errorf("claudeCellModel(claude-opus-5-5) = %q, want opus (so pins resolve it)", got)
	}
	if got := claudeCellModel("claude-opus-4-1"); got != "claude-opus-4-1" {
		t.Errorf("an alias-less row stays canonical, got %q", got)
	}
}

// profileCell refuses an exact model the fresh listing has NOT_READY, with the new message.
func TestProfileCellRejectsNotReadyExactModel(t *testing.T) {
	reg := servedEnv(t, "true", map[string]string{"m-a": "NOT_READY", "m-b": "READY"})
	_, why := routeCtx{reg: reg, exact: "m-a"}.profileCell(servedCand, router.Cell{})
	if !strings.Contains(why, "m-a is not a READY endpoint on dev") {
		t.Fatalf("skip reason = %q, want the not-READY message", why)
	}
	if cell, why := (routeCtx{reg: reg, exact: "m-b"}).profileCell(servedCand, router.Cell{}); why != "" || cell.Model != "m-b" {
		t.Errorf("READY m-b = %+v, %q, want accepted", cell, why)
	}
}

// a cold cache accepts the exact model on the static Responses check, even with the switch on.
func TestProfileCellAcceptsStaticallyOnColdCache(t *testing.T) {
	reg := servedEnv(t, "true", nil)
	cell, why := routeCtx{reg: reg, exact: "m-a"}.profileCell(servedCand, router.Cell{})
	if why != "" || cell.Model != "m-a" {
		t.Fatalf("cold cache = %+v, %q, want m-a accepted", cell, why)
	}
}

// copilotOnDev routes copilot (GitHub-pinned, model pin gpt-6.1-sol) on lead dev, a signed-in served-aware
// Databricks row whose fresh listing has glm-5-3 NOT_READY; it returns the dev dispatches and the skips.
func copilotOnDev(t *testing.T, model string) (dev []string, skipped []string) {
	t.Helper()
	reg := servedEnv(t, "true", nil)
	if err := router.SaveServed(router.Served{Profile: "dev", FetchedAt: time.Now(), Models: []router.ServedModel{
		{ID: "databricks-claude-opus-5-5", Ready: "READY"}, {ID: "databricks-gpt-6-1-sol", Ready: "READY"},
		{ID: "databricks-kimi-k3", Ready: "READY"}, {ID: "databricks-glm-5-3", Ready: "NOT_READY"}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	activateAgents(t, "copilot")
	vaulttest.SetRecord(t, "copilot", "model", "gpt-6.1-sol")
	c, _, err := RouteChainFor(context.Background(), "copilot", model, "sonnet", router.FallbackAll, false,
		router.PickOpts{Configured: allConfigured, Lead: "dev"})
	if err != nil || reg.Providers["dev"] == nil {
		t.Fatal(err)
	}
	for _, d := range c.Dispatches {
		if d.Agent.Name() == "copilot" && strings.HasPrefix(d.Model, "dev:") {
			dev = append(dev, d.Model)
		}
	}
	return dev, c.Skipped
}

// An explicit Claude id runs on the lead profile as asked, not swapped for the agent's model pin.
func TestCallerClaudeIDBeatsModelPinOnLeadProfile(t *testing.T) {
	dev, skipped := copilotOnDev(t, "databricks-claude-opus-5-5")
	if len(dev) != 1 || dev[0] != "dev:databricks-claude-opus-5-5" {
		t.Fatalf("dev dispatches = %v (skipped %v), want dev:databricks-claude-opus-5-5", dev, skipped)
	}
}

// A served non-Claude id is a direct hit on the lead profile; an unserved one is skipped under its own spelling.
func TestCallerExactIDServedOnLeadProfile(t *testing.T) {
	dev, skipped := copilotOnDev(t, "databricks-kimi-k3")
	if len(dev) != 1 || dev[0] != "dev:databricks-kimi-k3" || slices.ContainsFunc(skipped, isCopilotDevSkip) {
		t.Fatalf("kimi: dev dispatches = %v, skipped %v; want one direct dispatch, no dev skip", dev, skipped)
	}
	dev, skipped = copilotOnDev(t, "databricks-glm-5-3")
	if len(dev) != 0 || !slices.ContainsFunc(skipped, func(s string) bool {
		return isCopilotDevSkip(s) && strings.Contains(s, "serves no databricks-glm-5-3")
	}) {
		t.Fatalf("glm: dev dispatches = %v, skipped %v; want a dev skip naming databricks-glm-5-3", dev, skipped)
	}
}

func isCopilotDevSkip(s string) bool { return strings.HasPrefix(s, "copilot (databricks·dev):") }
