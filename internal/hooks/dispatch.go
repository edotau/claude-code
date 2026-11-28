// Package hooks implements `claude-code hook <Event> [--<module>|--all|--async]`: stdin JSON; exit 2 blocks.
package hooks

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/dispatchhook"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooks/hooktelemetry"
	"github.com/edotau/claude-code/internal/hooks/lane"
	"github.com/edotau/claude-code/internal/hooks/memhook"
	"github.com/edotau/claude-code/internal/hooks/pi"
	"github.com/edotau/claude-code/internal/hooks/quality"
	"github.com/edotau/claude-code/internal/hooks/safety"
	"github.com/edotau/claude-code/internal/hooks/secrets"
	"github.com/edotau/claude-code/internal/hooks/wthook"
	"github.com/edotau/claude-code/internal/platform/settings"
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
	n := 0
	for _, g := range settings.Registry {
		n += len(g.Events)
	}
	out := make([]Bound, 0, n)
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

var (
	planSave   = viaStderr(docs.Save)
	planMirror = viaStderr(docs.Mirror)
)

// docsPlan is the docs-plan module: save on ExitPlanMode, tick or bench on Bash, the watchdog at Stop, else mirror.
func docsPlan(in *core.Input) core.Result {
	switch tool := in.CanonicalTool(); {
	case tool == "ExitPlanMode":
		return planSave(in)
	case tool == "Bash":
		return docs.TickOrBench(in)
	case tool == "" && in.Nested("tool_input", "file_path") == "": // a Stop payload names no tool
		return dispatchhook.StopWatched(in)
	}
	return planMirror(in)
}

// runError is a handler that panicked or overran; Outcome is the telemetry word for it.
type runError struct{ msg, outcome string }

func (e *runError) Error() string { return e.msg }

// Keep direct calls and dispatched handlers on the same budget policy.
var gateDeadline = core.BudgetOf

// runBudgeted runs f with panic recovery; wait yields its value or a panic/overrun error. budget <= 0 never expires.
func runBudgeted[T any](f func() T, budget time.Duration, wg *sync.WaitGroup) (wait func() (T, time.Duration, error)) {
	type outcome struct {
		v   T
		dur time.Duration
		err error
	}
	res := make(chan outcome, 1)
	var once sync.Once // the first of the result and the deadline is the outcome
	deliver := func(o outcome) { once.Do(func() { res <- o }) }
	start := time.Now()
	var expire *time.Timer
	if budget > 0 {
		expire = time.AfterFunc(budget, func() {
			deliver(outcome{dur: time.Since(start), err: &runError{fmt.Sprintf("overran its %s budget", budget), hooktelemetry.OutcomeOverrun}})
		})
	}
	stop := func() {
		if expire != nil {
			expire.Stop()
		}
	}
	wg.Go(func() {
		defer func() {
			if p := recover(); p != nil {
				stop()
				deliver(outcome{dur: time.Since(start), err: &runError{fmt.Sprintf("panic: %v", p), hooktelemetry.OutcomePanic}})
			}
		}()
		o := outcome{v: f(), dur: time.Since(start)}
		stop()
		deliver(o)
	})
	return func() (T, time.Duration, error) {
		o := <-res
		return o.v, o.dur, o.err
	}
}

// outcomeOf is the telemetry word for one finished handler: a failed run, or its decision.
func outcomeOf(b Bound, r core.Result, err error) string {
	var re *runError
	switch {
	case errors.As(err, &re):
		return re.outcome
	case r.Decision == core.Block && !b.Spec.Blocks:
		return hooktelemetry.OutcomeBlock
	case r.Decision != core.Allow:
		return hooktelemetry.OutcomeDeny
	}
	return hooktelemetry.OutcomeOK
}

