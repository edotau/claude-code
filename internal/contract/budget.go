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

// LimitsFor is the limits a contract of kind held by role opens with; a non-zero field of spec overrides the default.
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

// SpentOf sums the tokens of id's subtree in uses (id included) and counts id's direct sub-dispatches as leaves.
func SpentOf(id string, uses []Use) Spent {
	in := map[string]bool{id: true}
	var s Spent
	for grew := true; grew; { // parents may be listed after children, so repeat until no new member joins
		grew = false
		for _, u := range uses {
			if !in[u.ID] && in[u.Parent] {
				in[u.ID], grew = true, true
			}
		}
	}
	seen := map[string]bool{}
	for _, u := range uses {
		if !in[u.ID] || seen[u.ID] {
			continue
		}
		seen[u.ID] = true
		s.Tokens += max(u.Tokens, 0)
		if u.Parent == id && u.ID != id {
			s.Leaves++
		}
	}
	return s
}

// LeavesSpent reports whether an owner has dispatched every leaf of its budget; its next dispatch gets BudgetNotice.
func (c Contract) LeavesSpent(s Spent) bool {
	return c.Owner.Role == RoleOwner && s.Leaves >= c.Limits.Leaves
}

// TokensSpent reports whether an owner's subtree is past its token limit; every tool result then gets BudgetNotice.
func (c Contract) TokensSpent(s Spent) bool {
	return c.Owner.Role == RoleOwner && s.Tokens > c.Limits.Tokens
}

// FailReason is limit when c's budget is reached with a return term unmet, else term.
func (c Contract) FailReason(s Spent) Reason {
	reached := c.LeavesSpent(s) || c.TokensSpent(s)
	if reached && slices.ContainsFunc(c.Unmet(), func(t Term) bool { return StageOf(t) == StageReturn }) {
		return ReasonLimit
	}
	return ReasonTerm
}
