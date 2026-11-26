package memory

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// No status means proposed; the owner's yes makes it approved, a landed change enforced.
const (
	statusApproved = "approved"
	statusEnforced = "enforced"
)

var (
	candidateStatuses = []string{"", statusApproved, statusEnforced}
	enforcedByRE      = regexp.MustCompile(`^(?:[0-9a-f]{7,40}|[^\s:]+:[1-9][0-9]*)$`)
	errNoCandidate    = errors.New("no candidate with that id")
)

// statusProblems checks status and enforced_by: an enforced record needs its pointer, no other has one.
func statusProblems(c candidate) []string {
	var out []string
	if !slices.Contains(candidateStatuses, c.Status) {
		out = append(out, "status must be absent, approved or enforced")
	}
	switch {
	case c.EnforcedBy != "" && c.Status != statusEnforced:
		out = append(out, "enforced_by belongs only on an enforced record")
	case c.Status == statusEnforced && !enforcedByRE.MatchString(c.EnforcedBy):
		out = append(out, "an enforced record needs enforced_by: a commit sha or file:line")
	}
	return out
}

// blockRecords calls fn with the index and text of each line inside the one jsonl block.
func blockRecords(lines []string, fn func(i int, t string)) {
	inBlock := false
	for i, line := range lines {
		switch t := strings.TrimSpace(line); {
		case !inBlock && t == "```jsonl":
			inBlock = true
		case inBlock && t == "```":
			inBlock = false
		case inBlock && t != "":
			fn(i, t)
		}
	}
}

// jsonEncoder writes compact JSON without HTML escaping, so `&&` and `<` stay readable.
func jsonEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc
}

// readCandidates returns the file's valid records; none when it is absent or unreadable.
func readCandidates() []candidate {
	out, _ := readCandidatesErr()
	return out
}

// readCandidatesErr is readCandidates that reports a read failure; a missing file is no error, just no records.
func readCandidatesErr() ([]candidate, error) {
	data, _, err := readCandidatesLocked()
	if err != nil {
		return nil, err
	}
	var out []candidate
	blockRecords(strings.Split(string(data), "\n"), func(_ int, t string) {
		if c, msg := decodeCandidate(t); msg == "" {
			out = append(out, c)
		}
	})
	return out, nil
}

// openCandidates is every record not yet enforced, most repeated first.
func openCandidates() []candidate { return openOf(readCandidates()) }

// openOf filters records to the open ones, most repeated first.
func openOf(all []candidate) []candidate {
	open := slices.DeleteFunc(all, func(c candidate) bool { return c.Status == statusEnforced })
	slices.SortStableFunc(open, func(a, b candidate) int {
		if a.Repeats != b.Repeats {
			return b.Repeats - a.Repeats
		}
		return strings.Compare(a.ID, b.ID)
	})
	return open
}

// openCandidatesNotice is the one session-start line counting open candidates, "" with none.
func openCandidatesNotice() string {
	n := len(openCandidates())
	switch n {
	case 0:
		return ""
	case 1:
		return "1 open feedback candidate"
	}
	return strconv.Itoa(n) + " open feedback candidates"
}

// writeOpenCandidates prints the open records as one compact block.
func writeOpenCandidates(w io.Writer) { writeOpen(w, openCandidates()) }

func writeOpen(w io.Writer, open []candidate) {
	if len(open) == 0 {
		return
	}
	fmt.Fprintf(w, "open feedback candidates (%d, most repeated first; `memory feedback show <id>`):\n", len(open))
	for _, c := range open {
		mark := ""
		if c.Status == statusApproved {
			mark = "  [approved]"
		}
		fmt.Fprintf(w, "  %-34s %-5s %-6s x%d  %s%s\n", c.ID, c.Kind, c.Size, c.Repeats, c.Target, mark)
	}
}