// tagOf is the rule and verdict word of a decision that fired; empty when the handler only allowed or failed.
func tagOf(module string, r core.Result, err error) (rule, verdict string) {
	verdict = r.Verdict
	switch {
	case err != nil:
		return "", ""
	case verdict != "":
	case r.Decision == core.Block:
		verdict = core.VerdictBlock
	case r.Decision == core.Ask:
		verdict = core.VerdictAsk
	case r.UpdatedInput != nil || r.UpdatedToolOutput != nil:
		verdict = core.VerdictRewrite
	default:
		return "", ""
	}
	return cmp.Or(r.Rule, module), verdict
}

// skipOrBlock names what a failed handler's log line reports, keyed by Spec.Blocks.
var skipOrBlock = map[bool]string{false: "skipped", true: "blocked"}

// dispatchMerge accumulates the handlers' results for one Dispatch call.
type dispatchMerge struct {
	merged         core.Result
	ctx, sys, errs []string
	wtAt           []int // ctx indexes of the worktree group: recorded deliveries, kept first when over the cap
	firstErr       error
	log            io.Writer
	rows           []hooktelemetry.Row
	timeoutInput   map[string]any // bash-timeout's updated input, laid over another handler's rewrite at the end
}

// applyTimeout sets bash-timeout's timeout on the merged update, or adopts its input when nothing else rewrote.
func (m *dispatchMerge) applyTimeout() {
	switch {
	case m.timeoutInput == nil:
	case m.merged.UpdatedInput == nil:
		m.merged.UpdatedInput = m.timeoutInput
	default:
		m.merged.UpdatedInput = maps.Clone(m.merged.UpdatedInput)
		m.merged.UpdatedInput["timeout"] = m.timeoutInput["timeout"]
	}
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
	mergeDecision(merged, r, event)
	merged.Floor = merged.Floor || r.Floor
	if r.ExitCode != 0 {
		r.Stderr = fmt.Sprintf("%s\n(hook exit %d)", r.Stderr, r.ExitCode)
	}
	m.ctx, m.sys, m.errs = append(m.ctx, r.Context), append(m.sys, r.SystemMessage), append(m.errs, r.Stderr)
	if b.Group == "worktree" {
		m.wtAt = append(m.wtAt, len(m.ctx)-1)
	}
	if r.UpdatedInput != nil {
		if merged.UpdatedInput != nil {
			m.fail(fmt.Errorf("%s/%s: second updatedInput on %s", b.Group, b.Spec.Name, event))
			return
		}
		merged.UpdatedInput = r.UpdatedInput
	}
	if r.UpdatedToolOutput != nil {
		if merged.UpdatedToolOutput != nil {
			m.fail(fmt.Errorf("%s/%s: second updatedToolOutput on %s", b.Group, b.Spec.Name, event))
			return
		}
		merged.UpdatedToolOutput = r.UpdatedToolOutput
	}
	m.addWorktree(b, r)
}

// joinContext joins the contexts in order; over the cap the worktree group's go first, so the cut falls elsewhere.
func (m *dispatchMerge) joinContext() string {
	joined := core.JoinContext(m.ctx...)
	if capped := core.CapContext(joined); len(m.wtAt) == 0 || capped == joined {
		return capped
	}
	first, rest := make([]string, 0, len(m.ctx)), []string{}
	for i, c := range m.ctx {
		if slices.Contains(m.wtAt, i) {
			first = append(first, c)
		} else {
			rest = append(rest, c)
		}
	}
	return core.CapContext(core.JoinContext(append(first, rest...)...))
}

// mergeDecision keeps the strongest decision's reason; Stop joins every block's reason so each continuation is read.
func mergeDecision(merged *core.Result, r core.Result, event string) {
	switch {
	case r.Decision > merged.Decision:
		merged.Decision, merged.Reason = r.Decision, r.Reason
	case r.Decision == merged.Decision && merged.Reason == "":
		merged.Reason = r.Reason
	case r.Decision == core.Block && r.Decision == merged.Decision && event == settings.EventStop:
		merged.Reason = core.JoinNonEmpty("\n", merged.Reason, r.Reason)
	}
}

