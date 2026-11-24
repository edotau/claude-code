package contract

import (
	"errors"
	"reflect"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/nonet"
)

var allFacts = Facts{Lane: true, ScopedProxy: true, PlanTask: true, Fleet: true}

func opened(t *testing.T, role Role, depth, leaves int) Contract {
	t.Helper()
	c, ps := Open(Scope{Unit: "func x", Kind: KindFunction, Own: []string{"a.go"}, PlanTask: "1.1", Plan: "/p.md", Lane: "l"},
		Party{Role: role, Agent: "pi"}, Limits{Depth: depth, Tokens: 400_000, Leaves: leaves}, allFacts)
	if len(ps) > 0 {
		t.Fatalf("Open: %v", ps)
	}
	return c
}

// passAll records a pass for every unpassed term c's state may record now.
func passAll(t *testing.T, c Contract) Contract {
	t.Helper()
	for _, term := range c.Terms {
		if c.passed(term) {
			continue
		}
		n, err := c.WithRecord(TermRecord{Term: term, Verdict: VerdictPass, By: ByGo, At: time.Now()})
		if err != nil && !errors.Is(err, ErrState) {
			t.Fatal(err)
		}
		c = n
	}
	return c
}

var done = Result{Status: StatusDone, Tests: TestsExecuted, Method: MethodSolo}

// inState builds a contract in s whose every legal transition succeeds.
func inState(t *testing.T, s State) Contract {
	t.Helper()
	c := passAll(t, opened(t, RoleOwner, 1, 2))
	steps := map[State]func(Contract) (Contract, error){
		StateWorking:  func(c Contract) (Contract, error) { return c.Start("d1") },
		StateVerified: func(c Contract) (Contract, error) { return c.Verify(nil) },
		StateClosed:   func(c Contract) (Contract, error) { return c.Close(done) },
	}
	for _, next := range []State{StateWorking, StateVerified, StateClosed} {
		if c.State() == s {
			return c
		}
		var err error
		if c, err = steps[next](c); err != nil {
			t.Fatal(err)
		}
		c = passAll(t, c)
	}
	if s == StateFailed {
		c = passAll(t, opened(t, RoleOwner, 1, 2))
		c, _ = c.Fail(ReasonCancelled)
	}
	return c
}

func TestTransitions(t *testing.T) {
	ops := map[string]func(Contract) (Contract, error){
		"start":     func(c Contract) (Contract, error) { return c.Start("d2") },
		"add-child": func(c Contract) (Contract, error) { return c.AddChild("k1") },
		"reassign":  func(c Contract) (Contract, error) { return c.Reassign("d3") },
		"verify":    func(c Contract) (Contract, error) { return c.Verify(nil) },
		"close":     func(c Contract) (Contract, error) { return c.Close(done) },
		"fail":      func(c Contract) (Contract, error) { return c.Fail(ReasonTerm) },
	}
	legal := map[State]map[string]State{
		StateOpened:   {"start": StateWorking, "fail": StateFailed},
		StateWorking:  {"add-child": StateWorking, "reassign": StateWorking, "verify": StateVerified, "fail": StateFailed},
		StateVerified: {"close": StateClosed, "fail": StateFailed},
	}
	for _, s := range States() {
		for name, op := range ops {
			c := inState(t, s)
			before := c.clone()
			got, err := op(c)
			if want, ok := legal[s][name]; ok {
				if err != nil || got.State() != want {
					t.Errorf("%s from %s: state %s err %v, want %s", name, s, got.State(), err, want)
				}
			} else if !errors.Is(err, ErrState) {
				t.Errorf("%s from %s: err %v, want ErrState", name, s, err)
			}
			if !reflect.DeepEqual(c, before) {
				t.Errorf("%s from %s mutated its input", name, s)
			}
		}
	}
}

func TestEveryOpenedContractTerminates(t *testing.T) {
	paths := map[string]func(Contract) (Contract, error){
		"settle": func(c Contract) (Contract, error) {
			c, _ = c.Start("d1")
			c, _ = passAll(t, c).Verify(nil)
			return passAll(t, c).Close(done)
		},
		"limit": func(c Contract) (Contract, error) {
			c, _ = c.Start("d1")
			c, _ = c.AddChild("k1")
			c, _ = c.AddChild("k2")
			if _, err := c.AddChild("k3"); !errors.Is(err, ErrLimit) {
				t.Errorf("third child: err %v, want ErrLimit", err)
			}
			return c.Fail(ReasonLimit)
		},
		"owner-dead": func(c Contract) (Contract, error) {
			c, _ = c.Start("d1")
			c, _ = c.Reassign("d2")
			if _, err := c.Reassign("d3"); !errors.Is(err, ErrLimit) {
				t.Errorf("second reassign: err %v, want ErrLimit", err)
			}
			return c.Fail(ReasonOwnerDead)
		},
		"cancel": func(c Contract) (Contract, error) { return c.Fail(ReasonCancelled) },
	}
	for name, path := range paths {
		c, err := path(passAll(t, opened(t, RoleOwner, 1, 2)))
		if err != nil || !c.State().Terminal() {
			t.Errorf("%s: ended %s, err %v", name, c.State(), err)
		}
	}
}

func TestParentCannotVerifyOverOpenChild(t *testing.T) {
	parent, _ := opened(t, RoleOwner, 1, 2).Start("p1")
	parent = passAll(t, parent)
	child := opened(t, RoleFleet, 2, 0)
	child.Parent = parent.ID
	child, _ = child.Start("k1")
	parent, _ = parent.AddChild(child.ID)
	stranger, _ := opened(t, RoleFleet, 2, 0).Start("k1")
	stranger, _ = stranger.Fail(ReasonTerm)
	for _, kids := range [][]Contract{nil, {child}, {stranger}} {
		if _, err := parent.Verify(kids); !errors.Is(err, ErrChildOpen) {
			t.Errorf("verify over %d open children: err %v, want ErrChildOpen", len(kids), err)
		}
	}
	child, _ = child.Fail(ReasonTerm)
	if got, err := parent.Verify([]Contract{child}); err != nil || got.State() != StateVerified {
		t.Errorf("verify over a failed child: %s, %v", got.State(), err)
	}
}
