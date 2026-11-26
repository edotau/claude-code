package contract

import "testing"

const testTokens = 100

func TestLimitsForDefaultsAndOverrides(t *testing.T) {
	if got := LimitsFor(KindFunction, RoleOwner, Limits{}); got != (Limits{Depth: 1, Tokens: 400_000, Leaves: 6}) {
		t.Errorf("function owner = %+v", got)
	}
	if got := LimitsFor(KindTestRepair, RoleOwner, Limits{Leaves: 2}); got != (Limits{Depth: 1, Tokens: 400_000, Leaves: 2}) {
		t.Errorf("leaves override = %+v", got)
	}
	if got := LimitsFor(KindFunction, RoleFleet, Limits{Tokens: 9, Leaves: 3}); got != (Limits{Depth: MaxDepth, Tokens: 9}) {
		t.Errorf("fleet = %+v", got)
	}
}

func TestSpentOfSumsTheSubtreeAndCountsLeaves(t *testing.T) {
	c := Contract{ID: "own", Holders: []string{"own", "re"}, Children: []string{"a"}}
	uses := []Use{
		{ID: "hop", Parent: "a", Tokens: 5}, // listed before its parent
		{ID: "a", Parent: "own", Tokens: 10}, {ID: "b", Parent: "re", Tokens: 20},
		{ID: "own", Tokens: 1}, {ID: "other", Parent: "x", Tokens: testTokens},
	}
	if got := SpentOf(c, uses); got != (Spent{Tokens: 36, Leaves: 1}) {
		t.Errorf("SpentOf = %+v, want 36 tokens and 1 leaf", got)
	}
}

func TestBudgetReachedAndFailReason(t *testing.T) {
	c := opened(t, RoleOwner, 1, 2)
	c.Limits.Tokens = testTokens
	switch {
	case c.LeavesSpent(Spent{Leaves: 1}) || !c.LeavesSpent(Spent{Leaves: 2}):
		t.Error("leaves: want reached at 2 of 2 only")
	case c.TokensSpent(Spent{Tokens: testTokens - 1}) || !c.TokensSpent(Spent{Tokens: testTokens}):
		t.Error("tokens: want reached at the limit only")
	case c.FailReason(Spent{Leaves: 2}) != ReasonLimit || c.FailReason(Spent{}) != ReasonTerm:
		t.Error("fail reason: want limit at the budget with return terms unmet, term below it")
	}
	if f := opened(t, RoleFleet, MaxDepth, 0); f.LeavesSpent(Spent{}) || f.TokensSpent(Spent{Tokens: 1 << 30}) {
		t.Error("a fleet member is never over an owner budget")
	}
}
