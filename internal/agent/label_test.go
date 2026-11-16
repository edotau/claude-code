package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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

// An untyped opencode run on a Databricks row runs the brief's tier: a typo fix and an audit run different models.
func TestRoutedModelRunsTaskTier(t *testing.T) {
	setConfigDir(t)
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeOverlay)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	run := func(prompt, model string, cell bool) string {
		req := RunOptions{Prompt: prompt, Model: model, CellModel: cell}
		got, _, err := SchemaFor("opencode").routedModel(context.Background(), req, providers.Target{Provider: reg.Providers["dev"]}, false)
		if err != nil {
			t.Fatalf("routedModel(%q): %v", prompt, err)
		}
		return got.Model
	}
	trivial, hard := "Fix the typo in internal/agent/label.go.", "Audit the safety-scan rules; needs judgment."
	if a, b := run(trivial, "sonnet", true), run(hard, "sonnet", true); a == b {
		t.Errorf("trivial and audit briefs both run %s, want the tier to pick different models", a)
	}
	if a, b := run(trivial, "dev:claude-opus-5-5", false), run(hard, "dev:claude-opus-5-5", false); a != b {
		t.Errorf("a typed model ran %s and %s, want the typed model on both", a, b)
	}
	vaulttest.SetRecord(t, "opencode", "model", "databricks-claude-sonnet-5-5")
	if a, b := run(trivial, "", false), run(hard, "", false); a != b {
		t.Errorf("a pinned agent ran %s and %s, want its pin on both", a, b)
	}
}

// TaskTier reads a Tier: line first, then deep, fast and standard signals from the work text alone.
func TestTaskTier(t *testing.T) {
	for _, tc := range []struct{ brief, want string }{
		{"Fix the typo in internal/agent/label.go.", "haiku"},
		{"Move internal/a.go to internal/b/a.go.", "haiku"},
		{"Add a --json flag to agents status in internal/cli/agents.go and internal/agent/status.go.", "sonnet"},
		{"Audit the safety-scan rules for false blocks.", "opus"},
		{"Write the parser for the Run: grammar.", "opus"},
		{"Tier: fast\nAudit the safety-scan rules.", "haiku"},
		{"Fix the typo.\nTier: deep", "opus"},
		{"Run: claude-code agents ask --agent opencode --profile dev\nRename one variable in x.go.", "haiku"},
		{"Rename one variable.\nVerify: `claude-code review simplify --fail` and an audit of nothing", "haiku"},
		{"Rename the helper across the codebase.", "sonnet"},
		{"", ""},
	} {
		if got, sig := TaskTier(tc.brief); got != tc.want {
			t.Errorf("TaskTier(%q) = %q (%s), want %q", tc.brief, got, sig, tc.want)
		}
	}
}

// A tier the row's fresh listing shows not READY falls back to the untiered pick and says so.
func TestResolveRunTierNotServed(t *testing.T) {
	setConfigDir(t)
	reg := routeRegistry(t)
	now := time.Now()
	if err := router.SaveServed(router.Served{Profile: "dev", FetchedAt: now, Models: []router.ServedModel{
		{ID: "databricks-claude-opus-5-5", Ready: "READY"}}}); err != nil {
		t.Fatal(err)
	}
	q := ModelQuery{Agent: "opencode", Profile: "dev", Default: "haiku"}
	r, err := ResolveRun(reg, q, now)
	base, _ := ResolveModel(reg, ModelQuery{Agent: "opencode", Profile: "dev"}, now)
	if err != nil || r.Model != base.Model || !strings.Contains(r.Note, "task tier haiku is not served") {
		t.Errorf("ResolveRun = %+v, %v; want the untiered %s with a note", r, err, base.Model)
	}
	q.Default = "opus"
	if r, err := ResolveRun(reg, q, now); err != nil || r.Note != "" {
		t.Errorf("served tier = %+v, %v; want it with no note", r, err)
	}
}

// An untyped local claude dispatch runs the brief's tier; a typed model, a claude pin or --host keep theirs.
func TestClaudeTierOnUntypedDispatch(t *testing.T) {
	setConfigDir(t)
	vaulttest.Use(t)
	d := Dispatch{Agent: labelEngine{stubEngine{name: "claude"}}, keepCaller: true, unroutedDispatch: true}
	brief := "Fix the typo in internal/agent/label.go."
	if got := dispatchRequest(RunOptions{Prompt: brief}, d, true).Model; got != "haiku" {
		t.Errorf("untyped claude model = %q, want the task tier haiku", got)
	}
	for _, req := range []RunOptions{{Prompt: brief, Model: "opus"}, {Prompt: brief, Host: "h"}} {
		if got := dispatchRequest(req, d, true).Model; got != req.Model {
			t.Errorf("%+v ran %q, want %q unchanged", req, got, req.Model)
		}
	}
	vaulttest.SetRecord(t, "claude", "model", "opus")
	if got := dispatchRequest(RunOptions{Prompt: brief}, d, true).Model; got != "" {
		t.Errorf("pinned claude model = %q, want the pin left to the launch", got)
	}
}
