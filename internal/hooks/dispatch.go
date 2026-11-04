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

	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
)

// Decision is a hook's verdict on a tool call; the strictest across groups wins.
type Decision int

const (
	Allow Decision = iota
	Ask
	Block
)

// Result is what one handler contributes to its event; the dispatcher merges them and an agent builder renders it.
type Result struct {
	Decision      Decision
	Reason        string
	Context       string // model-visible additionalContext
	SystemMessage string // user-visible note
	UpdatedInput  map[string]any
	Stdout        string // raw stdout: only the worktree group may set it
	Stderr        string
	ExitCode      int // non-zero exit other than 2; honored only from the worktree group
}

// Runner is one handler bound to an event.
type Runner func(*Input) Result

// Bound pairs a declared handler with its Runner.
type Bound struct {
	Group string
	Gate  bool
	Spec  settings.EventSpec
	Run   Runner
}

// Bind pairs every hookspec handler with its Native Runner.
func Bind() []Bound {
	var out []Bound
	for _, g := range settings.Registry {
		for _, s := range g.Events {
			s.Group = g.Name
			out = append(out, Bound{Group: g.Name, Gate: g.Gate, Spec: s, Run: Native[s.Name]})
		}
	}
	return out
}

// exitResult maps a helper's exit code and stderr text to a Result: 2 blocks with the text, other codes pass through.
func exitResult(code int, stderr string) Result {
	stderr = strings.TrimSpace(stderr)
	switch code {
	case ExitProceed:
		return Result{Stderr: stderr}
	case ExitBlock:
		return Result{Decision: Block, Reason: stderr}
	}
	return Result{Stderr: stderr, ExitCode: code}
}

// viaStderr makes a Runner of a helper that reports its notes on a stderr writer and returns an exit code.
func viaStderr(f func(*Input, io.Writer) int) Runner {
	return func(in *Input) Result {
		var b bytes.Buffer
		code := f(in, &b)
		return exitResult(code, b.String())
	}
}

// joinContext joins the non-empty context blocks into one additionalContext text.
func joinContext(blocks ...string) string {
	return strings.Join(slices.DeleteFunc(slices.Clone(blocks), func(b string) bool { return b == "" }), "\n\n")
}

// matches applies a handler matcher to a tool name; empty matches everything, anchored and case-insensitive.
func matches(matcher, tool string) bool {
	if matcher == "" {
		return true
	}
	re, err := regexp.Compile(`(?i)^(?:` + matcher + `)$`)
	return err == nil && tool != "" && re.MatchString(tool)
}

// runBudgeted runs one Runner under its deadline with panic recovery; a panic or overrun is an error.
func runBudgeted(b Bound, in *Input, wg *sync.WaitGroup) (Result, error) {
	type outcome struct {
		r   Result
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
		return Result{}, fmt.Errorf("overran its %s budget", budget)
	}
}

// skipOrBlock names what a failed handler's log line reports, keyed by Spec.Blocks.
var skipOrBlock = map[bool]string{false: "skipped", true: "blocked"}

// dispatchMerge accumulates the handlers' results for one Dispatch call.
type dispatchMerge struct {
	merged         Result
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
func (m *dispatchMerge) add(b Bound, event string, r Result) {
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
func (m *dispatchMerge) addWorktree(b Bound, r Result) {
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
// that panics or overruns is logged and skipped (a Blocks gate blocks); a second updatedInput or raw stdout errors.
func Dispatch(bound []Bound, event string, in *Input, log io.Writer) (Result, error) {
	m := &dispatchMerge{log: log}
	var wg sync.WaitGroup
	var longest time.Duration
	for _, b := range bound {
		if b.Spec.Event != event || !matches(b.Spec.Matcher, in.ToolName) {
			continue
		}
		longest = max(longest, b.Spec.Budget)
		r, err := runBudgeted(b, in, &wg)
		if err != nil {
			fmt.Fprintf(log, "hook dispatch: %s/%s %v (%s)\n", b.Group, b.Spec.Name, err, skipOrBlock[b.Spec.Blocks])
			if b.Spec.Blocks { // a gate that could not run fails closed
				m.merged.Decision, m.merged.Reason = Block, fmt.Sprintf("%s did not complete (%v); blocked", b.Spec.Name, err)
			}
			continue
		}
		m.add(b, event, r)
	}
	drain(&wg, longest, log)
	m.merged.Context = capContext(strings.Join(m.ctx, "\n\n"))
	m.merged.SystemMessage = strings.Join(m.sys, "; ")
	m.merged.Stderr = strings.Join(m.errs, "\n")
	return m.merged, m.firstErr
}

func appendNonEmpty(list []string, s string) []string {
	if s == "" {
		return list
	}
	return append(list, s)
}

// IsEvent reports whether name is a lifecycle event the dispatcher serves (`claude-code hook <Event>`).
func IsEvent(name string) bool {
	for _, g := range settings.Registry {
		for _, s := range g.Events {
			if s.Event == name {
				return true
			}
		}
	}
	return false
}

// enabledGroups reads the user's harnessHooks switches; gate groups are never switched off, errors mean all on.
func enabledGroups() func(group string, gate bool) bool {
	sw, err := settings.LoadHarnessHooks()
	if err != nil {
		sw = nil
	}
	return func(group string, gate bool) bool {
		on, set := sw[group]
		return gate || !set || on
	}
}

// RunEvent is `hook <Event> [--all]`: run every enabled group once, own-process modules only when all is set.
func RunEvent(event string, all bool, stdin io.Reader, stdout, stderr io.Writer) int {
	in := ParseInput(stdin)
	enabled := enabledGroups()
	var bound []Bound
	for _, b := range Bind() {
		g, _ := settings.GroupOf(b.Spec.Name)
		switch {
		case g.AgentOnly, !enabled(b.Group, b.Gate):
		case !all && (b.Gate || b.Spec.Async):
		case in.Agent() != AgentClaude && !slices.Contains(b.Spec.Agents, in.Agent()):
		default:
			bound = append(bound, b)
		}
	}
	res, _ := Dispatch(bound, event, in, stderr)
	return Emit(in.Agent(), event, res, stdout, stderr)
}

// drain waits, bounded by one more longest-budget period, for overrun handlers so none still runs at output time.
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

// capContext trims merged context to contextCapChars, cutting the later blocks first.
func capContext(s string) string {
	return textutil.CapRunes(s, contextCapChars, "…truncated")
}
