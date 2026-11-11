// Package hooks implements `claude-code hook <Event> [--<module>|--all]`: JSON on stdin, exit 0 = proceed, 2 = block.
// Hooks fail OPEN — an infra glitch never blocks, except a Blocks gate's panic, overrun, or empty/unparseable payload.
package hooks

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/hooks/agents"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooks/memhook"
	"github.com/edotau/claude-code/internal/hooks/quality"
	"github.com/edotau/claude-code/internal/hooks/safety"
	"github.com/edotau/claude-code/internal/hooks/wthook"
	"github.com/edotau/claude-code/internal/settings"
)

// Bound pairs a declared handler with its Runner.
type Bound struct {
	Group string
	Gate  bool
	Spec  settings.EventSpec
	Run   core.Runner
}

// Bind pairs every hookspec handler with its native Runner.
func Bind() []Bound {
	var out []Bound
	for _, g := range settings.Registry {
		for _, s := range g.Events {
			out = append(out, Bound{Group: g.Name, Gate: g.Gate, Spec: s, Run: native[s.Name]})
		}
	}
	return out
}

// viaStderr makes a Runner of a helper that reports its notes on a stderr writer and returns an exit code.
func viaStderr(f func(*core.Input, io.Writer) int) core.Runner {
	return func(in *core.Input) core.Result {
		var b bytes.Buffer
		code := f(in, &b)
		return core.ExitResult(code, b.String())
	}
}

// matchers caches each handler matcher compiled once per Dispatch; a matcher that does not compile is nil.
type matchers map[string]*regexp.Regexp

// matches applies a handler matcher to a tool name; empty matches everything, anchored and case-insensitive.
func (c matchers) matches(matcher, tool string) bool {
	if matcher == "" {
		return true
	}
	re, seen := c[matcher]
	if !seen {
		re, _ = regexp.Compile(`(?i)^(?:` + matcher + `)$`)
		c[matcher] = re
	}
	return re != nil && tool != "" && re.MatchString(tool)
}

// runBudgeted runs one Runner under its deadline with panic recovery; a panic or overrun is an error.
func runBudgeted(b Bound, in *core.Input, wg *sync.WaitGroup) (core.Result, error) {
	type outcome struct {
		r   core.Result
		err error
	}
	done := make(chan outcome, 1)
	wg.Go(func() {
		defer func() {
			if p := recover(); p != nil {
				done <- outcome{err: fmt.Errorf("panic: %v", p)}
			}
		}()
		done <- outcome{r: b.Run(in)}
	})
	budget := b.Spec.Budget
	if budget <= 0 {
		budget = 5 * time.Second
	}
	if b.Spec.Blocks {
		budget -= min(core.HookMargin/2, budget/2)
	}
	select {
	case o := <-done:
		return o.r, o.err
	case <-time.After(budget):
		return core.Result{}, fmt.Errorf("overran its %s budget", budget)
	}
}

// skipOrBlock names what a failed handler's log line reports, keyed by Spec.Blocks.
var skipOrBlock = map[bool]string{false: "skipped", true: "blocked"}

// failClosedModules exit non-zero when they fail: a worktree not made or not removed must never exit 0.
var failClosedModules = map[string]bool{"worktree-create": true, "worktree-remove": true}

// dispatchMerge accumulates the handlers' results for one Dispatch call.
type dispatchMerge struct {
	merged         core.Result
	ctx, sys, errs []string
	firstErr       error
	log            io.Writer
}

func (m *dispatchMerge) fail(err error) {
	fmt.Fprintln(m.log, "hook dispatch:", err)
	if m.firstErr == nil {
		m.firstErr = err
	}
}

// add folds one successful handler result into the merge.
func (m *dispatchMerge) add(b Bound, event string, r core.Result) {
	merged := &m.merged
	if r.Decision > merged.Decision {
		merged.Decision, merged.Reason = r.Decision, r.Reason
	} else if r.Decision == merged.Decision && merged.Reason == "" {
		merged.Reason = r.Reason
	}
	if r.ExitCode != 0 {
		r.Stderr = fmt.Sprintf("%s\n(hook exit %d)", r.Stderr, r.ExitCode)
	}
	m.ctx, m.sys, m.errs = append(m.ctx, r.Context), append(m.sys, r.SystemMessage), append(m.errs, r.Stderr)
	if r.UpdatedInput != nil {
		if merged.UpdatedInput != nil {
			m.fail(fmt.Errorf("%s/%s: second updatedInput on %s", b.Group, b.Spec.Name, event))
			return
		}
		merged.UpdatedInput = r.UpdatedInput
	}
	m.addWorktree(b, r)
}

