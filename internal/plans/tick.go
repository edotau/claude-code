package plans

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

var boxRe = regexp.MustCompile(`^(\s*[-*] \[)[ xX](\])`)

// Commit is one landed commit: the sha recorded on ticks and the files it changed.
type Commit struct {
	Sha   string
	Files []string
}

// TickResult is what one commit ticked or marked partly landed.
type TickResult struct {
	Sha             string
	Ticked, Partial []string
}

// Tick ticks fully covered tasks and notes partly covered ones; returns touched titles, writes only on change.
func Tick(planPath, sha string, files []string) (ticked, partial []string, err error) {
	results, err := TickCommits(planPath, []Commit{{Sha: sha, Files: files}}, false)
	for _, r := range results {
		ticked, partial = r.Ticked, r.Partial
	}
	return ticked, partial, err
}

// TickCommits ticks per commit in order under one lock, a task completing once the range's union covers it; dryRun skips writing.
func TickCommits(planPath string, commits []Commit, dryRun bool) (results []TickResult, err error) {
	err = paths.WithFileLock(filepath.Join(paths.StateDir(), "plans.lock"), func() error {
		info, err := os.Stat(planPath)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(planPath)
		if err != nil {
			return err
		}
		text := string(b)
		var seen []string
		for _, c := range commits {
			r := TickResult{Sha: strings.TrimSpace(c.Sha)}
			seen = append(seen, c.Files...)
			text, r.Ticked, r.Partial = tickText(planPath, text, r.Sha, c.Files, seen)
			if len(r.Ticked)+len(r.Partial) > 0 {
				results = append(results, r)
			}
		}
		if dryRun || text == string(b) {
			return nil
		}
		return writePlan(planPath, []byte(text), info.Mode().Perm())
	})
	return results, err
}

// tickText is Tick's pure edit: ticks on seen (files so far), notes partials from this commit's files.
func tickText(planPath, text, sha string, files, seen []string) (out string, ticked, partial []string) {
	lines := strings.Split(text, "\n")
	for _, t := range ParseBytes(planPath, []byte(text)).Tasks {
		if t.Done || len(t.Owns) == 0 {
			continue
		}
		all, _ := Covers(t.Owns, seen)
		_, any := Covers(t.Owns, files)
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

// editTask re-reads planPath under the plans lock, applies fn and writes back only when bytes changed.
func editTask(planPath, taskID string, fn func(Task, []string) []string) error {
	return paths.WithFileLock(filepath.Join(paths.StateDir(), "plans.lock"), func() error {
		info, err := os.Stat(planPath)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(planPath)
		if err != nil {
			return err
		}
		t, ok := findTask(ParseBytes(planPath, b), taskID)
		if !ok {
			return fmt.Errorf("plans: no task %q in %s", taskID, planPath)
		}
		lines := strings.Split(string(b), "\n")
		out := strings.Join(fn(t, lines), "\n")
		if out == string(b) {
			return nil
		}
		return writePlan(planPath, []byte(out), info.Mode().Perm())
	})
}

// WriteFile replaces a plan under the plans lock, so a mirror or save never interleaves with a tick or claim.
func WriteFile(dest string, data []byte) error {
	return paths.WithFileLock(filepath.Join(paths.StateDir(), "plans.lock"), func() error {
		mode := os.FileMode(0o600)
		if info, err := os.Stat(dest); err == nil {
			mode = info.Mode().Perm()
		}
		return writePlan(dest, data, mode)
	})
}

// writePlan is the package's one write site: atomic replace that keeps dest's mode.
func writePlan(dest string, data []byte, mode os.FileMode) error {
	return paths.AtomicWrite(dest, data, mode)
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
			shas := splitList(partialRe.FindStringSubmatch(comment)[1])
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
	for _, s := range shas {
		if strings.EqualFold(s, sha) {
			return true
		}
	}
	return false
}
