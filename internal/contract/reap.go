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
	Queue     func(Contract) error       // queues c's lane marked failed; nil skips
}

// Reaped is one contract Reap acted on: Action is "reassigned" or "failed".
type Reaped struct{ ID, Action, Holder string }

// Reap re-assigns once, else fails owner-dead, each working contract whose holder is lost; its lane is queued failed.
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

// orphaned reports whether c is working and its current holder's dispatch is lost.
func orphaned(c Contract, env Env) bool {
	return c.State() == StateWorking && len(c.Holders) > 0 && env.Lost != nil && env.Lost(c.Holders[len(c.Holders)-1])
}

// reapOne settles id under its lock; a failure is queued before it is saved, so a queue error retries next pass.
func reapOne(id string, env Env) (r Reaped, err error) {
	_, err = Update(id, func(c Contract) (Contract, error) {
		if !orphaned(c, env) {
			return c, nil
		}
		r = Reaped{ID: id, Holder: c.Holders[len(c.Holders)-1]}
		if n, ok := reassign(c, env); ok {
			r.Action = "reassigned"
			return n, nil
		}
		n, err := c.Fail(ReasonOwnerDead)
		if err != nil {
			return c, err
		}
		if env.Queue != nil && n.Scope.Lane != "" {
			if err := env.Queue(n); err != nil {
				return c, err
			}
		}
		r.Action = "failed"
		return n, nil
	})
	if err != nil {
		r.Action = ""
	}
	return r, err
}

// reassign hands c to its successor when it has not been re-assigned yet and one exists.
func reassign(c Contract, env Env) (Contract, bool) {
	if env.Successor == nil || len(c.Holders) > 1 {
		return c, false
	}
	next := env.Successor(c)
	if next == "" {
		return c, false
	}
	n, err := c.Reassign(next)
	return n, err == nil
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
