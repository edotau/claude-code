package agent

import (
	"context"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
)

type labelEngine struct{ stubEngine }

func (labelEngine) Run(context.Context, RunOptions, wire.Sink) (wire.Result, error) {
	return wire.Result{}, nil
}

// A ladder hop labels the model its engine runs: a `<row>:` cell as typed, an alias cell the row's own pick (opus).
func TestLadderHopIsTheRunModel(t *testing.T) {
	reg := routeRegistry(t)
	for _, tc := range []struct {
		name, want, cell, account, wantModel string
		keepCaller                           bool
	}{
		{"codex cell", "codex", "prod:databricks-gpt-6-1-sol", "databricks·prod", "databricks-gpt-6-1-sol", false},
		{"opencode alias cell", "opencode", "sonnet", "databricks·dev", "databricks-claude-opus-5-5", false},
		{"opencode keeps caller", "opencode", "dev:databricks-claude-sonnet-5-5", "databricks·dev", "databricks-claude-opus-5-5", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Dispatch{Agent: labelEngine{stubEngine{name: tc.want}}, Model: tc.cell, Account: tc.account, keepCaller: tc.keepCaller}
			hop, note, err := ladderHop(reg, ModelQuery{Agent: tc.want}, d)
			if err != nil || note != "" {
				t.Fatalf("hop = %+v, %q, %v", hop, note, err)
			}
			r, err := ResolveModel(reg, hop, time.Now())
			if err != nil || r.Label() != tc.wantModel {
				t.Errorf("hop %+v labels %q (%v), want %q", hop, r.Label(), err, tc.wantModel)
			}
		})
	}
	d := Dispatch{Agent: labelEngine{stubEngine{name: "claude"}}, Model: "sonnet", Account: "anthropic·nowhere"}
	if hop, note, _ := ladderHop(reg, ModelQuery{Agent: "codex"}, d); hop.Agent != "claude" || hop.Model != "sonnet" || hop.Profile != "" || note == "" {
		t.Errorf("fallback hop = %+v %q, want claude on sonnet, no unknown profile, and a note", hop, note)
	}
}

// A typed model, profile or overlay row is the route as written: no ladder runs.
func TestRunQueryTypedSkipsLadder(t *testing.T) {
	for _, q := range []ModelQuery{{Agent: "codex", Model: "gpt-6-1-sol"}, {Agent: "opencode", Profile: "dev"},
		{Agent: "copilot", Account: "x"}, {Agent: "claude"}} {
		if got, note, err := RunQuery(context.Background(), routeRegistry(t), q, "haiku"); got != q || note != "" || err != nil {
			t.Errorf("RunQuery(%+v) = %+v %q %v, want it unchanged", q, got, note, err)
		}
	}
}
