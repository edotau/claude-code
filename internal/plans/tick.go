package plans

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

var boxRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(\s*[-*] \[)[ xX](\])`) })

// Commit is one landed commit: the sha recorded on ticks and the files it changed.
type Commit struct {
	Sha   string
	Files []string
}

// tickResult is what one commit ticked or marked partly landed.
type tickResult struct {
	Sha             string
	Ticked, Partial []string
}

// Tick ticks fully covered tasks and notes partly covered ones; returns touched titles, writes only on change.
func Tick(root, planPath, sha string, files []string) (ticked, partial []string, err error) {
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
		var seen []string
		for _, c := range commits {
			r := tickResult{Sha: strings.TrimSpace(c.Sha)}
			seen = append(seen, c.Files...)
			text, r.Ticked, r.Partial = tickText(root, planPath, text, r.Sha, c.Files, seen, recorded, ok)
			if len(r.Ticked)+len(r.Partial) > 0 {
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

// recordedFiles reads the files of a commit already recorded in a partly landed note; root "" reads none.
func recordedFiles(root string) func(sha string) []string {
	cache := map[string][]string{}
	return func(sha string) []string {
		if root == "" {
			return nil
		}
		if f, ok := cache[sha]; ok {
			return f
		}
		ctx, cancel := context.WithTimeout(context.Background(), recordedTimeout)
		defer cancel()
		f, _ := CommitFiles(ctx, root, sha) // an unreadable sha (rewritten history) adds no coverage
		cache[sha] = f
		return f
	}
}

// recordedTimeout bounds each git read of a recorded commit.
const recordedTimeout = 10 * time.Second

// tickText is Tick's pure edit: ticks on seen plus each task's recorded partial commits, notes partials from files.
func tickText(root, planPath, text, sha string, files, seen []string, recorded func(string) []string, ok func(Task) bool) (out string, ticked, partial []string) {
	lines := strings.Split(text, "\n")
	for _, t := range parseBytes(planPath, []byte(text)).Tasks {
		if !open(t) || len(t.Owns) == 0 || (ok != nil && !ok(t)) {
			continue
		}
		have := slices.Clone(seen)
		for _, s := range t.Partial {
			if recorded != nil && !strings.EqualFold(s, sha) {
				have = append(have, recorded(s)...)
			}
		}
		all, _ := covers(root, t.Owns, have)
		_, any := covers(root, t.Owns, files)
		switch {
		case all:
			lines = markDone(lines, t, sha)
			ticked = append(ticked, t.Title)
		case any && !hasSha(t.Partial, sha):
			lines = addPartial(lines, t, sha)
			partial = append(partial, t.Title)
		}
	}
	return strings.Join(lines, "\n"), ticked, partial
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
	targets, done, err := pickPlans(dir, worktree, plan, drafts, commits, w)
	if err != nil || done {
		return err
	}
	for _, target := range targets {
		var ok func(Task) bool // a plan named by the caller is always eligible
		if plan == "" {
			ok = eligible(target, worktree)
		}
		results, err := tickCommits(dir, target, commits, dryRun, ok)
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

// pickPlans resolves the plans to tick; done is true when none was chosen and the reason is already printed.
func pickPlans(dir, worktree, plan string, drafts []string, commits []Commit, w io.Writer) ([]string, bool, error) {
	if plan != "" {
		if abs, err := filepath.Abs(plan); err == nil && slices.Contains(drafts, abs) {
			return nil, false, fmt.Errorf("%s mirrors an unapproved draft; approve it before ticking", plan)
		}
		return []string{plan}, false, nil
	}
	targets, tied := Select(Home(dir), dir, worktree, drafts, commits)
	if len(targets) > 0 {
		return targets, false, printTie(w, tied)
	}
	_, err := fmt.Fprintln(w, "plans: no plan owns these files; nothing to tick")
	return nil, true, err
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
	Plan            string
	Ticked, Partial []string
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
	return out
}

// TieNote is the one line naming plans that own the same file and were all updated; "" when none tied.
func TieNote(tied []string) string {
	if len(tied) == 0 {
		return ""
	}
	return "plans: open tasks of " + baseNames(tied) + " own the same file; all updated"
}

// TickCommit ticks every plan the commit's files advance and lists plans tied on a shared file; idempotent per sha.
func TickCommit(top, sha string, files []string) (out []PlanTick, tied []string, err error) {
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	wt := WorktreeName(top)
	targets, tied := Select(Home(top), top, wt, MirrorDests(), []Commit{{Sha: short, Files: files}})
	var errs []error
	for _, p := range targets {
		results, terr := tickCommits(top, p, []Commit{{Sha: short, Files: files}}, false, eligible(p, wt))
		errs = append(errs, terr)
		for _, r := range results {
			out = append(out, PlanTick{p, r.Ticked, r.Partial})
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

// Reopen unticks taskID and removes its landing sha, for a reverted commit.
func Reopen(planPath, taskID string) error {
	return editTask(planPath, taskID, func(t Task, lines []string) []string {
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