// addWorktree folds the worktree group's exit code and raw stdout, the only group allowed either.
func (m *dispatchMerge) addWorktree(b Bound, r core.Result) {
	if r.ExitCode != 0 && b.Group == "worktree" && m.merged.ExitCode == 0 {
		m.merged.ExitCode = r.ExitCode
	}
	if r.Stdout == "" {
		return
	}
	if b.Group != "worktree" {
		m.fail(fmt.Errorf("%s/%s: raw stdout is reserved for the worktree group", b.Group, b.Spec.Name))
		return
	}
	m.merged.Stdout = r.Stdout
}

// Dispatch runs, in order, every bound handler on event whose matcher hits and merges the results. A handler
// that panics or overruns is logged and skipped (a Blocks gate blocks, failClosedModules exit 1); a second
// updatedInput or raw stdout errors.
func Dispatch(bound []Bound, event string, in *core.Input, log io.Writer) (core.Result, error) {
	m := &dispatchMerge{log: log}
	var wg sync.WaitGroup
	var longest time.Duration
	var gateFailed bool
	match := matchers{}
	for _, b := range bound {
		if b.Spec.Event != event {
			continue
		}
		if problem := payloadProblem(in); problem != "" && b.Spec.Blocks { // tool unknown: every gate fails closed
			m.merged.Decision, m.merged.Reason = core.Block, unreadable(b.Spec.Name, problem)
			continue
		}
		if !match.matches(b.Spec.Matcher, in.CanonicalTool()) {
			continue
		}
		longest = max(longest, b.Spec.Budget)
		r, err := runBudgeted(b, in, &wg)
		if err != nil {
			fmt.Fprintf(log, "hook dispatch: %s/%s %v (%s)\n", b.Group, b.Spec.Name, err, skipOrBlock[b.Spec.Blocks])
			if b.Spec.Blocks { // a gate that could not run fails closed
				m.merged.Decision, m.merged.Reason = core.Block, fmt.Sprintf("%s did not complete (%v); blocked", b.Spec.Name, err)
				gateFailed = true
			}
			if failClosedModules[b.Spec.Name] { // exit 1, not 2: a worktree not made or not removed reports failure
				m.merged.ExitCode = 1
			}
			continue
		}
		m.add(b, event, r)
	}
	if !gateFailed {
		drain(&wg, longest, log)
	}
	m.merged.Context = core.CapContext(core.JoinContext(m.ctx...))
	m.merged.SystemMessage = core.JoinNonEmpty("; ", m.sys...)
	m.merged.Stderr = core.JoinNonEmpty("\n", m.errs...)
	return m.merged, m.firstErr
}

// unreadable is a gate's fail-closed reason when its hook payload could not be read or parsed.
func unreadable(name, problem string) string {
	return fmt.Sprintf("%s: unreadable hook payload (%s); blocked", name, problem)
}

// runEvent is `hook <Event> [--all]`: run every group once, own-process modules only when all is set.
func runEvent(event string, all bool, stdin io.Reader, stdout, stderr io.Writer) int {
	in := core.ParseInput(stdin)
	applies := settings.SpecsFor(in.Agent(), "native")
	var bound []Bound
	for _, b := range Bind() {
		if (all || !b.Gate) && slices.ContainsFunc(applies, func(s settings.EventSpec) bool { return s.Name == b.Spec.Name }) {
			bound = append(bound, b)
		}
	}
	res, _ := Dispatch(bound, event, in, stderr)
	return core.Emit(in.Agent(), event, res, stdout, stderr)
}

// drain waits up to one longest-budget period for overrun handlers; any still running is logged and abandoned.
func drain(wg *sync.WaitGroup, longest time.Duration, log io.Writer) {
	if longest <= 0 {
		longest = 5 * time.Second
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(longest):
		fmt.Fprintln(log, "hook dispatch: an overrun handler is still running at exit")
	}
}

