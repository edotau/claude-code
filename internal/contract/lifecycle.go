package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

var (
	// ErrState means the transition is not legal from the contract's state.
	ErrState = errors.New("illegal transition")
	// ErrTerms means a term the transition needs has no pass record.
	ErrTerms = errors.New("terms not passed")
	// ErrChildOpen means a sub-contract has not reached closed or failed.
	ErrChildOpen = errors.New("sub-contract open")
	// ErrLimit means the depth or leaf budget is spent.
	ErrLimit = errors.New("limit reached")
	// ErrInput means an argument is empty, unknown or a repeat.
	ErrInput = errors.New("bad argument")
)

// TransitionError names the transition, the state it was called in and why it was refused.
type TransitionError struct {
	Op     string
	From   State
	Err    error // one of the Err values above, for errors.Is
	Detail string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("contract %s from %q: %v: %s", e.Op, e.From, e.Err, e.Detail)
}

func (e *TransitionError) Unwrap() error { return e.Err }

func refuse(op string, from State, err error, format string, a ...any) error {
	return &TransitionError{Op: op, From: from, Err: err, Detail: fmt.Sprintf(format, a...)}
}

// States lists every State in lifecycle order.
func States() []State {
	return []State{StateOpened, StateWorking, StateVerified, StateClosed, StateFailed}
}

// Terminal reports whether s is closed or failed.
func (s State) Terminal() bool { return s == StateClosed || s == StateFailed }

// State is c's lifecycle state.
func (c Contract) State() State { return c.state }

// guard refuses op unless c is in one of from.
func (c Contract) guard(op string, from ...State) error {
	if slices.Contains(from, c.state) {
		return nil
	}
	return refuse(op, c.state, ErrState, "legal only from %v", from)
}

// Open builds an opened contract with the fixed terms for f; on a problem it returns the zero Contract.
func Open(scope Scope, owner Party, lim Limits, f Facts) (Contract, []Problem) {
	c := Contract{Schema: SchemaVersion, Scope: scope, Owner: owner, Terms: TermsFor(f), Limits: lim,
		Executed: NeedsExecutedTests(f) || scope.Kind == KindTestRepair}.clone()
	if ps := Validate(c); len(ps) > 0 {
		return Contract{}, ps
	}
	c.state = StateOpened
	return c, nil
}

// Start moves opened → working under its first holder; the contract takes that dispatch id when it has none.
func (c Contract) Start(dispatch string) (Contract, error) {
	if err := c.guard("start", StateOpened); err != nil {
		return c, err
	}
	if dispatch == "" {
		return c, refuse("start", c.state, ErrInput, "empty dispatch id")
	}
	n := c.clone()
	if n.ID == "" {
		n.ID = dispatch
	}
	n.Holders, n.state = []string{dispatch}, StateWorking
	return n, nil
}

// AddChild records a sub-contract while working, within the depth and leaf budget.
func (c Contract) AddChild(id string) (Contract, error) {
	if err := c.guard("add-child", StateWorking); err != nil {
		return c, err
	}
	switch {
	case id == "" || id == c.ID || slices.Contains(c.Children, id):
		return c, refuse("add-child", c.state, ErrInput, "child id %q is empty, the contract itself or a repeat", id)
	case c.Owner.Role != RoleOwner || c.Limits.Depth >= MaxDepth:
		return c, refuse("add-child", c.state, ErrLimit, "depth %d of %d: a fleet member never sub-contracts",
			c.Limits.Depth, MaxDepth)
	case len(c.Children) >= c.Limits.Leaves:
		return c, refuse("add-child", c.state, ErrLimit, "leaves %d of %d", len(c.Children), c.Limits.Leaves)
	}
	n := c.clone()
	n.Children = append(n.Children, id)
	return n, nil
}

// Reassign hands a working contract whose holder died to a new holder, once.
func (c Contract) Reassign(dispatch string) (Contract, error) {
	if err := c.guard("reassign", StateWorking); err != nil {
		return c, err
	}
	if dispatch == "" || slices.Contains(c.Holders, dispatch) {
		return c, refuse("reassign", c.state, ErrInput, "dispatch id %q is empty or already held it", dispatch)
	}
	if len(c.Holders) > 1 {
		return c, refuse("reassign", c.state, ErrLimit, "already re-assigned once (holders %v)", c.Holders)
	}
	n := c.clone()
	n.Holders = append(n.Holders, dispatch)
	return n, nil
}

// Verify moves working → verified once gate and tests passed and every child in children is terminal.
func (c Contract) Verify(children []Contract) (Contract, error) {
	if err := c.guard("verify", StateWorking); err != nil {
		return c, err
	}
	if miss := c.missing(verifyTerms...); len(miss) > 0 {
		return c, refuse("verify", c.state, ErrTerms, "no pass record for %v", miss)
	}
	for _, id := range c.Children {
		i := slices.IndexFunc(children, func(k Contract) bool { return k.ID == id })
		if i < 0 || !children[i].state.Terminal() {
			return c, refuse("verify", c.state, ErrChildOpen, "sub-contract %q is not closed or failed", id)
		}
	}
	n := c.clone()
	n.state = StateVerified
	return n, nil
}

