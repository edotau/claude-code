package cli

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	usagepkg "github.com/edotau/claude-code/internal/usage"
)

// routeRefresh re-fetches a stale account for a pick; a var so tests stay offline.
var routeRefresh = usagepkg.RefreshAccount

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
	fallback := fs.String("fallback", router.FallbackAll, "all, agents or none")
	route := fs.String("route", "", "fallback ladder: default|first-party (default: by session profile)")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := applyRoute(*route); err != nil {
		return fail(name+": %v", err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat(name, err)
	}
	if !slices.Contains(router.FallbackModes, *fallback) {
		return fail(name+": --fallback %q unknown (%s)", *fallback, strings.Join(router.FallbackModes, "|"))
	}
	if *want != "" && !slices.Contains(router.TableAgents, *want) {
		return fail(name+": --agent %q unknown (%s)", *want, strings.Join(router.TableAgents, "|"))
	}
	res, err := agent.PreviewRoute(*model, *task, *want, *fallback, routeRefresh)
	if err != nil {
		return fail(name+": %v", err)
	}
	src := routeSources{Weights: providers.RouteWeightsSource()}
	src.Subagents.On, src.Subagents.Source = providers.RouteSubagentsOn()
	pins, err := routePins()
	if err != nil {
		return fail(name+": %v", err)
	}
	rj := newRouteJSON(res, src, pins)
	var ps proxyState
	if withProxy {
		if ps, err = loadProxyState(context.Background()); err != nil {
			return fail(name+": %v", err)
		}
	}
	if *format == helptext.JSON {
		var payload any = rj
		if withProxy {
			payload = struct {
				Route routeJSON  `json:"route"`
				Proxy proxyState `json:"proxy"`
			}{rj, ps}
		}
		if err := helptext.WriteJSON(os.Stdout, payload); err != nil {
			return 1
		}
		return 0
	}
	printRoute(res, src, pins)
	if withProxy {
		printProxyState(ps)
	}
	return 0
}

// applyRoute exports --route as HARNESS_ROUTE, so this process's picks (and the jobs it spawns) follow that ladder.
func applyRoute(route string) error {
	if route == "" {
		return nil
	}
	if !slices.Contains(router.RouteModes, route) {
		return fmt.Errorf("--route %q unknown (%s)", route, strings.Join(router.RouteModes, "|"))
	}
	return os.Setenv(router.EnvRoute, route)
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
	Candidates []router.Candidate `json:"candidates"`
	Skipped    []router.Skip      `json:"skipped,omitempty"`
	Sources    routeSources       `json:"sources"`
	Pins       map[string]pinJSON `json:"pins"`
	Ends       string             `json:"ends"` // the claude candidate's account; "" when no claude hop
}

// pinJSON is one agent's routing pins; active is false when the agent has no record profile.
type pinJSON struct {
	Profile string `json:"profile"`
	Model   string `json:"model"`
	Active  bool   `json:"active"`
}

// pinAgents is the order the text view prints pins in.
var pinAgents = []string{"claude", "codex", "copilot", "gemini", "opencode"}

// routePins maps agent.AgentPins onto the emitted shape.
func routePins() (map[string]pinJSON, error) {
	pins, err := agent.AgentPins()
	if err != nil {
		return nil, err
	}
	out := map[string]pinJSON{}
	for a, p := range pins {
		out[a] = pinJSON{Profile: p.Profile, Model: p.Model, Active: !p.Off}
	}
	return out, nil
}

// newRouteJSON maps a pick onto the emitted shape (agent for agent, <provider>·<profile> account keys).
func newRouteJSON(res router.PickResult, src routeSources, pins map[string]pinJSON) routeJSON {
	out := routeJSON{Model: res.Alias, Want: res.Want, Weights: res.Weights, Sources: src,
		Candidates: make([]router.Candidate, 0, len(res.Candidates)), Skipped: res.Skipped, Pins: pins}
	for _, c := range res.Candidates {
		out.Candidates = append(out.Candidates, c)
		if c.Agent == "claude" {
			out.Ends = c.Account
		}
	}
	return out
}

// printRoute renders the pick: weights (and their source), one scored row per candidate, then each
// skip's gate and reason, then the subagents switch and its source.
func printRoute(res router.PickResult, src routeSources, pins map[string]pinJSON) {
	var ws []string
	for _, d := range router.Dimensions {
		ws = append(ws, fmt.Sprintf("%s=%.2f", d.Name(), res.Weights[d.Name()]))
	}
	fmt.Printf("model %s  weights %s (%s)\n", res.Alias, strings.Join(ws, " "), src.Weights)
	onOff := map[bool]string{true: "on", false: "off"}[src.Subagents.On]
	fmt.Printf("subagents %s (%s)\n", onOff, src.Subagents.Source)
	printPins(pins)
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
func printEnds(res router.PickResult) {
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
