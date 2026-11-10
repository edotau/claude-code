// Session timeline: the recent-sessions recall strategy rendered from update receipts and
// sessionHistory entries already on disk. Read-only; no new state.
package memory

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// timelineEntry is one recall row from an update receipt.
type timelineEntry struct {
	Time time.Time
	Line string
}

// timeline returns the newest-first update-receipt trail (bounded by limit) plus the most recent
// sessionHistory headings from the local then home bank (their files are newest-first already).
func timeline(root string, limit int) (entries []timelineEntry, sessions []string) {
	for _, rc := range readReceipts() {
		id, r := rc.id, rc.r
		if r.Time.IsZero() {
			continue
		}
		line := r.Status
		if r.BankUpdated {
			line += ", bank updated"
		}
		entries = append(entries, timelineEntry{Time: r.Time, Line: line + "  (session " + headID(id) + ")"})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time.After(entries[j].Time) })
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, recentSessions(root, limit)
}

// recentSessions lists the sessionHistory headings of root's bank then the home bank, deduped, up to limit (0 = all).
func recentSessions(root string, limit int) (sessions []string) {
	seen := map[string]bool{}
	dirs := []string{HomeBankDir()}
	if !isHomeBank(root) {
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
				return sessions
			}
		}
	}
	return sessions
}

// headID shortens a session id for the timeline's "(session ...)" suffix.
func headID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// receipt is one readable update-result file: its session id and parsed result.
type receipt struct {
	id string
	r  UpdateResult
}

// readReceipts returns every parseable StateDir()/update-result-* receipt, skipping unreadable ones.
func readReceipts() []receipt {
	var out []receipt
	matches, _ := filepath.Glob(filepath.Join(StateDir(), "update-result-*"))
	for _, p := range matches {
		id := strings.TrimPrefix(filepath.Base(p), "update-result-")
		if r, ok := ReadUpdateResult(id); ok {
			out = append(out, receipt{id, r})
		}
	}
	return out
}
