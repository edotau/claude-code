package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// RosterPick fills o's unset Configured, Pins, CanServe and CodexModel from the roster (the router imports no agent).
func RosterPick(o router.PickOpts) (router.PickOpts, error) {
	if o.Configured == nil {
		o.Configured = Configured
	}
	if o.Pins == nil {
		pins, err := AgentPins()
		if err != nil {
			return router.PickOpts{}, err
		}
		o.Pins = pins
	}
	if o.CanServe == nil {
		o.CanServe = CanServe
	}
	if o.CodexModel == nil {
		o.CodexModel = func(t providers.Target) providers.Target {
			t, _ = ResolveAgentModel(ModelAsk{Agent: "codex", Target: t})
			return t
		}
	}
	return o, nil
}

// previewRoute resolves the pick through the chain `ask` runs, so each candidate carries the model it would execute;
// a dispatch dropped after the pick (model or profile gate) moves to Skipped.
func previewRoute(ctx context.Context, q RouteReq, lead string) (Chain, router.PickResult, error) {
	tb, err := router.Load()
	if err != nil {
		return Chain{}, router.PickResult{}, err
	}
	hint, err := previewHint(tb, q.Model, q.Task)
	if err != nil {
		return Chain{}, router.PickResult{}, err
	}
	chain, pick, err := chainFor(ctx, q, hint, lead)
	if err != nil {
		return Chain{}, router.PickResult{}, err
	}
	return chain, previewOutcome(chain, pick), nil
}

func previewHint(tb *router.RouteTable, model, task string) (string, error) {
	id, _, ok := models.Resolve(model)
	if exact := ok && models.FamilyKind(id) != models.FamilyClaude; model != "" && model != router.DiscoveryModel && !exact && tb.AliasOf(model) == "" {
		return "", fmt.Errorf("--model %q names no alias or version the table routes", model)
	}
	hint := tb.AliasOf(model)
	if hint == "" && task != "" {
		hint = router.RuleClassifier{}.Alias(task)
	}
	if hint == "" {
		hint = router.Aliases[1]
	}
	return hint, nil
}

func previewOutcome(chain Chain, pick router.PickResult) router.PickResult {
	res := pick
	res.Candidates = nil
	for _, d := range chain.Dispatches {
		if d.rung < 1 || d.rung > len(pick.Candidates) {
			continue
		}
		c := pick.Candidates[d.rung-1]
		if c.Agent != "claude" { // the terminal claude hop keeps its cell id; dispatch passes the same row's alias
			c.Model, c.Effort = d.Model, d.Effort
		}
		res.Candidates = append(res.Candidates, c)
	}
	res.Skipped = slices.Clone(pick.Skipped)
	for _, s := range chain.Skipped {
		if slices.ContainsFunc(pick.Skipped, func(k router.Skip) bool { return s == k.Agent+": "+k.Reason }) {
			continue
		}
		who, why, _ := strings.Cut(s, ": ")
		res.Skipped = append(res.Skipped, router.Skip{Agent: who, Gate: "model", Reason: why})
	}
	return res
}
