package gateway

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cmdGatewayRouter is `claude-code gateway router`: the pick and each skipped agent's gate; never dispatches.
func cmdGatewayRouter(args []string) int { return gatewayRoute(args, false) }

// routePage is the verb's name and help page; withProxy is bare `gateway`.
func routePage(withProxy bool) (string, helptext.Page) {
	about := "Show which agent and model a task routes to: gated on availability, the pinned\n" +
		"profile and quota, then ordered by ladder rung, then account state, then weighted score."
	if withProxy {
		return "gateway", helptext.Page{Usage: "claude-code gateway [router|proxy] [--model M] [flags]",
			About: about + "\nThen the loopback proxy's status; `gateway proxy` runs its lifecycle."}
	}
	return "gateway router", helptext.Page{
		Usage: "claude-code gateway router [--model M] [--agent a] [--task T] [flags]", About: about}
}

// gatewayRoute runs the router pick; withProxy (bare `gateway`) adds the proxy status footer or the "proxy" JSON key.
func gatewayRoute(args []string, withProxy bool) int {
	name, page := routePage(withProxy)
	fs := helptext.NewFlagSet(name, page)
	model := fs.String("model", "", "alias (opus) or version (opus-5-5) (default: classify --task, else sonnet)")
	want := fs.String("agent", "", "the agent asked for; it leads when it passes the gates")
	task := fs.String("task", "", "task text the classifier reads when --model is absent")
	fallback := fs.String("fallback", router.FallbackAll, "all, agents or none")
	route := fs.String("route", "", "fallback ladder: default|first-party (default: by session profile)")
	profile := fs.String("profile", "", "lead profile, or session")
	provider := fs.String("provider", "", "lead on this provider's signed-in profile")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.Fail(name+": unexpected argument %q", fs.Arg(0))
	}
	if code := checkRouteFlags(name, *route, *format, *fallback, *want); code != 0 {
		return code
	}
	q := agent.RouteReq{Agent: *want, Model: *model, Task: *task, Profile: *profile, Provider: *provider,
		Fallback: *fallback, Preview: true, Refresh: ask.RouteRefresh}
	_, res, err := agent.Resolve(context.Background(), q)
	if _, ok := errors.AsType[*agent.LeadError](err); ok {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 2
	}
	if err != nil {
		return helptext.Fail(name+": %v", err)
	}
	return renderGatewayRoute(name, res, *format, withProxy)
}

// checkRouteFlags applies --route and validates the flags; a nonzero code is the exit status (format errors are 2).
func checkRouteFlags(name, route, format, fallback, want string) int {
	if err := ask.ApplyRoute(route); err != nil {
		return helptext.Fail(name+": %v", err)
	}
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat(name, err)
	}
	if !slices.Contains(router.FallbackModes, fallback) {
		return helptext.Fail(name+": --fallback %q unknown (%s)", fallback, strings.Join(router.FallbackModes, "|"))
	}
	if want != "" && !slices.Contains(router.TableAgents, want) {
		return helptext.Fail(name+": --agent %q unknown (%s)", want, strings.Join(router.TableAgents, "|"))
	}
	return 0
}

func renderGatewayRoute(name string, res router.PickResult, format string, withProxy bool) int {
	src := routeSources{Weights: providers.RouteWeightsSource()}
	src.Subagents.On, src.Subagents.Source = providers.RouteSubagentsOn()
	pins, err := routePins()
	if err != nil {
		return helptext.Fail(name+": %v", err)
	}
	rj := newRouteJSON(res, src, pins)
	var ps proxy.Status
	if withProxy {
		if ps, err = proxy.LoadStatus(context.Background()); err != nil {
			return helptext.Fail(name+": %v", err)
		}
	}
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("gateway router", routePayload(rj, ps, withProxy))
	}
	var buf bytes.Buffer
	writeRoute(&buf, res, src, pins)
	if _, err := os.Stdout.Write(buf.Bytes()); err != nil {
		return helptext.Fail(name+": %v", err)
	}
	if withProxy {
		if err := printProxyState(ps); err != nil {
			return helptext.Fail(name+": %v", err)
		}
	}
	return 0
}

// routePayload is the JSON document: the route alone, or {route, proxy} for bare `gateway`.
func routePayload(rj routeJSON, ps proxy.Status, withProxy bool) any {
	if !withProxy {
		return rj
	}
	return struct {
		Route routeJSON    `json:"route"`
		Proxy proxy.Status `json:"proxy"`
	}{rj, ps}
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

// pinAgents is the routed agents, sorted: the order the text view prints pins in.
var pinAgents = slices.Sorted(slices.Values(providers.RouteAgents))

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
		Candidates: append([]router.Candidate{}, res.Candidates...), Skipped: res.Skipped, Pins: pins}
	for _, c := range res.Candidates {
		if c.Agent == "claude" {
			out.Ends = c.Account
			break
		}
	}
	return out
}