// addWorktree folds the worktree group's exit code and raw stdout, the only group allowed either.
func (m *dispatchMerge) addWorktree(b Bound, r core.Result) {
	if r.ExitCode != 0 && b.Spec.Raw && m.merged.ExitCode == 0 {
		m.merged.ExitCode = r.ExitCode
	}
	if r.Stdout == "" {
		return
	}
	if !b.Spec.Raw {
		m.fail(fmt.Errorf("%s/%s: raw stdout is reserved for the worktree group", b.Group, b.Spec.Name))
		return
	}
	m.merged.Stdout = r.Stdout
}

// failed logs a handler that panicked or overran; it reports whether a gate failed closed.
func (m *dispatchMerge) failed(b Bound, err error) (gateFailed bool) {
	fmt.Fprintf(m.log, "hook dispatch: %s/%s %v (%s)\n", b.Group, b.Spec.Name, err, skipOrBlock[b.Spec.Blocks])
	if b.Spec.Blocks { // a gate that could not run fails closed
		m.merged.Decision, m.merged.Reason = core.Block, fmt.Sprintf("%s did not complete (%v); blocked", b.Spec.Name, err)
		m.merged.Floor = true
		gateFailed = true
	}
	if b.Spec.FailExit { // exit 1, not 2: a worktree not made or not removed reports failure
		m.merged.ExitCode = 1
	}
	return gateFailed
}

// step is one matched handler: a skip note, an unreadable-payload block, or a started run to collect.
type step struct {
	b           Bound
	note, block string
	wait        func() (core.Result, time.Duration, error)
}

// plan starts every runnable handler for event at once and lists the steps in declaration order.
func plan(bound []Bound, event string, in *core.Input, wg *sync.WaitGroup) (steps []step) {
	tool, problem := in.CanonicalTool(), payloadProblem(in)
	for _, b := range bound {
		if b.Spec.Event != event {
			continue
		}
		hit := matchTool(b.Spec.Matcher, tool)
		switch note := skipNote(b.Spec.Name); {
		case note != "" && hit:
			steps = append(steps, step{b: b, note: note})
		case problem != "" && b.Spec.Blocks: // tool unknown: every gate fails closed
			steps = append(steps, step{b: b, block: unreadable(b.Spec.Name, problem)})
		case hit:
			run := b.Run // Capture only the runner so the full Bound stays on the stack.
			steps = append(steps, step{b: b, wait: runBudgeted(func() core.Result { return run(in) }, core.BudgetOf(b.Spec), wg)})
		}
	}
	return steps
}

// apply folds one step into the merge in declaration order; it reports whether a gate failed closed.
func (m *dispatchMerge) apply(s step, event string) (gateFailed bool) {
	switch {
	case s.note != "":
		m.sys = append(m.sys, s.note)
	case s.block != "":
		m.merged.Decision, m.merged.Reason, m.merged.Floor = core.Block, s.block, true
	default:
		r, dur, err := s.wait()
		rule, verdict := tagOf(s.b.Spec.Name, r, err)
		m.rows = append(m.rows, hooktelemetry.Row{Event: event, Module: s.b.Spec.Name, Outcome: outcomeOf(s.b, r, err), Dur: dur, Rule: rule, Verdict: verdict})
		if err != nil {
			return m.failed(s.b, err)
		}
		if s.b.Spec.Name == "bash-timeout" && r.UpdatedInput != nil { // it sets one key, so it composes with a rewrite
			m.timeoutInput, r.UpdatedInput = r.UpdatedInput, nil
		}
		m.add(s.b, event, r)
	}
	return false
}

// Dispatch runs every matching handler concurrently and merges results in declaration order; a failed gate blocks.
func Dispatch(bound []Bound, event string, in *core.Input, log io.Writer) (core.Result, error) {
	res, rows, err := dispatch(bound, event, in, log)
	hooktelemetry.Record(rows)
	return res, err
}

