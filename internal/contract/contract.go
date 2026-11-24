// Package contract is the typed contract schema: one unit of work, its fixed terms, limits and lifecycle.
package contract

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

type (
	// Kind is the size of the unit.
	Kind string
	// Role is the holder's place in the tree.
	Role string
	// Term is one fixed checklist item Go performs and records.
	Term string
	// Stage is when a term is performed.
	Stage string
	// Verdict is a term record's outcome.
	Verdict string
	// Tests is how the hand-back's test verdict was reached.
	Tests string
	// Method is how the owner did the work.
	Method string
	// State is a contract's lifecycle state; only the transition functions change it.
	State string
	// Reason is why a contract failed.
	Reason string
	// Status is the hand-back's status.
	Status string
)

const (
	KindFunction   Kind = "function"
	KindTestRepair Kind = "test-repair"
	KindFile       Kind = "file"
	KindTask       Kind = "task"
)

const (
	RoleOwner Role = "owner"
	RoleFleet Role = "fleet"
)

const (
	TermLane        Term = "lane"
	TermProxy       Term = "proxy"
	TermLeaves      Term = "leaves"
	TermGate        Term = "gate"
	TermTests       Term = "tests"
	TermHandback    Term = "handback"
	TermReport      Term = "report"
	TermTick        Term = "tick"
	TermProxyClosed Term = "proxy-closed"
	TermLanded      Term = "landed"
)

const (
	StageOpen   Stage = "open"
	StageDuring Stage = "during"
	StageReturn Stage = "return"
	StageClose  Stage = "close"
)

const (
	VerdictPass Verdict = "pass"
	VerdictFail Verdict = "fail"
)

const (
	TestsJudged   Tests = "judged"
	TestsExecuted Tests = "executed"
	TestsUnknown  Tests = "unknown"
)

const (
	MethodSolo  Method = "solo"
	MethodFleet Method = "fleet"
)

const (
	StateOpened   State = "opened"
	StateWorking  State = "working"
	StateVerified State = "verified"
	StateClosed   State = "closed"
	StateFailed   State = "failed"
)

const (
	ReasonLimit      Reason = "limit"
	ReasonTerm       Reason = "term"
	ReasonOwnerDead  Reason = "owner-dead"
	ReasonCancelled  Reason = "cancelled"
	ReasonDeadline   Reason = "deadline"   // the harness cut the run off at its deadline
	ReasonUnreadable Reason = "unreadable" // the contract file could not be read
)

const (
	StatusDone             Status = "DONE"
	StatusDoneWithConcerns Status = "DONE_WITH_CONCERNS"
	StatusNeedsContext     Status = "NEEDS_CONTEXT"
	StatusBlocked          Status = "BLOCKED"
)

// By values of a TermRecord: Go records every term; the owner records only the judged tests verdict.
const (
	ByGo    = "go"
	ByOwner = "owner"
)

// Kinds lists every Kind.
func Kinds() []Kind { return []Kind{KindFunction, KindTestRepair, KindFile, KindTask} }

// Roles lists every Role.
func Roles() []Role { return []Role{RoleOwner, RoleFleet} }

// Terms lists every Term in stage order.
func Terms() []Term {
	out := make([]Term, len(termStages))
	for i, ts := range termStages {
		out[i] = ts.term
	}
	return out
}

// Stages lists every Stage in order.
func Stages() []Stage { return []Stage{StageOpen, StageDuring, StageReturn, StageClose} }

// Verdicts lists every Verdict.
func Verdicts() []Verdict { return []Verdict{VerdictPass, VerdictFail} }

// TestsValues lists every Tests value.
func TestsValues() []Tests { return []Tests{TestsJudged, TestsExecuted, TestsUnknown} }

// Methods lists every Method.
func Methods() []Method { return []Method{MethodSolo, MethodFleet} }

// Reasons lists every Reason.
func Reasons() []Reason {
	return []Reason{ReasonLimit, ReasonTerm, ReasonOwnerDead, ReasonCancelled, ReasonDeadline, ReasonUnreadable}
}

// Statuses lists every Status.
func Statuses() []Status {
	return []Status{StatusDone, StatusDoneWithConcerns, StatusNeedsContext, StatusBlocked}
}

