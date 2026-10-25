package cli

import (
	"context"
	"encoding/json"
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

// cmdRoute is `claude-code route`: the router's pick at a tier and every skipped runner with its gate; it reads
// quota state (refreshing a stale account) and never dispatches.
func cmdRoute(args []string) int {
	fs := helptext.NewFlagSet("route", helptext.Page{
		Usage: "claude-code route [--tier T] [--agent a] [--task \"<text>\"] [flags]",
		About: "Show which runner and model a task routes to: gated on availability, the pinned\n" +
			"account and quota, then ranked by a weighted price + headroom score.",
	})
	tier := fs.String("tier", "", "opus, sonnet or haiku (default: classify --task, else sonnet)")
	want := fs.String("agent", "", "the runner asked for; it leads when it passes the gates")
	task := fs.String("task", "", "task text the classifier reads when --tier is absent")
	fallback := fs.String("fallback", agent.FallbackAll, "all, vendors or none")
	format := outfmt.Flag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("route: %v", err)
	}
	if !slices.Contains(agent.FallbackModes, *fallback) {
		return fail("route: --fallback %q unknown (%s)", *fallback, strings.Join(agent.FallbackModes, "|"))
	}
	t := *tier
	if t == "" && *task != "" {
		t = hooks.RuleClassifier{}.Tier(*task)
	}
	if t == "" {
		t = route.Tiers[1]
	}
	if !slices.Contains(route.Tiers, t) {
		return fail("route: --tier %q unknown (%s)", t, strings.Join(route.Tiers, "|"))
	}
	if *want != "" && !slices.Contains(route.Runners, *want) {
		return fail("route: --agent %q unknown (%s)", *want, strings.Join(route.Runners, "|"))
	}
	tb, err := route.Load()
	if err != nil {
		return fail("route: %v", err)
	}
	res := tb.Pick(context.Background(), t, *want, route.LoadState(),
		route.Opts{Fallback: *fallback, Refresh: routeRefresh})
	src := routeSources{Weights: routeWeightsSource()}
	src.Subagents.On, src.Subagents.Source = providers.RouteSubagentsOn()
	if *format == outfmt.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(routeJSON{Result: res, Sources: src}); err != nil {
			return 1
		}
		return 0
	}
	printRoute(res, src)
	return 0
}

// routeSources names where each routing knob printed by `route` came from.
type routeSources struct {
	Weights   string `json:"weights"` // "default" or "vault"
	Subagents struct {
		On     bool   `json:"on"`
		Source string `json:"source"` // "env", "vault" or "default"
	} `json:"subagents"`
}

// routeJSON is `route --format json`'s payload: the pick plus where its knobs came from.
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
	} else {
		fmt.Printf("%-3s %-9s %-32s %-6s %-26s %-8s %6s %8s %6s\n", "#", "RUNNER", "MODEL", "EFFORT", "ACCOUNT", "STATE", "PRICE", "HEADROOM", "TOTAL")
	}
	for i, c := range res.Candidates {
		fmt.Printf("%-3d %-9s %-32s %-6s %-26s %-8s %6.2f %8.2f %6.2f\n", i+1, c.Runner, c.Model, dashIf(c.Effort),
			c.Account, c.State, c.Scores["price"], c.Scores["headroom"], c.Total)
	}
	for _, s := range res.Skipped {
		fmt.Printf("skip %-9s %-10s %s\n", s.Runner, s.Gate, s.Reason)
	}
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