func dispatch(bound []Bound, event string, in *core.Input, log io.Writer) (core.Result, []hooktelemetry.Row, error) {
	m := &dispatchMerge{log: log}
	var wg sync.WaitGroup
	steps := plan(bound, event, in, &wg)
	var gateFailed bool
	for _, s := range steps {
		gateFailed = m.apply(s, event) || gateFailed
	}
	if !gateFailed {
		drain(&wg, log)
	}
	m.applyTimeout()
	m.merged.Context = m.joinContext()
	m.merged.SystemMessage = core.JoinNonEmpty("; ", m.sys...)
	m.merged.Stderr = core.JoinNonEmpty("\n", m.errs...)
	for i := range m.rows {
		m.rows[i].Sid, m.rows[i].Tool = in.SessionID, in.ToolName
	}
	return m.merged, m.rows, m.firstErr
}

// hookSkipEnv lists hook modules to skip (or "all"), read only from the hook process's own environment.
const hookSkipEnv = "HARNESS_HOOK_SKIP"

// skipNote is the note for a module HARNESS_HOOK_SKIP skips, or "" when it runs.
func skipNote(name string) string {
	var all bool
	alwaysOn := specOf(name).AlwaysOn
	for f := range strings.SplitSeq(os.Getenv(hookSkipEnv), ",") {
		switch f = strings.TrimSpace(f); {
		case f == name && alwaysOn:
			return "hook " + name + " skipped (" + hookSkipEnv + ") — guards off"
		case f == name, f == "all" && !alwaysOn:
			all = true
		}
	}
	if all {
		return "hook " + name + " skipped (" + hookSkipEnv + ")"
	}
	return ""
}

// unreadable is a gate's fail-closed reason when its hook payload could not be read or parsed.
func unreadable(name, problem string) string {
	return fmt.Sprintf("%s: unreadable hook payload (%s); blocked", name, problem)
}

// runEvent is `hook <Event> [--all|--async]`: every group once, own-process modules only with --all.
func runEvent(event, flag string, stdin io.Reader, stdout, stderr io.Writer) int {
	in := core.ParseInput(stdin)
	res, rows, _ := dispatch(chain(Bind(), in.Agent(), flag), event, in, stderr)
	code := core.Emit(in.Agent(), event, res, stdout, stderr)
	hooktelemetry.Record(rows)
	return code
}

// chain is the bound modules one `hook <Event> [flag]` call runs for agent.
func chain(all []Bound, agent, flag string) []Bound {
	applies := settings.SpecsFor(agent, "native")
	bound := make([]Bound, 0, len(all))
	for _, b := range all {
		if (flag == settings.AllFlag || !b.Gate) && slices.ContainsFunc(applies, func(s settings.EventSpec) bool {
			return s.Name == b.Spec.Name && s.Event == b.Spec.Event && s.Matcher == b.Spec.Matcher
		}) {
			bound = append(bound, b)
		}
	}
	switch {
	case flag == settings.AsyncFlag:
		return asyncOnly(bound)
	case agent == core.AgentClaude && flag == "":
		return syncOnly(bound)
	}
	return bound
}

// syncOnly is Claude's synchronous entry's chain: each module less the Async alternatives its background entry runs.
func syncOnly(bound []Bound) []Bound {
	var out []Bound
	for _, b := range bound {
		if b.Spec.Async != "" {
			async := strings.Split(b.Spec.Async, "|")
			b.Spec.Matcher = strings.Join(slices.DeleteFunc(strings.Split(b.Spec.Matcher, "|"), func(alt string) bool {
				return slices.Contains(async, alt)
			}), "|")
			if b.Spec.Matcher == "" {
				continue
			}
		}
		out = append(out, b)
	}
	return out
}

// asyncOnly is the background entry's chain: each module that has Async alternatives, matching on those alone.
func asyncOnly(bound []Bound) []Bound {
	var out []Bound
	for _, b := range bound {
		if b.Spec.Async != "" {
			b.Spec.Matcher = b.Spec.Async
			out = append(out, b)
		}
	}
	return out
}

const overrunDrainGrace = 200 * time.Millisecond

var drainBudget = overrunDrainGrace

// Let late side effects finish briefly; an expired handler does not get another full budget.
func drain(wg *sync.WaitGroup, log io.Writer) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(drainBudget):
		fmt.Fprintln(log, "hook dispatch: an overrun handler is still running at exit")
	}
}

