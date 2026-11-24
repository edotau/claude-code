package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
)

// contractStore is the contract persistence the close-term recording needs (task 10.3 supplies the real one).
type contractStore interface {
	Pending() ([]contract.Contract, error) // every non-terminal contract
	Save(contract.Contract) error
}

const revListTimeout = 10 * time.Second

// openContractStore is the one call site that plugs the real store; nil means no contracts exist.
var openContractStore = func() contractStore { return nil }

// recordCloseTerms records tick (and landed after a merge) on pending contracts whose task a commit in shas ticked.
func recordCloseTerms(top string, shas func() ([]string, error), landed string, warn io.Writer) {
	st := openContractStore()
	if st == nil {
		return
	}
	pending, err := st.Pending()
	if err != nil {
		fmt.Fprintf(warn, "worktree: warning: contracts: %v\n", err)
		return
	}
	var full []string
	for _, c := range pending {
		if !takesCloseTerms(c) {
			continue
		}
		if full == nil {
			if full, err = shas(); err != nil {
				fmt.Fprintf(warn, "worktree: warning: contracts: %v\n", err)
				return
			}
		}
		sha := tickedBy(top, c.Scope.PlanTask, full)
		if sha == "" {
			continue
		}
		if err := saveTerms(st, c, sha, landed); err != nil {
			fmt.Fprintf(warn, "worktree: warning: contract %s: %v\n", c.ID, err)
		}
	}
}

// takesCloseTerms is true for a verified contract with a plan task to tick; earlier states refuse close records.
func takesCloseTerms(c contract.Contract) bool {
	return c.State() == contract.StateVerified && c.Scope.PlanTask != "" && slices.Contains(c.Terms, contract.TermTick)
}

// tickedBy is the full sha in shas that the plan records as having ticked task, or "".
func tickedBy(top, task string, shas []string) string {
	var landed string
	plans.TaskPlans(plans.Home(top), plans.MirrorDests(), task, func(t plans.Task) bool {
		if t.Done && len(t.Sha) >= 7 && landed == "" {
			for _, s := range shas {
				if strings.HasPrefix(s, strings.ToLower(t.Sha)) {
					landed = s
				}
			}
		}
		return false
	})
	return landed
}

// saveTerms records tick (and landed after a merge) as Go's pass, skipping terms already passed, then saves once.
func saveTerms(st contractStore, c contract.Contract, tickSha, landed string) error {
	for _, rec := range []struct {
		term     contract.Term
		evidence string
	}{{contract.TermTick, tickSha}, {contract.TermLanded, landed}} {
		if rec.evidence == "" || !slices.Contains(c.Unmet(), rec.term) {
			continue
		}
		var err error
		c, err = c.WithRecord(contract.TermRecord{Term: rec.term, Verdict: contract.VerdictPass, By: contract.ByGo,
			Evidence: rec.evidence, At: time.Now().UTC()})
		if err != nil {
			return err
		}
	}
	return st.Save(c)
}

// revList is the commit shas of rng; it has its own bounded context so an interrupt after promotion cannot skip it.
func revList(dir, rng string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), revListTimeout)
	defer cancel()
	out, err := proc.RunGit(ctx, dir, "rev-list", rng)
	return strings.Fields(out), err
}
