package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/dev/contract"
	"github.com/edotau/claude-code/internal/dev/plans"
	"github.com/edotau/claude-code/internal/platform/paths"
)

// contractStore is the part of the contract store the close-term recording needs.
type contractStore interface {
	Pending() ([]contract.Contract, error)
	Update(id string, fn func(contract.Contract) (contract.Contract, error)) (contract.Contract, error)
}

type diskContracts struct{}

func (diskContracts) Pending() ([]contract.Contract, error) { return contract.Pending() }

func (diskContracts) Update(id string, fn func(contract.Contract) (contract.Contract, error)) (contract.Contract, error) {
	return contract.Update(id, fn)
}

// openContractStore is the one place the real store is plugged in.
var openContractStore = func() contractStore { return diskContracts{} }

// recordCloseTerms records tick (and landed) on each verified contract match accepts, then closes it.
func recordCloseTerms(match func(contract.Contract) bool, landed string, warn io.Writer) {
	st := openContractStore()
	if st == nil {
		return
	}
	pending, err := st.Pending()
	if err != nil {
		fmt.Fprintf(warn, "worktree: warning: contracts: %v\n", err)
	}
	for _, c := range pending {
		if c.State() != contract.StateVerified || !match(c) {
			continue
		}
		closeOne(st, c.ID, landed, warn)
	}
}

// closeOne records c's close terms under its lock and closes it with the held hand-back once every term passed.
func closeOne(st contractStore, id, landed string, warn io.Writer) {
	held := true
	_, err := st.Update(id, func(c contract.Contract) (contract.Contract, error) {
		n, err := withCloseTerms(c, landed)
		if err != nil || len(n.Unmet()) > 0 {
			return n, err
		}
		if held = n.Handback != nil; !held {
			return n, nil
		}
		return n.Close(*n.Handback)
	})
	if err != nil {
		fmt.Fprintf(warn, "worktree: warning: contract %s: %v\n", id, err)
	} else if !held {
		fmt.Fprintf(warn, "worktree: warning: contract %s: no hand-back held, left verified\n", id)
	}
}

// withCloseTerms adds the tick pass (its sha from the plan) and the landed pass (landed sha) c still lacks.
func withCloseTerms(c contract.Contract, landed string) (contract.Contract, error) {
	evidence := map[contract.Term]string{contract.TermTick: tickSha(c), contract.TermLanded: landed}
	for _, term := range []contract.Term{contract.TermTick, contract.TermLanded} {
		if evidence[term] == "" || !slices.Contains(c.Unmet(), term) {
			continue
		}
		var err error
		c, err = c.WithRecord(contract.TermRecord{Term: term, Verdict: contract.VerdictPass, By: contract.ByGo,
			Evidence: evidence[term], At: time.Now().UTC()})
		if err != nil {
			return c, err
		}
	}
	return c, nil
}

// tickSha is the sha the contract's plan records for its ticked task, or "" while the task is open.
func tickSha(c contract.Contract) string {
	if c.Scope.Plan == "" || c.Scope.PlanTask == "" {
		return ""
	}
	p, err := plans.Parse(c.Scope.Plan)
	if err != nil {
		return ""
	}
	for _, t := range p.Tasks {
		if t.ID == c.Scope.PlanTask && t.Done {
			return t.Sha
		}
	}
	return ""
}

// inRepo is true for a contract whose plan lives in home (a repo's plans dir); one with no plan names no repo.
func inRepo(c contract.Contract, home string) bool {
	return c.Scope.Plan == "" || paths.SamePath(filepath.Dir(c.Scope.Plan), home)
}

// onLane matches the repo's contracts held by the named lane.
func onLane(name, home string) func(contract.Contract) bool {
	return func(c contract.Contract) bool { return name != "" && c.Scope.Lane == name && inRepo(c, home) }
}

// forTasks matches a commit's contracts: its lane's, or the named tasks' in the named plan ("": any plan of the repo).
func forTasks(lane, home, plan string, tasks []string) func(contract.Contract) bool {
	return func(c contract.Contract) bool {
		named := c.Scope.PlanTask != "" && slices.Contains(tasks, c.Scope.PlanTask) &&
			(plan == "" && inRepo(c, home) || plan != "" && paths.SamePath(plan, c.Scope.Plan))
		return named || onLane(lane, home)(c)
	}
}
