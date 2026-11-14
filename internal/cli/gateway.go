package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/gateway/server"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cmdGateway dispatches the gateway verb tree.
func cmdGateway(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "help":
			gatewayPage.Fprint(os.Stdout)
			return 0
		case "proxy":
			return cmdGatewayProxy(args[1:])
		case "token":
			return cmdGatewayToken(args[1:])
		}
		if !strings.HasPrefix(args[0], "-") {
			return helptext.Fail("gateway: unknown command %q (proxy, token)", args[0])
		}
	}
	return gatewayRoute(args)
}

// printProxyState renders the proxy status block and writes it once; a write error is returned.
func printProxyState(ps gateway.Status) error {
	var buf bytes.Buffer
	if ps.Running {
		fmt.Fprintf(&buf, "proxy     running  pid %d  %s  since %s\n", ps.PID, ps.URL, ps.Started)
	} else {
		fmt.Fprintf(&buf, "proxy     not running  (log %s)\n", ps.Log)
	}
	if len(ps.Fallback) == 0 {
		buf.WriteString("fallback  none\n")
	} else {
		fmt.Fprintln(&buf, "fallback ", strings.Join(ps.Fallback, " → "))
	}
	return stdoutf("", "%s", buf.String())
}

// proxyOpts are `gateway proxy`'s flag values.
type proxyOpts struct {
	start, stop, restart, status, serve bool
	port                                int
	format                              *string
	clientToken, revokeClientToken      string
}

func proxyFlags() (*flag.FlagSet, *proxyOpts) {
	fs := helptext.NewFlagSet("gateway proxy", helptext.Page{
		Usage: "claude-code gateway proxy [--start|--stop|--restart|--status|--serve]", // FLAGS lists the rest
		About: "The loopback proxy that translates Anthropic requests for OpenAI-style\n" +
			"providers and falls back across the registry's fallback chain. No flag = --status.",
	})
	o := &proxyOpts{}
	fs.BoolVar(&o.start, "start", false, "start the background proxy if needed; print its base URL")
	fs.BoolVar(&o.stop, "stop", false, "stop the background proxy; stays down until the next start")
	fs.BoolVar(&o.restart, "restart", false, "stop the proxy if running, then start it; print its base URL")
	fs.BoolVar(&o.status, "status", false, "proxy pid, URL and the profile fallback chain; exit 1 when not running")
	fs.BoolVar(&o.serve, "serve", false, "run the proxy in the foreground until interrupted")
	fs.IntVar(
		&o.port,
		"port",
		gateway.DefaultPort,
		"with --serve: loopback port (a busy port held by an older proxy is reclaimed; a foreign holder "+
			"of the default port moves to a free one, recorded in proxy.json; other busy ports are an error)",
	)
	o.format = fs.String("format", helptext.Text, "output: text|json (json: --status only)")
	fs.StringVar(&o.clientToken, "client-token", "", "mint a named editor token; print it once")
	fs.StringVar(&o.revokeClientToken, "revoke-client-token", "", "delete the named editor token")
	return fs, o
}

// check rejects stray args and flag combinations; ok is false when code is the exit status.
func (o *proxyOpts) check(fs *flag.FlagSet) (code int, ok bool) {
	if fs.NArg() > 0 {
		return helptext.Fail("gateway proxy: unexpected argument %q", fs.Arg(0)), false
	}
	if err := helptext.CheckFormat(*o.format); err != nil {
		return helptext.BadFormat("gateway proxy", err), false
	}
	return o.checkCombos(fs)
}

func (o *proxyOpts) modeCount() int {
	n := 0
	for _, b := range []bool{o.start, o.stop, o.restart, o.status, o.serve, o.clientToken != "", o.revokeClientToken != ""} {
		if b {
			n++
		}
	}
	return n
}

func (o *proxyOpts) checkCombos(fs *flag.FlagSet) (code int, ok bool) {
	if o.modeCount() > 1 {
		return helptext.Fail("gateway proxy: at most one of --start, --stop, --restart, --status, --serve, " +
			"--client-token, --revoke-client-token"), false
	}
	portSet := false
	fs.Visit(func(f *flag.Flag) { portSet = portSet || f.Name == "port" })
	if portSet && !o.serve {
		return helptext.Fail("gateway proxy: --port needs --serve"), false
	}
	if *o.format == helptext.JSON && o.modeCount() > 0 && !o.status {
		return helptext.Fail("gateway proxy: --format json applies to --status only"), false
	}
	return 0, true
}