// candidateAction shows one record with its launch line, or approves or closes it for the owner's main session.
func candidateAction(w io.Writer, o ReviewOptions) error {
	if o.Propose != "" {
		if o.Candidate != "" || o.Approve || o.Enforced != "" {
			return errors.New("add stands alone")
		}
		return proposeCandidate(w, o.Propose)
	}
	if o.Approve || o.Enforced != "" {
		if isDreamChild() {
			return errors.New("approving and closing a candidate is the owner's, never a memory job's")
		}
		return closeCandidate(w, o)
	}
	for _, c := range readCandidates() {
		if c.ID == o.Candidate {
			showCandidate(w, c)
			return nil
		}
	}
	return errNoCandidate
}

func closeCandidate(w io.Writer, o ReviewOptions) error {
	edit, verb := approveEdit, "approved"
	if o.Enforced != "" {
		if err := enforcedRefProblem(o.Enforced); err != nil {
			return err
		}
		edit, verb = func(c *candidate) error { return enforceEdit(c, o.Enforced) }, "enforced"
	}
	if err := rewriteCandidate(o.Candidate, verb, edit); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: %s\n", o.Candidate, verb)
	return nil
}

func approveEdit(c *candidate) error {
	if c.Status != "" {
		return errors.New("only a proposed candidate can be approved")
	}
	c.Status = statusApproved
	return nil
}

func enforceEdit(c *candidate, ref string) error {
	if c.Status != statusApproved {
		return errors.New("only an approved candidate can be marked enforced (approve it first)")
	}
	c.Status, c.EnforcedBy = statusEnforced, ref
	return nil
}

// rewriteCandidate edits one record in place under the bank lock; the file's other lines are kept byte for byte.
func rewriteCandidate(id, verb string, edit func(*candidate) error) error {
	return withBankLock(BankLockWait, func() error {
		data, pre, err := readCandidatesLocked()
		if err != nil {
			return err
		}
		if pre == "" {
			return errors.New("no candidates file in the store")
		}
		text, err := replaceCandidate(string(data), id, edit)
		if err != nil {
			return err
		}
		if n := len(validateCandidates(text)); n > 0 {
			return fmt.Errorf("the candidates file would have %d problem(s); run `claude-code memory review`", n)
		}
		_, err = commitChangesLocked("memory: candidate "+id+" "+verb,
			[]storeChange{{Rel: candidatesRel, Data: []byte(text), Planned: true, PreSHA256: pre}})
		return err
	})
}

// readCandidatesLocked reads the file and its hash ("" when absent).
func readCandidatesLocked() (data []byte, pre string, err error) {
	data, err = os.ReadFile(filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(candidatesRel)))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, "", nil
	case err != nil:
		return nil, "", err
	}
	return data, textutil.SHA256Hex(data), nil
}

// replaceCandidate returns text with id's record rewritten by edit and every other byte unchanged.
func replaceCandidate(text, id string, edit func(*candidate) error) (string, error) {
	lines := strings.Split(text, "\n")
	found := false
	var editErr error
	blockRecords(lines, func(i int, t string) {
		c, msg := decodeCandidate(t)
		if msg != "" || c.ID != id || editErr != nil {
			return
		}
		if editErr = edit(&c); editErr != nil {
			return
		}
		var b bytes.Buffer
		_ = jsonEncoder(&b).Encode(c) // a plain struct always encodes
		lines[i], found = strings.TrimRight(b.String(), "\n"), true
	})
	switch {
	case editErr != nil:
		return "", editErr
	case !found:
		return "", errNoCandidate
	}
	return strings.Join(lines, "\n"), nil
}

// enforcedRefProblem accepts a commit reachable from main or master, or an existing file:line in the repository.
func enforcedRefProblem(ref string) error {
	switch {
	case !enforcedByRE.MatchString(ref):
		return errors.New("close wants a commit sha (7-40 hex digits) or file:line")
	case strings.Contains(ref, ":"):
		return fileLineProblem(ref)
	}
	dir := paths.ConfigDir()
	if _, err := runGit([]string{"-C", dir, "rev-parse", "--verify", "--quiet", ref + "^{commit}"}); err != nil {
		return errors.New("close: no such commit in the repository")
	}
	for _, trunk := range []string{"main", "master"} {
		if _, err := runGit([]string{"-C", dir, "merge-base", "--is-ancestor", ref, trunk}); err == nil {
			return nil
		}
	}
	return errors.New("close: that commit is not reachable from main")
}

