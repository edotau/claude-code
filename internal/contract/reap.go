package contract

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"time"
)

// Env is what Reap needs from the dispatch layer, which imports this package and so cannot be imported here.
type Env struct {
	Lost      func(dispatch string) bool // the dispatch's record is lost
	Successor func(Contract) string      // a live replacement dispatch id for c, or "" for none; nil means none
	Claimed   func(dispatch string) bool // a recovery of the dispatch is under way: spare its contract this round
}

// Reaped is one contract Reap acted on: Action is "reassigned" or "failed".
type Reaped struct{ ID, Action, Holder string }

// Reap reassigns lost holders once or fails them, inheriting a terminal parent's reason; lanes are kept.
func Reap(env Env) ([]Reaped, error) {
	orphans, err := Orphans(env)
	errs := []error{err}
	var out []Reaped
	for _, c := range orphans {
		r, err := reapOne(c.ID, env)
		if err != nil {
			errs = append(errs, fmt.Errorf("contract %s: %w", c.ID, err))
		}
		if r.Action != "" {
			out = append(out, r)
		}
	}
	return out, errors.Join(errs...)
}

// Orphans lists the working contracts whose current holder's dispatch is lost, without changing anything.
func Orphans(env Env) ([]Contract, error) {
	pending, err := Pending()
	return slices.DeleteFunc(pending, func(c Contract) bool { return !orphaned(c, env) }), err
}

// reapGrace spares a contract written this recently: ask saves it just before its dispatch record exists.
var reapGrace = 30 * time.Second

// orphaned reports whether c is working, past the grace and its current holder's dispatch is lost.
func orphaned(c Contract, env Env) bool {
	return c.State() == StateWorking && len(c.Holders) > 0 && env.Lost != nil && age(c.ID, time.Now()) >= reapGrace &&
		env.Lost(c.Holders[len(c.Holders)-1])
}

// reapOne re-sends outside the lock (Resend's resumed marker keeps it single), then settles id re-checking the holder.
func reapOne(id string, env Env) (r Reaped, err error) {
	c, err := Load(id)
	if err != nil {
		return Reaped{}, err
	}
	if !orphaned(c, env) {
		return Reaped{}, nil
	}
	holder := c.Holders[len(c.Holders)-1]
	why, terminal, err := terminalParentFailure(c)
	if err != nil {
		return Reaped{}, err
	}
	if terminal {
		return failOrphanedLeaf(c, env, holder, why)
	}
	next := successorOf(c, env)
	_, err = Update(id, func(c Contract) (Contract, error) {
		if !orphaned(c, env) || c.Holders[len(c.Holders)-1] != holder || spared(c, env, next) {
			return c, nil
		}
		r = Reaped{ID: id, Holder: holder}
		n, action, err := reassignOrFail(c, next)
		r.Action = action
		return n, err
	})
	if err != nil {
		r.Action = ""
	}
	return r, err
}

// successorOf is c's re-send, made once only ("" for none).
func successorOf(c Contract, env Env) string {
	if env.Successor == nil || len(c.Holders) > 1 {
		return ""
	}
	return env.Successor(c)
}

// spared reports whether c lacks a successor only because another recovery of its holder is under way.
func spared(c Contract, env Env, next string) bool {
	return next == "" && len(c.Holders) == 1 && env.Claimed != nil && env.Claimed(c.Holders[0])
}

// reassignOrFail hands c to next when there is one and it takes, else fails c owner-dead.
func reassignOrFail(c Contract, next string) (Contract, string, error) {
	if next != "" {
		if n, err := c.Reassign(next); err == nil {
			return n, "reassigned", nil
		}
	}
	n, err := c.Fail(ReasonOwnerDead)
	if err != nil {
		return c, "", err
	}
	return n, "failed", nil
}

// Summary is the doctor row: counts by state, the oldest non-terminal contract and verified ones waiting on a commit.
type Summary struct {
	ByState       map[State]int
	Oldest        string        // id of the oldest non-terminal contract, "" when none
	OldestAge     time.Duration // time since its file was last written
	StaleVerified []string      // verified contracts untouched for over StaleVerifiedAfter, by id
}

// StaleVerifiedAfter is how long a verified contract may wait for the main session's commit.
const StaleVerifiedAfter = 24 * time.Hour

// Summarize counts every saved contract by state; an unreadable file is joined into the error.
func Summarize(now time.Time) (Summary, error) {
	all, err := List()
	s := Summary{ByState: map[State]int{}}
	for _, c := range all {
		s.ByState[c.State()]++
		if c.State().Terminal() {
			continue
		}
		a := age(c.ID, now)
		if s.Oldest == "" || a > s.OldestAge {
			s.Oldest, s.OldestAge = c.ID, a
		}
		if c.State() == StateVerified && a > StaleVerifiedAfter {
			s.StaleVerified = append(s.StaleVerified, c.ID)
		}
	}
	slices.Sort(s.StaleVerified)
	return s, err
}

// age is the time since id's file was last written, 0 when it cannot be read.
func age(id string, now time.Time) time.Duration {
	p, err := Path(id)
	if err != nil {
		return 0
	}
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return now.Sub(fi.ModTime())
}

// terminalParentFailure prevents a lost leaf from being re-sent under an already terminal owner.
func terminalParentFailure(c Contract) (Reason, bool, error) {
	if c.Parent == "" {
		return "", false, nil
	}
	p, err := Load(c.Parent)
	if err != nil {
		return "", false, fmt.Errorf("parent contract %s: recovery deferred: %w", c.Parent, err)
	}
	if !p.State().Terminal() {
		return "", false, nil
	}
	if p.Failed != "" {
		return p.Failed, true, nil
	}
	return ReasonTerm, true, nil
}

func failOrphanedLeaf(c Contract, env Env, holder string, why Reason) (Reaped, error) {
	r := Reaped{}
	_, err := Update(c.ID, func(k Contract) (Contract, error) {
		if !orphaned(k, env) || k.Holders[len(k.Holders)-1] != holder {
			return k, nil
		}
		n, err := k.Fail(why)
		if err == nil {
			r = Reaped{ID: k.ID, Action: "failed", Holder: holder}
		}
		return n, err
	})
	return r, err
}
