package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/tree"
	"github.com/edotau/claude-code/internal/contract"
)

// contractRows reaps the contracts of lost owners, then reports contracts by state; soft: it never fails doctor.
func contractRows(_ context.Context) ([]check, bool) {
	reaped, rerr := contract.Reap(tree.ContractEnv())
	return []check{contractCheck(time.Now(), len(reaped), rerr)}, true
}

func contractCheck(now time.Time, reaped int, reapErr error) check {
	s, err := contract.Summarize(now)
	row := check{Name: "contracts", OK: err == nil && reapErr == nil && len(s.StaleVerified) == 0}
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
	if reaped > 0 {
		row.Detail += fmt.Sprintf("; reaped %d of lost owners", reaped)
	}
	return withWarns(row, err, reapErr)
}

func withWarns(row check, errs ...error) check {
	for _, e := range errs {
		if e != nil {
			row.Detail += "; warn " + e.Error()
		}
	}
	return row
}