// native maps every hook module to its Runner; names must match settings.Registry.
var native = map[string]core.Runner{
	"safety-scan":          safety.Scan,
	"rtk-rewrite":          safety.RTKRewrite,
	"docs-placement":       viaStderr(docs.Placement),
	"memory-start":         memhook.Start,
	"memory-recall":        memhook.Recall,
	"paths-resolve":        docs.ResolvePaths,
	"memory-update":        memhook.Stop,
	"review-simplicity":    quality.ReviewSimplicity,
	"format-stop":          quality.StopFormat,
	"memory-end":           memhook.End,
	"dispatch-effort":      agents.Effort,
	"codex-agent-route":    agents.CodexAgentRoute,
	"codex-routing-start":  agents.CodexRoutingStart,
	"codex-routing-result": agents.CodexRoutingResult,
	"codex-subagent-start": agents.CodexSubagentStart,
	"dispatch-failure":     agents.Release,
	"dispatch-workflow":    agents.Release,
	"docs-plan-gate":       viaStderr(docs.Gate),
	"docs-plan-tick":       docs.Tick,
	"docs-plan-reminder":   docs.Reminder,
	"docs-plan-mirror":     viaStderr(docs.Mirror),
	"docs-plan-save":       viaStderr(docs.Save),
	"dispatch-stop":        agents.Stop,
	"dispatch-handback":    agents.Handback,
	"memory-compact":       memhook.Compact,
	"worktree-create":      wthook.Create,
	"worktree-remove":      viaStderr(wthook.Remove),
	"worktree-mode":        wthook.Mode,
	"worktree-busy":        wthook.Busy,
	"worktree-claim":       wthook.Claim,
	"worktree-claim-check": wthook.ClaimCheck,
	"permission-gate":      safety.PermissionGate,
}

// gateDeadline is how long a Blocks module may run in its own process: the host's timeout less half HookMargin, so a
// handler's HookContext fail-open (a full margin early) still wins and the host's timer never lets the call through.
var gateDeadline = func(s settings.EventSpec) time.Duration {
	host := time.Duration(s.Seconds()) * time.Second
	if d := host - core.HookMargin/2; d > 0 {
		return d
	}
	return host / 2
}

// runDirect is `hook <Event> --<module>`: one Runner rendered for its own process. A module that blocks exits 2 with
// its reason on stderr, which is how a guard run by an agent or by safety-scan's own entry blocks. A Blocks module
// that overruns gateDeadline (stdin read included) blocks with a retry reason; any other module has no deadline.
func runDirect(name string, stdin io.Reader, stdout, stderr io.Writer) int {
	run, ok := native[name]
	if !ok {
		return core.ExitProceed
	}
	spec := specOf(name)
	if !spec.Blocks {
		return runModuleDirect(name, run, stdin, stdout, stderr)
	}
	g := &gateWriters{}
	done := make(chan int, 1)
	go func() { done <- runModuleDirect(name, run, stdin, g.wrap(stdout), g.wrap(stderr)) }()
	select {
	case code := <-done:
		return code
	case <-time.After(gateDeadline(spec)):
		g.close() // the late module can no longer write; one that finished at the wire keeps its own verdict
		select {
		case code := <-done:
			return code
		default:
		}
		res := core.Result{Decision: core.Block, Reason: name + " timed out; blocked, retry the command"}
		return renderBlock(name, os.Getenv(settings.AgentEnv), res, stdout, stderr)
	}
}

// gateWriters hands a module's writers to a goroutine that may be abandoned: after close their writes are dropped.
type gateWriters struct {
	mu     sync.Mutex
	closed bool
}

func (g *gateWriters) wrap(w io.Writer) io.Writer { return gateWriter{g, w} }

func (g *gateWriters) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

type gateWriter struct {
	g *gateWriters
	w io.Writer
}

func (gw gateWriter) Write(p []byte) (int, error) {
	gw.g.mu.Lock()
	defer gw.g.mu.Unlock()
	if gw.g.closed {
		return len(p), nil
	}
	return gw.w.Write(p)
}

// runModuleDirect parses stdin, runs one module and renders its result for the agent that sent it.
func runModuleDirect(name string, run core.Runner, stdin io.Reader, stdout, stderr io.Writer) int {
	in := core.ParseInput(stdin)
	res := runGated(name, run, in, stderr)
	if res.Decision == core.Block && specOf(name).Blocks {
		return renderBlock(name, in.Agent(), res, stdout, stderr)
	}
	res.Context = core.CapContext(res.Context)
	return core.Emit(in.Agent(), eventOf(name), res, stdout, stderr)
}

