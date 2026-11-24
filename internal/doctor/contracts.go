package doctor

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/tree"
	"github.com/edotau/claude-code/internal/contract"
)

// contractRows reports contracts by state, read-only like the rest of doctor; `agents cancel --lost` reaps them.
func contractRows(_ context.Context) ([]check, bool) {
	orphans, oerr := contract.Orphans(tree.ContractEnv())
	return []check{contractCheck(time.Now(), orphans, oerr)}, true
}

// orphanLanes names each orphaned contract with its kept lane, "-" when it has none.
func orphanLanes(cs []contract.Contract) string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID + " lane " + cmp.Or(c.Scope.Lane, "-")
	}
	return strings.Join(out, ", ")
}

func contractCheck(now time.Time, orphans []contract.Contract, orphanErr error) check {
	s, err := contract.Summarize(now)
	row := check{Name: "contracts", OK: err == nil && orphanErr == nil && len(s.StaleVerified) == 0}
	var parts []string
	for _, st := range contract.States() {
		if n := s.ByState[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st))
		}
	}
	if len(parts) == 0 {
		parts = []string{"none"}
	}
	row.Detail = strings.Join(parts, ", ")
	if s.Oldest != "" {
		row.Detail += fmt.Sprintf("; oldest open %s (%s)", s.Oldest, s.OldestAge.Round(time.Minute))
	}
	if len(s.StaleVerified) > 0 {
		row.Detail += "; verified, waiting over a day on the main session's commit: " + strings.Join(s.StaleVerified, ",")
	}
	if len(orphans) > 0 {
		row.OK = false
		row.Detail += fmt.Sprintf("; %d held by a lost owner (%s): run claude-code agents cancel --lost",
			len(orphans), orphanLanes(orphans))
	}
	return withWarns(row, err, orphanErr)
}

func withWarns(row check, errs ...error) check {
	for _, e := range errs {
		if e != nil {
			row.Detail += "; warn " + e.Error()
		}
	}
	return row
}
