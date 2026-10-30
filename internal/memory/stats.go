// Bank stats: aggregate the update receipts already on disk into the "is the bank actually
// working" numbers — update success rate and bank-updated rate.
package memory

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// BankStats aggregates every update receipt under StateDir().
type BankStats struct {
	Updates     int // receipt files parsed
	OK          int
	Failed      int
	Pending     int
	BankUpdated int // ok updates that changed the bank
	Oldest      time.Time
	Newest      time.Time
}

// CollectBankStats scans StateDir()/update-result-*. Unreadable or statusless receipts are skipped.
func CollectBankStats() BankStats {
	var s BankStats
	matches, _ := filepath.Glob(filepath.Join(StateDir(), "update-result-*"))
	for _, p := range matches {
		id := strings.TrimPrefix(filepath.Base(p), "update-result-")
		r, ok := ReadUpdateResult(id)
		if !ok {
			continue
		}
		s.Updates++
		switch r.Status {
		case UpdateOK:
			s.OK++
			if r.BankUpdated {
				s.BankUpdated++
			}
		case UpdateFailed:
			s.Failed++
		case UpdatePending:
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
	if s.Updates == 0 {
		b.WriteString("updates: no receipts yet (state/harness/memory/update-result-*)\n")
		return b.String()
	}
	fmt.Fprintf(&b, "updates: %d receipt(s)", s.Updates)
	if !s.Oldest.IsZero() {
		fmt.Fprintf(&b, "  %s → %s", s.Oldest.Format("2006-01-02"), s.Newest.Format("2006-01-02"))
	}
	b.WriteByte('\n')
	if terminal := s.OK + s.Failed; terminal > 0 {
		fmt.Fprintf(&b, "  success rate: %d/%d ok (%.0f%%)\n", s.OK, terminal, 100*float64(s.OK)/float64(terminal))
	}
	if s.OK > 0 {
		fmt.Fprintf(&b, "  bank updated: %d/%d ok updates (%.0f%%)\n", s.BankUpdated, s.OK, 100*float64(s.BankUpdated)/float64(s.OK))
	}
	if s.Pending > 0 {
		fmt.Fprintf(&b, "  unreported:   %d pending\n", s.Pending)
	}
	return b.String()
}