// renderBlock writes a gate's block for agent: exit 2 with the reason on stderr, or a JSON deny where agy needs one.
func renderBlock(name, agent string, res core.Result, stdout, stderr io.Writer) int {
	d := core.DialectOf(agent)
	if name == "safety-scan" && agent == core.AgentCopilot {
		fmt.Fprintln(stdout, d.Build(settings.EventPreToolUse, res))
	}
	if d.JSONDeny { // agy blocks on a JSON deny, not exit 2
		if _, err := fmt.Fprintln(stdout, d.Build(settings.EventPreToolUse, res)); err != nil {
			fmt.Fprintf(stderr, "%s (deny JSON not written: %v)\n", res.Reason, err)
			return core.ExitBlock // fail closed: a lost deny must not proceed
		}
		return core.ExitProceed
	}
	fmt.Fprintln(stderr, res.Reason)
	return core.ExitBlock
}

// payloadProblem is why a gate cannot trust its payload: it did not parse, or the host sent nothing on a gated event.
func payloadProblem(in *core.Input) string {
	if in.Invalid != "" {
		return in.Invalid
	}
	if len(bytes.TrimSpace(in.Raw)) == 0 {
		return "empty payload"
	}
	return ""
}

// runGated runs one module, except a Blocks gate on a payload that was empty or did not parse: that blocks unrun.
// A panic is logged to stderr: a Blocks module blocks, any other proceeds (Go's own exit 2 would read as a block).
func runGated(name string, run core.Runner, in *core.Input, stderr io.Writer) (res core.Result) {
	blocks := specOf(name).Blocks
	if problem := payloadProblem(in); problem != "" && blocks {
		return core.Result{Decision: core.Block, Reason: unreadable(name, problem)}
	}
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(stderr, "hook %s: panic: %v (%s)\n", name, p, skipOrBlock[blocks])
			res = core.Result{}
			if blocks {
				res = core.Result{Decision: core.Block, Reason: fmt.Sprintf("%s did not complete (panic: %v); blocked", name, p)}
			}
		}
	}()
	return run(in)
}

// specOf is the hookspec entry for a module; the zero spec when unknown.
func specOf(name string) settings.EventSpec {
	s, _ := core.SpecOf(name)
	return s
}

// eventOf names the hookspec event a handler is declared on.
func eventOf(name string) string { return specOf(name).Event }

// RunArgs is `hook <Event> [--<module>|--all]`; an unknown event or module fails closed only when it reads as a guard.
func RunArgs(event string, flags []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if !slices.Contains(settings.HookEvents(), event) {
		return unknownHook(event, "unknown hook event", stderr)
	}
	switch {
	case len(flags) == 0:
		return runEvent(event, false, stdin, stdout, stderr)
	case len(flags) == 1 && flags[0] == settings.AllFlag:
		return runEvent(event, true, stdin, stdout, stderr)
	case len(flags) == 1 && strings.HasPrefix(flags[0], "--"):
		return runModule(event, strings.TrimPrefix(flags[0], "--"), stdin, stdout, stderr)
	}
	fmt.Fprintf(stderr, "claude-code hook %s: want no flag, --all or one --<module>, got %q\n", event, flags)
	return 1
}

// runModule runs one module on its declared event.
func runModule(event, name string, stdin io.Reader, stdout, stderr io.Writer) int {
	if _, ok := native[name]; !ok || eventOf(name) != event {
		return unknownModule(event, name, stderr)
	}
	return runDirect(name, stdin, stdout, stderr)
}

// unknownModule decides a declared module on the wrong event by its Blocks flag; an undeclared name by unknownHook.
func unknownModule(event, name string, stderr io.Writer) int {
	what := "no module on " + event + " named"
	spec, declared := core.SpecOf(name)
	if !declared {
		return unknownHook(name, what, stderr)
	}
	if !spec.Blocks {
		fmt.Fprintf(stderr, "claude-code hook: %s %q\n", what, name)
		return 1
	}
	fmt.Fprintf(stderr, "claude-code hook: %s %q; it runs on %s\n", what, name, spec.Event)
	return core.ExitBlock
}

// unknownHook fails closed when the unknown name reads as a guard; a stale agent hook file must not drop its gate.
func unknownHook(name, what string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "claude-code hook: %s %q\n", what, name)
	for _, w := range []string{"guard", "gate", "safety", "scan", "placement"} {
		if strings.Contains(name, w) {
			return core.ExitBlock
		}
	}
	return 1
}
