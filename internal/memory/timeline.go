// Session timeline: the recent-sessions recall strategy rendered from harvest receipts and
// sessionHistory entries already on disk. Read-only; no new state.
package memory

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TimelineEntry is one recall row from a harvest receipt.
type TimelineEntry struct {
	Time time.Time
	Kind string // harvest
	Line string
}

// Timeline returns the newest-first harvest-receipt trail (bounded by limit) plus the most recent
// sessionHistory headings from the local then home bank (their files are newest-first already).
func Timeline(root string, limit int) (entries []TimelineEntry, sessions []string) {
	matches, _ := filepath.Glob(filepath.Join(StateDir(), "harvest-result-*"))
	for _, p := range matches {
		id := strings.TrimPrefix(filepath.Base(p), "harvest-result-")
		r, ok := ReadHarvestResult(id)
		if !ok || r.Time.IsZero() {
			continue
		}
		line := r.Status
		if r.BankUpdated {
			line += ", bank updated"
		}
		entries = append(entries, TimelineEntry{Time: r.Time, Kind: "harvest", Line: line + "  (session " + headID(id) + ")"})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time.After(entries[j].Time) })
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}

	seen := map[string]bool{}
	dirs := []string{HomeBankDir()}
	if !IsHomeBank(root) {
		dirs = []string{Dir(root), HomeBankDir()}
	}
	for _, dir := range dirs {
		for _, h := range entriesWithPrefix(filepath.Join(dir, "sessionHistory.md"), "## ") {
			if seen[h] {
				continue
			}
			seen[h] = true
			sessions = append(sessions, h)
			if limit > 0 && len(sessions) >= limit {
				return entries, sessions
			}
		}
	}
	return entries, sessions
}

// headID shortens a session id for the timeline's "(session ...)" suffix.
func headID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