// termStages is the one table of terms and their stage, in stage order.
var termStages = []struct {
	term  Term
	stage Stage
}{
	{TermLane, StageOpen}, {TermProxy, StageOpen},
	{TermLeaves, StageDuring},
	{TermGate, StageReturn}, {TermTests, StageReturn}, {TermHandback, StageReturn}, {TermReport, StageReturn},
	{TermTick, StageClose}, {TermProxyClosed, StageClose}, {TermLanded, StageClose},
}

// StageOf is the stage of t; an unknown term has the empty Stage, which Validate reports.
func StageOf(t Term) Stage {
	for _, ts := range termStages {
		if ts.term == t {
			return ts.stage
		}
	}
	return ""
}

// Contract is one unit of work held by one owner from open to close or fail.
type Contract struct {
	Schema   int          `json:"schema"`
	ID       string       `json:"id"`               // the owner's dispatch id
	Parent   string       `json:"parent,omitempty"` // the contract that sub-contracted this one
	Scope    Scope        `json:"scope"`
	Owner    Party        `json:"owner"`
	Terms    []Term       `json:"terms"` // fixed at open by TermsFor; never edited after
	Limits   Limits       `json:"limits"`
	Records  []TermRecord `json:"records,omitempty"`
	Result   *Result      `json:"result,omitempty"`
	Handback *Result      `json:"handback,omitempty"` // the owner's hand-back, held from verify for Close
	Holders  []string     `json:"holders"`            // dispatch ids that held it; two after a re-assign
	Children []string     `json:"children,omitempty"` // sub-contract ids
	Failed   Reason       `json:"failed,omitempty"`
	Executed bool         `json:"executed_tests,omitempty"` // only an executed run (by go) passes the tests term
	state    State        // only the transition functions change it; JSON key "state"
}

// Scope is the unit and the files it may touch.
type Scope struct {
	Unit     string   `json:"unit"` // "func piActionArgv", "TestCountLines"
	Kind     Kind     `json:"kind"`
	Own      []string `json:"own"`
	PlanTask string   `json:"plan_task,omitempty"`
	Plan     string   `json:"plan,omitempty"` // the plan file's absolute path, with PlanTask
	Lane     string   `json:"lane,omitempty"` // the lane name `claude-code worktree list` prints
	Verify   string   `json:"verify,omitempty"`
}

// Party is the holder of a contract.
type Party struct {
	Role   Role   `json:"role"`
	Agent  string `json:"agent"`
	Tier   string `json:"tier,omitempty"`
	Runner string `json:"runner,omitempty"`
}

// Limits are the depth (1 owner, 2 fleet) and the token and leaf budget.
type Limits struct {
	Depth  int `json:"depth"`
	Tokens int `json:"tokens"`
	Leaves int `json:"leaves"`
}

// TermRecord is one performed term.
type TermRecord struct {
	Term     Term      `json:"term"`
	Verdict  Verdict   `json:"verdict"`
	By       string    `json:"by"`                 // ByGo, or ByOwner for the judged tests verdict only
	Evidence string    `json:"evidence,omitempty"` // an exit code, a sha, a job id, a path: never prose
	At       time.Time `json:"at"`
}

// Result is the owner's typed hand-back; Unmet is filled by Go.
type Result struct {
	Status  Status   `json:"status"`
	Summary string   `json:"summary"`
	Unfixed []string `json:"unfixed"`
	Tests   Tests    `json:"tests"`
	Method  Method   `json:"method"`
	Unmet   []Term   `json:"unmet,omitempty"`
	Spent   Spent    `json:"spent"`
}

// Spent is what the contract's subtree used.
type Spent struct {
	Tokens int `json:"tokens"`
	Leaves int `json:"leaves"`
}

// Facts are the typed dispatch facts TermsFor and NeedsExecutedTests read.
type Facts struct{ Lane, ScopedProxy, PlanTask, Fleet, TestStep, TestRepair bool }

// Problem is one validation finding.
type Problem struct{ Field, Detail string }

func (p Problem) String() string {
	if p.Field == "" {
		return p.Detail
	}
	return p.Field + ": " + p.Detail
}

