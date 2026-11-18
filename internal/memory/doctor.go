// Bank health: per-file cap roll-ups and the advisories `memory review` + SessionStart's cached notice read.
package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
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

func statBank() bankStat {
	s := bankStat{Dir: SubDir(""), data: map[string][]byte{}}
	for _, name := range bankFiles {
		path := Path(name)
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
		if textutil.CredentialRE().Match(data) {
			s.Credentials = append(s.Credentials, name+": possible credential-shaped value — scrub before promoting")
		}
	}
	return s
}

// DefaultStaleDays is the doctor's bank-staleness horizon.
const DefaultStaleDays = 90

// doctorNoticePath is the per-store cached doctor summary read at SessionStart.
func doctorNoticePath() string {
	return filepath.Join(StateDir(), "doctor-notice-"+stateKey())
}

// doctorNotice is the cached review line for SessionStart, "" with none; a full Doctor pass is too slow for launch.
func doctorNotice() string {
	data, err := os.ReadFile(doctorNoticePath())
	if err != nil {
		return ""
	}
	notice := strings.TrimSpace(string(data))
	if notice == "" {
		return ""
	}
	return " memory review: " + textutil.TruncateRunes(notice, 200) + "."
}

// writeDoctorNotice runs the doctor and caches its notice for root.
func writeDoctorNotice(root string, staleDays int, now time.Time) error {
	return cacheDoctorNotice(doctor(root, staleDays, now))
}

// cacheDoctorNotice caches problems' first line and a count (hot hints excluded); empty means checked and healthy.
func cacheDoctorNotice(problems []string) error {
	problems = slices.DeleteFunc(slices.Clone(problems), func(p string) bool {
		return strings.Contains(p, ": hot — recalled") // advice, not a problem; `memory review` still lists it
	})
	line := ""
	if len(problems) > 0 {
		if line = problems[0]; strings.HasPrefix(line, errIndexProblem.Error()) {
			line = indexNotice // path-free and fixed, so the 200-rune cut never splits a command
		}
		if len(problems) > 1 {
			line += " (+" + strconv.Itoa(len(problems)-1) + " more)"
		}
	}
	return paths.AtomicWrite(filepath.Join(StateDir(), "doctor-notice-"+stateKey()), []byte(line), 0o600)
}

// refreshNotice re-caches root's session-start notice once its bank is settled; a failure is a note on w, never fatal.
func refreshNotice(w io.Writer, verb, root string) {
	if err := writeDoctorNotice(root, DefaultStaleDays, time.Now()); err != nil {
		fmt.Fprintf(w, "%s: doctor notice: %v\n", verb, err)
	}
}

func refreshBankNotice(w io.Writer, verb, root string) {
	if err := cacheDoctorNotice(doctorBank(root, DefaultStaleDays, time.Now())); err != nil {
		fmt.Fprintf(w, "%s: doctor notice: %v\n", verb, err)
	}
}

// doctor returns advisory problems for the local and home banks (caps, staleness, credentials); never fatal.
func doctor(root string, staleDays int, now time.Time) []string {
	return doctorBank(root, staleDays, now)
}

func doctorBank(root string, staleDays int, now time.Time) []string {
	problems := indexProblems(now) // first: the session-start notice shows only the first problem
	problems = append(problems, bankProblems("home", statBank(), staleDays, now)...)
	problems = append(problems, updateProblems(now)...)
	problems = append(problems, journalProblems(now)...)
	if root != "" {
		problems = append(problems, nativeLinkProblems(root)...)
	}
	problems = append(problems, candidateProblems()...)
	return append(problems, accessNominations(now)...)
}

// indexNotice is the session-start line for any index problem; `memory review` prints the cause and the fix.
const indexNotice = "MEMORY.md is not being refreshed: run `claude-code memory review` for the cause and the fix"

// indexProblems is what stops MEMORY.md's refresh: what a dry plan finds now, else the last settle's failure.
func indexProblems(now time.Time) []string {
	if _, err := refreshNativeIndex(now, true, nil); err != nil {
		return []string{indexProblem(err)}
	}
	if last, err := os.ReadFile(indexProblemPath()); err == nil && len(last) > 0 {
		return []string{string(last)}
	}
	return nil
}

// indexProblem is err as one review row that starts like every index problem; an I/O error gets its own advice.
func indexProblem(err error) string {
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if errors.Is(err, errIndexProblem) {
		return msg
	}
	return fmt.Sprintf("%v: %s; fix the path it names (type, permissions, free space), then run `claude-code memory index`",
		errIndexProblem, msg)
}

func indexProblemPath() string { return filepath.Join(StateDir(), "index-problem-"+stateKey()) }

