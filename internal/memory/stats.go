// Bank stats: aggregate the harvest receipts already on disk into the "is the bank actually
// working" numbers — harvest success rate and bank-updated rate.
package memory

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// BankStats aggregates every harvest receipt under StateDir().
type BankStats struct {
	Harvests    int // receipt files parsed
	OK          int
	Failed      int
	Pending     int
	BankUpdated int // ok harvests that changed the bank
	Oldest      time.Time
	Newest      time.Time
}

// CollectBankStats scans StateDir()/harvest-result-*. Unreadable or statusless receipts are skipped.
func CollectBankStats() BankStats {
	var s BankStats
	matches, _ := filepath.Glob(filepath.Join(StateDir(), "harvest-result-*"))
	for _, p := range matches {
		id := strings.TrimPrefix(filepath.Base(p), "harvest-result-")
		r, ok := ReadHarvestResult(id)
		if !ok {
			continue
		}
		s.Harvests++
		switch r.Status {
		case HarvestOK:
			s.OK++
			if r.BankUpdated {
				s.BankUpdated++
			}
		case HarvestFailed:
			s.Failed++
		case HarvestPending:
			s.Pending++
		}
		if !r.Time.IsZero() {
			if s.Oldest.IsZero() || r.Time.Before(s.Oldest) {
				s.Oldest = r.Time
			}
			if r.Time.After(s.Newest) {
				s.Newest = r.Time
			}
		}
	}
	return s
}

// Report renders the aggregate as the human `memory stats` block.
func (s BankStats) Report() string {
	var b strings.Builder
	b.WriteString("== memory stats (receipts on disk) ==\n")
	if s.Harvests == 0 {
		b.WriteString("harvests: no receipts yet (state/harness/memory/harvest-result-*)\n")
		return b.String()
	}
	fmt.Fprintf(&b, "harvests: %d receipt(s)", s.Harvests)
	if !s.Oldest.IsZero() {
		fmt.Fprintf(&b, "  %s → %s", s.Oldest.Format("2006-01-02"), s.Newest.Format("2006-01-02"))
	}
	b.WriteByte('\n')
	if terminal := s.OK + s.Failed; terminal > 0 {
		fmt.Fprintf(&b, "  success rate: %d/%d ok (%.0f%%)\n", s.OK, terminal, 100*float64(s.OK)/float64(terminal))
	}
	if s.OK > 0 {
		fmt.Fprintf(&b, "  bank updated: %d/%d ok harvests (%.0f%%)\n", s.BankUpdated, s.OK, 100*float64(s.BankUpdated)/float64(s.OK))
	}
	if s.Pending > 0 {
		fmt.Fprintf(&b, "  unreported:   %d pending\n", s.Pending)
	}
	return b.String()
}
