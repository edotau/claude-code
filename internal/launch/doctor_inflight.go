package launch

import (
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/inflight"
)

// inflightDoctorDetail is the live agent count against the machine cap, then each busy account against its own.
func inflightDoctorDetail() string {
	lim := inflight.CurrentLimits()
	snap := inflight.Count()
	var parts []string
	for _, acct := range slices.Sorted(func(yield func(string) bool) {
		for k, n := range snap.Per {
			if n > 0 && !yield(k) {
				return
			}
		}
	}) {
		parts = append(parts, fmt.Sprintf("%s %d/%d", acct, snap.Per[acct], lim.For(acct)))
	}
	if len(parts) == 0 {
		parts = []string{"none"}
	}
	return fmt.Sprintf("%d/%d (%s)", snap.Total, lim.Total, strings.Join(parts, ", "))
}
