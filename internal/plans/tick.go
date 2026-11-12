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

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

var boxRe = regexp.MustCompile(`^(\s*[-*] \[)[ xX](\])`)

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
func Tick(planPath, sha string, files []string) (ticked, partial []string, err error) {
	results, err := tickCommits(planPath, []Commit{{Sha: sha, Files: files}}, false)
	for _, r := range results {
		ticked, partial = r.Ticked, r.Partial
	}
	return ticked, partial, err
}

// tickCommits ticks per commit under one lock, a task completing once the range's union covers it; dryRun skips writing.
func tickCommits(planPath string, commits []Commit, dryRun bool) (results []tickResult, err error) {
	err = editPlan(planPath, func(text string) (string, error) {
		orig := text
		var seen []string
		for _, c := range commits {
			r := tickResult{Sha: strings.TrimSpace(c.Sha)}
			seen = append(seen, c.Files...)
			text, r.Ticked, r.Partial = tickText(planPath, text, r.Sha, c.Files, seen)
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

// tickText is Tick's pure edit: ticks on seen (files so far), notes partials from this commit's files.
func tickText(planPath, text, sha string, files, seen []string) (out string, ticked, partial []string) {
	lines := strings.Split(text, "\n")
	for _, t := range parseBytes(planPath, []byte(text)).Tasks {
		if t.Done || len(t.Owns) == 0 {
			continue
		}
		all, _ := covers(t.Owns, seen)
		_, any := covers(t.Owns, files)
		switch {
		case all:
			lines = removeClaims(lines, t)
			lines[t.Start] = boxRe.ReplaceAllString(lines[t.Start], "${1}x${2}")
			lines[t.End] = appendSuffix(lines[t.End], " ("+sha+")")
			ticked = append(ticked, t.Title)
		case any && !hasSha(t.Partial, sha):
			lines = addPartial(lines, t, sha)
			partial = append(partial, t.Title)
		}
	}
	return strings.Join(lines, "\n"), ticked, partial
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
		if info, err := os.Stat(dest); err == nil {
			mode = info.Mode().Perm()
		}
		return writePlan(dest, data, mode)
	})
}

// removeClaims drops every working:/finished: comment across t's line span.
func removeClaims(lines []string, t Task) []string {
	for i := t.Start; i <= t.End; i++ {
		if claimRe.MatchString(lines[i]) {
			lines[i] = strings.TrimRight(claimRe.ReplaceAllString(lines[i], ""), " \t")
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
	for i := t.Start; i <= t.End; i++ {
		if !partialRe.MatchString(lines[i]) {
			continue
		}
		lines[i] = partialRe.ReplaceAllStringFunc(lines[i], func(comment string) string {
			shas := textutil.SplitCSV(partialRe.FindStringSubmatch(comment)[1])
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

// ClaimFree writes <!-- text --> at a task's last line unless it already carries a claim, decided under the lock.
func ClaimFree(planPath, taskID, text string) error {
	text = strings.TrimSpace(text)
	if strings.Contains(text, "-->") {
		return fmt.Errorf("plans: claim text must not contain %q", "-->")
	}
	if !strings.HasPrefix(strings.ToLower(text), "working:") {
		return errors.New("plans: claim text must start with \"working:\"")
	}
	return editTask(planPath, taskID, func(t Task, lines []string) []string {
		if t.Claim != "" {
			return lines
		}
		lines[t.End] = appendSuffix(lines[t.End], " <!-- "+text+" -->")
		return lines
	})
}

// ClearClaimOwned removes a task's claim only when a non-empty owner appears in its text, decided under the lock.
func ClearClaimOwned(planPath, taskID, owner string) error {
	return editTask(planPath, taskID, func(t Task, lines []string) []string {
		if owner == "" || !strings.Contains(t.Claim, owner) {
			return lines
		}
		return removeClaims(lines, t)
	})
}

// findTask resolves taskID to a task: an exact Task.ID first, then a Title prefix.
func findTask(p Plan, taskID string) (Task, bool) {
	for _, t := range p.Tasks {
		if t.ID != "" && t.ID == taskID {
			return t, true
		}
	}
	for _, t := range p.Tasks {
		if taskID != "" && strings.HasPrefix(t.Title, taskID) {
			return t, true
		}
	}
	return Task{}, false
}

// TickRange ticks plan (default: the one plan the range's files match) for each commit in rng; drafts are never ticked.
func TickRange(ctx context.Context, dir, worktree, rng, plan string, drafts []string, dryRun bool, w io.Writer) error {
	commits, err := rangeCommits(ctx, dir, rng)
	if err != nil {
		return err
	}
	plan, done, err := pickPlan(dir, worktree, plan, drafts, commits, w)
	if err != nil || done {
		return err
	}
	results, err := tickCommits(plan, commits, dryRun)
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(filepath.Base(plan), ".md")
	if err := printTicked(w, results, base, dryRun); err != nil {
		return err
	}
	if len(results) == 0 {
		_, err = fmt.Fprintf(w, "plans: %d commits, nothing to tick in %s\n", len(commits), base)
		return err
	}
	return nil
}

// pickPlan resolves the plan to tick; done is true when none was chosen and the reason is already printed.
func pickPlan(dir, worktree, plan string, drafts []string, commits []Commit, w io.Writer) (string, bool, error) {
	if plan != "" {
		if abs, err := filepath.Abs(plan); err == nil && slices.Contains(drafts, abs) {
			return "", false, fmt.Errorf("%s mirrors an unapproved draft; approve it before ticking", plan)
		}
		return plan, false, nil
	}
	plan, tied := Select(Home(dir), worktree, drafts, commits)
	if plan != "" {
		return plan, false, nil
	}
	var err error
	if len(tied) > 0 {
		_, err = fmt.Fprintf(w, "plans: ambiguous, %d plans match equally (%s); nothing annotated; "+
			"pick one with `claude-code plan tick --plan <file>`\n", len(tied), baseNames(tied))
	} else {
		_, err = fmt.Fprintln(w, "plans: no plan owns these files; nothing to tick")
	}
	return "", true, err
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

// readDestRecord is the destination a record names, "" when it is emptied (retired) or not one absolute .md path.
func readDestRecord(draftBase string) (string, error) {
	data, err := os.ReadFile(DestRecordPath(draftBase))
	if err != nil {
		return "", err
	}
	dest := strings.TrimSpace(string(data))
	if !filepath.IsAbs(dest) || !strings.HasSuffix(dest, ".md") || strings.ContainsAny(dest, "\r\n") {
		return "", nil
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
			return allDatedPlans() // an unreadable record hides its mirror, so exclude every plan (fail closed)
		}
		if dest != "" { // an emptied or malformed record names no mirror
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