func fileLineProblem(ref string) error {
	i := strings.LastIndex(ref, ":")
	file, line := ref[:i], ref[i+1:]
	full := file
	if !filepath.IsAbs(full) {
		full = filepath.Join(paths.ConfigDir(), file)
	}
	if !paths.InsideDir(full, paths.ConfigDir()) {
		return errors.New("close: the file is outside the repository")
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return errors.New("close: the file does not exist")
	}
	n, _ := strconv.Atoi(line)
	if n > bytes.Count(data, []byte("\n"))+1 {
		return errors.New("close: the file has no such line")
	}
	return nil
}

// showCandidate prints one record and the launch the main session runs after the owner's yes.
func showCandidate(w io.Writer, c candidate) {
	status := cmp.Or(c.Status, "proposed")
	fmt.Fprintf(w, "%s  %s  %s  repeats %d  %s\n", c.ID, c.Kind, c.Size, c.Repeats, status)
	fmt.Fprintf(w, "change:   %s\ntarget:   %s\nevidence: %s\n", c.Change, c.Target, strings.Join(c.Evidence, ", "))
	switch c.Status {
	case statusEnforced:
		fmt.Fprintf(w, "enforced by %s: nothing to launch\n", c.EnforcedBy)
		return
	case "":
		fmt.Fprintf(w, "after the owner's yes: claude-code memory feedback approve %s\n", c.ID)
	}
	fmt.Fprintf(w, "launch:   %s\nwhen it lands: claude-code memory feedback close %s <sha|file:line>\n",
		launchLine(c), c.ID)
}

// launchLine is the one-line edit for a small prose change, else the agents ask command the main session runs.
func launchLine(c candidate) string {
	if c.Kind == "prose" && c.Size == "small" {
		return "a one-line edit in " + c.Target + ": do it in the main session, no dispatch"
	}
	args := map[string]string{"cwd": paths.ConfigDir(), "task": c.Change + " Target: " + c.Target + "."}
	if g := candidateGate(c.Target); g != "" {
		args["gate"] = g
	}
	var b bytes.Buffer
	_ = jsonEncoder(&b).Encode(args)
	return `claude-code agents ask --brief ` + strings.TrimSpace(b.String())
}

// candidateGate scopes the gate to the target's Go package, "" when the target is not Go code in this repository.
func candidateGate(target string) string {
	if targetProblem(target) != "" {
		return ""
	}
	rel := target
	if filepath.IsAbs(target) {
		r, err := filepath.Rel(paths.ConfigDir(), target)
		if err != nil {
			return ""
		}
		rel = r
	}
	rel = filepath.ToSlash(rel)
	if !strings.HasPrefix(rel, "internal/") {
		return ""
	}
	dir := rel
	if path.Ext(rel) != "" {
		dir = path.Dir(rel)
	}
	pkgs := textutil.QuoteSH("./" + dir + "/...")
	return "go build ./... && go vet " + pkgs + " && go test " + pkgs
}

// WriteCandidates prints the open candidates (a missing file is none, an unreadable one an error), then the ranking.
func WriteCandidates(w io.Writer, rank bool) error {
	all, err := readCandidatesErr()
	if err != nil {
		return fmt.Errorf("read %s: %w", candidatesRel, err)
	}
	open := openOf(all)
	if len(open) == 0 {
		fmt.Fprintln(w, "no open feedback candidates")
	}
	writeOpen(w, open)
	if rank {
		fmt.Fprintln(w)
		fmt.Fprintln(w, assets.MemoryReviewProcedure())
	}
	return nil
}

func (o ReviewOptions) candidateVerb() bool { return o.Candidate != "" || o.Propose != "" }

