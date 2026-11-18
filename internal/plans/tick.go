package plans

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

var boxRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(\s*[-*] \[)[ xX](\])`) })

// Commit is one landed commit: the sha recorded on ticks and the files it changed.
type Commit struct {
	Sha      string
	Files    []string
	Message  string
	mainHead bool
}

// tickResult is what one commit ticked or marked partly landed.
type tickResult struct {
	Sha             string
	Ticked, Partial []string
	Stale           []string
	Candidate       []string
}

// Tick advances only tasks named by the commit, recording coverage until complete.
func Tick(root, planPath, sha string, files []string) (ticked, partial []string, err error) {
	landed, err := commitsOnMain(context.Background(), root, []Commit{{Sha: sha}})
	if !landed {
		return nil, nil, err
	}
	results, err := tickCommits(root, planPath, []Commit{{Sha: sha, Files: files}}, false, nil)
	for _, r := range results {
		ticked, partial = r.Ticked, r.Partial
	}
	return ticked, partial, err
}

// tickCommits ticks per commit under one lock, a task completing once the range's union covers it; ok nil allows all.
func tickCommits(root, planPath string, commits []Commit, dryRun bool, ok func(Task) bool) (results []tickResult, err error) {
	recorded := recordedFiles(root)
	err = editPlan(planPath, func(text string) (string, error) {
		orig := text
		seen := map[string][]string{}
		for _, c := range commits {
			r := tickResult{Sha: strings.TrimSpace(c.Sha)}
			message, readErr := commitMessage(root, c)
			if readErr != nil {
				return "", readErr
			}
			named := func(t Task) bool { return namesTask(message, planPath, t.ID) && (ok == nil || ok(t)) }
			for _, t := range parseBytes(planPath, []byte(text)).Tasks {
				if named(t) {
					seen[t.ID] = append(seen[t.ID], c.Files...)
				}
			}

			c.Message = message
			r.Candidate = candidates(root, planPath, text, c, ok)
			text, r.Ticked, r.Partial, r.Stale = tickText(root, planPath, text, c.Sha, c.Files, seen, recorded, named)
			if len(r.Ticked)+len(r.Partial)+len(r.Stale)+len(r.Candidate) > 0 {
				results = append(results, r)
			}
		}
		if dryRun {
			return orig, nil
		}
		return text, nil
	})
	return results, err
}

// recordedFiles reads the files of a commit recorded in a partly landed note; stale means it left landing's history.
func recordedFiles(root string) func(sha, landing string) (files []string, stale bool) {
	type evidence struct {
		files []string
		stale bool
	}
	cache := map[string]evidence{}
	return func(sha, landing string) ([]string, bool) {
		if root == "" {
			return nil, false
		}
		key := sha + ">" + landing
		if e, ok := cache[key]; ok {
			return e.files, e.stale
		}
		ctx, cancel := context.WithTimeout(context.Background(), recordedTimeout)
		defer cancel()
		var e evidence
		_, err := proc.RunGitInherit(ctx, root, "merge-base", "--is-ancestor", sha, landing)
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 { // an amend or rebase left the object but not the history
			e.stale = true
		} else if e.files, err = CommitFiles(ctx, root, sha); err != nil {
			e.stale = true
		}
		cache[key] = e
		return e.files, e.stale
	}
}

// recordedTimeout bounds each git read of a recorded commit.
const recordedTimeout = 10 * time.Second

// tickText is Tick's pure edit: ticks on seen plus each task's recorded partial commits, notes partials from files.
func tickText(root, planPath, text, sha string, files []string, seen map[string][]string, recorded func(string, string) ([]string, bool), ok func(Task) bool) (out string, ticked, partial, stale []string) {
	lines := strings.Split(text, "\n")
	for _, t := range parseBytes(planPath, []byte(text)).Tasks {
		if !open(t) || len(t.Owns) == 0 || (ok != nil && !ok(t)) {
			continue
		}
		have := slices.Clone(seen[t.ID])
		var gone []string
		for _, s := range t.Partial {
			if recorded == nil || strings.EqualFold(s, sha) {
				continue
			}
			f, lost := recorded(s, sha)
			have = append(have, f...)
			if lost {
				gone = append(gone, s)
			}
		}
		all, _ := covers(root, t.Owns, have)
		_, any := covers(root, t.Owns, files)
		if id := cmp.Or(t.ID, t.Title); len(gone) > 0 && !all {
			lines = dropPartial(lines, t, gone)
			stale = append(stale, fmt.Sprintf("plans: earlier commit %s of %s is no longer in history; tick it by hand with `claude-code plan task %s --done`", strings.Join(gone, ", "), id, id))
		}
		switch {
		case all:
			lines = markDone(lines, t, sha)
			ticked = append(ticked, t.Title)
		case any && !hasSha(t.Partial, sha):
			lines = addPartial(lines, t, sha)
			partial = append(partial, t.Title)
		}
	}
	return strings.Join(lines, "\n"), ticked, partial, stale
}

// dropPartial removes shas from t's partly-landed note and deletes the note once it names none.
func dropPartial(lines []string, t Task, shas []string) []string {
	re := partialRe()
	for i := t.Start; i <= t.End; i++ {
		if !re.MatchString(lines[i]) {
			continue
		}
		lines[i] = strings.TrimRight(re.ReplaceAllStringFunc(lines[i], func(comment string) string {
			kept := slices.DeleteFunc(textutil.SplitCSV(re.FindStringSubmatch(comment)[1]), func(s string) bool { return hasSha(shas, s) })
			if len(kept) == 0 {
				return ""
			}
			return "<!-- partly landed: " + strings.Join(kept, ", ") + " -->"
		}), " \t")
	}
	return lines
}

// markDone ticks t's box, drops its claim and partial notes and appends sha (when given) as its landing commit.
func markDone(lines []string, t Task, sha string) []string {
	lines = removeNotes(lines, t)
	lines[t.Start] = boxRe().ReplaceAllString(lines[t.Start], "${1}x${2}")
	if sha != "" {
		lines[t.End] = appendSuffix(lines[t.End], " ("+sha+")")
	}
	return lines
}

// removeNotes drops every claim and partly-landed comment across t's line span.
func removeNotes(lines []string, t Task) []string {
	lines = removeClaims(lines, t)
	re := partialRe()
	for i := t.Start; i <= t.End; i++ {
		if re.MatchString(lines[i]) {
			lines[i] = strings.TrimRight(re.ReplaceAllString(lines[i], ""), " \t")
		}
	}
	return lines
}

// plansLock is the path of the lock every plan write holds.
func plansLock() string { return filepath.Join(paths.StateDir(), "plans.lock") }

// editPlan re-reads planPath under the plans lock, applies fn and writes back only when bytes changed.
func editPlan(planPath string, fn func(text string) (string, error)) error {
	return paths.WithFileLock(plansLock(), func() error {
		info, err := os.Stat(planPath)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(planPath)
		if err != nil {
			return err
		}
		out, err := fn(string(b))
		if err != nil || out == string(b) {
			return err
		}
		return writePlan(planPath, []byte(out), info.Mode().Perm())
	})
}

// editTask applies fn to taskID's task and plan lines under the plans lock.
func editTask(planPath, taskID string, fn func(Task, []string) []string) error {
	return editPlan(planPath, func(text string) (string, error) {
		t, ok := findTask(parseBytes(planPath, []byte(text)), taskID)
		if !ok {
			return "", fmt.Errorf("plans: no task %q in %s", taskID, planPath)
		}
		return strings.Join(fn(t, strings.Split(text, "\n")), "\n"), nil
	})
}

// WriteFile replaces a plan under the plans lock, so a mirror or save never interleaves with a tick or claim.
func WriteFile(dest string, data []byte) error {
	return paths.WithFileLock(plansLock(), func() error {
		mode := os.FileMode(0o600)
		info, err := os.Stat(dest)
		if err == nil {
			mode = info.Mode().Perm()
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return writePlan(dest, data, mode)
	})
}

// removeClaims drops every working:/finished: comment across t's line span.
func removeClaims(lines []string, t Task) []string {
	re := claimRe()
	for i := t.Start; i <= t.End; i++ {
		if re.MatchString(lines[i]) {
			lines[i] = strings.TrimRight(re.ReplaceAllString(lines[i], ""), " \t")
		}
	}
	return lines
}

// appendSuffix adds suffix to line after trimming its trailing whitespace.
func appendSuffix(line, suffix string) string {
	return strings.TrimRight(line, " \t") + suffix
}

// addPartial records sha on t's existing partly-landed comment, else appends a new one.
func addPartial(lines []string, t Task, sha string) []string {
	re := partialRe()
	for i := t.Start; i <= t.End; i++ {
		if !re.MatchString(lines[i]) {
			continue
		}
		lines[i] = re.ReplaceAllStringFunc(lines[i], func(comment string) string {
			shas := textutil.SplitCSV(re.FindStringSubmatch(comment)[1])
			if !hasSha(shas, sha) {
				shas = append(shas, sha)
			}
			return "<!-- partly landed: " + strings.Join(shas, ", ") + " -->"
		})
		return lines
	}
	lines[t.End] = appendSuffix(lines[t.End], " <!-- partly landed: "+sha+" -->")
	return lines
}

// hasSha reports whether shas already names sha, ignoring case.
func hasSha(shas []string, sha string) bool {
	return slices.ContainsFunc(shas, func(s string) bool { return strings.EqualFold(s, sha) })
}

// ErrClaimed is ClaimFree's error when another claim already holds the task.
var ErrClaimed = errors.New("already claimed")

// ClaimFree writes <!-- text --> at a task's last line unless it already carries a claim, decided under the lock.
func ClaimFree(planPath, taskID, text string) error {
	text = strings.TrimSpace(text)
	if strings.ContainsAny(text, "\r\n") {
		return errors.New("plans: claim text must be a single line")
	}
	if strings.Contains(text, "-->") {
		return fmt.Errorf("plans: claim text must not contain %q", "-->")
	}
	if !strings.HasPrefix(strings.ToLower(text), "working:") {
		return errors.New("plans: claim text must start with \"working:\"")
	}
	lost := false
	err := editTask(planPath, taskID, func(t Task, lines []string) []string {
		if t.Claim != "" {
			lost = t.Claim != text
			return lines
		}
		lines[t.End] = appendSuffix(lines[t.End], " <!-- "+text+" -->")
		return lines
	})
	if err == nil && lost {
		return fmt.Errorf("plans: task %q: %w", taskID, ErrClaimed)
	}
	return err
}

// ClearClaimOwned removes a task's claim only when a non-empty owner appears in its text as whole words, under lock.
func ClearClaimOwned(planPath, taskID, owner string) error {
	words := strings.Join(strings.Fields(owner), " ")
	return editTask(planPath, taskID, func(t Task, lines []string) []string {
		if words == "" || !strings.Contains(" "+strings.Join(strings.Fields(t.Claim), " ")+" ", " "+words+" ") {
			return lines
		}
		return removeClaims(lines, t)
	})
}

// titleHasPrefix reports whether title starts with prefix at an id boundary, so "3.1" does not match "3.10".
func titleHasPrefix(title, prefix string) bool {
	rest, ok := strings.CutPrefix(title, prefix)
	if !ok || rest == "" {
		return ok
	}
	r, size := utf8.DecodeRuneInString(rest)
	if r == '.' {
		next, _ := utf8.DecodeRuneInString(rest[size:])
		return !unicode.IsLetter(next) && !unicode.IsDigit(next)
	}
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// findTask resolves taskID to a task: an exact Task.ID first, then a Title prefix.
func findTask(p Plan, taskID string) (Task, bool) {
	for _, t := range p.Tasks {
		if t.ID != "" && t.ID == taskID {
			return t, true
		}
	}
	for _, t := range p.Tasks {
		if taskID != "" && titleHasPrefix(t.Title, taskID) {
			return t, true
		}
	}
	return Task{}, false
}

// TickRange ticks plan (default: every plan the range's files match) for each commit in rng; drafts are never ticked.
func TickRange(ctx context.Context, dir, worktree, rng, plan string, drafts []string, dryRun bool, w io.Writer) error {
	commits, err := rangeCommits(ctx, dir, rng)
	if err != nil {
		return err
	}
	landed, err := commitsOnMain(ctx, dir, commits)
	if !landed {
		return err
	}
	targets, allow, done, err := pickPlans(dir, worktree, plan, drafts, commits, w)
	if err != nil || done {
		return err
	}
	for _, target := range targets {
		results, err := tickCommits(dir, target, commits, dryRun, allow[target]) // a named plan has no allow: every task
		if err != nil {
			return err
		}
		base := strings.TrimSuffix(filepath.Base(target), ".md")
		if err := printTicked(w, results, base, dryRun); err != nil {
			return err
		}
		if len(results) == 0 {
			if _, err = fmt.Fprintf(w, "plans: %d commits, nothing to tick in %s\n", len(commits), base); err != nil {
				return err
			}
		}
	}
	return nil
}

// pickPlans resolves the plans to tick and their allowed tasks; done is true when none was chosen and printed why.
func pickPlans(dir, worktree, plan string, drafts []string, commits []Commit, w io.Writer) ([]string, map[string]func(Task) bool, bool, error) {
	if plan != "" {
		if abs, err := filepath.Abs(plan); err == nil && slices.Contains(drafts, abs) {
			return nil, nil, false, fmt.Errorf("%s mirrors an unapproved draft; approve it before ticking", plan)
		}
		return []string{plan}, nil, false, nil
	}
	targets, allow := tickTargets(dir, worktree, drafts, commits)
	var tied []string
	if len(targets) > 0 {
		return targets, allow, false, printTie(w, tied)
	}
	_, err := fmt.Fprintln(w, "plans: no plan owns these files; nothing to tick")
	return nil, nil, true, err
}

// printTie prints TieNote when plans tied.
func printTie(w io.Writer, tied []string) error {
	if n := TieNote(tied); n != "" {
		_, err := fmt.Fprintln(w, n)
		return err
	}
	return nil
}

// PlanTick is what one commit ticked or marked partly landed in one plan.
type PlanTick struct {
	Plan                   string
	Ticked, Partial, Stale []string
	Candidate              []string
}

// Notes are the report lines for sha: what ticked and what is partly landed in this plan.
func (r PlanTick) Notes(sha string) []string {
	base := strings.TrimSuffix(filepath.Base(r.Plan), ".md")
	var out []string
	if len(r.Ticked) > 0 {
		out = append(out, "plans: ticked "+strings.Join(r.Ticked, "; ")+" ("+sha[:min(len(sha), 7)]+") in "+base)
	}
	if len(r.Partial) > 0 {
		out = append(out, "plans: partly landed "+strings.Join(r.Partial, "; ")+" in "+base)
	}
	return append(append(out, r.Stale...), r.Candidate...)
}

// TieNote is the one line naming plans that own the same file and were all updated; "" when none tied.
func TieNote(tied []string) string {
	if len(tied) == 0 {
		return ""
	}
	return "plans: open tasks of " + baseNames(tied) + " own the same file; all updated"
}

// TickCommit advances named tasks and reports unnamed coverage without writing it.
func TickCommit(top, sha string, files []string) (out []PlanTick, tied []string, err error) {
	if WorktreeName(top) != "" {
		return nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordedTimeout)
	defer cancel()
	c, err := readCommit(ctx, top, sha)
	if err != nil {
		return nil, nil, err
	}
	if !c.mainHead {
		landed, err := onMain(ctx, top, sha)
		if err != nil || !landed {
			return nil, nil, err
		}
	}
	if files != nil {
		c.Files = files
	}
	wt := WorktreeName(top)
	targets, allow := tickTargets(top, wt, MirrorDests(), []Commit{c})
	var errs []error
	for _, p := range targets {
		results, terr := tickCommits(top, p, []Commit{c}, false, allow[p])
		errs = append(errs, terr)
		for _, r := range results {
			out = append(out, PlanTick{p, r.Ticked, r.Partial, r.Stale, r.Candidate})
		}
	}
	return out, tied, errors.Join(errs...)
}

// TaskPlans lists the non-skipped dated plans in home holding a task matching id for which want is true.
func TaskPlans(home string, skip []string, id string, want func(Task) bool) []string {
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range loadDatedPlans(entries, home, skip) {
		if t, ok := findTask(d.plan, id); ok && want(t) {
			out = append(out, d.path)
		}
	}
	return out
}

// MarkDone ticks taskID by hand (a task with no owns:), appending sha when given; a ticked task stays as it is.
func MarkDone(planPath, taskID, sha string) error {
	return editTask(planPath, taskID, func(t Task, lines []string) []string {
		if t.Done || t.Dropped {
			return lines
		}
		return markDone(lines, t, strings.TrimSpace(sha))
	})
}

// shaTail matches the landing sha of a ticked task, ahead of any trailing comments.
var shaTail = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:\s*\([0-9a-f]{7,40}\)|\s+(?:—|--|-)\s+[0-9a-f]{7,40})((?:\s*<!--.*?-->)*)\s*$`)
})

