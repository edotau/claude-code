package contract

import "slices"

// BudgetNotice is the dispatch result at the budget and the line every owner tool result carries past the tokens.
const BudgetNotice = "budget reached: hand back now and list what is unmet"

// budgets are the default owner limits per kind; a gated edit costs about 15k tokens on the pi runner (Phase 0.2).
var budgets = map[Kind]Limits{
	KindFunction:   {Tokens: 400_000, Leaves: 6},
	KindTestRepair: {Tokens: 400_000, Leaves: 6},
	KindFile:       {Tokens: MaxTokens, Leaves: MaxLeaves},
	KindTask:       {Tokens: MaxTokens, Leaves: MaxLeaves},
}

// LimitsFor is the limits a contract of kind held by role opens with; spec's non-zero tokens or leaves override.
func LimitsFor(kind Kind, role Role, spec Limits) Limits {
	l := budgets[kind]
	if spec.Tokens != 0 {
		l.Tokens = spec.Tokens
	}
	if spec.Leaves != 0 {
		l.Leaves = spec.Leaves
	}
	l.Depth = 1
	if role == RoleFleet {
		l.Depth, l.Leaves = MaxDepth, 0 // a fleet member never sub-contracts
	}
	return l // Open's Validate refuses a value over the caps
}

// Use is one dispatch record's share of a budget: its final usage, else its live activity so far.
type Use struct {
	ID, Parent string
	Tokens     int
}

// SpentOf sums the tokens of the subtrees of c and its holders in uses; its leaves are the sub-contracts AddChild took.
func SpentOf(c Contract, uses []Use) Spent {
	in := subtree(append([]string{c.ID}, c.Holders...), uses)
	s := Spent{Leaves: len(c.Children)}
	seen := map[string]bool{}
	for _, u := range uses {
		if in[u.ID] && !seen[u.ID] {
			seen[u.ID] = true
			s.Tokens += max(u.Tokens, 0)
		}
	}
	return s
}

// subtree is the set of roots and every id in uses that descends from one of them.
func subtree(roots []string, uses []Use) map[string]bool {
	in := map[string]bool{}
	for _, r := range roots {
		in[r] = r != "" // an empty root would adopt every parentless record
	}
	for grew := true; grew; { // parents may be listed after children, so repeat until no new member joins
		grew = false
		for _, u := range uses {
			if u.ID != "" && !in[u.ID] && in[u.Parent] {
				in[u.ID], grew = true, true
			}
		}
	}
	return in
}

// LeavesSpent reports whether an owner has dispatched every leaf of its budget; its next dispatch gets BudgetNotice.
func (c Contract) LeavesSpent(s Spent) bool {
	return c.Owner.Role == RoleOwner && s.Leaves >= c.Limits.Leaves
}

// TokensSpent reports whether an owner's subtree is at its token limit; every tool result then gets BudgetNotice.
func (c Contract) TokensSpent(s Spent) bool {
	return c.Owner.Role == RoleOwner && c.Limits.Tokens > 0 && s.Tokens >= c.Limits.Tokens
}

// FailReason is limit when c's budget is reached with a return term unmet, else term.
func (c Contract) FailReason(s Spent) Reason {
	reached := c.LeavesSpent(s) || c.TokensSpent(s)
	if reached && slices.ContainsFunc(c.Unmet(), func(t Term) bool { return StageOf(t) == StageReturn }) {
		return ReasonLimit
	}
	return ReasonTerm
}