// native maps every hook module to its Runner; names must match settings.Registry.
var native = map[string]core.Runner{
	"safety-scan":          safety.Scan,
	"rtk-rewrite":          safety.RTKRewrite,
	"review-grep-exit":     quality.GrepExit,
	"bash-timeout":         dispatchhook.BashTimeout,
	"docs-placement":       placementRun,
	"memory-start":         dispatchhook.WithDispatchLabels(memhook.Start),
	"memory-recall":        dispatchhook.WithDispatchLabels(memhook.Recall),
	"paths-resolve":        docs.ResolvePaths,
	"msg-inbox":            wthook.Inbox,
	"msg-inbox-start":      wthook.Inbox,
	"memory-update":        memhook.Stop,
	"review-simplicity":    quality.ReviewSimplicity,
	"review-handback":      quality.ReviewHandback,
	"review-notified":      quality.ReviewHandback,
	"review-decide":        quality.ReviewDecide,
	"format-stop":          quality.StopFormat,
	"memory-end":           memhook.End,
	"dispatch-effort":      dispatchhook.EffortSpawn,
	"codex-agent-route":    dispatchhook.CodexAgentRoute,
	"codex-routing-start":  dispatchhook.CodexRoutingStart,
	"codex-routing-result": dispatchhook.CodexRoutingResult,
	"codex-subagent-start": dispatchhook.CodexSubagentStart,
	"dispatch-failure":     dispatchhook.Release,
	"dispatch-workflow":    dispatchhook.AfterTool,
	"docs-plan":            docsPlan,
	"dispatch-stop":        dispatchhook.Stop,
	"dispatch-handback":    dispatchhook.Handback,
	"memory-compact":       memhook.Compact,
	"worktree-create":      wthook.Create,
	"worktree-remove":      viaStderr(wthook.Remove),
	"worktree-busy":        dispatchhook.BusyWatched,
	"worktree-fixnow":      wthook.FixNow,
	"worktree-claim":       wthook.Claim,
	"worktree-claim-check": wthook.ClaimCheck,
	"worktree-clean-end":   wthook.CleanEnd,
	"lane-land":            lane.Land,
	"lane-land-queue":      lane.Queue,
	"secrets-out":          secrets.Out,
	"secrets-end":          secrets.End,
	"web-tavily":           webTavily,
}

// runDirect is `hook <Event> --<module>`: a blocking module exits 2 with its reason on stderr.
func runDirect(event, name string, stdin io.Reader, stdout, stderr io.Writer) int {
	run, ok := native[name]
	if !ok {
		return core.ExitProceed
	}
	spec := specOn(name, event)
	if note := skipNote(name); note != "" {
		return core.Emit(os.Getenv(settings.AgentEnv), spec.Event, core.Result{SystemMessage: note}, stdout, stderr)
	}
	return runModuleDirect(event, name, run, stdin, stdout, stderr)
}

type directOutcome struct {
	in  *core.Input
	res core.Result
	err error
}

func invokeDirect(name string, run core.Runner, stdin io.Reader, spec settings.EventSpec) directOutcome {
	var parsed atomic.Pointer[core.Input]
	budget := gateDeadline(spec)
	var wg sync.WaitGroup
	o, _, err := runBudgeted(func() directOutcome {
		in := core.ParseInput(stdin)
		parsed.Store(in)
		if problem := payloadProblem(in); problem != "" && spec.Blocks {
			return directOutcome{in, core.Result{Decision: core.Block, Reason: unreadable(name, problem), Floor: true}, nil}
		}
		return directOutcome{in, run(in), nil}
	}, budget, &wg)()
	var re *runError
	switch {
	case !errors.As(err, &re):
		return o
	case spec.Blocks && re.outcome == hooktelemetry.OutcomeOverrun:
		return directOutcome{res: core.Result{Decision: core.Block, Reason: name + " timed out; blocked, retry the command", Floor: true}, err: err}
	case spec.Blocks:
		return directOutcome{parsed.Load(), core.Result{Decision: core.Block, Reason: fmt.Sprintf("%s did not complete (%v); blocked", name, err), Floor: true}, err}
	}
	return directOutcome{parsed.Load(), core.Result{Stderr: fmt.Sprintf("hook %s: %v (skipped)", name, err)}, err}
}

