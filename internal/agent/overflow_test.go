package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/router"
)

// A Sonnet cell whose prompt overflows re-picks a larger window; kept or largest models fail naming the fact.
func TestPlanOverflowRepicksByWindow(t *testing.T) {
	autoFixture(t)
	disp := Dispatch{Agent: &fakeRunner{name: "opencode"}, Account: router.ProviderAccount("dev")}
	over := errors.New("opencode: prompt is too long: 230000 tokens > 200000 maximum")
	cell := RunOptions{Model: "databricks-claude-sonnet-4-6", CellModel: true, Fallback: router.FallbackAll}
	plan := planOverflow(disp, cell, cell.Model, over)
	if plan.model == "" || plan.to <= plan.from || plan.refusal != "" {
		t.Fatalf("cell plan = %+v, want a larger-window re-pick", plan)
	}
	for name, req := range map[string]RunOptions{"none": {Model: cell.Model, CellModel: true, Fallback: router.FallbackNone},
		"typed": {Model: cell.Model, ModelTyped: true, Fallback: router.FallbackAll}} {
		kept := planOverflow(disp, req, req.Model, over)
		if kept.model != "" || !strings.Contains(kept.refusal, "larger windows:") {
			t.Fatalf("%s plan = %+v, want a refusal naming larger ids", name, kept)
		}
	}
	top := planOverflow(disp, cell, "databricks-gpt-6-1-sol", over)
	if top.model != "" || !strings.Contains(top.refusal, "no model with a larger window") {
		t.Fatalf("largest plan = %+v, want the nothing-larger refusal", top)
	}
	if got := planOverflow(disp, cell, cell.Model, errors.New("429 too many tokens per minute")); got != (overflowPlan{}) {
		t.Fatalf("a rate limit planned an overflow: %+v", got)
	}
}