// TermsFor is the fixed checklist for f in stage order, without the terms that do not apply.
func TermsFor(f Facts) []Term {
	skip := map[Term]bool{
		TermLane: !f.Lane, TermLanded: !f.Lane,
		TermProxy: !f.ScopedProxy, TermProxyClosed: !f.ScopedProxy,
		TermTick: !f.PlanTask, TermLeaves: !f.Fleet,
	}
	var out []Term
	for _, t := range Terms() {
		if !skip[t] {
			out = append(out, t)
		}
	}
	return out
}

// factProblems are what f needs of scope at open: a lane names its lane, a plan task names its task and plan.
func factProblems(scope Scope, f Facts) []Problem {
	var ps []Problem
	if f.Lane && scope.Lane == "" {
		ps = append(ps, Problem{"scope.lane", "empty: a lane contract names its lane"})
	}
	if f.PlanTask && (scope.PlanTask == "" || scope.Plan == "") {
		ps = append(ps, Problem{"scope.plan", "a plan-task contract names its plan_task and plan"})
	}
	return ps
}

// NeedsExecutedTests reports whether only an executed run can pass the tests term.
func NeedsExecutedTests(f Facts) bool { return f.TestStep || f.TestRepair }

// Unmet lists c's terms, in order, whose last record is not a pass.
func (c Contract) Unmet() []Term {
	var out []Term
	for _, t := range c.Terms {
		if !c.passed(t) {
			out = append(out, t)
		}
	}
	return out
}

// passed reports whether t's last record is a pass, so a later failed re-run outweighs an earlier pass.
func (c Contract) passed(t Term) bool {
	for _, r := range slices.Backward(c.Records) {
		if r.Term == t {
			return r.Verdict == VerdictPass
		}
	}
	return false
}

// Fulfilled reports whether c closed with every term passed.
func (c Contract) Fulfilled() bool { return c.state == StateClosed && len(c.Unmet()) == 0 }

// Field is one named value of a Contract, in display order.
type Field struct{ Name, Value string }

// Fields lists the contract's display fields without reflection.
func (c Contract) Fields() []Field {
	terms := make([]string, len(c.Terms))
	for i, t := range c.Terms {
		terms[i] = string(t)
	}
	return []Field{
		{"id", c.ID}, {"parent", c.Parent}, {"state", string(c.state)}, {"failed", string(c.Failed)},
		{"unit", c.Scope.Unit}, {"kind", string(c.Scope.Kind)}, {"own", strings.Join(c.Scope.Own, ",")},
		{"plan_task", c.Scope.PlanTask}, {"plan", c.Scope.Plan}, {"lane", c.Scope.Lane}, {"verify", c.Scope.Verify},
		{"role", string(c.Owner.Role)}, {"agent", c.Owner.Agent}, {"tier", c.Owner.Tier}, {"runner", c.Owner.Runner},
		{"terms", strings.Join(terms, ",")}, {"unmet", strconv.Itoa(len(c.Unmet()))},
		{"depth", strconv.Itoa(c.Limits.Depth)}, {"tokens", strconv.Itoa(c.Limits.Tokens)},
		{"leaves", strconv.Itoa(len(c.Children)) + "/" + strconv.Itoa(c.Limits.Leaves)},
		{"holders", strings.Join(c.Holders, ",")}, {"executed_tests", strconv.FormatBool(c.Executed)},
	}
}

// clone deep-copies every slice and pointer so a transition never shares state with its input.
func (c Contract) clone() Contract {
	c.Scope.Own = slices.Clone(c.Scope.Own)
	c.Terms = slices.Clone(c.Terms)
	c.Records = slices.Clone(c.Records)
	c.Holders = slices.Clone(c.Holders)
	c.Children = slices.Clone(c.Children)
	c.Result, c.Handback = cloneResult(c.Result), cloneResult(c.Handback)
	return c
}

func cloneResult(p *Result) *Result {
	if p == nil {
		return nil
	}
	r := *p
	r.Unfixed, r.Unmet = slices.Clone(r.Unfixed), slices.Clone(r.Unmet)
	return &r
}