const directWriteBudget = 50 * time.Millisecond

// boundedHookWriter drops diagnostics when the host stops draining stderr.
type boundedHookWriter struct {
	w    io.Writer
	busy atomic.Bool
}

func (w *boundedHookWriter) Write(p []byte) (int, error) {
	if !w.busy.CompareAndSwap(false, true) {
		return len(p), nil
	}
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	data := bytes.Clone(p)
	go func() { n, err := w.w.Write(data); w.busy.Store(false); done <- result{n, err} }()
	select {
	case r := <-done:
		return r.n, r.err
	case <-time.After(directWriteBudget):
		return len(p), nil
	}
}

func emitDirect(event, name string, o directOutcome, stdout, stderr io.Writer) int {
	agent := os.Getenv(settings.AgentEnv)
	if o.in != nil {
		agent = o.in.Agent()
	}
	spec := specOn(name, event)
	if o.res.Decision == core.Block && spec.Blocks {
		return renderBlock(spec, agent, o.res, stdout, stderr)
	}
	o.res.Context = core.CapContext(o.res.Context)
	return core.Emit(agent, spec.Event, o.res, stdout, stderr)
}

func runModuleDirect(event, name string, run core.Runner, stdin io.Reader, stdout, stderr io.Writer) int {
	spec := specOn(name, event)
	start := time.Now()
	o := invokeDirect(name, run, stdin, spec)
	dur := time.Since(start)
	code := emitDirect(event, name, o, stdout, &boundedHookWriter{w: stderr})
	rule, verdict := tagOf(name, o.res, o.err)
	row := hooktelemetry.Row{Event: spec.Event, Module: name, Outcome: outcomeOf(Bound{Spec: spec}, o.res, o.err), Dur: dur, Rule: rule, Verdict: verdict}
	if o.in != nil {
		row.Sid, row.Tool = o.in.SessionID, o.in.ToolName
	}
	hooktelemetry.Record([]hooktelemetry.Row{row})
	return code
}

