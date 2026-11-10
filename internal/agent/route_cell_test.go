package agent

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/router"
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