// proposeCandidate appends one proposed record under the bank lock; Go owns the file shape.
func proposeCandidate(w io.Writer, record string) error {
	c, msg := decodeCandidate(strings.TrimSpace(record))
	switch {
	case msg != "":
		return errors.New(msg)
	case c.Status != "" || c.EnforcedBy != "":
		return errors.New("a proposal carries no status or enforced_by; the owner's approval sets them")
	}
	var line bytes.Buffer
	_ = jsonEncoder(&line).Encode(c) // a plain struct always encodes
	err := withBankLock(BankLockWait, func() error {
		data, pre, err := readCandidatesLocked()
		if err != nil {
			return err
		}
		text := "# Feedback candidates\n\n```jsonl\n```\n"
		if pre != "" {
			text = string(data)
		}
		text, err = appendCandidateLine(text, strings.TrimRight(line.String(), "\n"))
		if err != nil {
			return err
		}
		if p := validateCandidates(text); len(p) > 0 {
			return errors.New(p[0])
		}
		_, err = commitChangesLocked("memory: candidate "+c.ID+" proposed",
			[]storeChange{{Rel: candidatesRel, Data: []byte(text), Planned: true, PreSHA256: pre}})
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: proposed\n", c.ID)
	return nil
}

// appendCandidateLine inserts line before the closing fence of the one jsonl block.
func appendCandidateLine(text, line string) (string, error) {
	lines := strings.Split(text, "\n")
	open := false
	for i, l := range lines {
		switch t := strings.TrimSpace(l); {
		case !open && t == "```jsonl":
			open = true
		case open && t == "```":
			return strings.Join(slices.Insert(lines, i, line), "\n"), nil
		}
	}
	return "", errors.New("the candidates file has no closed jsonl block; run `claude-code memory review`")
}

// maxTestProposals caps techdebt/tests.jsonl; the oldest lines fall off first.
const maxTestProposals = 40

// watermarkSlack re-reads the last minutes of reports (a rename keeps its mtime); the dedupe makes that harmless.
const watermarkSlack = 5 * time.Minute

// testProposal is one test an agent proposed but did not write.
type testProposal struct {
	Pkg     string `json:"pkg"`
	Case    string `json:"case"`
	Reason  string `json:"reason"`
	Runtime string `json:"runtime,omitempty"`
	Source  string `json:"source,omitempty"`
}

var fenceRE = regexp.MustCompile("^(`{3,})[ \t]*([^`\\s]*)[^`]*$")

// proposalBlocks returns the body lines of each top-level proposed_tests fence; a fence nested in another is skipped.
func proposalBlocks(report string) (blocks [][]string) {
	var open string
	var isProposal bool
	var cur []string
	for line := range strings.SplitSeq(report, "\n") {
		line = strings.TrimRight(line, "\r")
		m := fenceRE.FindStringSubmatch(line)
		switch {
		case open == "" && m != nil:
			open, isProposal, cur = m[1], m[2] == "proposed_tests", nil
		case open != "" && m != nil && m[2] == "" && len(m[1]) >= len(open):
			if isProposal {
				blocks = append(blocks, cur)
			}
			open = ""
		case open != "":
			cur = append(cur, line)
		}
	}
	return blocks
}

func (p testProposal) key() string {
	return strings.TrimSpace(p.Pkg) + "\x00" + strings.TrimSpace(p.Case)
}

func parseProposalLine(line string) (testProposal, error) {
	var p testProposal
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return p, errors.New("trailing content after the object")
	}
	if strings.TrimSpace(p.Pkg) == "" || strings.TrimSpace(p.Case) == "" || strings.TrimSpace(p.Reason) == "" {
		return p, errors.New("pkg, case and reason are required")
	}
	return p, nil
}

// parseTestProposals reads the ```proposed_tests fence of a report: one JSON object per line, source filled in.
func parseTestProposals(report, source string) (out []testProposal, bad error) {
	for _, block := range proposalBlocks(report) {
		for _, line := range block {
			if strings.TrimSpace(line) == "" {
				continue
			}
			p, err := parseProposalLine(line)
			if err != nil {
				bad = errors.Join(bad, fmt.Errorf("proposed_tests line %q: %w", line, err))
				continue
			}
			p.Source = source
			out = append(out, p)
		}
	}
	return out, bad
}

func mergeProposals(add []testProposal, old []byte) ([]testProposal, error) {
	seen := map[string]bool{}
	var kept []testProposal
	push := func(p testProposal) {
		if k := p.key(); !seen[k] && len(kept) < maxTestProposals {
			seen[k] = true
			kept = append(kept, p)
		}
	}
	for _, p := range add {
		push(p)
	}
	for line := range bytes.SplitSeq(old, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var p testProposal
		if err := json.Unmarshal(line, &p); err != nil {
			return nil, fmt.Errorf("existing proposal line %q: %w", line, err)
		}
		push(p)
	}
	return kept, nil
}

// appendTestProposals puts add first in the file (newest first), drops repeats of pkg+case, keeps 40 lines.
func appendTestProposals(file string, add []testProposal) (n int, err error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return 0, err
	}
	// the lock lives in the state folder, never beside the file in the store
	err = paths.WithFileLock(filepath.Join(paths.MemoryStateDir(), "tests.jsonl"), func() error {
		n, err = appendLocked(file, add)
		return err
	})
	return n, err
}

