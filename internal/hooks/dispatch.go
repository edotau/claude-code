package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/settings"
)

// Decision is a hook's verdict on a tool call; the strictest across groups wins.
type Decision int

const (
	Allow Decision = iota
	Ask
	Block
)

// Result is what one handler contributes to its event; the dispatcher merges them and a vendor builder renders it.
type Result struct {
	Decision      Decision
	Reason        string
	Context       string // model-visible additionalContext
	SystemMessage string // user-visible note
	UpdatedInput  map[string]any
	Stdout        string // raw stdout: only the worktree group may set it
	Stderr        string
}

// Runner is one handler bound to an event.
type Runner func(*Input) Result

// Bound pairs a declared handler with its Runner.
type Bound struct {
	Group string
	Gate  bool
	Spec  hookspec.EventSpec
	Run   Runner
}

// Bind pairs every hookspec handler with its legacy Handler, wrapped by an adapter that captures its
// stdout/stderr/exit into a Result; later phases replace adapters with native Runners.
func Bind() []Bound {
	var out []Bound
	for _, g := range hookspec.Registry {
		for _, s := range g.Events {
			s.Group = g.Name
			out = append(out, Bound{Group: g.Name, Gate: g.Gate, Spec: s, Run: adapt(Registry[s.Name])})
		}
	}
	return out
}

func adapt(h Handler) Runner {
	return func(in *Input) Result {
		var stdout, stderr bytes.Buffer
		code := h(bytes.NewReader(in.Raw), &stdout, &stderr)
		return resultFromLegacy(code, stdout.String(), stderr.String())
	}
}

var knownTop = []string{"hookSpecificOutput", "systemMessage", "additionalContext", "permissionDecision", "permissionDecisionReason"}
var knownInner = []string{"hookEventName", "additionalContext", "updatedInput", "permissionDecision", "permissionDecisionReason"}

// resultFromLegacy turns a legacy handler's exit/stdout/stderr into a Result; output it cannot map stays raw.
func resultFromLegacy(code int, stdout, stderr string) Result {
	r := Result{}
	stderr = strings.TrimSpace(stderr)
	switch {
	case code == ExitBlock:
		r.Decision, r.Reason = Block, stderr
		return r
	case code != ExitProceed:
		r.Stderr = fmt.Sprintf("%s\n(hook exit %d)", stderr, code)
		return r
	}
	r.Stderr = stderr
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return r
	}
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(stdout), &top) != nil || !onlyKeys(top, knownTop) {
		r.Stdout = stdout
		return r
	}
	var inner map[string]json.RawMessage
	if raw, ok := top["hookSpecificOutput"]; ok && (json.Unmarshal(raw, &inner) != nil || !onlyKeys(inner, knownInner)) {
		r.Stdout = stdout
		return r
	}
	str := func(m map[string]json.RawMessage, k string) string {
		var s string
		_ = json.Unmarshal(m[k], &s)
		return s
	}
	r.SystemMessage = str(top, "systemMessage")
	for _, m := range []map[string]json.RawMessage{top, inner} {
		if v := str(m, "additionalContext"); v != "" {
			r.Context = v
		}
		if v := str(m, "permissionDecisionReason"); v != "" {
			r.Reason = v
		}
		switch str(m, "permissionDecision") {
		case "deny":
			r.Decision = Block
		case "ask":
			r.Decision = Ask
		}
	}
	if raw, ok := inner["updatedInput"]; ok {
		_ = json.Unmarshal(raw, &r.UpdatedInput)
	}
	return r
}

func onlyKeys(m map[string]json.RawMessage, allowed []string) bool {
	for k := range m {
		if !slices.Contains(allowed, k) {
			return false
		}
	}
	return true
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
func runBudgeted(b Bound, in *Input) (Result, error) {
	type outcome struct {
		r   Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- outcome{err: fmt.Errorf("panic: %v", p)}
			}
		}()
		done <- outcome{r: b.Run(in)}
	}()
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

// Dispatch runs, in order, every bound handler on event whose matcher hits and merges the results. A handler
// that panics or overruns is logged and skipped; a second updatedInput or a non-worktree raw stdout is an error.
func Dispatch(bound []Bound, event string, in *Input, log io.Writer) (Result, error) {
	var merged Result
	var ctx, sys, errs []string
	var firstErr error
	fail := func(err error) {
		fmt.Fprintln(log, "hook dispatch:", err)
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, b := range bound {
		if b.Spec.Event != event || !matches(b.Spec.Matcher, in.ToolName) {
			continue
		}
		r, err := runBudgeted(b, in)
		if err != nil {
			fmt.Fprintf(log, "hook dispatch: %s/%s %v (skipped)\n", b.Group, b.Spec.Name, err)
			continue
		}
		if r.Decision > merged.Decision {
			merged.Decision, merged.Reason = r.Decision, r.Reason
		} else if r.Decision == merged.Decision && merged.Reason == "" {
			merged.Reason = r.Reason
		}
		ctx, sys, errs = appendNonEmpty(ctx, r.Context), appendNonEmpty(sys, r.SystemMessage), appendNonEmpty(errs, r.Stderr)
		if r.UpdatedInput != nil {
			if merged.UpdatedInput != nil {
				fail(fmt.Errorf("%s/%s: second updatedInput on %s", b.Group, b.Spec.Name, event))
				continue
			}
			merged.UpdatedInput = r.UpdatedInput
		}
		if r.Stdout != "" {
			if b.Group != "worktree" {
				fail(fmt.Errorf("%s/%s: raw stdout is reserved for the worktree group", b.Group, b.Spec.Name))
				continue
			}
			merged.Stdout = r.Stdout
		}
	}
	merged.Context = strings.Join(ctx, "\n\n")
	merged.SystemMessage = strings.Join(sys, "; ")
	merged.Stderr = strings.Join(errs, "\n")
	return merged, firstErr
}

func appendNonEmpty(list []string, s string) []string {
	if s == "" {
		return list
	}
	return append(list, s)
}

// IsEvent reports whether name is a lifecycle event the dispatcher serves (`claude-code hook <Event>`).
func IsEvent(name string) bool {
	for _, g := range hookspec.Registry {
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

// RunEvent is `claude-code hook <Event>`: read the payload once, run every enabled group, print the merged result.
func RunEvent(event string, noOwn bool, stdin io.Reader, stdout, stderr io.Writer) int {
	in := ParseInput(stdin)
	enabled := enabledGroups()
	var bound []Bound
	for _, b := range Bind() {
		g, _ := hookspec.GroupOf(b.Spec.Name)
		switch {
		case g.VendorOnly, !enabled(b.Group, b.Gate):
		case noOwn && (b.Gate || b.Spec.Async):
		case in.Vendor() != VendorClaude && !slices.Contains(b.Spec.Vendors, in.Vendor()):
		default:
			bound = append(bound, b)
		}
	}
	res, _ := Dispatch(bound, event, in, stderr)
	return Emit(in.Vendor(), event, res, stdout, stderr)
}