// cmdGatewayProxy is `gateway proxy`: at most one lifecycle flag, no flag = --status; stray args are errors.
func cmdGatewayProxy(args []string) int {
	fs, o := proxyFlags()
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if code, ok := o.check(fs); !ok {
		return code
	}
	ctx := context.Background()
	switch {
	case o.serve:
		return proxyServe(ctx, o.port)
	case o.start:
		return proxyStart(ctx)
	case o.restart:
		return proxyRestart(ctx)
	case o.stop:
		return proxyStopHold(ctx)
	case o.clientToken != "":
		return proxyClientToken(o.clientToken)
	case o.revokeClientToken != "":
		return proxyRevokeClientToken(o.revokeClientToken)
	}
	return proxyStatus(ctx, *o.format)
}

func proxyStart(ctx context.Context) int {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	base, err := gateway.Ensure(ctx)
	if err != nil {
		return helptext.Fail("gateway proxy: %v", err)
	}
	if _, err := fmt.Println(base); err != nil {
		return helptext.Fail("gateway proxy: %v", err)
	}
	return 0
}

// proxyClientToken mints name's client token and prints it once to stdout; the instruction goes to stderr.
func proxyClientToken(name string) int {
	tok, err := server.MintClientToken(name)
	if err != nil {
		return helptext.Fail("gateway proxy: %v", err)
	}
	fmt.Fprintln(os.Stderr, "claude-code: paste this once into VS Code's harnessProxyToken prompt; it is not shown again")
	if _, err := fmt.Println(tok); err != nil {
		return helptext.Fail("gateway proxy: %v", err)
	}
	return 0
}

// proxyRevokeClientToken deletes name's client token; an unknown name exits 1.
func proxyRevokeClientToken(name string) int {
	existed, err := server.RevokeClientToken(name)
	if err != nil {
		return helptext.Fail("gateway proxy: %v", err)
	}
	if !existed {
		fmt.Fprintf(os.Stderr, "claude-code: no client token named %s\n", name)
		return 1
	}
	fmt.Printf("claude-code: revoked client token %s\n", name)
	return 0
}

// proxyServe runs the proxy in the foreground until SIGINT/SIGTERM.
func proxyServe(ctx context.Context, port int) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Serve(ctx, port); err != nil {
		return helptext.Fail("gateway proxy: %v", err)
	}
	return 0
}

func proxyStatus(ctx context.Context, format string) int {
	ps, err := gateway.LoadStatus(ctx)
	if err != nil {
		return helptext.Fail("gateway proxy: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, ps); err != nil {
			return helptext.Fail("gateway proxy: %v", err)
		}
	} else if err := printProxyState(ps); err != nil {
		return helptext.Fail("gateway proxy: %v", err)
	}
	if !ps.Running {
		return 1
	}
	return 0
}

// gatewayPage is bare `gateway`: the route pick and why, then the proxy status; never dispatches.
var gatewayPage = helptext.Page{
	Usage: "claude-code gateway [--model M] [--agent a] [--task T] [flags]\n" +
		"claude-code gateway proxy [--start|--stop|--restart|--status|--serve]",
	About: "Show which agent and model a task routes to: gated on availability, the pinned\n" +
		"profile and quota, then ordered by ladder rung, then account state, then weighted score; " +
		"then the loopback proxy's status.",
	Commands: [][2]string{{"proxy", "start, stop, restart or serve the loopback proxy"}},
}

// gatewayRoute runs the router pick, then the proxy status (text footer or the JSON "proxy" key).
func gatewayRoute(args []string) int {
	const name = "gateway"
	fs, q, overlay, format := gatewayFlags()
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.Fail(name+": unexpected argument %q", fs.Arg(0))
	}
	if !helptext.Explicit(fs, "fallback") {
		q.Fallback = router.DefaultFallback()
	}
	ctx := context.Background()
	ov, err := gatewayOverlay(ctx, fs, *overlay)
	if err != nil {
		return helptext.FailUsage(name+": %v", err)
	}
	ctx, code := checkCoworkerFlags(ctx, name, ov.Confinement(), q.Profile, *format, q.Fallback, q.Agent, q.Model)
	if code != 0 {
		return code
	}

	return resolveGatewayRoute(ctx, *q, *format)
}