func appendLocked(file string, add []testProposal) (int, error) {
	old, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	kept, err := mergeProposals(add, old)
	if err != nil {
		return 0, err
	}
	var buf bytes.Buffer
	for _, p := range kept {
		b, err := json.Marshal(p)
		if err != nil {
			return 0, err
		}
		buf.Write(append(b, '\n'))
	}
	return len(kept), paths.AtomicWrite(file, buf.Bytes(), 0o600)
}

// testProposalsFile is the Go-kept list under the store; techdebt/tests.md stays the owner's, read only.
func testProposalsFile() string { return paths.MemorySub("techdebt", "tests.jsonl") }

// newReports lists the reports modified after since, newest first.
func newReports(since time.Time) ([]string, error) {
	ents, err := os.ReadDir(paths.MemorySubDir("reports"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	mod := map[string]time.Time{}
	var names []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		fi, err := e.Info()
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil && fi.ModTime().After(since) {
			mod[e.Name()] = fi.ModTime()
			names = append(names, e.Name())
		}
	}
	slices.SortFunc(names, func(a, b string) int { return mod[b].Compare(mod[a]) })
	return names, nil
}

func proposalWatermarkFile() string { return paths.MemorySub("techdebt", "tests.watermark") }

// proposalWatermark is the newest report time already ingested; zero when none (every report is new).
func proposalWatermark() time.Time {
	b, err := os.ReadFile(proposalWatermarkFile())
	if err != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	return t
}

// ingestTestProposals keeps new reports' proposals, then moves the watermark to the pre-scan cutoff.
func ingestTestProposals(out io.Writer) error {
	cutoff := time.Now()
	names, err := newReports(proposalWatermark())
	if err != nil {
		return err
	}
	var found []testProposal
	for _, name := range names {
		body, err := os.ReadFile(paths.MemorySub("reports", name))
		if err != nil {
			return err
		}
		got, perr := parseTestProposals(string(body), "reports/"+name)
		if perr != nil {
			fmt.Fprintf(out, "memory dream: reports/%s: %v\n", name, perr)
		}
		found = append(found, got...)
	}
	if len(found) > 0 {
		n, err := appendTestProposals(testProposalsFile(), found)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "memory dream: %d proposed test(s) kept\n", n)
	}
	if err := os.MkdirAll(filepath.Dir(proposalWatermarkFile()), 0o700); err != nil {
		return err
	}
	return paths.AtomicWrite(proposalWatermarkFile(), []byte(cutoff.Add(-watermarkSlack).Format(time.RFC3339Nano)+"\n"), 0o600)
}