// renderBlock writes a gate's block for agent: exit 2 with the reason on stderr, or a JSON deny where agy needs one.
func renderBlock(spec settings.EventSpec, agent string, res core.Result, stdout, stderr io.Writer) int {
	d := core.DialectOf(agent)
	if slices.Contains(spec.BlockJSON, agent) {
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

// specOf is the hookspec entry for a module; the zero spec when unknown.
func specOf(name string) settings.EventSpec {
	s, _ := core.SpecOf(name)
	return s
}

// specOn is the hookspec entry for name on event, falling back to specOf(name) when event is empty.
func specOn(name, event string) settings.EventSpec {
	if event != "" {
		for _, s := range settings.Specs() {
			if s.Name == name && s.Event == event {
				return s
			}
		}
	}
	return specOf(name)
}

// declaredOn reports whether hookspec declares name on event (a module may span events, like docs-plan).
func declaredOn(name, event string) bool {
	return slices.ContainsFunc(settings.Specs(), func(s settings.EventSpec) bool { return s.Name == name && s.Event == event })
}

// RunArgs is `hook <Event> [--<module>|--all|--async]`; an unknown event or module fails closed if it reads as a guard.
func RunArgs(event string, flags []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if !slices.Contains(settings.HookEvents(), event) {
		return unknownHook(event, "unknown hook event", stderr)
	}
	switch {
	case len(flags) > 0 && flags[0] == secrets.JobFlag:
		return secrets.RunJob(event, flags[1:])
	case len(flags) == 0:
		return runEvent(event, "", stdin, stdout, stderr)
	case len(flags) == 1 && (flags[0] == settings.AllFlag || flags[0] == settings.AsyncFlag):
		return runEvent(event, flags[0], stdin, stdout, stderr)
	case len(flags) == 1 && strings.HasPrefix(flags[0], "--"):
		return runModule(event, strings.TrimPrefix(flags[0], "--"), stdin, stdout, stderr)
	}
	fmt.Fprintf(stderr, "claude-code hook %s: want no flag, --all, --async or one --<module>, got %q\n", event, flags)
	return 1
}

// runModule runs one module on its declared event.
func runModule(event, name string, stdin io.Reader, stdout, stderr io.Writer) int {
	if name == pi.GuardName && event == settings.EventPreToolUse {
		return pi.Guard(stdin, stdout, stderr, piModules)
	}
	if _, ok := native[name]; !ok || !declaredOn(name, event) {
		return unknownModule(event, name, stderr)
	}
	return runDirect(event, name, stdin, stdout, stderr)
}

// unknownModule decides a declared module on the wrong event by its Blocks flag; an undeclared name by unknownHook.
func unknownModule(event, name string, stderr io.Writer) int {
	what := "no module on " + event + " named"
	spec, declared := core.SpecOf(name)
	if !declared {
		return unknownHook(name, what, stderr)
	}
	if !slices.ContainsFunc(settings.Specs(), func(s settings.EventSpec) bool { return s.Name == name && s.Blocks }) {
		fmt.Fprintf(stderr, "claude-code hook: %s %q\n", what, name)
		return 1
	}
	fmt.Fprintf(stderr, "claude-code hook: %s %q; it runs on %s%s\n", what, name, spec.Event, staleHookHint)
	return core.ExitBlock
}

// staleHookHint names the re-render verbs on a fail-closed unknown module, which a stale hook file causes.
const staleHookHint = "; blocked: re-render stale hook files with `claude-code agents sync` " +
	"(Claude Code's settings.json: `claude-code configure restore` or a hand edit)"

// unknownHook fails closed when the unknown name reads as a guard; a stale agent hook file must not drop its gate.
func unknownHook(name, what string, stderr io.Writer) int {
	hint, code := "", 1
	for _, w := range []string{"guard", "gate", "safety", "scan", "placement"} {
		if strings.Contains(name, w) {
			hint, code = staleHookHint, core.ExitBlock
		}
	}
	fmt.Fprintf(stderr, "claude-code hook: %s %q%s\n", what, name, hint)
	return code
}

// piModules runs the PreToolUse modules Pi sessions load against one tool call; "" allows.
func piModules(tool string, payload []byte) string {
	return piModulesBlock(settings.SpecsFor("pi", "plugin"), tool, payload)
}

// piModulesBlock runs the PreToolUse modules matching tool; fail closed: none judging the call blocks.
func piModulesBlock(specs []settings.EventSpec, tool string, payload []byte) string {
	judged := 0
	for _, s := range specs {
		if s.Event != settings.EventPreToolUse || !matchTool(s.Matcher, tool) {
			continue
		}
		judged++
		if reason := piModuleBlock(s, payload); reason != "" {
			return reason
		}
	}
	if judged == 0 {
		return "pi guard: no guard module matched " + tool + "; blocked"
	}
	return ""
}

// piModuleBlock is one module's block reason ("" allows); fail closed: a missing, failed or asking module blocks.
func piModuleBlock(s settings.EventSpec, payload []byte) string {
	run, ok := native[s.Name]
	if !ok {
		return "pi guard: " + s.Name + " is not available; blocked"
	}
	o := invokeDirect(s.Name, run, bytes.NewReader(payload), s)
	switch {
	case o.res.Decision == core.Block:
		return reasonOr(o.res.Reason, "pi guard: "+s.Name+" blocked the call")
	case o.res.Decision == core.Ask:
		return reasonOr(o.res.Reason, "approval required; blocked") // no one to ask
	case o.err != nil:
		return "pi guard: " + s.Name + " did not complete (" + o.err.Error() + "); blocked"
	}
	return ""
}

func reasonOr(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return reason
}