// struckLine matches one line struckLines wrote: its ~~ wrapping and, on the last line, the superseded reason.
var struckLine = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(\s*(?:[-*] \[ \] )?)~~(.*?)~~(?: — superseded: .*)?$`)
})

// Reopen unticks taskID and removes its landing sha, or restores a struck task; for a reverted commit or a bad strike.
func Reopen(planPath, taskID string) error {
	return editTask(planPath, taskID, func(t Task, lines []string) []string {
		if t.Dropped {
			span := slices.DeleteFunc(slices.Clone(lines[t.Start:t.End+1]), func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "— superseded:") })
			for i, l := range span {
				span[i] = struckLine().ReplaceAllString(l, "${1}${2}")
			}
			return slices.Concat(lines[:t.Start], span, lines[t.End+1:])
		}
		if !t.Done {
			return lines
		}
		lines[t.Start] = boxRe().ReplaceAllString(lines[t.Start], "${1} ${2}")
		lines[t.End] = shaTail().ReplaceAllString(lines[t.End], "$1")
		return lines
	})
}

// strikeWidth is the column cap of a struck task's lines.
const strikeWidth = 120

// Strike wraps taskID's text in ~~…~~, one wrapped line each, and appends " — superseded: <reason>".
func Strike(planPath, taskID, reason string) error {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		return errors.New("plans: a strike needs a reason")
	}
	ticked := false
	err := editTask(planPath, taskID, func(t Task, lines []string) []string {
		if t.Dropped {
			return lines
		}
		if t.Done {
			ticked = true
			return lines
		}
		lines = removeNotes(lines, t)
		return slices.Concat(lines[:t.Start], struckLines(lines[t.Start:t.End+1], reason), lines[t.End+1:])
	})
	if err == nil && ticked {
		return fmt.Errorf("plans: task %q is ticked; reopen it before striking", taskID)
	}
	return err
}

// struckLines re-flows a task's lines into ~~-wrapped lines of at most strikeWidth columns, ending in the reason.
func struckLines(task []string, reason string) []string {
	m := checkLine().FindStringSubmatch(task[0])
	head := m[1] + "- [ ] "
	if strings.HasPrefix(strings.TrimSpace(task[0]), "*") {
		head = m[1] + "* [ ] "
	}
	pad := strings.Repeat(" ", len(m[1])+2)
	words := strings.Fields(m[3])
	for _, l := range task[1:] {
		words = append(words, strings.Fields(l)...)
	}
	var out []string
	prefix, cur := head, []string{}
	flush := func() {
		out = append(out, prefix+"~~"+strings.Join(cur, " ")+"~~")
		prefix, cur = pad, nil
	}
	for _, w := range words {
		if len(cur) > 0 && utf8.RuneCountInString(prefix)+utf8.RuneCountInString(strings.Join(cur, " "))+utf8.RuneCountInString(w)+5 > strikeWidth {
			flush()
		}
		cur = append(cur, w)
	}
	flush()
	tail := " — superseded: " + reason
	if last := out[len(out)-1]; utf8.RuneCountInString(last)+utf8.RuneCountInString(tail) <= strikeWidth {
		out[len(out)-1] = last + tail
	} else {
		out = append(out, pad+strings.TrimPrefix(tail, " "))
	}
	return out
}

// printTicked writes one line per commit that ticked or partly landed tasks in base, returning the first write error.
func printTicked(w io.Writer, results []tickResult, base string, dryRun bool) error {
	verb := "ticked"
	if dryRun {
		verb = "would tick"
	}
	for _, r := range results {
		if len(r.Ticked) > 0 {
			if _, err := fmt.Fprintf(w, "plans: %s %s (%s) in %s\n", verb, strings.Join(r.Ticked, "; "), r.Sha, base); err != nil {
				return err
			}
		}
		if len(r.Partial) > 0 {
			if _, err := fmt.Fprintf(w, "plans: partly landed %s (%s) in %s\n", strings.Join(r.Partial, "; "), r.Sha, base); err != nil {
				return err
			}
		}
		for _, line := range append(slices.Clone(r.Stale), r.Candidate...) {
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
	}
	return nil
}

// baseNames joins the plan file names without their extension.
func baseNames(ps []string) string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = strings.TrimSuffix(filepath.Base(p), ".md")
	}
	return strings.Join(names, ", ")
}

// ScratchDir holds one <draft-basename>.dest record per mirrored draft, pointing at its dated file.
func ScratchDir() string { return filepath.Join(paths.StateDir(), "plans-scratch") }

// DestRecordPath is the .dest record for the draft file with this base name.
func DestRecordPath(draftBase string) string {
	return filepath.Join(ScratchDir(), draftBase+".dest")
}

// ReadRecordedDest returns the dated file a draft was mirrored to; an unreadable, emptied or malformed record is none.
func ReadRecordedDest(draftBase string) (string, bool) {
	dest, err := readDestRecord(draftBase)
	return dest, err == nil && dest != ""
}

// errMalformedDest marks a record that is neither emptied nor one absolute .md path.
var errMalformedDest = errors.New("malformed .dest record")

// readDestRecord is the destination a record names, "" when it is emptied (retired); errMalformedDest otherwise bad.
func readDestRecord(draftBase string) (string, error) {
	data, err := os.ReadFile(DestRecordPath(draftBase))
	if err != nil {
		return "", err
	}
	dest := strings.TrimSpace(string(data))
	if dest == "" {
		return "", nil
	}
	if !filepath.IsAbs(dest) || !strings.HasSuffix(dest, ".md") || strings.ContainsAny(dest, "\r\n") {
		return "", errMalformedDest
	}
	return dest, nil
}

// MirrorDests lists every unapproved draft's dated mirror, so no tick ever lands on a plan still being drafted.
func MirrorDests() []string {
	ents, err := os.ReadDir(ScratchDir()) // not Glob: a metacharacter in the config path must not empty the list
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return allDatedPlans()
	}
	var out []string
	for _, e := range ents {
		base, ok := strings.CutSuffix(e.Name(), ".dest")
		if !ok {
			continue
		}
		dest, err := readDestRecord(base)
		if err != nil {
			return allDatedPlans() // an unreadable or malformed record hides its mirror, so exclude every plan (fail closed)
		}
		if dest != "" { // an emptied record is retired
			out = append(out, dest)
		}
	}
	return out
}

// allDatedPlans lists every plan file under the plans home, the fail-closed exclusion when a record is unreadable.
func allDatedPlans() []string {
	var out []string
	for _, d := range Dirs(true) {
		ents, _ := os.ReadDir(d) // an unreadable dir lists nothing
		for _, e := range ents {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				out = append(out, filepath.Join(d, e.Name()))
			}
		}
	}
	return out
}

// RecordDest records dest as the dated mirror of the draft with this base name.
func RecordDest(draftBase, dest string) error {
	if err := os.MkdirAll(ScratchDir(), 0o700); err != nil {
		return err
	}
	return paths.AtomicWrite(DestRecordPath(draftBase), []byte(dest), 0o600)
}

// writePlan is the package's one write site (the schemas registry keys on it): atomic replace keeping dest's mode.
func writePlan(dest string, data []byte, mode os.FileMode) error {
	return paths.AtomicWrite(dest, data, mode)
}

func commitMessage(root string, c Commit) (string, error) {
	if c.Message != "" {
		return c.Message, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordedTimeout)
	defer cancel()
	return proc.RunGitInherit(ctx, root, "show", "-s", "--format=%B", c.Sha, "--")
}

func namesTask(message, plan, id string) bool {
	want := "Task: " + strings.TrimSuffix(filepath.Base(plan), ".md") + " " + id
	return id != "" && slices.Contains(strings.Split(strings.TrimSpace(message), "\n"), want)
}

func candidates(root, plan, text string, c Commit, ok func(Task) bool) []string {
	var out []string
	for _, t := range parseBytes(plan, []byte(text)).Tasks {
		if !open(t) || (ok != nil && !ok(t)) || namesTask(c.Message, plan, t.ID) {
			continue
		}
		if _, any := covers(root, t.Owns, c.Files); any {
			out = append(out, fmt.Sprintf("plans: candidate %s %s in %s (not ticked); tick with claude-code plan task %q --done --sha %s --plan %s", t.ID, strings.TrimSpace(strings.TrimPrefix(t.Title, t.ID)), filepath.Base(plan), t.ID, c.Sha, textutil.ShellQuote(plan)))
		}
	}
	return out
}

func tickTargets(root, worktree string, skip []string, commits []Commit) ([]string, map[string]func(Task) bool) {
	entries, err := os.ReadDir(Home(root))
	if err != nil {
		return nil, nil
	}
	var files, messages []string
	for _, c := range commits {
		files = append(files, c.Files...)
		messages = append(messages, c.Message)
	}
	message := strings.Join(messages, "\n")
	var targets []string
	allow := map[string]func(Task) bool{}
	for _, d := range loadDatedPlans(entries, Home(root), skip) {
		recent := eligible(d.path, worktree)
		elig := func(t Task) bool { return recent(t) || t.Claim != "" }
		for _, t := range d.plan.Tasks {
			if !open(t) || !elig(t) {
				continue
			}
			_, any := covers(root, t.Owns, files)
			hit := any || namesTask(message, d.path, t.ID)
			if hit {
				targets = append(targets, d.path)
				allow[d.path] = elig
				break
			}
		}
	}
	return targets, allow
}

// TaskTrailers validates every requested task before the commit touches the index.
func TaskTrailers(root, plan string, ids []string) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	home := Home(root)
	entries, err := os.ReadDir(home)
	if err != nil && plan == "" {
		return "", fmt.Errorf("plans: no open tasks: %w", err)
	}
	all := loadDatedPlans(entries, home, MirrorDests())
	if plan != "" {
		plan, err = filepath.Abs(plan)
		if err != nil {
			return "", err
		}
		if slices.Contains(MirrorDests(), plan) {
			return "", fmt.Errorf("plans: %s is an unapproved draft mirror", plan)
		}
		data, readErr := os.ReadFile(plan)
		if readErr != nil {
			return "", readErr
		}
		all = []datedPlan{{path: plan, plan: parseBytes(plan, data)}}
	}
	var trailers []string
	for _, id := range ids {
		line, err := resolveTrailer(all, WorktreeName(root), id)
		if err != nil {
			return "", err
		}
		trailers = append(trailers, line)
	}
	slices.Sort(trailers)
	return "\n\n" + strings.Join(slices.Compact(trailers), "\n"), nil
}

func resolveTrailer(all []datedPlan, worktree, id string) (string, error) {
	var hits, near []string
	for _, d := range all {
		for _, t := range d.plan.Tasks {
			if !open(t) || (!eligible(d.path, worktree)(t) && t.Claim == "") {
				continue
			}
			near = append(near, t.ID)
			if t.ID == id {
				hits = append(hits, "Task: "+strings.TrimSuffix(filepath.Base(d.path), ".md")+" "+id)
			}
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		return "", fmt.Errorf("plans: task %q is ambiguous; choose --plan", id)
	}
	slices.SortFunc(near, func(a, b string) int {
		return cmp.Or(cmp.Compare(taskDistance(id, a), taskDistance(id, b)), strings.Compare(a, b))
	})
	near = slices.Compact(near)
	return "", fmt.Errorf("plans: no open task %q in a recent or claimed plan; nearest ids: %s", id, strings.Join(near[:min(5, len(near))], ", "))
}

func taskDistance(a, b string) int {
	row := make([]int, len(b)+1)
	for i := range row {
		row[i] = i
	}
	for i := range len(a) {
		next := make([]int, len(b)+1)
		next[0] = i + 1
		for j := range len(b) {
			cost := 0
			if a[i] != b[j] {
				cost = 1
			}
			next[j+1] = min(next[j]+1, row[j+1]+1, row[j]+cost)
		}
		row = next
	}
	return row[len(b)]
}

func onMain(ctx context.Context, root, sha string) (bool, error) {
	if WorktreeName(root) != "" {
		return false, nil
	}
	raw, err := proc.RunGitInherit(ctx, root, "for-each-ref", "--contains="+sha, "--format=%(HEAD)", "refs/heads/main")
	return strings.TrimSpace(raw) == "*", err
}

func readCommit(ctx context.Context, root, sha string) (Commit, error) {
	raw, err := proc.RunGitInherit(ctx, root, "show", "--no-renames", "-z", "--name-only", "--format=%H%x00%B%x00", sha, "--")
	if err != nil {
		return Commit{}, err
	}
	oid, rest, _ := strings.Cut(raw, "\x00")
	message, files, _ := strings.Cut(rest, "\x00")
	c := Commit{Sha: sha[:min(7, len(sha))], Message: message, mainHead: isMainHead(root, oid)}
	for f := range strings.SplitSeq(strings.TrimPrefix(files, "\x00\n"), "\x00") {
		if f != "" {
			c.Files = append(c.Files, f)
		}
	}
	return c, nil
}

func isMainHead(root, oid string) bool {
	head, err := os.ReadFile(filepath.Join(root, ".git", "HEAD"))
	if err != nil || strings.TrimSpace(string(head)) != "ref: refs/heads/main" {
		return false
	}
	ref, err := os.ReadFile(filepath.Join(root, ".git", "refs", "heads", "main"))
	return err == nil && strings.TrimSpace(string(ref)) == oid
}

func commitsOnMain(ctx context.Context, root string, commits []Commit) (bool, error) {
	if len(commits) == 0 {
		return onMain(ctx, root, "HEAD")
	}
	for _, c := range commits {
		landed, err := onMain(ctx, root, c.Sha)
		if err != nil || !landed {
			return false, err
		}
	}
	return true, nil
}