// gatewayFlags makes bare gateway's flag set: the route request, --overlay and --format.
func gatewayFlags() (fs *flag.FlagSet, q *agent.RouteReq, overlay, format *string) {
	fs = helptext.NewFlagSet("gateway", gatewayPage)
	q = &agent.RouteReq{Preview: true, Refresh: ask.RouteRefresh}
	fs.StringVar(&q.Model, "model", "", "alias (opus), version (opus-5-5) or wire id (default: classify --task, else sonnet)")
	fs.StringVar(&q.Agent, "agent", "", "the agent asked for; it leads when it passes the gates")
	fs.StringVar(&q.Task, "task", "", "task text the classifier reads when --model is absent")
	fs.StringVar(&q.Fallback, "fallback", "", "all|agents|none; all if "+router.EnvFallback+" set")
	fs.StringVar(&q.Profile, "profile", "", "lead profile, or session")
	fs.StringVar(&q.Provider, "provider", "", "lead on this provider's signed-in profile")
	overlay = fs.String("overlay", "", "dispatch config: file, JSON or -; else $"+agentconfig.OverlayVar)
	return fs, q, overlay, helptext.FormatFlag(fs)
}

func resolveGatewayRoute(ctx context.Context, q agent.RouteReq, format string) int {
	const name = "gateway"
	if err := ask.CheckArm(ctx, q.Agent, q.Profile, q.Provider, q.Model, false); err != nil {
		return helptext.FailUsage(name+": %v", err)
	}
	q.Profile, q.Provider = router.ConfineLeadContext(ctx, q.Agent, q.Profile, q.Provider, q.Model)
	_, res, err := agent.Resolve(ctx, q)
	if _, ok := errors.AsType[*agent.LeadError](err); ok {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 2
	}
	if err != nil {
		return helptext.Fail(name+": %v", err)
	}
	return renderGatewayRoute(ctx, name, res, format)
}

// gatewayOverlay loads the overlay and folds its model and fallback under the typed flags; the rest is not previewed.
func gatewayOverlay(ctx context.Context, fs *flag.FlagSet, value string) (agentconfig.Overlay, error) {
	busy := ""
	if proc.IsTerminal(os.Stdin) {
		busy = "stdin is a terminal; pipe the JSON document in, or pass a file"
	}
	cfg, err := ask.LoadOverlay(ctx, value, busy)
	if err == nil {
		cfg.Fold(fs)
	}
	return cfg, err
}

// checkCoworkerFlags applies the overlay's confinement to ctx and validates the flags; nonzero is the exit (preflight: 1).
func checkCoworkerFlags(ctx context.Context, name, confinement, profile, format, fallback, want, model string) (context.Context, int) {
	ctx, err := ask.ApplyCoworker(ctx, confinement, profile, want, model, fallback)
	if err != nil {
		helptext.Fail(name+": %v", err)
		return ctx, ask.ApplyCode(err)
	}
	if err := helptext.CheckFormat(format); err != nil {
		return ctx, helptext.BadFormat(name, err)
	}
	if !slices.Contains(router.FallbackModes, fallback) {
		return ctx, helptext.Fail(name+": --fallback %q unknown (%s)", fallback, strings.Join(router.FallbackModes, "|"))
	}
	if want != "" && !slices.Contains(router.TableAgents, want) {
		return ctx, helptext.Fail(name+": --agent %q unknown (%s)", want, strings.Join(router.TableAgents, "|"))
	}
	return ctx, 0
}

func renderGatewayRoute(ctx context.Context, name string, res router.PickResult, format string) int {
	src := routeSources{Weights: providers.RouteWeightsSource()}
	pins, err := routePins(ctx)
	if err != nil {
		return helptext.Fail(name+": %v", err)
	}
	rj := newRouteJSON(res, src, pins)
	ps, err := gateway.LoadStatus(ctx)
	if err != nil {
		return helptext.Fail(name+": %v", err)
	}
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail(name, routePayload{rj, ps})
	}
	var buf bytes.Buffer
	writeRoute(&buf, res, src, pins)
	if _, err := os.Stdout.Write(buf.Bytes()); err != nil {
		return helptext.Fail(name+": %v", err)
	}
	if err := printProxyState(ps); err != nil {
		return helptext.Fail(name+": %v", err)
	}
	return 0
}

// routePayload is `gateway --format json`: the route fields at the top level plus the proxy status.
type routePayload struct {
	routeJSON
	Proxy gateway.Status `json:"proxy"`
}

// routeSources names where each routing knob printed by `gateway` came from.
type routeSources struct {
	Weights string `json:"weights"` // "default" or "vault"
}

// routeJSON is the route pick plus where its knobs came from.
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

