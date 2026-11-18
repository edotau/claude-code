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
	Sha     string
	Files   []string
	Message string
}

// tickResult is what one commit ticked, the notes about it and the unnamed tasks it covers.
type tickResult struct {
	Sha                      string
	Ticked, Notes, Candidate []string
}

// ErrNotOnTrunk is TickRange's refusal when the commits are not on the primary checkout's trunk branch.
var ErrNotOnTrunk = errors.New("plans: nothing ticked: commits tick only once they are on the trunk branch of the primary checkout")

// tickCommits ticks per commit under one lock the tasks its trailers name; ok narrows only the candidate lines.
func tickCommits(root, planPath string, commits []Commit, dryRun bool, ok func(Task) bool) (results []tickResult, err error) {
	err = editPlan(planPath, func(text string) (string, error) {
		orig := text
		for _, c := range commits {
			message, readErr := commitMessage(root, c)
			if readErr != nil {
				return "", readErr
			}
			c.Message = message
			r := tickResult{Sha: strings.TrimSpace(c.Sha)}
			r.Candidate = candidates(root, planPath, text, c, ok, dryRun)
			text, r.Ticked, r.Notes = tickText(root, planPath, text, c)
			if len(r.Ticked)+len(r.Notes)+len(r.Candidate) > 0 {
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

// tickText ticks every open task c's trailers name, noting the owned files c did not touch.
func tickText(root, planPath, text string, c Commit) (out string, ticked, notes []string) {
	lines := strings.Split(text, "\n")
	for _, t := range parseBytes(planPath, []byte(text)).Tasks {
		if !open(t) || !namesTask(c.Message, planPath, t.ID) {
			continue
		}
		lines = markDone(lines, t, c.Sha)
		ticked = append(ticked, t.Title)
		if missing := missingOwns(root, t.Owns, c.Files); len(missing) > 0 {
			notes = append(notes, fmt.Sprintf("plans: %s ticked without owns: %s; `claude-code plan task %q --reopen` if it is unfinished", t.ID, strings.Join(missing, ", "), t.ID))
		}
	}
	return strings.Join(lines, "\n"), ticked, notes
}

// missingOwns lists the file entries of owns that files do not touch; directory entries never count.
func missingOwns(root string, owns, files []string) []string {
	var out []string
	for _, entry := range ownEntries(owns, files) {
		if !isDirOwn(entry) && !touchesEntry(root, entry, strings.TrimSuffix(entry, "/"), files) {
			out = append(out, entry)
		}
	}
	return out
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

// gitReadTimeout bounds each git read of a commit.
const gitReadTimeout = 10 * time.Second

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
	if err != nil {
		return err
	}
	if !landed {
		return ErrNotOnTrunk
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

// PlanTick is what one commit ticked, noted and covered without naming in one plan.
type PlanTick struct {
	Plan                     string
	Ticked, Notes, Candidate []string
}

// Lines are the report lines for sha: what ticked, then the notes and candidates of this plan.
func (r PlanTick) Lines(sha string) []string {
	var out []string
	if len(r.Ticked) > 0 {
		out = append(out, "plans: ticked "+strings.Join(r.Ticked, "; ")+" ("+sha[:min(len(sha), 7)]+") in "+strings.TrimSuffix(filepath.Base(r.Plan), ".md"))
	}
	return append(append(out, r.Notes...), r.Candidate...)
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
	ctx, cancel := context.WithTimeout(context.Background(), gitReadTimeout)
	defer cancel()
	c, err := readCommit(ctx, top, sha)
	if err != nil {
		return nil, nil, err
	}
	landed, err := commitsOnMain(ctx, top, []Commit{c})
	if err != nil || !landed {
		return nil, nil, err
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
			out = append(out, PlanTick{p, r.Ticked, r.Notes, r.Candidate})
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

// printTicked writes each commit's tick line in base, then its notes and candidates; it returns the first write error.
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
		for _, line := range append(slices.Clone(r.Notes), r.Candidate...) {
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
	ctx, cancel := context.WithTimeout(context.Background(), gitReadTimeout)
	defer cancel()
	return proc.RunGitInherit(ctx, root, "show", "-s", "--format=%B", c.Sha, "--")
}

func namesTask(message, plan, id string) bool {
	want := "Task: " + strings.TrimSuffix(filepath.Base(plan), ".md") + " " + id
	if id == "" {
		return false
	}
	for line := range strings.Lines(message) {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// candidates lists open unnamed tasks c covers, once per task: a repeat would nag on every later commit.
func candidates(root, plan, text string, c Commit, ok func(Task) bool, dryRun bool) []string {
	var out []string
	for _, t := range parseBytes(plan, []byte(text)).Tasks {
		if !open(t) || (ok != nil && !ok(t)) || namesTask(c.Message, plan, t.ID) {
			continue
		}
		if _, any := covers(root, t.Owns, c.Files); any && announce(plan, t.ID, dryRun) {
			out = append(out, fmt.Sprintf("plans: candidate %s %s in %s (not ticked); tick with claude-code plan task %s --done --sha %s --plan %s", t.ID, strings.TrimSpace(strings.TrimPrefix(t.Title, t.ID)), filepath.Base(plan), textutil.ShellQuote(t.ID), c.Sha, textutil.ShellQuote(plan)))
		}
	}
	return out
}

// announce reports whether task id of plan has not been announced as a candidate yet, recording it unless dryRun.
func announce(plan, id string, dryRun bool) bool {
	file, key := filepath.Join(ScratchDir(), "candidates"), plan+"\t"+id
	data, err := os.ReadFile(file)
	if err == nil && slices.Contains(strings.Split(string(data), "\n"), key) {
		return false
	}
	if !dryRun && os.MkdirAll(ScratchDir(), 0o700) == nil {
		_ = paths.AtomicWrite(file, append(data, key+"\n"...), 0o600) // an unwritable memo only repeats a hint
	}
	return true
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
			if !open(t) {
				continue
			}
			if _, any := covers(root, t.Owns, files); namesTask(message, d.path, t.ID) || (any && elig(t)) {
				targets = append(targets, d.path)
				allow[d.path] = elig
				break
			}
		}
	}
	return targets, allow
}

// TaskTrailers validates every requested task before staging; plan "" searches recent or claimed plans.
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
		line, err := resolveTrailer(all, WorktreeName(root), id, plan != "")
		if err != nil {
			return "", err
		}
		trailers = append(trailers, line)
	}
	slices.Sort(trailers)
	return "\n\n" + strings.Join(slices.Compact(trailers), "\n"), nil
}

// resolveTrailer finds the one open task id; an explicit plan skips the recent-or-claimed filter.
func resolveTrailer(all []datedPlan, worktree, id string, explicit bool) (string, error) {
	var hits, near, closed []string
	for _, d := range all {
		base := strings.TrimSuffix(filepath.Base(d.path), ".md")
		for _, t := range d.plan.Tasks {
			if !explicit && !eligible(d.path, worktree)(t) && t.Claim == "" {
				continue
			}
			switch {
			case t.ID == id && open(t):
				hits = append(hits, base)
			case t.ID == id:
				closed = append(closed, base)
			case open(t):
				near = append(near, t.ID)
			}
		}
	}
	switch {
	case len(hits) == 1:
		return "Task: " + hits[0] + " " + id, nil
	case len(hits) > 1:
		return "", fmt.Errorf("plans: task %q is open in %s; choose one with --plan <file>", id, strings.Join(hits, " and "))
	case len(closed) > 0:
		return "", fmt.Errorf("plans: task %q is already ticked or struck in %s; reopen it with `claude-code plan task %q --reopen`", id, strings.Join(closed, ", "), id)
	}
	slices.SortFunc(near, func(a, b string) int {
		return cmp.Or(cmp.Compare(taskDistance(id, a), taskDistance(id, b)), strings.Compare(a, b))
	})
	near = slices.Compact(near)
	return "", fmt.Errorf("plans: no open task %q in a recent or claimed plan (--plan <file> reaches any); nearest ids: %s", id, strings.Join(near[:min(5, len(near))], ", "))
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

// onTrunk reports whether root is the primary checkout with HEAD attached to the repository's trunk branch.
func onTrunk(ctx context.Context, root string) bool {
	if WorktreeName(root) != "" {
		return false
	}
	git := func(args ...string) string {
		out, _ := proc.RunGitInherit(ctx, root, args...) // a missing ref or detached HEAD is the empty answer
		return strings.TrimSpace(out)
	}
	cur := git("symbolic-ref", "-q", "--short", "HEAD")
	if cur == "" {
		return false
	}
	if ref := git("symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); ref != "" {
		return strings.TrimPrefix(ref, "origin/") == cur
	}
	for _, name := range []string{git("config", "--get", "init.defaultBranch"), "main", "master"} {
		if name != "" && git("rev-parse", "--verify", "-q", "refs/heads/"+name) != "" {
			return name == cur
		}
	}
	return true
}

func readCommit(ctx context.Context, root, sha string) (Commit, error) {
	raw, err := proc.RunGitInherit(ctx, root, "show", "--no-renames", "-z", "--name-only", "--format=%H%x00%B%x00", sha, "--")
	if err != nil {
		return Commit{}, err
	}
	_, rest, _ := strings.Cut(raw, "\x00")
	message, files, _ := strings.Cut(rest, "\x00")
	c := Commit{Sha: sha[:min(7, len(sha))], Message: message}
	for f := range strings.SplitSeq(strings.TrimPrefix(files, "\x00\n"), "\x00") {
		if f != "" {
			c.Files = append(c.Files, f)
		}
	}
	return c, nil
}

// commitsOnMain reports whether the primary checkout is on its trunk and every commit is in that branch's history.
func commitsOnMain(ctx context.Context, root string, commits []Commit) (bool, error) {
	if !onTrunk(ctx, root) {
		return false, nil
	}
	for _, c := range commits {
		_, err := proc.RunGitInherit(ctx, root, "merge-base", "--is-ancestor", c.Sha, "HEAD")
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}
