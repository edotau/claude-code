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

// Tick ticks fully covered tasks and notes partly covered ones; returns touched titles, writes only on change.
func Tick(planPath, sha string, files []string) (ticked, partial []string, err error) {
	sha = strings.TrimSpace(sha)
	err = paths.WithFileLock(filepath.Join(paths.StateDir(), "plans.lock"), func() error {
		info, err := os.Stat(planPath)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(planPath)
		if err != nil {
			return err
		}
		lines := strings.Split(string(b), "\n")
		changed := false
		for _, t := range ParseBytes(planPath, b).Tasks {
			if t.Done || len(t.Owns) == 0 {
				continue
			}
			all, any := Covers(t.Owns, files)
			switch {
			case all:
				lines = removeClaims(lines, t)
				lines[t.Start] = boxRe.ReplaceAllString(lines[t.Start], "${1}x${2}")
				lines[t.End] = appendSuffix(lines[t.End], " ("+sha+")")
				ticked = append(ticked, t.Title)
				changed = true
			case any && !hasSha(t.Partial, sha):
				lines = addPartial(lines, t, sha)
				partial = append(partial, t.Title)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return writePlan(planPath, []byte(strings.Join(lines, "\n")), info.Mode().Perm())
	})
	return ticked, partial, err
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
