package contract

import (
	"fmt"
	"slices"
)

const (
	// SchemaVersion is the contract record's format version.
	SchemaVersion = 1
	// MaxDepth is main session → owner (1) → fleet (2); a fleet member never sub-contracts.
	MaxDepth = 2
	// MaxTokens caps a contract's token budget (5x the 400k function default).
	MaxTokens = 2_000_000
	// MaxLeaves caps a contract's leaf budget (2x the 6-leaf function default).
	MaxLeaves = 12
)

// Validate lists every problem of c, one per broken rule; Open runs it on every new contract.
func Validate(c Contract) []Problem {
	var ps []Problem
	add := func(field, format string, a ...any) { ps = append(ps, Problem{field, fmt.Sprintf(format, a...)}) }
	if c.Schema != SchemaVersion {
		add("schema", "version %d, want %d", c.Schema, SchemaVersion)
	}
	validateScope(c.Scope, add)
	validateOwner(c.Owner, c.Limits, add)
	validateLimits(c.Limits, add)
	validateTerms(c.Terms, add)
	if c.Scope.Kind == KindTestRepair && !c.Executed {
		add("executed_tests", "a %s contract needs an executed test run", KindTestRepair)
	}
	for _, r := range c.Records {
		if err := checkRecord(c, r); err != "" {
			add("records", "%s", err)
		}
	}
	validateState(c, add)
	return ps
}

// alwaysTerms are in every TermsFor list, so a contract without one was not built by Open.
var alwaysTerms = []Term{TermGate, TermTests, TermHandback, TermReport}

func validateTerms(terms []Term, add func(string, string, ...any)) {
	for _, t := range alwaysTerms {
		if !slices.Contains(terms, t) {
			add("terms", "missing %q", t)
		}
	}
	for _, p := range [][2]Term{{TermLane, TermLanded}, {TermProxy, TermProxyClosed}} {
		if slices.Contains(terms, p[0]) != slices.Contains(terms, p[1]) {
			add("terms", "%q and %q come as a pair", p[0], p[1])
		}
	}
	last := -1
	for _, t := range terms {
		i := slices.Index(Terms(), t)
		switch {
		case i < 0:
			add("terms", "unknown term %q (one of %v)", t, Terms())
		case i <= last:
			add("terms", "%q is repeated or out of stage order", t)
		default:
			last = i
		}
	}
}

// validateState checks the invariants each state's entering transition guarantees.
func validateState(c Contract, add func(string, string, ...any)) {
	s := c.state
	if s != "" && !slices.Contains(States(), s) {
		add("state", "unknown state %q (one of %v)", s, States())
		return
	}
	validateFailed(c, add)
	if slices.Contains([]State{StateWorking, StateVerified, StateClosed}, s) && (c.ID == "" || len(c.Holders) == 0) {
		add("holders", "state %q needs an id and a holder", s)
	}
	if miss := c.missing(entryTerms(s, c)...); len(miss) > 0 {
		add("records", "state %q with no pass record for %v", s, miss)
	}
	validateTree(c, add)
	validateResult(c, add)
}

// validateTree checks what AddChild and Reassign guarantee: the leaf budget and one re-assign at most.
func validateTree(c Contract, add func(string, string, ...any)) {
	if len(c.Children) > c.Limits.Leaves {
		add("children", "%d sub-contracts over a budget of %d leaves", len(c.Children), c.Limits.Leaves)
	}
	if len(c.Holders) > 2 {
		add("holders", "%d holders: a contract is re-assigned once at most", len(c.Holders))
	}
	if !distinctIDs(c.Holders, "") {
		add("holders", "%v holds an empty or repeated id", c.Holders)
	}
	if !distinctIDs(c.Children, c.ID) {
		add("children", "%v holds an empty, repeated or own id", c.Children)
	}
}

