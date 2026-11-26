package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// An attempt whose own context already expired ends Do (terminal) as a deadline budget; no real clock is waited on.
func TestBudgetDeadlineNotRetried(t *testing.T) {
	p := fastPolicy(t)
	p.AttemptTimeout = time.Minute
	e := Classify("x", context.DeadlineExceeded, false)
	if !p.terminal(context.Background(), e, context.DeadlineExceeded) || e.Budget != BudgetDeadline || !IsBudget(e) || MayRedispatch(e) {
		t.Fatalf("own deadline: budget=%q redispatch=%v", e.Budget, MayRedispatch(e))
	}
}

func TestBudgetTransportStillRetried(t *testing.T) {
	p := fastPolicy(t)
	calls := 0
	e := Do(context.Background(), p, func(context.Context, int) *Error {
		calls++
		return Classify("x", statusErr{503}, false)
	})
	if calls != 3 || IsBudget(e) || !MayRedispatch(e) {
		t.Fatalf("503 must retry as before: calls=%d budget=%q", calls, e.Budget)
	}
}

func TestBudgetAdmissionWaitIsTransport(t *testing.T) {
	p := fastPolicy(t)
	p.AttemptTimeout = time.Minute
	e := Classify("x", &AdmissionError{Account: "a", Err: context.DeadlineExceeded}, false)
	p.terminal(context.Background(), e, context.DeadlineExceeded)
	if IsBudget(e) || !IsAdmission(e) {
		t.Fatalf("admission wait read as budget: %q %v", e.Budget, e)
	}
}

func TestWithBudgetKeepsKindAndFirstBudget(t *testing.T) {
	typed := &Error{Kind: KindContext, Agent: "a", Err: errors.New("overflow")}
	got := WithBudget(typed, BudgetContext)
	if e, _ := errors.AsType[*Error](got); e.Kind != KindContext || BudgetOf(got) != BudgetContext || typed.Budget != BudgetNone {
		t.Fatalf("typed: %+v original %+v", e, typed)
	}
	if again := WithBudget(got, BudgetCost); BudgetOf(again) != BudgetContext {
		t.Fatalf("first budget lost: %q", BudgetOf(again))
	}
	if wrapped := WithBudget(fmt.Errorf("x: %w", errors.New("y")), BudgetTokens); BudgetOf(wrapped) != BudgetTokens || MayRedispatch(wrapped) {
		t.Fatalf("plain error: %v", wrapped)
	}
}

func TestBudgetFoundInJoinedAndNestedErrors(t *testing.T) {
	budget := &Error{Kind: KindFatal, Budget: BudgetCost, Err: errors.New("cap")}
	if got := BudgetOf(errors.Join(&Error{Kind: KindTransient}, budget)); got != BudgetCost {
		t.Errorf("joined sibling budget lost: %q", got)
	}
	outer := &Error{Kind: KindTransient, Agent: "x", Err: budget}
	if outer.retryable() || MayRedispatch(outer) {
		t.Error("a transient wrapper around a budget end stayed retryable")
	}
}