// Close moves verified → closed once the hand-back and every close term passed; Go fills Result.Unmet.
func (c Contract) Close(r Result) (Contract, error) {
	if err := c.guard("close", StateVerified); err != nil {
		return c, err
	}
	if err := checkResult(r); err != "" {
		return c, refuse("close", c.state, ErrInput, "%s", err)
	}
	if c.Executed && r.Tests != TestsExecuted {
		return c, refuse("close", c.state, ErrInput, "tests %q, this contract needs %q", r.Tests, TestsExecuted)
	}
	if miss := c.missing(append(slices.Clone(verifyTerms), c.closeTerms()...)...); len(miss) > 0 {
		return c, refuse("close", c.state, ErrTerms, "no pass record for %v", miss)
	}
	n := c.clone()
	n.Result, n.state = settled(r, n.Unmet()), StateClosed
	return n, nil
}

// Fail moves opened, working or verified → failed for why; Go fills Result.Unmet.
func (c Contract) Fail(why Reason) (Contract, error) {
	if err := c.guard("fail", StateOpened, StateWorking, StateVerified); err != nil {
		return c, err
	}
	if !slices.Contains(Reasons(), why) {
		return c, refuse("fail", c.state, ErrInput, "unknown reason %q (one of %v)", why, Reasons())
	}
	n := c.clone()
	r := Result{}
	if n.Result != nil {
		r = *n.Result
	}
	n.Result, n.Failed, n.state = settled(r, n.Unmet()), why, StateFailed
	return n, nil
}

// WithRecord appends one term record to a non-terminal contract; it is not a transition.
func (c Contract) WithRecord(r TermRecord) (Contract, error) {
	if err := c.guard("record", StateOpened, StateWorking, StateVerified); err != nil {
		return c, err
	}
	if c.state == StateVerified && slices.Contains(verifyTerms, r.Term) {
		return c, refuse("record", c.state, ErrState, "%q was settled by verify", r.Term)
	}
	if err := checkRecord(c, r); err != "" {
		return c, refuse("record", c.state, ErrInput, "%s", err)
	}
	n := c.clone()
	n.Records = append(n.Records, r)
	return n, nil
}

// verifyTerms must pass before Verify and stay passed until Close.
var verifyTerms = []Term{TermGate, TermTests}

// closeTerms are the hand-back and c's close-stage terms, which must pass before Close.
func (c Contract) closeTerms() []Term {
	out := []Term{TermHandback}
	for _, t := range c.Terms {
		if StageOf(t) == StageClose {
			out = append(out, t)
		}
	}
	return out
}

// missing lists the terms of want that c holds and has not passed; a term c does not hold never blocks.
func (c Contract) missing(want ...Term) []Term {
	var out []Term
	for _, t := range want {
		if slices.Contains(c.Terms, t) && !c.passed(t) {
			out = append(out, t)
		}
	}
	return out
}

func settled(r Result, unmet []Term) *Result {
	r.Unfixed, r.Unmet = slices.Clone(r.Unfixed), unmet
	return &r
}

func checkResult(r Result) string {
	switch {
	case !slices.Contains(Statuses(), r.Status):
		return fmt.Sprintf("unknown status %q (one of %v)", r.Status, Statuses())
	case !slices.Contains(TestsValues(), r.Tests):
		return fmt.Sprintf("unknown tests %q (one of %v)", r.Tests, TestsValues())
	case !slices.Contains(Methods(), r.Method):
		return fmt.Sprintf("unknown method %q (one of %v)", r.Method, Methods())
	case r.Spent.Tokens < 0 || r.Spent.Leaves < 0:
		return fmt.Sprintf("negative spend %+v", r.Spent)
	}
	return ""
}

// wire is Contract's JSON shape: every exported field plus the state.
type wire struct {
	alias
	State State `json:"state"`
}

type alias Contract

// MarshalJSON writes the contract with its state under the key "state".
func (c Contract) MarshalJSON() ([]byte, error) { return json.Marshal(wire{alias(c), c.state}) }

// UnmarshalJSON reads a contract and validates it; an unknown state or a broken invariant is an error.
func (c *Contract) UnmarshalJSON(b []byte) error {
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	if !slices.Contains(States(), w.State) {
		return fmt.Errorf("contract: unknown state %q (one of %v)", w.State, States())
	}
	n := Contract(w.alias)
	n.state = w.State
	if ps := Validate(n); len(ps) > 0 {
		return fmt.Errorf("contract %q: %v", n.ID, ps)
	}
	*c = n
	return nil
}
