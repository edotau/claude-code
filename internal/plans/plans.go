// Package plans parses plan checklists and keeps their ticks, claims and status in one place.
package plans

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// Task is one checklist item: its line span, state, identity, owned files and plan metadata.
type Task struct {
	Start, End int // 0-based line indexes, inclusive
	Done       bool
	ID         string // "Phase N.M" or a leading lane id such as "3.2a"
	Title      string
	Owns       []string
	Sha        string
	Claim      string // inner text of a working:/finished: comment
	Partial    []string
}

// Plan is one markdown file's parsed checklist.
type Plan struct {
	Path  string
	Tasks []Task
}

// Summary is one plan file's progress line for `plan status`.
type Summary struct {
	Path    string
	Ticked  int
	Total   int
	Claims  []string
	Partial int
}

var (
	checkLine  = regexp.MustCompile(`^(\s*)[-*] \[([ xX])\] (.*)$`)
	listLine   = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
	commentRe  = regexp.MustCompile(`(?s)<!--(.*?)-->`)
	shaParen   = regexp.MustCompile(`(?i)\s*\(([0-9a-f]{7,40})\)\s*$`)
	shaDash    = regexp.MustCompile(`(?i)\s+(?:—|--|-)\s+([0-9a-f]{7,40})\s*$`)
	idPhase    = regexp.MustCompile(`(?i)^Phase\s+\d+(?:\.\d+)*[a-z]?\b`)
	idLane     = regexp.MustCompile(`^\d+(?:\.\d+)*[a-z]?\b`)
	datedName  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-.*\.md$`)
	claimRe    = regexp.MustCompile(`(?i)<!--\s*(?:working|finished):.*?-->`)
	partialRe  = regexp.MustCompile(`(?i)<!--\s*partly landed:\s*(.*?)\s*-->`)
	claimWord  = regexp.MustCompile(`(?i)^(?:working|finished):`)
	partialKey = regexp.MustCompile(`(?i)^partly landed:\s*(.*)$`)
	ownNote    = regexp.MustCompile(`\s*\([^)]*\)\s*$`) // "cli.go (verb row only)" owns cli.go
)

// Parse reads and parses path's checklist.
func Parse(path string) (Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Plan{Path: path}, err
	}
	return ParseBytes(path, b), nil
}

// ParseBytes parses b; a checklist item runs from its bullet through its more-indented continuation lines.
func ParseBytes(path string, b []byte) Plan {
	lines := strings.Split(string(b), "\n")
	p := Plan{Path: path}
	for i := 0; i < len(lines); i++ {
		m := checkLine.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		indent := len(m[1])
		text := m[3]
		end := i
		for j := i + 1; j < len(lines); j++ {
			line := lines[j]
			if strings.TrimSpace(line) == "" || listLine.MatchString(line) {
				break
			}
			if len(line)-len(strings.TrimLeft(line, " \t")) <= indent {
				break
			}
			text += " " + strings.TrimSpace(line)
			end = j
		}
		t := Task{Start: i, End: end, Done: m[2] == "x" || m[2] == "X"}
		t.ID, t.Title, t.Owns, t.Sha, t.Claim, t.Partial = parseTaskText(text)
		p.Tasks = append(p.Tasks, t)
		i = end
	}
	return p
}

// parseTaskText splits a joined item into id, title and the owns/sha/claim/partial metadata.
func parseTaskText(text string) (id, title string, owns []string, sha, claim string, partial []string) {
	for _, m := range commentRe.FindAllStringSubmatch(text, -1) {
		inner := strings.TrimSpace(m[1])
		switch {
		case claimWord.MatchString(inner):
			claim = inner
		case partialKey.MatchString(inner):
			partial = append(partial, splitList(partialKey.FindStringSubmatch(inner)[1])...)
		}
	}
	cleaned := strings.Join(strings.Fields(commentRe.ReplaceAllString(text, " ")), " ")
	if m := shaParen.FindStringSubmatch(cleaned); m != nil {
		sha = m[1]
		cleaned = strings.TrimSpace(cleaned[:len(cleaned)-len(m[0])])
	} else if m := shaDash.FindStringSubmatch(cleaned); m != nil {
		sha = m[1]
		cleaned = strings.TrimSpace(cleaned[:len(cleaned)-len(m[0])])
	}
	title = cleaned
	if idPhase.MatchString(cleaned) {
		id = idPhase.FindString(cleaned)
	} else if idLane.MatchString(cleaned) {
		id = idLane.FindString(cleaned)
	}
	lower := strings.ToLower(cleaned)
	if i := strings.LastIndex(lower, "owns:"); i >= 0 {
		owns = splitList(cleaned[i+len("owns:"):])
	}
	return id, title, owns, sha, claim, partial
}

// splitList splits a comma-separated list, dropping empty entries.
func splitList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// Home is the plans directory for repoRoot, tagged like every other repo-scoped artifact.
func Home(repoRoot string) string {
	return filepath.Join(paths.ConfigDir(), "plans", paths.RepoSlug(repoRoot))
}

// Active picks home's tick target: a dated plan with an open owned task, never an unapproved draft in skip; a plan
// whose claim names worktree wins, else the newest by mtime.
func Active(home, worktree string, skip []string) (string, bool) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return "", false
	}
	best, bestMod, bestClaimed := "", time.Time{}, false
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(home, name)
		if !e.Type().IsRegular() || !datedName.MatchString(name) || slices.Contains(skip, p) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		plan, err := Parse(p)
		if err != nil || !hasOpenOwnedTask(plan) {
			continue
		}
		claimed := worktree != "" && claimsWorktree(plan, worktree)
		if best != "" && (bestClaimed && !claimed || claimed == bestClaimed && !info.ModTime().After(bestMod)) {
			continue
		}
		best, bestMod, bestClaimed = p, info.ModTime(), claimed
	}
	return best, best != ""
}

// claimsWorktree reports whether an open task's claim names worktree ("· worktree <w>").
func claimsWorktree(plan Plan, worktree string) bool {
	for _, t := range plan.Tasks {
		if !t.Done && strings.Contains(t.Claim, "worktree "+worktree) {
			return true
		}
	}
	return false
}

// hasOpenOwnedTask reports whether plan still has a task to land.
func hasOpenOwnedTask(plan Plan) bool {
	for _, t := range plan.Tasks {
		if !t.Done && len(t.Owns) > 0 {
			return true
		}
	}
	return false
}

// Covers reports which of owns a commit's files touch: all entries and/or at least one.
func Covers(owns, files []string) (all, any bool) {
	if len(owns) == 0 {
		return false, false
	}
	all = true
	for _, own := range owns {
		entry := filepath.ToSlash(strings.TrimSpace(ownNote.ReplaceAllString(own, "")))
		dir := strings.TrimSuffix(entry, "/")
		touched := false
		for _, f := range files {
			file := filepath.ToSlash(f)
			if entry != "" && (file == entry || file == dir || strings.HasPrefix(file, dir+"/")) {
				touched = true
				break
			}
			if ok, err := path.Match(entry, file); err == nil && ok {
				touched = true
				break
			}
		}
		if touched {
			any = true
		} else {
			all = false
		}
	}
	return all, any
}

// Status summarizes every *.md directly in each dir that carries at least one task, sorted by path.
func Status(dirs ...string) ([]Summary, error) {
	var out []Summary
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".md") { // a symlink may point outside plans/
				continue
			}
			plan, err := Parse(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			if len(plan.Tasks) == 0 {
				continue
			}
			s := Summary{Path: plan.Path, Total: len(plan.Tasks)}
			for _, t := range plan.Tasks {
				if t.Done {
					s.Ticked++
				}
				if len(t.Partial) > 0 {
					s.Partial++
				}
				if t.Claim != "" {
					label := t.ID
					if label == "" {
						label = t.Title
					}
					s.Claims = append(s.Claims, label+": "+t.Claim)
				}
			}
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Skeleton reads the plan template from <configDir>/agents/Plan.md's first ```markdown block holding "# Plan:".
func Skeleton(configDir string) (string, error) {
	src := filepath.Join(configDir, "agents", "Plan.md")
	b, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(b), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "```") || !strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "```")), "markdown") {
			continue
		}
		var body []string
		closed := false
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
				i = j
				closed = true
				break
			}
			body = append(body, lines[j])
		}
		if !closed {
			return "", fmt.Errorf("plans: %s: unterminated markdown fence", src)
		}
		for _, l := range body {
			if strings.HasPrefix(l, "# Plan:") {
				return strings.Join(body, "\n") + "\n", nil
			}
		}
	}
	return "", fmt.Errorf("plans: %s: no ```markdown block with a # Plan: heading", src)
}
