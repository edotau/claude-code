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

// PreviewRoute resolves the pick through the chain `ask` runs, so each candidate carries the model it would execute;
// a dispatch dropped after the pick (model or profile gate) moves to Skipped. It starts no agent.
func PreviewRoute(model, task, want, fallback string, refresh func(context.Context, string) bool) (router.PickResult, error) {
	tb, err := router.Load()
	if err != nil {
		return router.PickResult{}, err
	}
	hint, err := previewHint(tb, model, task)
	if err != nil {
		return router.PickResult{}, err
	}
	chain, pick, err := RouteChainFor(context.Background(), want, model, hint, fallback, false,
		router.PickOpts{Refresh: refresh, WorkRepo: func() bool { return router.IsWorkRepo("") }})
	if err != nil {
		return router.PickResult{}, err
	}
	return previewOutcome(chain, pick), nil
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
	dispatches := chain.Dispatches
	for _, c := range pick.Candidates {
		if len(dispatches) == 0 || dispatches[0].Agent.Name() != c.Agent || dispatches[0].Account != c.Account {
			continue
		}
		if c.Agent != "claude" { // the terminal claude hop keeps its cell id; dispatch passes the same row's alias
			c.Model, c.Effort = dispatches[0].Model, dispatches[0].Effort
		}
		res.Candidates, dispatches = append(res.Candidates, c), dispatches[1:]
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
