// Bank health: per-file cap roll-ups and the advisories `memory doctor` + SessionStart's cached notice read.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// bankStat is one bank's roll-up from a single read per typed file, so a caller never re-reads the bytes.
type bankStat struct {
	Dir         string
	Updated     time.Time
	Oversized   []string
	Unreadable  []string // "name: error" for each file whose read failed for a reason other than absence
	Credentials []string // "name: ..." for each file holding a credential-shaped value
	data        map[string][]byte
}

func statBank(dir string) bankStat {
	s := bankStat{Dir: dir, data: map[string][]byte{}}
	for _, name := range bankFiles {
		path := filepath.Join(dir, name)
		data, err := readFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				s.Unreadable = append(s.Unreadable, name+": "+err.Error())
			}
			continue
		}
		s.data[name] = data
		if capLines, ok := fileCaps[name]; ok && logicalLineCount(string(data)) > capLines {
			s.Oversized = append(s.Oversized, name)
		}
		if fi, err := os.Stat(path); err == nil && fi.ModTime().After(s.Updated) {
			s.Updated = fi.ModTime()
		}
		// The visible surface for commitStore's refusal to stage credential-shaped values.
		if textutil.CredentialRE.Match(data) {
			s.Credentials = append(s.Credentials, name+": possible credential-shaped value — scrub before promoting")
		}
	}
	return s
}

// DefaultStaleDays is the doctor's bank-staleness horizon.
const DefaultStaleDays = 90

// doctorNoticePath is the per-store cached doctor summary read at SessionStart.
func doctorNoticePath(root string) string {
	return filepath.Join(StateDir(), "doctor-notice-"+filepath.Base(Dir(root)))
}

// doctorNotice is the cached review line for SessionStart, "" with none; a full Doctor pass is too slow for launch.
func doctorNotice(root string) string {
	data, err := os.ReadFile(doctorNoticePath(root))
	if err != nil {
		return ""
	}
	notice := strings.TrimSpace(string(data))
	if notice == "" {
		return ""
	}
	return " memory review: " + textutil.TruncateRunes(notice, 200) + "."
}

// writeDoctorNotice caches root's first problem and a count (hot hints excluded); an empty file means checked and healthy.
func writeDoctorNotice(root string, staleDays int, now time.Time) error {
	problems := slices.DeleteFunc(doctor(root, staleDays, now), func(p string) bool {
		return strings.Contains(p, ": hot — recalled") // advice, not a problem; `memory review` still lists it
	})
	line := ""
	if len(problems) > 0 {
		line = problems[0]
		if len(problems) > 1 {
			line += " (+" + strconv.Itoa(len(problems)-1) + " more)"
		}
	}
	return paths.AtomicWrite(doctorNoticePath(root), []byte(line), 0o600)
}

// doctor returns advisory problems for the local and home banks (caps, staleness, credentials); never fatal.
func doctor(root string, staleDays int, now time.Time) []string {
	var problems []string
	if !isHomeBank(root) {
		problems = append(problems, bankProblems("local", statBank(Dir(root)), staleDays, now)...)
	}
	problems = append(problems, bankProblems("home", statBank(HomeBankDir()), staleDays, now)...)
	problems = append(problems, updateProblems(now)...)
	problems = append(problems, journalProblems(now)...)
	problems = append(problems, nativeLinkProblems(root)...)
	return append(problems, accessNominations(now)...)
}

// updateProblems surfaces receipts needing a human: a failed update, or a pending one past the grace window.
func updateProblems(now time.Time) []string {
	var problems []string
	for _, rc := range readReceipts() {
		id, r := rc.id, rc.r
		// A lost "pending" receipt is a bug to chase whenever it happened; a plain failure ages out after a week.
		if !r.NeedsAttention(now) || (r.Status == updateFailed && now.Sub(r.Time) > 7*24*time.Hour) {
			continue
		}
		problems = append(problems, "update "+headID(id)+" "+r.Status+" since "+r.Time.Format("2006-01-02")+
			updateLogNote(r.Log)+" (claude-code memory update --session "+id+")")
	}
	return problems
}

