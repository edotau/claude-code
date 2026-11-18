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

	"github.com/edotau/claude-code/internal/paths"
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
	data, err := os.ReadFile(filepath.Join(StoreDir(), filepath.FromSlash(candidatesRel)))
	if err != nil {
		return nil
	}
	var out []candidate
	blockRecords(strings.Split(string(data), "\n"), func(_ int, t string) {
		if c, msg := decodeCandidate(t); msg == "" {
			out = append(out, c)
		}
	})
	return out
}

// openCandidates is every record not yet enforced, most repeated first.
func openCandidates() []candidate {
	open := slices.DeleteFunc(readCandidates(), func(c candidate) bool { return c.Status == statusEnforced })
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
func writeOpenCandidates(w io.Writer) {
	open := openCandidates()
	if len(open) == 0 {
		return
	}
	fmt.Fprintf(w, "open feedback candidates (%d, most repeated first; `memory review --candidate <id>`):\n", len(open))
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
		return errors.New("only an approved candidate can be marked enforced (--approve first)")
	}
	c.Status, c.EnforcedBy = statusEnforced, ref
	return nil
}

// rewriteCandidate edits one record in place under the bank lock; the file's other lines are kept byte for byte.
func rewriteCandidate(id, verb string, edit func(*candidate) error) error {
	return withFlatLock(BankLockWait, func() error {
		data, err := os.ReadFile(filepath.Join(StoreDir(), filepath.FromSlash(candidatesRel)))
		if err != nil {
			return errors.New("no candidates file in the store")
		}
		text, err := replaceCandidate(string(data), id, edit)
		if err != nil {
			return err
		}
		if n := len(validateCandidates(text)); n > 0 {
			return fmt.Errorf("the candidates file would have %d problem(s); run `claude-code memory review`", n)
		}
		_, err = commitChangesLocked("memory: candidate "+id+" "+verb, []Change{{Rel: candidatesRel, Data: []byte(text)}})
		return err
	})
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
		return errors.New("--enforced wants a commit sha (7-40 hex digits) or file:line")
	case strings.Contains(ref, ":"):
		return fileLineProblem(ref)
	}
	dir := paths.ConfigDir()
	if _, err := runGit([]string{"-C", dir, "rev-parse", "--verify", "--quiet", ref + "^{commit}"}); err != nil {
		return errors.New("--enforced: no such commit in the repository")
	}
	for _, trunk := range []string{"main", "master"} {
		if _, err := runGit([]string{"-C", dir, "merge-base", "--is-ancestor", ref, trunk}); err == nil {
			return nil
		}
	}
	return errors.New("--enforced: that commit is not reachable from main")
}

func fileLineProblem(ref string) error {
	i := strings.LastIndex(ref, ":")
	file, line := ref[:i], ref[i+1:]
	full := file
	if !filepath.IsAbs(full) {
		full = filepath.Join(paths.ConfigDir(), file)
	}
	if !paths.InsideDir(full, paths.ConfigDir()) {
		return errors.New("--enforced: the file is outside the repository")
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return errors.New("--enforced: the file does not exist")
	}
	n, _ := strconv.Atoi(line)
	if n > bytes.Count(data, []byte("\n"))+1 {
		return errors.New("--enforced: the file has no such line")
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
		fmt.Fprintf(w, "after the owner's yes: claude-code memory review --candidate %s --approve\n", c.ID)
	}
	fmt.Fprintf(w, "launch:   %s\nwhen it lands: claude-code memory review --candidate %s --enforced <sha|file:line>\n",
		launchLine(c), c.ID)
}

// launchLine is the one-line edit for a small prose change, else the code-engineer call the main session runs.
func launchLine(c candidate) string {
	if c.Kind == "prose" && c.Size == "small" {
		return "a one-line edit in " + c.Target + ": do it in the main session, no workflow"
	}
	kind := "feature"
	if c.Kind == "prose" {
		kind = "refactor"
	}
	args := map[string]string{"cwd": paths.ConfigDir(), "kind": kind, "task": c.Change + " Target: " + c.Target + "."}
	if g := candidateGate(c.Target); g != "" {
		args["gate"] = g
	}
	var b bytes.Buffer
	_ = jsonEncoder(&b).Encode(args)
	return `Workflow({scriptPath: "~/.claude/workflows/code-engineer.js", args: ` + strings.TrimSpace(b.String()) + `})`
}

// candidateGate scopes the gate to the target's Go package, "" when the target is not Go code in this repository.
func candidateGate(target string) string {
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
	return "go build ./... && go vet ./" + dir + "/... && go test ./" + dir + "/..."
}
