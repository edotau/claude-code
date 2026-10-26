package cli

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	usagepkg "github.com/edotau/claude-code/internal/usage"
)

// routeRefresh re-fetches a stale account for a pick; a var so tests stay offline.
var routeRefresh = usagepkg.RefreshAccount

// cmdGatewayRoute is `claude-code gateway route`: the pick at a tier and every skipped runner with its gate; it
// reads quota state (refreshing a stale account) and never dispatches.
func cmdGatewayRoute(args []string) int { return gatewayRoute(args, false) }

// gatewayRoute runs the route pick; withProxy (bare `gateway`) adds the proxy status footer or the "proxy" JSON key.
func gatewayRoute(args []string, withProxy bool) int {
	name, usage := "gateway route", "claude-code gateway route [--tier T] [--agent a] [--task <text>] [flags]"
	about := "Show which runner and model a task routes to: gated on availability, the pinned\n" +
		"account and quota, then ranked by a weighted price + headroom score."
	if withProxy {
		name, usage = "gateway", "claude-code gateway [route|proxy] [--tier T] [flags]"
		about += "\nThen the loopback proxy's status; `gateway proxy` runs its lifecycle."
	}
	fs := helptext.NewFlagSet(name, helptext.Page{Usage: usage, About: about})
	tier := fs.String("tier", "", "opus, sonnet or haiku (default: classify --task, else sonnet)")
	want := fs.String("agent", "", "the runner asked for; it leads when it passes the gates")
	task := fs.String("task", "", "task text the classifier reads when --tier is absent")
	fallback := fs.String("fallback", agent.FallbackAll, "all, vendors or none")
	format := outfmt.Flag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := outfmt.Check(*format); err != nil {
		return fail(name+": %v", err)
	}
	if !slices.Contains(agent.FallbackModes, *fallback) {
		return fail(name+": --fallback %q unknown (%s)", *fallback, strings.Join(agent.FallbackModes, "|"))
	}
	t := *tier
	if t == "" && *task != "" {
		t = hooks.RuleClassifier{}.Tier(*task)
	}
	if t == "" {
		t = route.Tiers[1]
	}
	if !slices.Contains(route.Tiers, t) {
		return fail(name+": --tier %q unknown (%s)", t, strings.Join(route.Tiers, "|"))
	}
	if *want != "" && !slices.Contains(route.Runners, *want) {
		return fail(name+": --agent %q unknown (%s)", *want, strings.Join(route.Runners, "|"))
	}
	tb, err := route.Load()
	if err != nil {
		return fail(name+": %v", err)
	}
	res := tb.Pick(context.Background(), t, *want, route.LoadState(),
		route.Opts{Fallback: *fallback, Refresh: routeRefresh})
	src := routeSources{Weights: routeWeightsSource()}
	src.Subagents.On, src.Subagents.Source = providers.RouteSubagentsOn()
	rj := routeJSON{Result: res, Sources: src}
	var ps proxyState
	if withProxy {
		if ps, err = loadProxyState(context.Background()); err != nil {
			return fail(name+": %v", err)
		}
	}
	if *format == outfmt.JSON {
		var payload any = rj
		if withProxy {
			payload = struct {
				Route routeJSON  `json:"route"`
				Proxy proxyState `json:"proxy"`
			}{rj, ps}
		}
		if err := outfmt.Write(os.Stdout, payload); err != nil {
			return 1
		}
		return 0
	}
	printRoute(res, src)
	if withProxy {
		printProxyState(ps)
	}
	return 0
}

// routeSources names where each routing knob printed by `gateway route` came from.
type routeSources struct {
	Weights   string `json:"weights"` // "default" or "vault"
	Subagents struct {
		On     bool   `json:"on"`
		Source string `json:"source"` // "env", "vault" or "default"
	} `json:"subagents"`
}

// routeJSON is `gateway route --format json`'s payload: the pick plus where its knobs came from.
type routeJSON struct {
	route.Result
	Sources routeSources `json:"sources"`
}

// routeWeightsSource reports "vault" when the vault overlay's own routing object sets weights, else
// "default" — derived from presence in the overlay (providers.RoutingOverlay), never from comparing the
// resolved weights to the registry's default.
func routeWeightsSource() string {
	overlay, err := providers.RoutingOverlay()
	if err != nil || overlay.Weights == nil {
		return "default"
	}
	return "vault"
}

// printRoute renders the pick: weights (and their source), one scored row per candidate, then each
// skip's gate and reason, then the subagents switch and its source.
func printRoute(res route.Result, src routeSources) {
	var ws []string
	for _, d := range route.Dimensions {
		ws = append(ws, fmt.Sprintf("%s=%.2f", d.Name(), res.Weights[d.Name()]))
	}
	fmt.Printf("tier %s  weights %s (%s)\n", res.Tier, strings.Join(ws, " "), src.Weights)
	onOff := map[bool]string{true: "on", false: "off"}[src.Subagents.On]
	fmt.Printf("subagents %s (%s)\n", onOff, src.Subagents.Source)
	if len(res.Candidates) == 0 {
		fmt.Println("no runner passes the gates")
	}
	wRun, wModel, wAcct := len("RUNNER"), len("MODEL"), len("ACCOUNT")
	for _, c := range res.Candidates {
		wRun, wModel, wAcct = max(wRun, len(c.Runner)), max(wModel, len(c.Model)), max(wAcct, len(c.Account))
	}
	if len(res.Candidates) > 0 {
		fmt.Printf("%-3s %-*s %-*s %-6s %-*s %-8s %6s %8s %6s\n", "#", wRun, "RUNNER", wModel, "MODEL", "EFFORT",
			wAcct, "ACCOUNT", "STATE", "PRICE", "HEADROOM", "TOTAL")
	}
	for i, c := range res.Candidates {
		fmt.Printf("%-3d %-*s %-*s %-6s %-*s %-8s %6.2f %8.2f %6.2f\n", i+1, wRun, c.Runner, wModel, c.Model,
			cmp.Or(c.Effort, "-"), wAcct, c.Account, c.State, c.Scores["price"], c.Scores["headroom"], c.Total)
	}
	if len(res.Skipped) == 0 {
		return
	}
	wSkip, wGate := len("SKIP"), len("GATE")
	for _, s := range res.Skipped {
		wSkip, wGate = max(wSkip, len(s.Runner)), max(wGate, len(s.Gate))
	}
	fmt.Printf("%-*s %-*s %s\n", wSkip, "SKIP", wGate, "GATE", "REASON")
	for _, s := range res.Skipped {
		fmt.Printf("%-*s %-*s %s\n", wSkip, s.Runner, wGate, s.Gate, s.Reason)
	}
}