// updateLogNote points at the receipt's log, says it was swept once pruned, and stays silent when none was recorded.
func updateLogNote(log string) string {
	if log == "" {
		return ""
	}
	if _, err := os.Stat(log); err != nil {
		return " — log swept" // session-state pruning removes old update-*.log files; don't name a dead path
	}
	return " — see " + log
}

// journalGrace leaves an in-flight commit's journal alone; older ones are ones recovery keeps skipping.
const journalGrace = 10 * time.Minute

// journalProblems names each bank journal left on disk past the grace window, so a stuck one is seen, not retried silently.
func journalProblems(now time.Time) []string {
	entries, err := os.ReadDir(JournalDir())
	if err != nil {
		return nil // a missing dir means no journals; any other read error is the next commit's to report
	}
	var problems []string
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || !strings.HasSuffix(e.Name(), ".json") || now.Sub(fi.ModTime()) <= journalGrace {
			continue
		}
		problems = append(problems, "bank journal "+filepath.Join(JournalDir(), e.Name())+" left on disk for "+
			now.Sub(fi.ModTime()).Round(time.Minute).String()+" — recovery keeps skipping it; inspect, then delete or fix")
	}
	return problems
}

// bankProblems is one bank's advisories, each prefixed with the bank's tier so a reader knows which file to open.
func bankProblems(tier string, s bankStat, staleDays int, now time.Time) []string {
	var problems []string
	for _, f := range s.Oversized {
		problems = append(problems, tier+" "+f+": over "+strconv.Itoa(fileCaps[f])+" lines — consolidate or promote")
	}
	for _, u := range s.Unreadable {
		problems = append(problems, tier+" "+u+" — unreadable, so not checked")
	}
	if !s.Updated.IsZero() && s.Updated.Before(now.AddDate(0, 0, -staleDays)) {
		problems = append(problems, tier+" bank untouched since "+s.Updated.Format("2006-01-02")+
			" (>"+strconv.Itoa(staleDays)+"d) — stale")
	}
	for _, p := range s.Credentials {
		problems = append(problems, tier+" "+p)
	}
	return problems
}

// bankStats aggregates every update receipt under StateDir() into update-success and bank-updated rates.
type bankStats struct {
	Updates     int // receipt files parsed
	OK          int
	Failed      int
	Pending     int
	BankUpdated int // ok updates that changed the bank
	Oldest      time.Time
	Newest      time.Time
}

// collectBankStats scans StateDir()/update-result-*. Unreadable or statusless receipts are skipped.
func collectBankStats() bankStats {
	var s bankStats
	for _, rc := range readReceipts() {
		r := rc.r
		s.Updates++
		switch r.Status {
		case updateOK:
			s.OK++
			if r.BankUpdated {
				s.BankUpdated++
			}
		case updateFailed:
			s.Failed++
		case updatePending:
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
func (s bankStats) Report() string {
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

// timelineEntry is one recall row from an update receipt (the recent-sessions strategy, read-only).
type timelineEntry struct {
	Time time.Time
	Line string
}

// timeline returns the newest-first receipt trail (up to limit) plus recent sessionHistory headings, local then home.
func timeline(root string, limit int) (entries []timelineEntry, sessions []string, err error) {
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
	sessions, err = recentSessions(root, limit)
	return entries, sessions, err
}

// recentSessions lists the sessionHistory headings of root's bank then the home bank, deduped, up to limit (0 = all).
func recentSessions(root string, limit int) (sessions []string, err error) {
	seen := map[string]bool{}
	dirs := []string{HomeBankDir()}
	if !isHomeBank(root) {
		dirs = []string{Dir(root), HomeBankDir()}
	}
	for _, dir := range dirs {
		heads, err := entriesWithPrefix(filepath.Join(dir, "sessionHistory.md"), "## ")
		if err != nil {
			return sessions, err
		}
		for _, h := range heads {
			if seen[h] {
				continue
			}
			seen[h] = true
			sessions = append(sessions, h)
			if limit > 0 && len(sessions) >= limit {
				return sessions, nil
			}
		}
	}
	return sessions, nil
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
	for _, p := range globIn(StateDir(), "update-result-*") {
		id := strings.TrimPrefix(filepath.Base(p), "update-result-")
		if r, ok := readUpdateResult(id); ok {
			out = append(out, receipt{id, r})
		}
	}
	return out
}
