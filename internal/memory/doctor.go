// Bank health: per-file cap roll-ups and the advisories `memory review` + SessionStart's cached notice read.
package memory

import (
	"bufio"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
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
	"unicode/utf8"

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
	if !isLostPinProblem(notice) {
		notice = textutil.TruncateRunes(notice, 200)
	}
	return " memory review: " + notice + "."
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
	if len(problems) == 0 {
		line = openCandidatesNotice()
	}
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

// indexProblems prefers live findings to a cached outcome.
func indexProblems(now time.Time) []string {
	rep, err := refreshNativeIndex(now, true, nil)
	if err != nil {
		return []string{indexProblem(err)}
	}
	if rep.LostPins != "" {
		return []string{rep.LostPins}
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

func isLostPinProblem(line string) bool {
	_, rest, found := strings.Cut(line, " ")
	return found && strings.HasPrefix(rest, lostPinLabel)
}

// Keep the advisory after replacing the baseline, without turning a successful refresh into a failure.
func finishIndexRefresh(rep indexReport, err error) (indexReport, error) {
	if err == nil && !rep.Left {
		recordIndexProblem(nil, rep.LostPins)
	}
	return rep, err
}

// Only a completed refresh can acknowledge a pin advisory; the settle defer records errors only.
func recordIndexProblem(err error, refreshed ...string) {
	var row string
	switch {
	case err != nil:
		row = indexProblem(err) + " (last settle)"
	case len(refreshed) == 0:
		return
	case refreshed[0] != "":
		row = refreshed[0]
	default:
		if rerr := os.Remove(indexProblemPath()); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "memory index: clear the last problem:", rerr)
		}
		return
	}
	if werr := errors.Join(paths.AtomicWrite(indexProblemPath(), []byte(row), 0o600), cacheDoctorNotice([]string{row})); werr != nil {
		fmt.Fprintln(os.Stderr, "memory index: record the problem:", werr)
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

const (
	signalDays   = 14
	signalBudget = 128 << 20
	signalLine   = 16 << 10
	signalTop    = 2
)

// Keep the telemetry projection local because hooks already depend on memory.
type signalRow struct {
	Module  string `json:"module"`
	Rule    string `json:"rule"`
	Verdict string `json:"verdict"`
	Outcome string `json:"outcome"`
}

type signalRule struct{ rule, module string }

type hookSignals struct {
	runs      map[string]int
	rules     map[string]map[signalRule]int
	totals    map[string]int
	dropped   int
	truncated bool
}

func writeHookSignals(w io.Writer, now time.Time) {
	dir := filepath.Join(paths.StateDir(), "hooks")
	s := hookSignals{
		runs: make(map[string]int), totals: make(map[string]int),
		rules: map[string]map[signalRule]int{"block": {}, "ask": {}, "refused": {}},
	}
	left := int64(signalBudget)
	for day := range signalDays {
		date := now.AddDate(0, 0, -day).Format("20060102")
		used, truncated := s.readFile(filepath.Join(dir, "telemetry-"+date+".jsonl"), left)
		left -= used
		if truncated {
			s.truncated = true
			break
		}
	}
	if len(s.runs) == 0 && s.dropped == 0 && !s.truncated {
		return
	}
	start := now.AddDate(0, 0, 1-signalDays).Format(time.DateOnly)
	s.report(w, start)
}

func (s *hookSignals) readFile(path string, budget int64) (used int64, truncated bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	limit := io.LimitedReader{R: f, N: budget}
	r := bufio.NewReaderSize(&limit, signalLine)
	for {
		line, oversized, err := textutil.ReadLineCapped(r, signalLine)
		if err != nil {
			break
		}
		if !oversized {
			s.add(line)
		}
	}
	return budget - limit.N, limit.N == 0 && info.Size() > budget
}

var signalName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,99}$`)

func (s *hookSignals) add(line []byte) {
	var row signalRow
	if jsonv2.Unmarshal(line, &row) != nil {
		return
	}
	if row.Outcome == "dropped" {
		s.dropped++
		return
	}
	if !signalName.MatchString(row.Module) {
		return
	}
	s.runs[row.Module]++
	switch row.Verdict {
	case "block", "ask", "refused", "held", "passed", "rewrite":
		s.totals[row.Verdict]++
	}
	if counts := s.rules[row.Verdict]; counts != nil && signalName.MatchString(row.Rule) {
		counts[signalRule{row.Rule, row.Module}]++
	}
}

func rankedSignals(counts map[signalRule]int) []signalRule {
	keys := make([]signalRule, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if counts[a] != counts[b] {
			return counts[a] > counts[b]
		}
		if a.rule != b.rule {
			return a.rule < b.rule
		}
		return a.module < b.module
	})
	return keys
}

func (s hookSignals) report(w io.Writer, start string) {
	fmt.Fprintf(w, "signals (since %s", start)
	if s.truncated {
		fmt.Fprintf(w, "; %d MiB read limit reached", signalBudget>>20)
	}
	fmt.Fprintln(w, "):")
	for _, verdict := range []string{"block", "ask"} {
		counts := s.rules[verdict]
		keys := rankedSignals(counts)
		for _, key := range keys[:min(signalTop, len(keys))] {
			fmt.Fprintf(w, "  %s: %s (%s) %d (%.1f%%)\n", verdict, key.rule, key.module,
				counts[key], 100*float64(counts[key])/float64(s.runs[key.module]))
		}
	}
	fmt.Fprintf(w, "  hand-backs: held %d, refused %d, passed %d", s.totals["held"], s.totals["refused"], s.totals["passed"])
	counts := s.rules["refused"]
	if keys := rankedSignals(counts); len(keys) > 0 {
		key := keys[0]
		fmt.Fprintf(w, "; top refused: %s (%s) %d", key.rule, key.module, counts[key])
	}
	fmt.Fprintf(w, "\n  rewrite: %d\n", s.totals["rewrite"])
	if s.dropped > 0 {
		fmt.Fprintf(w, "  dropped: >=%d (cap markers)\n", s.dropped)
	}
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
	// Status and EnforcedBy are absent until the owner approves the change and it lands.
	Status     string `json:"status,omitempty"`
	EnforcedBy string `json:"enforced_by,omitempty"`
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

// decodeCandidate parses exactly one UTF-8 JSON object with unique names; its errors never quote the input.
func decodeCandidate(line string) (candidate, string) {
	var c candidate
	if !utf8.ValidString(line) {
		return c, "invalid record: not valid UTF-8"
	}
	d := jsontext.NewDecoder(strings.NewReader(line))
	if _, err := d.ReadValue(); err != nil {
		if se, ok := errors.AsType[*jsontext.SyntacticError](err); ok && se.Err == jsontext.ErrDuplicateName {
			return c, "invalid record: duplicate key"
		}
		return c, "invalid record: not valid JSON"
	}
	if _, err := d.ReadValue(); err != io.EOF {
		return c, "invalid record: more than one JSON value on the line"
	}
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	err := dec.Decode(&c)
	if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		if te.Field == "" {
			return c, "invalid record: not a JSON object"
		}
		return c, "invalid record: field " + te.Field + " has the wrong type"
	}
	if err != nil {
		return c, "invalid record: unknown field (allowed: id, evidence, repeats, change, target, kind, size, status, enforced_by)"
	}
	return c, ""
}

// checkCandidateLine returns one record's problems, none quoting its text; seen maps ids to their first line.
func checkCandidateLine(line string, n int, seen map[string]int) []string {
	c, msg := decodeCandidate(line)
	if msg != "" {
		return []string{msg}
	}
	var out []string
	switch {
	case !candidateIDRE.MatchString(c.ID):
		out = append(out, "id must match fc-<slug> (lower-case letters, digits, hyphens)")
	case seen[c.ID] > 0:
		out = append(out, fmt.Sprintf("duplicate id (first at line %d)", seen[c.ID]))
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
	return append(append(out, statusProblems(c)...), evidenceProblems(c)...)
}

// oneSentence is true for non-empty text under the cap with no sentence break inside it.
func oneSentence(s string) bool {
	s = strings.TrimRight(strings.TrimSpace(s), ".!?")
	return s != "" && utf8.RuneCountInString(s) <= candidateMaxText && !strings.ContainsAny(s, "\n") &&
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
	if !plainPath(target) {
		return "target must use only letters, digits and / . _ - (no leading - or .. in an element)"
	}
	p := target
	if !filepath.IsAbs(p) && !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, `\`) {
		p = filepath.Join(paths.ConfigDir(), p)
	}
	if paths.InsideDir(p, paths.ConfigDir()) || paths.InsideDir(p, StoreDir()) {
		return ""
	}
	return "target is outside the repository and the store"
}

// plainPath is true for a path of letters, digits and / . _ - with no element that starts with - or is "..".
func plainPath(p string) bool {
	for _, r := range p {
		if r > 'z' || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-", r) {
			return false
		}
	}
	return !slices.ContainsFunc(strings.Split(p, "/"), func(e string) bool { return e == ".." || strings.HasPrefix(e, "-") })
}

// evidenceProblems needs real, unaliased feedback/ or notes/ files: two distinct, or one plus repeats of at least two.
func evidenceProblems(c candidate) []string {
	var out []string
	distinct := map[string]bool{}
	store := paths.Real(StoreDir())
	for i, e := range c.Evidence {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(e)))
		dir, _, _ := strings.Cut(clean, "/")
		if clean != e || (dir != "feedback" && dir != "notes") || !strings.HasSuffix(e, ".md") {
			out = append(out, fmt.Sprintf("evidence[%d] must be feedback/<name>.md or notes/<name>.md", i))
			continue
		}
		full := filepath.Join(StoreDir(), filepath.FromSlash(e))
		if fi, err := os.Lstat(full); err != nil || !fi.Mode().IsRegular() {
			out = append(out, fmt.Sprintf("evidence[%d] does not exist in the store as a regular file", i))
			continue
		}
		real := paths.Real(full)
		if !paths.SameFile(real, filepath.Join(store, filepath.FromSlash(e))) {
			out = append(out, fmt.Sprintf("evidence[%d] passes through a symlink", i))
			continue
		}
		distinct[paths.FoldCase(real)] = true
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