// distinctIDs reports whether ids are non-empty, unique and never self.
func distinctIDs(ids []string, self string) bool {
	seen := map[string]bool{"": true}
	if self != "" {
		seen[self] = true
	}
	for _, id := range ids {
		if seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// validateResult checks the hand-back a terminal contract carries and the Unmet list Go filled.
func validateResult(c Contract, add func(string, string, ...any)) {
	switch {
	case c.Result == nil:
		if c.state.Terminal() {
			add("result", "state %q needs a result", c.state)
		}
	case !c.state.Terminal():
		add("result", "set in state %q", c.state)
	case !slices.Equal(c.Result.Unmet, c.Unmet()):
		add("result.unmet", "%v, the records say %v", c.Result.Unmet, c.Unmet())
	default:
		if err := checkResult(c, *c.Result); err != "" {
			add("result", "%s", err)
		}
	}
}

func validateFailed(c Contract, add func(string, string, ...any)) {
	switch {
	case (c.Failed != "") != (c.state == StateFailed):
		add("failed", "reason %q in state %q", c.Failed, c.state)
	case c.Failed != "" && !slices.Contains(Reasons(), c.Failed):
		add("failed", "unknown reason %q (one of %v)", c.Failed, Reasons())
	}
}

// entryTerms are the terms that must have passed for c to be in s.
func entryTerms(s State, c Contract) []Term {
	switch s {
	case StateVerified:
		return verifyTerms
	case StateClosed:
		return append(slices.Clone(verifyTerms), c.closeTerms()...)
	}
	return nil
}

func validateScope(s Scope, add func(string, string, ...any)) {
	if s.Unit == "" {
		add("scope.unit", "empty")
	}
	if !slices.Contains(Kinds(), s.Kind) {
		add("scope.kind", "unknown kind %q (one of %v)", s.Kind, Kinds())
	}
	if len(s.Own) == 0 {
		add("scope.own", "empty: a contract owns at least one file")
	}
	if slices.Contains(s.Own, "") {
		add("scope.own", "holds an empty path")
	}
}

func validateOwner(p Party, l Limits, add func(string, string, ...any)) {
	if p.Agent == "" {
		add("owner.agent", "empty")
	}
	switch p.Role {
	case RoleOwner:
		if l.Depth != 1 {
			add("limits.depth", "an owner runs at depth 1, not %d", l.Depth)
		}
	case RoleFleet:
		if l.Depth != MaxDepth {
			add("limits.depth", "a fleet member runs at depth %d, not %d", MaxDepth, l.Depth)
		}
		if l.Leaves != 0 {
			add("limits.leaves", "a fleet member dispatches no leaves, not %d", l.Leaves)
		}
	default:
		add("owner.role", "unknown role %q (one of %v)", p.Role, Roles())
	}
}

func validateLimits(l Limits, add func(string, string, ...any)) {
	if l.Depth < 1 || l.Depth > MaxDepth {
		add("limits.depth", "%d, want 1 to %d", l.Depth, MaxDepth)
	}
	if l.Tokens < 0 || l.Tokens > MaxTokens {
		add("limits.tokens", "%d, want 0 to %d", l.Tokens, MaxTokens)
	}
	if l.Leaves < 0 || l.Leaves > MaxLeaves {
		add("limits.leaves", "%d, want 0 to %d", l.Leaves, MaxLeaves)
	}
}

// checkRecord is the empty string for a record c may hold, else what is wrong with it.
func checkRecord(c Contract, r TermRecord) string {
	switch {
	case !slices.Contains(c.Terms, r.Term):
		return fmt.Sprintf("term %q is not one of the contract's terms %v", r.Term, c.Terms)
	case tooEarly(c.state, r.Term):
		return fmt.Sprintf("%q is a %s term, too early for state %q", r.Term, StageOf(r.Term), c.state)
	case r.By == ByOwner && c.Executed:
		return fmt.Sprintf("only an executed run (by %q) passes %q here", ByGo, TermTests)
	case !slices.Contains(Verdicts(), r.Verdict):
		return fmt.Sprintf("unknown verdict %q (one of %v)", r.Verdict, Verdicts())
	case r.By == ByOwner && r.Term != TermTests:
		return fmt.Sprintf("the owner records only %q, not %q", TermTests, r.Term)
	case r.By != ByGo && r.By != ByOwner:
		return fmt.Sprintf("by %q, want %q or %q", r.By, ByGo, ByOwner)
	}
	return ""
}