// printRoute writes the text view to stdout, ignoring write errors (tests); the verb checks them.
func printRoute(res router.PickResult, src routeSources, pins map[string]pinJSON) {
	writeRoute(os.Stdout, res, src, pins)
}

// writeRoute renders weights, subagents switch, pins, scored candidates, the claude end and skipped gates.
func writeRoute(w io.Writer, res router.PickResult, src routeSources, pins map[string]pinJSON) {
	var ws []string
	for _, d := range router.Dimensions {
		ws = append(ws, fmt.Sprintf("%s=%.2f", d.Name(), res.Weights[d.Name()]))
	}
	fmt.Fprintf(w, "model %s  weights %s (%s)\n", res.Alias, strings.Join(ws, " "), src.Weights)
	onOff := map[bool]string{true: "on", false: "off"}[src.Subagents.On]
	fmt.Fprintf(w, "subagents %s (%s)\n", onOff, src.Subagents.Source)
	printPins(w, pins)
	printCandidates(w, res.Candidates)
	printEnds(w, res)
	printSkipped(w, res.Skipped)
}

// printCandidates prints the scored candidate table.
func printCandidates(w io.Writer, cands []router.Candidate) {
	if len(cands) == 0 {
		fmt.Fprintln(w, "no agent passes the gates")
		return
	}
	wRun, wModel, wAcct := len("AGENT"), len("MODEL"), len("ACCOUNT")
	for _, c := range cands {
		wRun, wModel, wAcct = max(wRun, len(c.Agent)), max(wModel, len(c.Model)), max(wAcct, len(c.Account))
	}
	fmt.Fprintf(w, "%-3s %-*s %-*s %-6s %-*s %-8s %6s %8s %6s\n", "#", wRun, "AGENT", wModel, "MODEL", "EFFORT",
		wAcct, "ACCOUNT", "STATE", "PRICE", "HEADROOM", "TOTAL")
	for i, c := range cands {
		fmt.Fprintf(w, "%-3d %-*s %-*s %-6s %-*s %-8s %6.2f %8.2f %6.2f\n", i+1, wRun, c.Agent, wModel, c.Model,
			cmp.Or(c.Effort, "-"), wAcct, c.Account, c.State, c.Scores["price"], c.Scores["headroom"], c.Total)
	}
}

// printSkipped prints each skipped agent with its gate and reason.
func printSkipped(w io.Writer, skipped []router.Skip) {
	if len(skipped) == 0 {
		return
	}
	wSkip, wGate := len("SKIP"), len("GATE")
	for _, s := range skipped {
		wSkip, wGate = max(wSkip, len(s.Agent)), max(wGate, len(s.Gate))
	}
	fmt.Fprintf(w, "%-*s %-*s %s\n", wSkip, "SKIP", wGate, "GATE", "REASON")
	for _, s := range skipped {
		fmt.Fprintf(w, "%-*s %-*s %s\n", wSkip, s.Agent, wGate, s.Gate, s.Reason)
	}
}

// printPins prints each agent's profile pin (off, or - when unset), wrapped at 80 cols under a 7-space indent.
func printPins(w io.Writer, pins map[string]pinJSON) {
	line, n := "pins   ", 0
	for _, a := range pinAgents {
		tok := a + "=" + cmp.Or(pins[a].Profile, "-")
		if !pins[a].Active {
			tok = a + "=off"
		}
		if n > 0 && len(line)+1+len(tok) > 80 {
			fmt.Fprintln(w, line)
			line, n = "       ", 0
		}
		if n > 0 {
			line += " "
		}
		line, n = line+tok, n+1
	}
	fmt.Fprintln(w, line)
}

// printEnds names where the chain ends: the claude hop's account and model, or why there is none.
func printEnds(w io.Writer, res router.PickResult) {
	for _, c := range res.Candidates {
		if c.Agent == "claude" {
			fmt.Fprintf(w, "ends at claude on %s (%s)\n", c.Account, c.Model)
			return
		}
	}
	for _, s := range res.Skipped {
		if s.Agent == "claude" {
			fmt.Fprintf(w, "no claude hop: %s\n", s.Reason)
			return
		}
	}
	fmt.Fprintln(w, "no claude hop (--fallback agents|none)")
}
