package plans

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/paths"
)

// Doc is one plan file as its lines and the tasks parsed from them; Render is the one serializer every write uses.
type Doc struct {
	Plan
	Lines []string
}

// ParseDoc parses b, read from path, into its lines and tasks.
func ParseDoc(path string, b []byte) *Doc {
	return &Doc{Plan: parseBytes(path, b), Lines: strings.Split(string(b), "\n")}
}

// Render is the plan text the lines hold, byte for byte what ParseDoc read when nothing changed.
func (d *Doc) Render() string { return strings.Join(d.Lines, "\n") }

// Find resolves ref the way every by-id verb does: "#N" is the Nth checklist item, else an id, then a title prefix.
func (d *Doc) Find(ref string) (Task, bool) { return findTask(d.Plan, ref) }

// Edit rewrites t's lines through fn and re-parses, so a second edit in the same write sees fresh spans.
func (d *Doc) Edit(t Task, fn func(Task, []string) []string) {
	d.Lines = fn(t, d.Lines)
	d.Plan = parseBytes(d.Path, []byte(d.Render()))
}

// EditDoc re-reads planPath under the plans lock, applies fn to its Doc and writes it back only when bytes changed.
func EditDoc(planPath string, fn func(*Doc) error) error {
	return editPlan(planPath, func(text string) (string, error) {
		d := ParseDoc(planPath, []byte(text))
		if err := fn(d); err != nil {
			return "", err
		}
		return d.Render(), nil
	})
}

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
	return EditDoc(planPath, func(d *Doc) error {
		t, ok := d.Find(taskID)
		if !ok {
			return fmt.Errorf("plans: no task %q in %s", taskID, planPath)
		}
		d.Edit(t, fn)
		return nil
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

// taskIndex reads a "#N" reference, the 1-based position of a checklist item in its plan.
func taskIndex(ref string) (int, bool) {
	digits, ok := strings.CutPrefix(ref, "#")
	n, err := strconv.Atoi(digits)
	return n, ok && err == nil && n > 0 && digits == strconv.Itoa(n)
}

// findTask resolves taskID to a task: "#N" by position, else an exact Task.ID first, then a Title prefix.
func findTask(p Plan, taskID string) (Task, bool) {
	if n, ok := taskIndex(taskID); ok {
		if n > len(p.Tasks) {
			return Task{}, false
		}
		return p.Tasks[n-1], true
	}
	for _, t := range p.Tasks {
		if t.ID != "" && t.ID == taskID {
			return t, true
		}
	}
	for _, t := range p.Tasks {
		title := strings.TrimLeft(strings.TrimPrefix(t.Title, "**you**"), " —") // the owner marker and its dash precede the id
		if taskID != "" && titleHasPrefix(title, taskID) {
			return t, true
		}
	}
	return Task{}, false
}

// laneRe is the in-lane mark a verified lane commit leaves: an HTML comment, so no reader takes it as text or a sha.
var laneRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)<!--\s*lane\s+([^\s:]+):\s*([0-9a-f]{7,40})\s*-->`)
})

// markLane replaces any lane mark across t's span with one naming lane and sha, on t's last line; no line is added.
func markLane(lines []string, t Task, lane, sha string) []string {
	lines = dropComments(lines, t, laneRe())
	lines[t.End] = appendSuffix(lines[t.End], " <!-- lane "+lane+": "+sha[:min(7, len(sha))]+" -->")
	return lines
}

// MarkLane marks open task taskID as in lane at sha; wrote is false when it is closed or already so marked.
func MarkLane(planPath, taskID, lane, sha string) (wrote bool, err error) {
	if lane == "" || strings.ContainsAny(lane, " \t\r\n:") || strings.Contains(lane, "-->") {
		return false, fmt.Errorf("plans: lane %q must be one word with no colon", lane)
	}
	if !shaHex().MatchString(sha) {
		return false, fmt.Errorf("plans: %q is not a commit sha", sha)
	}
	err = editTask(planPath, taskID, func(t Task, lines []string) []string {
		if !open(t) || t.Lane == lane && strings.HasPrefix(sha, t.LaneSha) {
			return lines
		}
		wrote = true
		return markLane(lines, t, lane, sha)
	})
	return wrote && err == nil, err
}