// recordIndexProblem keeps a failed settle's cause for `memory review` and the next session-start line.
func recordIndexProblem(err error) {
	if err == nil {
		if rerr := os.Remove(indexProblemPath()); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "memory index: clear the last failure:", rerr)
		}
		return
	}
	row := indexProblem(err) + " (last settle)"
	if werr := errors.Join(paths.AtomicWrite(indexProblemPath(), []byte(row), 0o600), cacheDoctorNotice([]string{row})); werr != nil {
		fmt.Fprintln(os.Stderr, "memory index: record the failure:", werr)
	}
}

// updateProblems surfaces receipts needing a human: a failed update, or a pending one past the grace window.
func updateProblems(now time.Time) []string {
	var problems []string
	for _, rc := range readReceipts() {
		id, r := rc.id, rc.r
		// A lost "pending" receipt is a bug to chase whenever it happened; a plain failure ages out after a week.
		if !r.needsAttention(now) || (r.Status == updateFailed && now.Sub(r.Time) > 7*24*time.Hour) {
			continue
		}
		if LiveTranscript("", id) == "" { // a transcript that no longer exists can never be retried
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

// journalProblems names each bank journal left on disk past the grace window, so a stuck one is seen.
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
	Blocked     int // the gate refused every write, twice
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
		s.tally(r)
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

// tally counts one receipt by status.
func (s *bankStats) tally(r UpdateResult) {
	switch r.Status {
	case updateOK:
		s.OK++
		if r.BankUpdated {
			s.BankUpdated++
		}
	case updateFailed:
		s.Failed++
	case updateBlocked:
		s.Blocked++
	case updatePending:
		s.Pending++
	}
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
	if terminal := s.OK + s.Failed + s.Blocked; terminal > 0 {
		fmt.Fprintf(&b, "  success rate: %d/%d ok (%.0f%%)\n", s.OK, terminal, 100*float64(s.OK)/float64(terminal))
	}
	if s.OK > 0 {
		fmt.Fprintf(&b, "  bank updated: %d/%d ok updates (%.0f%%)\n", s.BankUpdated, s.OK, 100*float64(s.BankUpdated)/float64(s.OK))
	}
	if s.Blocked > 0 {
		fmt.Fprintf(&b, "  not saved:    %d blocked by the permission gate\n", s.Blocked)
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

// timelineBank returns the newest-first receipt trail (up to limit) plus recent sessionHistory headings.
func timelineBank(limit int) (entries []timelineEntry, sessions []string, err error) {
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
	sessions, err = recentSessions(limit)
	return entries, sessions, err
}

func recentSessions(limit int) (sessions []string, err error) {
	seen := map[string]bool{}
	heads, err := entriesWithPrefix(Path("sessionHistory.md"), "## ")
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
	for _, p := range transcript.GlobIn(StateDir(), "update-result-*") {
		id := strings.TrimPrefix(filepath.Base(p), "update-result-")
		if r, ok := readUpdateResult(id); ok {
			out = append(out, receipt{id, r})
		}
	}
	return out
}

// candidatesRel is the dream-written candidate list; knowledge/ is inside the dream job's write scope.
const candidatesRel = "knowledge/feedback-candidates.md"

// candidate is one proposed harness change; unknown fields are rejected so the shape stays machine-checked.
type candidate struct {
	ID       string   `json:"id"`
	Evidence []string `json:"evidence"`
	Repeats  int      `json:"repeats"`
	Change   string   `json:"change"`
	Target   string   `json:"target"`
	Kind     string   `json:"kind"`
	Size     string   `json:"size"`
}

var (
	candidateIDRE    = regexp.MustCompile(`^fc-[a-z0-9][a-z0-9-]*$`)
	candidateKinds   = []string{"prose", "hook", "verb", "test"}
	candidateSizes   = []string{"small", "medium", "large"}
	candidateMaxText = 200
)

// candidateProblems validates the candidates file, one `<file>:<line>: <message>` row each; none when it is absent.
func candidateProblems() []string {
	data, err := os.ReadFile(filepath.Join(StoreDir(), filepath.FromSlash(candidatesRel)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []string{candidatesRel + ": " + err.Error()}
	}
	return validateCandidates(string(data))
}

// candidateError fails `memory review` when problems hold candidate rows; every other row stays advisory.
func candidateError(problems []string) error {
	n := 0
	for _, p := range problems {
		if strings.HasPrefix(p, candidatesRel+":") {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	return fmt.Errorf("%d problem(s) in %s", n, candidatesRel)
}

// validateCandidates checks the file's one ```jsonl block, one JSON record per line, and every line for secrets.
func validateCandidates(text string) []string {
	var problems []string
	add := func(n int, msg string) { problems = append(problems, fmt.Sprintf("%s:%d: %s", candidatesRel, n, msg)) }
	seen := map[string]int{}
	inBlock, blocks := false, 0
	for i, line := range strings.Split(text, "\n") {
		n, trimmed := i+1, strings.TrimSpace(line)
		if len(textutil.SecretSpans(line)) > 0 {
			add(n, "credential-shaped text; remove it")
		}
		switch {
		case !inBlock && trimmed == "```jsonl":
			inBlock, blocks = true, blocks+1
			if blocks > 1 {
				add(n, "second jsonl block; the file holds exactly one")
			}
		case inBlock && trimmed == "```":
			inBlock = false
		case inBlock && trimmed != "":
			for _, msg := range checkCandidateLine(trimmed, n, seen) {
				add(n, msg)
			}
		}
	}
	if inBlock {
		add(len(strings.Split(text, "\n")), "jsonl block is not closed")
	}
	if blocks == 0 {
		add(1, "no ```jsonl block")
	}
	return problems
}

// checkCandidateLine decodes one record strictly and returns its problems; seen maps ids to their first line.
func checkCandidateLine(line string, n int, seen map[string]int) []string {
	var c candidate
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return []string{"invalid record: " + err.Error()}
	}
	if dec.More() {
		return []string{"invalid record: more than one JSON value on the line"}
	}
	var out []string
	switch {
	case !candidateIDRE.MatchString(c.ID):
		out = append(out, "id must match fc-<slug> (lower-case letters, digits, hyphens)")
	case seen[c.ID] > 0:
		out = append(out, fmt.Sprintf("duplicate id %s (first at line %d)", c.ID, seen[c.ID]))
	default:
		seen[c.ID] = n
	}
	if !slices.Contains(candidateKinds, c.Kind) {
		out = append(out, "kind must be one of "+strings.Join(candidateKinds, ", "))
	}
	if !slices.Contains(candidateSizes, c.Size) {
		out = append(out, "size must be one of "+strings.Join(candidateSizes, ", "))
	}
	if !oneSentence(c.Change) {
		out = append(out, fmt.Sprintf("change must be one sentence of at most %d characters", candidateMaxText))
	}
	if msg := targetProblem(c.Target); msg != "" {
		out = append(out, msg)
	}
	return append(out, evidenceProblems(c)...)
}

// oneSentence is true for non-empty text under the cap with no sentence break inside it.
func oneSentence(s string) bool {
	s = strings.TrimRight(strings.TrimSpace(s), ".!?")
	return s != "" && len(s) <= candidateMaxText && !strings.ContainsAny(s, "\n") &&
		!strings.Contains(s, ". ") && !strings.Contains(s, "? ") && !strings.Contains(s, "! ")
}

// targetProblem accepts "unknown" or a path inside the repository (the config dir) or the store.
func targetProblem(target string) string {
	if target == "" {
		return `target is missing (a path, or "unknown")`
	}
	if target == "unknown" {
		return ""
	}
	p := target
	if !filepath.IsAbs(p) && !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, `\`) {
		p = filepath.Join(paths.ConfigDir(), p)
	}
	for _, root := range []string{paths.ConfigDir(), StoreDir()} {
		if rel, err := filepath.Rel(root, filepath.Clean(p)); err == nil && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return ""
		}
	}
	return "target " + target + " is outside the repository and the store"
}

// evidenceProblems needs existing feedback/ or notes/ files: two distinct, or one plus repeats of at least two.
func evidenceProblems(c candidate) []string {
	var out []string
	distinct := map[string]bool{}
	for _, e := range c.Evidence {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(e)))
		dir, _, _ := strings.Cut(clean, "/")
		if clean != e || (dir != "feedback" && dir != "notes") || !strings.HasSuffix(e, ".md") {
			out = append(out, "evidence "+e+" must be feedback/<name>.md or notes/<name>.md")
			continue
		}
		if fi, err := os.Stat(filepath.Join(StoreDir(), filepath.FromSlash(e))); err != nil || !fi.Mode().IsRegular() {
			out = append(out, "evidence "+e+" does not exist in the store")
			continue
		}
		distinct[e] = true
	}
	switch {
	case len(c.Evidence) == 0:
		out = append(out, "evidence is missing")
	case c.Repeats < 0:
		out = append(out, "repeats must not be negative")
	case len(out) == 0 && len(distinct) < 2 && (len(distinct) != 1 || c.Repeats < 2):
		out = append(out, "evidence needs two distinct files, or one file with repeats of at least 2")
	}
	return out
}
