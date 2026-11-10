// Package hooks implements `claude-code hook <Event> [--<module>|--all]`: JSON on stdin, exit 0 = proceed, 2 = block.
// Hooks fail OPEN — an infra glitch never blocks, except a Blocks gate's panic, overrun or unparseable payload.
package hooks

import (
	"bytes"
	"fmt"
	"io"
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
	"github.com/edotau/claude-code/internal/textutil"
)

// Bound pairs a declared handler with its Runner.
type Bound struct {
	Group     string
	Gate      bool
	AgentOnly bool
	Spec      settings.EventSpec
	Run       core.Runner
}

// Bind pairs every hookspec handler with its native Runner.
func Bind() []Bound {
	var out []Bound
	for _, g := range settings.Registry {
		for _, s := range g.Events {
			out = append(out, Bound{Group: g.Name, Gate: g.Gate, AgentOnly: g.AgentOnly, Spec: s, Run: native[s.Name]})
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
	m.ctx, m.sys, m.errs = appendNonEmpty(m.ctx, r.Context), appendNonEmpty(m.sys, r.SystemMessage), appendNonEmpty(m.errs, r.Stderr)
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
	match := matchers{}
	for _, b := range bound {
		if b.Spec.Event != event {
			continue
		}
		if in.Invalid != "" && b.Spec.Blocks { // the tool is unknown, so every gate on the event fails closed
			m.merged.Decision, m.merged.Reason = core.Block, unreadable(b.Spec.Name, in.Invalid)
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
			}
			if failClosedModules[b.Spec.Name] { // exit 1, not 2: a worktree not made or not removed reports failure
				m.merged.ExitCode = 1
			}
			continue
		}
		m.add(b, event, r)
	}
	drain(&wg, longest, log)
	m.merged.Context = core.CapContext(strings.Join(m.ctx, "\n\n"))
	m.merged.SystemMessage = strings.Join(m.sys, "; ")
	m.merged.Stderr = strings.Join(m.errs, "\n")
	return m.merged, m.firstErr
}

// unreadable is a gate's fail-closed reason when its hook payload could not be read or parsed.
func unreadable(name, problem string) string {
	return fmt.Sprintf("%s: unreadable hook payload (%s); blocked", name, problem)
}

func appendNonEmpty(list []string, s string) []string {
	if s == "" {
		return list
	}
	return append(list, s)
}

// isEvent reports whether name is a lifecycle event the dispatcher serves (`claude-code hook <Event>`).
func isEvent(name string) bool {
	for _, g := range settings.Registry {
		for _, s := range g.Events {
			if s.Event == name {
				return true
			}
		}
	}
	return false
}

// loadHarnessHooks is the vault read behind enabledGroups; a test seam.
var loadHarnessHooks = settings.LoadHarnessHooks

// enabledGroups reads the user's harnessHooks switches; gate groups are never switched off, errors mean all on.
func enabledGroups() func(group string, gate bool) bool {
	sw, err := loadHarnessHooks()
	if err != nil {
		sw = nil
	}
	return func(group string, gate bool) bool {
		on, set := sw[group]
		return gate || !set || on
	}
}

// runEvent is `hook <Event> [--all]`: run every enabled group once, own-process modules only when all is set.
func runEvent(event string, all bool, stdin io.Reader, stdout, stderr io.Writer) int {
	in := core.ParseInput(stdin)
	enabled := enabledGroups()
	var bound []Bound
	for _, b := range Bind() {
		switch {
		case b.AgentOnly && in.Agent() == core.AgentClaude, !enabled(b.Group, b.Gate):
		case !all && (b.Gate || b.Spec.Async):
		case in.Agent() != core.AgentClaude && !slices.Contains(b.Spec.Agents, in.Agent()):
		default:
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
	"safety-scan":               safety.Scan,
	"rtk-rewrite":               safety.RTKRewrite,
	"docs-placement":            viaStderr(docs.Placement),
	"memory-start":              memhook.Start,
	"memory-recall":             memhook.Recall,
	"paths-resolve":             docs.ResolvePaths,
	"memory-update":             memhook.Stop,
	"review-simplicity":         quality.ReviewSimplicity,
	"format-stop":               quality.StopFormat,
	"memory-end":                memhook.End,
	"dispatch-effort":           agents.Effort,
	"codex-agent-route":         agents.CodexAgentRoute,
	"codex-routing-start":       agents.CodexRoutingStart,
	"codex-routing-result":      agents.CodexRoutingResult,
	"codex-subagent-start":      agents.CodexSubagentStart,
	"dispatch-failure":          agents.Release,
	"dispatch-workflow":         agents.Release,
	"docs-plan-gate":            viaStderr(docs.Gate),
	"docs-plan-skeleton":        docs.Skeleton,
	"docs-plan-skeleton-prompt": docs.Skeleton,
	"docs-plan-tick":            docs.Tick,
	"docs-plan-reminder":        docs.Reminder,
	"docs-plan-mirror":          viaStderr(docs.Mirror),
	"docs-plan-save":            viaStderr(docs.Save),
	"dispatch-stop":             agents.Stop,
	"dispatch-handback":         agents.Handback,
	"memory-compact":            memhook.Compact,
	"worktree-create":           wthook.Create,
	"worktree-remove":           viaStderr(wthook.Remove),
	"worktree-mode":             wthook.Mode,
	"worktree-busy":             wthook.Busy,
	"worktree-claim":            wthook.Claim,
	"worktree-claim-check":      wthook.ClaimCheck,
	"permission-gate":           safety.PermissionGate,
}

// runDirect is `hook <Event> --<module>`: one Runner rendered for its own process. A module that blocks exits 2 with
// its reason on stderr, which is how a guard run by an agent or by safety-scan's own entry blocks.
func runDirect(name string, stdin io.Reader, stdout, stderr io.Writer) int {
	run, ok := native[name]
	if !ok {
		return core.ExitProceed
	}
	in := core.ParseInput(stdin)
	res := runGated(name, run, in)
	if res.Decision == core.Block && specOf(name).Blocks {
		if name == "safety-scan" && in.Agent() == core.AgentCopilot {
			fmt.Fprintln(stdout, core.OutputCopilot(settings.EventPreToolUse, res))
		}
		if in.Agent() == core.AgentGemini { // agy blocks on a JSON deny, not exit 2
			if _, err := fmt.Fprintln(stdout, core.OutputGemini(settings.EventPreToolUse, res)); err != nil {
				fmt.Fprintf(stderr, "%s (deny JSON not written: %v)\n", res.Reason, err)
				return core.ExitBlock // fail closed: a lost deny must not proceed
			}
			return core.ExitProceed
		}
		fmt.Fprintln(stderr, res.Reason)
		return core.ExitBlock
	}
	res.Context = textutil.CapRunes(res.Context, core.ContextCapChars, "…")
	if !bareStdout[name] {
		return core.Emit(in.Agent(), eventOf(name), res, stdout, stderr)
	}
	var out strings.Builder
	code := core.Emit(in.Agent(), eventOf(name), res, &out, stderr)
	_, _ = io.WriteString(stdout, strings.TrimSuffix(out.String(), "\n"))
	return code
}

// runGated runs one module, except a Blocks gate on a payload that did not parse: that blocks without running.
func runGated(name string, run core.Runner, in *core.Input) core.Result {
	if in.Invalid != "" && specOf(name).Blocks {
		return core.Result{Decision: core.Block, Reason: unreadable(name, in.Invalid)}
	}
	return run(in)
}

// bareStdout lists the modules whose own-process stdout never carried a trailing newline.
var bareStdout = map[string]bool{
	"review-simplicity": true, "docs-plan-skeleton": true, "docs-plan-skeleton-prompt": true,
	"docs-plan-tick": true, "docs-plan-reminder": true,
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
	if !isEvent(event) {
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

// runModule runs one module on its declared event; a group switched off in harnessHooks proceeds.
func runModule(event, name string, stdin io.Reader, stdout, stderr io.Writer) int {
	if _, ok := native[name]; !ok || eventOf(name) != event {
		return unknownModule(event, name, stderr)
	}
	if g, found := settings.GroupOf(name); found && !g.Gate && !enabledGroups()(g.Name, g.Gate) {
		return core.ExitProceed
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