// routePins maps agent.AgentPinsContext onto the emitted shape.
func routePins(ctx context.Context) (map[string]pinJSON, error) {
	pins, err := agent.AgentPinsContext(ctx)
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

// writeRoute renders weights, pins, scored candidates, the claude end and skipped gates.
func writeRoute(w io.Writer, res router.PickResult, src routeSources, pins map[string]pinJSON) {
	var ws []string
	for _, d := range router.Dimensions {
		ws = append(ws, fmt.Sprintf("%s=%.2f", d.Name(), res.Weights[d.Name()]))
	}
	fmt.Fprintf(w, "model %s  weights %s (%s)\n", res.Alias, strings.Join(ws, " "), src.Weights)
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

// printPins prints each agent's profile[/model] pin (off, or - when unset), wrapped at 80 cols under a 7-space indent.
func printPins(w io.Writer, pins map[string]pinJSON) {
	line, n := "pins   ", 0
	for _, a := range pinAgents {
		tok := a + "=" + cmp.Or(pins[a].Profile, "-")
		if pins[a].Model != "" {
			tok += "/" + pins[a].Model
		}
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

// gatewaySpec describes completion for the gateway command.
func gatewaySpec() *helptext.Node {
	fs, _, _, _ := gatewayFlags()
	proxy, _ := proxyFlags()
	return &helptext.Node{Flags: helptext.FlagsOf(fs, map[string]func() []string{
		"model":    func() []string { return router.Aliases },
		"agent":    func() []string { return router.TableAgents },
		"fallback": func() []string { return router.FallbackModes },
	}), Sub: map[string]*helptext.Node{"proxy": {Flags: helptext.FlagsOf(proxy, nil)}}}
}

func cmdGatewayToken(args []string) int {
	fs := helptext.NewFlagSet("gateway token", helptext.Page{
		Usage: "claude-code gateway token --profile p",
		About: "Print a proxy session token for a credential helper (stdout only).\n" +
			"The token is bound to the proxy daemon and scoped to one profile.",
	})
	profile := fs.String("profile", "", "profile the token is scoped to (required)")
	cfg := fs.String("config-dir", "", "CLAUDE_CONFIG_DIR to use (native Windows helper)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() != 0 || *profile == "" {
		return helptext.Fail("gateway token: want --profile <name> and no other arguments")
	}
	token, code := mintProxyToken(*profile, *cfg)
	if code != 0 {
		return code
	}
	return printToken(token)
}

// mintProxyToken ensures the proxy is up and mints a token for profile; a nonzero code is the exit status.
func mintProxyToken(profile, cfgDir string) (string, int) {
	if cfgDir != "" {
		if err := proc.Setenv("CLAUDE_CONFIG_DIR", cfgDir); err != nil {
			return "", helptext.Fail("gateway token: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	token, err := gateway.Token(ctx, profile)
	if err != nil {
		return "", helptext.Fail("gateway token: %v", err)
	}
	return token, 0
}

func printToken(token string) int {
	if err := stdoutf("gateway token: write stdout", "%s\n", token); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// gatewayStop is gateway.Stop; tests stub it so no live pid is signalled.
var gatewayStop = gateway.Stop

// proxyRestart stops the proxy if running, starts it, and prints its base URL.
func proxyRestart(ctx context.Context) int {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stoppedCode := 0
	if st, err := gatewayStop(ctx); err == nil {
		stoppedCode = sayCode("proxy: stopped pid %d\n", st.PID)
	} else if !errors.Is(err, gateway.ErrNotRunning) {
		return helptext.Fail("gateway proxy: %v", err)
	}
	base, err := gateway.Ensure(ctx)
	if err != nil {
		return helptext.Fail("gateway proxy: %v", err)
	}
	if code := sayCode("%s\n", base); code != 0 {
		return code
	}
	return stoppedCode
}

// sayCode prints one stdout line; a failed write is an error exit, never a silent 0.
func sayCode(format string, a ...any) int {
	if err := stdoutf("gateway proxy: write output", format, a...); err != nil {
		return helptext.Fail("%v", err)
	}
	return 0
}

// proxyStopHold stops the proxy and holds off watchdog restarts until the next start.
func proxyStopHold(ctx context.Context) int {
	if err := gateway.Hold(); err != nil {
		return helptext.Fail("gateway proxy: hold: %v", err)
	}
	st, err := gatewayStop(ctx)
	if errors.Is(err, gateway.ErrNotRunning) {
		return sayCode("proxy: not running\n")
	}
	if err != nil {
		return helptext.Fail("gateway proxy: %v", err)
	}
	return sayCode("proxy: stopped pid %d\n", st.PID)
}
