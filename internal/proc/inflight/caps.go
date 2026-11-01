package inflight

import "cmp"

// Base is the per-account cap on concurrent agents unless a profile's `concurrency` overrides it.
const Base = 12

// Limits are the caps: Base per account, Total across the machine, Override per account key.
type Limits struct {
	Base      int
	Total     int
	Available []string
	Override  map[string]int
}

// For is the cap of one account: its override, else Base.
func (l Limits) For(account string) int {
	if n := l.Override[account]; n > 0 {
		return n
	}
	return cmp.Or(l.Base, Base)
}

// ComputeLimits: Total = min(Base × max(len(available), 1), 4 × cpus); pure.
func ComputeLimits(available []string, override map[string]int, cpus int) Limits {
	total := Base * max(len(available), 1)
	if cpus > 0 {
		total = min(total, 4*cpus)
	}
	return Limits{Base: Base, Total: total, Available: available, Override: override}
}
