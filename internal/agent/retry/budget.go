package retry

import (
	"errors"
	"fmt"
	"time"
)

// Budget names the limit that ended a dispatch; "" is no budget. A budget end is never re-sent (R34).
type Budget string

const (
	BudgetNone     Budget = ""
	BudgetDeadline Budget = "deadline" // the attempt's own --timeout or tools default
	BudgetIdle     Budget = "idle"     // silent after output or a tool call
	BudgetOutput   Budget = "output"   // the model's output token limit
	BudgetContext  Budget = "context"  // the window overflowed again after the one re-pick
	BudgetTokens   Budget = "tokens"   // the dispatch tree's token cap
	BudgetCost     Budget = "cost"     // the agent's own cost or turn cap
	BudgetContract Budget = "contract" // a contract's token or deadline limit
)

// BudgetOf is the budget kind of the first classified error in err's tree, wrapped or joined, that carries one.
func BudgetOf(err error) Budget {
	switch u := err.(type) {
	case nil:
		return BudgetNone
	case *Error:
		if u.Budget != BudgetNone {
			return u.Budget
		}
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return BudgetOf(u.Unwrap())
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if b := BudgetOf(e); b != BudgetNone {
				return b
			}
		}
	}
	return BudgetNone
}

// IsBudget is true when a budget ended err: no retry, hop, re-pick, takeover or resend answers it.
func IsBudget(err error) bool { return BudgetOf(err) != BudgetNone }

// WithBudget marks err as ended by budget b, keeping its Kind; nil stays nil, a set kind is kept.
func WithBudget(err error, b Budget) error {
	if err == nil || IsBudget(err) || IsAdmission(err) { // an admission wait never started the task
		return err
	}
	if e, ok := errors.AsType[*Error](err); ok && err == error(e) {
		out := *e
		out.Budget = b
		return &out
	}
	agent := ""
	if e, ok := errors.AsType[*Error](err); ok {
		agent = e.Agent
	}
	return &Error{Kind: KindFatal, Agent: agent, Budget: b, Err: err}
}

// AdmissionError is a wait for an account slot that ran out its time: transport, the run never started.
type AdmissionError struct {
	Account string
	Waited  time.Duration
	Err     error
}

func (e *AdmissionError) Error() string {
	return fmt.Sprintf("admission for account %q waited %s: %v", e.Account, e.Waited, e.Err)
}

func (e *AdmissionError) Unwrap() error { return e.Err }

// IsAdmission is true when err is an admission wait, never a deadline of the task itself.
func IsAdmission(err error) bool {
	_, ok := errors.AsType[*AdmissionError](err)
	return ok
}
