package cli

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/routetable"
	usagepkg "github.com/edotau/claude-code/internal/usage"
	"github.com/edotau/claude-code/internal/vendors"
)

// routeRefresh re-fetches a stale account for a pick; a var so tests stay offline.
var routeRefresh = usagepkg.RefreshAccount

func init() { vendors.CopilotUsedPct = copilotUsedPct }

// copilotUsedPct is github·<profile>'s used share from route state, refreshed through routeRefresh past its TTL.
func copilotUsedPct(ctx context.Context, login string) (float64, bool) {
	reg, _ := providers.Load()
	key, now := "github·"+providers.GithubAccountName(reg, login), time.Now()
	e := router.LoadState()[key]
	if e.ExhaustedUntil.After(now) {
		return router.ExhaustedPct, true
	}
	if now.Sub(e.Seen) > router.StaleAfter {
		if !routeRefresh(ctx, key) {
			return 0, false
		}
		e = router.LoadState()[key]
	}
	return e.UsedPct, !e.Seen.IsZero()
}

// cmdGatewayRouter is `claude-code gateway router`: the pick for a model and every skipped agent with its gate; it
// reads quota state (refreshing a stale account) and never dispatches.
func cmdGatewayRouter(args []string) int { return gatewayRoute(args, false) }

// gatewayRoute runs the router pick; withProxy (bare `gateway`) adds the proxy status footer or the "proxy" JSON key.
func gatewayRoute(args []string, withProxy bool) int {
	name, usage := "gateway router", "claude-code gateway router [--model M] [--agent a] [--task T] [flags]"
	about := "Show which agent and model a task routes to: gated on availability, the pinned\n" +
		"profile and quota, then ranked by a weighted price + headroom score."
	if withProxy {
		name, usage = "gateway", "claude-code gateway [router|proxy] [--model M] [flags]"
		about += "\nThen the loopback proxy's status; `gateway proxy` runs its lifecycle."
	}
	fs := helptext.NewFlagSet(name, helptext.Page{Usage: usage, About: about})
	model := fs.String("model", "", "alias (opus) or version (opus-5-5) (default: classify --task, else sonnet)")
	want := fs.String("agent", "", "the agent asked for; it leads when it passes the gates")
	task := fs.String("task", "", "task text the classifier reads when --model is absent")
	fallback := fs.String("fallback", agent.FallbackAll, "all, agents or none")
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
	if *want != "" && !slices.Contains(router.Runners, *want) {
		return fail(name+": --agent %q unknown (%s)", *want, strings.Join(router.Runners, "|"))
	}
	res, err := previewRoute(*model, *task, *want, *fallback)
	if err != nil {
		return fail(name+": %v", err)
	}
	src := routeSources{Weights: routeWeightsSource()}
	src.Subagents.On, src.Subagents.Source = providers.RouteSubagentsOn()
	rj := newRouteJSON(res, src)
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

// previewRoute resolves the pick through the chain `ask` runs, so each candidate carries the model it would execute;
// a leg dropped after the pick (model or profile gate) moves to Skipped. It starts no agent.
func previewRoute(model, task, want, fallback string) (router.Result, error) {
	tb, err := router.Load()
	if err != nil {
		return router.Result{}, err
	}
	id, _, ok := models.Resolve(model)
	if exact := ok && !strings.HasPrefix(id, "claude-"); model != "" && !exact && tb.TierOf(model) == "" {
		return router.Result{}, fmt.Errorf("--model %q names no alias or version the table routes", model)
	}
	hint := tb.TierOf(model)
	if hint == "" && task != "" {
		hint = hooks.RuleClassifier{}.Tier(task)
	}
	if hint == "" {
		hint = router.Tiers[1]
	}
	chain, pick, err := agent.RouteChainFor(context.Background(), want, model, hint, fallback, false,
		router.Opts{Refresh: routeRefresh, WorkRepo: func() bool { return router.IsWorkRepo("") }})
	if err != nil {
		return router.Result{}, err
	}
	res := pick
	res.Candidates = nil
	legs := chain.Legs
	for _, c := range pick.Candidates {
		if len(legs) == 0 || legs[0].Agent.Name() != c.Agent || legs[0].Account != c.Account {
			continue
		}
		if c.Agent != "claude" { // the terminal claude hop keeps its cell id; dispatch passes the same row's alias
			c.Model, c.Effort = legs[0].Model, legs[0].Effort
		}
		res.Candidates, legs = append(res.Candidates, c), legs[1:]
	}
	res.Skipped = slices.Clone(pick.Skipped)
	for _, s := range chain.Skipped {
		if slices.ContainsFunc(pick.Skipped, func(k router.Skip) bool { return s == k.Agent+": "+k.Reason }) {
			continue
		}
		who, why, _ := strings.Cut(s, ": ")
		res.Skipped = append(res.Skipped, router.Skip{Agent: who, Gate: "model", Reason: why})
	}
	return res, nil
}

// routeSources names where each routing knob printed by `gateway router` came from.
type routeSources struct {
	Weights   string `json:"weights"` // "default" or "vault"
	Subagents struct {
		On     bool   `json:"on"`
		Source string `json:"source"` // "env", "vault" or "default"
	} `json:"subagents"`
}

// routeJSON is `gateway router --format json`'s payload: the pick plus where its knobs came from.
type routeJSON struct {
	Model      string             `json:"model"`
	Want       string             `json:"want,omitempty"`
	Weights    map[string]float64 `json:"weights"`
	Candidates []candidateJSON    `json:"candidates"`
	Skipped    []skipJSON         `json:"skipped,omitempty"`
	Sources    routeSources       `json:"sources"`
	Pins       map[string]pinJSON `json:"pins"`
	Ends       string             `json:"ends"` // the claude candidate's account; "" when no claude hop
}

// pinJSON is one agent's routing pins; active is false when <AGENT>_ACTIVE=off.
type pinJSON struct {
	Profile string `json:"profile"`
	Model   string `json:"model"`
	Active  bool   `json:"active"`
}

// pinAgents is the order the text view prints pins in.
var pinAgents = []string{"claude", "codex", "copilot", "gemini", "opencode"}

// routePins maps providers.AgentPins onto the emitted shape.
func routePins() map[string]pinJSON {
	out := map[string]pinJSON{}
	for a, p := range providers.AgentPins() {
		out[a] = pinJSON{Profile: p.Profile, Model: p.Model, Active: !p.Off}
	}
	return out
}

// candidateJSON is one runnable agent; account_key is <provider>·<profile>.
type candidateJSON struct {
	Agent      string             `json:"agent"`
	Model      string             `json:"model"`
	Effort     string             `json:"effort,omitempty"`
	AccountKey string             `json:"account_key"`
	Price      routetable.Price   `json:"price"`
	State      string             `json:"state"`
	Billing    string             `json:"billing"`
	Scores     map[string]float64 `json:"scores,omitempty"`
	Total      float64            `json:"total"`
	Profile    string             `json:"profile,omitempty"`
}

// skipJSON is an agent dropped by a gate, with why.
type skipJSON struct {
	Agent  string `json:"agent"`
	Gate   string `json:"gate"`
	Reason string `json:"reason"`
}

// newRouteJSON maps a pick onto the emitted shape (agent for runner, <provider>·<profile> account keys).
func newRouteJSON(res router.Result, src routeSources) routeJSON {
	out := routeJSON{Model: res.Tier, Want: res.Want, Weights: res.Weights, Sources: src,
		Candidates: make([]candidateJSON, 0, len(res.Candidates)), Pins: routePins()}
	for _, c := range res.Candidates {
		out.Candidates = append(out.Candidates, candidateJSON{Agent: c.Agent, Model: c.Model, Effort: c.Effort,
			AccountKey: c.Account, Price: c.Price, State: c.State, Billing: c.Billing, Scores: c.Scores, Total: c.Total,
			Profile: c.Profile})
		if c.Agent == "claude" {
			out.Ends = c.Account
		}
	}
	for _, s := range res.Skipped {
		out.Skipped = append(out.Skipped, skipJSON{Agent: s.Agent, Gate: s.Gate, Reason: s.Reason})
	}
	return out
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
func printRoute(res router.Result, src routeSources) {
	var ws []string
	for _, d := range router.Dimensions {
		ws = append(ws, fmt.Sprintf("%s=%.2f", d.Name(), res.Weights[d.Name()]))
	}
	fmt.Printf("model %s  weights %s (%s)\n", res.Tier, strings.Join(ws, " "), src.Weights)
	onOff := map[bool]string{true: "on", false: "off"}[src.Subagents.On]
	fmt.Printf("subagents %s (%s)\n", onOff, src.Subagents.Source)
	printPins(routePins())
	if len(res.Candidates) == 0 {
		fmt.Println("no agent passes the gates")
	}
	wRun, wModel, wAcct := len("AGENT"), len("MODEL"), len("ACCOUNT")
	for _, c := range res.Candidates {
		wRun, wModel, wAcct = max(wRun, len(c.Agent)), max(wModel, len(c.Model)), max(wAcct, len(c.Account))
	}
	if len(res.Candidates) > 0 {
		fmt.Printf("%-3s %-*s %-*s %-6s %-*s %-8s %6s %8s %6s\n", "#", wRun, "AGENT", wModel, "MODEL", "EFFORT",
			wAcct, "ACCOUNT", "STATE", "PRICE", "HEADROOM", "TOTAL")
	}
	for i, c := range res.Candidates {
		fmt.Printf("%-3d %-*s %-*s %-6s %-*s %-8s %6.2f %8.2f %6.2f\n", i+1, wRun, c.Agent, wModel, c.Model,
			cmp.Or(c.Effort, "-"), wAcct, c.Account, c.State, c.Scores["price"], c.Scores["headroom"], c.Total)
	}
	printEnds(res)
	if len(res.Skipped) == 0 {
		return
	}
	wSkip, wGate := len("SKIP"), len("GATE")
	for _, s := range res.Skipped {
		wSkip, wGate = max(wSkip, len(s.Agent)), max(wGate, len(s.Gate))
	}
	fmt.Printf("%-*s %-*s %s\n", wSkip, "SKIP", wGate, "GATE", "REASON")
	for _, s := range res.Skipped {
		fmt.Printf("%-*s %-*s %s\n", wSkip, s.Agent, wGate, s.Gate, s.Reason)
	}
}

// printPins prints each agent's profile pin (off, or - when unset), wrapped at 80 cols under a 7-space indent.
func printPins(pins map[string]pinJSON) {
	line, n := "pins   ", 0
	for _, a := range pinAgents {
		tok := a + "=" + cmp.Or(pins[a].Profile, "-")
		if !pins[a].Active {
			tok = a + "=off"
		}
		if n > 0 && len(line)+1+len(tok) > 80 {
			fmt.Println(line)
			line, n = "       ", 0
		}
		if n > 0 {
			line += " "
		}
		line, n = line+tok, n+1
	}
	fmt.Println(line)
}

// printEnds names where the chain ends: the claude hop's account and model, or why there is none.
func printEnds(res router.Result) {
	for _, c := range res.Candidates {
		if c.Agent == "claude" {
			fmt.Printf("ends at claude on %s (%s)\n", c.Account, c.Model)
			return
		}
	}
	for _, s := range res.Skipped {
		if s.Agent == "claude" {
			fmt.Printf("no claude hop: %s\n", s.Reason)
			return
		}
	}
	fmt.Println("no claude hop (--fallback agents|none)")
}
