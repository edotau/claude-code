// Package plans parses plan checklists and keeps their ticks, claims and status in one place.
package plans

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
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
	UserMark   string // "ticked <RFC3339>" or "unticked <RFC3339>" from a <!-- user: ... --> comment
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
	userRe     = regexp.MustCompile(`(?i)<!--\s*user:\s*((?:un)?ticked\s+\S+)\s*-->`)
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
	var open fenceOpen // the fence now open, zero when none
	for i := 0; i < len(lines); i++ {
		if ch, n, rest := fenceMarker(lines[i]); n > 0 {
			open = open.next(ch, n, rest)
			continue
		}
		m := checkLine.FindStringSubmatch(lines[i])
		if open.n > 0 || m == nil {
			continue
		}
		cont, end := continuation(lines, i, len(m[1]))
		text := m[3] + cont
		t := Task{Start: i, End: end, Done: m[2] == "x" || m[2] == "X"}
		t.ID, t.Title, t.Owns, t.Sha, t.Claim, t.Partial = parseTaskText(text)
		if um := userRe.FindStringSubmatch(text); um != nil {
			t.UserMark = um[1]
		}
		p.Tasks = append(p.Tasks, t)
		i = end
	}
	return p
}

// continuation joins the wrapped lines after lines[i] that are indented past indent, and returns the last line used.
func continuation(lines []string, i, indent int) (string, int) {
	end := i
	var cont strings.Builder
	for j := i + 1; j < len(lines); j++ {
		line := lines[j]
		if strings.TrimSpace(line) == "" || listLine.MatchString(line) || isFence(line) {
			break
		}
		if len(line)-len(strings.TrimLeft(line, " \t")) <= indent {
			break
		}
		cont.WriteString(" ")
		cont.WriteString(strings.TrimSpace(line))
		end = j
	}
	return cont.String(), end
}

// fenceOpen is the opening fence's character and length; a closer needs the same character and at least that length.
type fenceOpen struct {
	ch byte
	n  int
}

// next returns the fence state after a marker line: it opens when none is open, closes on a matching bare marker.
func (o fenceOpen) next(ch byte, n int, rest string) fenceOpen {
	switch {
	case o.n == 0:
		return fenceOpen{ch, n}
	case ch == o.ch && n >= o.n && rest == "":
		return fenceOpen{}
	}
	return o
}

// fenceMarker returns line's leading fence character, its run length (0 when line is no fence) and the text after it.
func fenceMarker(line string) (ch byte, n int, rest string) {
	l := strings.TrimSpace(line)
	if l == "" || (l[0] != '`' && l[0] != '~') {
		return 0, 0, ""
	}
	for n < len(l) && l[n] == l[0] {
		n++
	}
	if n < 3 {
		return 0, 0, ""
	}
	return l[0], n, strings.TrimSpace(l[n:])
}

// isFence reports whether line is a fence-like line (opener or closer).
func isFence(line string) bool {
	_, n, _ := fenceMarker(line)
	return n > 0
}

// parseTaskText splits a joined item into id, title and the owns/sha/claim/partial metadata.
func parseTaskText(text string) (id, title string, owns []string, sha, claim string, partial []string) {
	for _, m := range commentRe.FindAllStringSubmatch(text, -1) {
		inner := strings.TrimSpace(m[1])
		switch {
		case claimWord.MatchString(inner):
			claim = inner
		case partialKey.MatchString(inner):
			partial = append(partial, textutil.SplitCSV(partialKey.FindStringSubmatch(inner)[1])...)
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
		owns = textutil.SplitCSV(cleaned[i+len("owns:"):])
	}
	return id, title, owns, sha, claim, partial
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
		p := filepath.Join(home, e.Name())
		mod, claimed, ok := activeCandidate(e, p, worktree, skip)
		if !ok {
			continue
		}
		if best != "" && !beatsActive(claimed, mod, bestClaimed, bestMod) {
			continue
		}
		best, bestMod, bestClaimed = p, mod, claimed
	}
	return best, best != ""
}

// activeCandidate returns a dated, non-skipped plan's mtime and worktree claim when it still has an open owned task.
func activeCandidate(e os.DirEntry, p, worktree string, skip []string) (mod time.Time, claimed, ok bool) {
	if !e.Type().IsRegular() || !datedName.MatchString(e.Name()) || slices.Contains(skip, p) {
		return time.Time{}, false, false
	}
	info, err := e.Info()
	if err != nil {
		return time.Time{}, false, false
	}
	plan, err := Parse(p)
	if err != nil || !hasOpenOwnedTask(plan) {
		return time.Time{}, false, false
	}
	return info.ModTime(), worktree != "" && claimsWorktree(plan, worktree), true
}

// beatsActive reports whether a candidate outranks the current best: a worktree claim first, then a newer mtime.
func beatsActive(claimed bool, mod time.Time, bestClaimed bool, bestMod time.Time) bool {
	if claimed != bestClaimed {
		return claimed
	}
	return mod.After(bestMod)
}

// claimNamesWorktree matches "worktree <w>" as a whole name, so "fix" does not match "worktree fix-2".
func claimNamesWorktree(claim, worktree string) bool {
	needle := "worktree " + worktree
	for i := strings.Index(claim, needle); i >= 0; {
		end := i + len(needle)
		if end == len(claim) || !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-", rune(claim[end])) {
			return true
		}
		next := strings.Index(claim[end:], needle)
		if next < 0 {
			return false
		}
		i = end + next
	}
	return false
}

// claimsWorktree reports whether an open task's claim names worktree ("· worktree <w>").
func claimsWorktree(plan Plan, worktree string) bool {
	for _, t := range plan.Tasks {
		if !t.Done && claimNamesWorktree(t.Claim, worktree) {
			return true
		}
	}
	return false
}

// hasLiveClaim reports whether an open task carries a claim.
func hasLiveClaim(plan Plan) bool {
	for _, t := range plan.Tasks {
		if !t.Done && t.Claim != "" {
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

// spliceOwn expands an entry abbreviated against the previous one (a bare name takes its directory); an entry a
// committed file already matches stays as is.
func spliceOwn(prev, entry string, files []string) string {
	first, _, ok := strings.Cut(entry, "/")
	if !ok {
		return spliceBare(prev, entry, files)
	}
	if prev == "" || first == "" || matchesCommitted(entry, files) {
		return entry
	}
	segs := strings.Split(prev, "/")
	for i := len(segs) - 2; i >= 1; i-- {
		if segs[i] == first {
			return strings.Join(segs[:i], "/") + "/" + entry
		}
	}
	return entry
}

// spliceBare puts a bare file name beside the previous entry, unless a committed file is literally that name.
func spliceBare(prev, entry string, files []string) string {
	if prev == "" || entry == "" || slices.Contains(files, entry) || !strings.Contains(prev, "/") {
		return entry
	}
	return path.Dir(prev) + "/" + entry
}

// matchesCommitted reports whether a committed file is entry or lives under it.
func matchesCommitted(entry string, files []string) bool {
	for _, f := range files {
		if filepath.ToSlash(f) == entry || strings.HasPrefix(filepath.ToSlash(f), strings.TrimSuffix(entry, "/")+"/") {
			return true
		}
	}
	return false
}

// isDirOwn reports whether an owns entry names a directory: a trailing slash, or a pathed name without an extension.
func isDirOwn(entry string) bool {
	if strings.HasSuffix(entry, "/") {
		return true
	}
	base := path.Base(entry)
	return strings.Contains(entry, "/") && !strings.Contains(base, ".") && !strings.ContainsAny(base, "*?[")
}

// Covers reports which of owns a commit's files touch: all entries and/or at least one; a directory entry never
// completes a task alone, so all needs at least one file entry.
func Covers(owns, files []string) (all, any bool) {
	if len(owns) == 0 {
		return false, false
	}
	all = true
	hasFile := false
	prev := ""
	for _, own := range owns {
		entry := filepath.ToSlash(strings.TrimSpace(ownNote.ReplaceAllString(own, "")))
		entry = spliceOwn(prev, entry, files)
		prev = entry
		dir := strings.TrimSuffix(entry, "/")
		hasFile = hasFile || !isDirOwn(entry)
		touched := touchesEntry(entry, dir, files)
		if touched {
			any = true
		} else {
			all = false
		}
	}
	return all && hasFile, any
}

// touchesEntry reports whether any file is entry, lives under dir, or matches entry as a glob.
func touchesEntry(entry, dir string, files []string) bool {
	for _, f := range files {
		file := filepath.ToSlash(f)
		if entry != "" && (file == entry || file == dir || strings.HasPrefix(file, dir+"/")) {
			return true
		}
		if ok, err := path.Match(entry, file); err == nil && ok {
			return true
		}
	}
	return false
}

// planScore is how well a commit range matches one plan: tasks fully owned by it, then tasks with a file entry touched.
type planScore struct{ exact, touched int }

// below reports whether s scores lower than o: fewer exact tasks, or equal exact and fewer touched.
func (s planScore) below(o planScore) bool {
	return s.exact < o.exact || s.exact == o.exact && s.touched < o.touched
}

// scorePlan counts open tasks the range's union of files completes (exact) or touches through a file entry.
func scorePlan(plan Plan, files []string) (s planScore) {
	for _, t := range plan.Tasks {
		if t.Done || len(t.Owns) == 0 {
			continue
		}
		if all, _ := Covers(t.Owns, files); all {
			s.exact++
			continue
		}
		for _, o := range t.Owns {
			entry := filepath.ToSlash(strings.TrimSpace(ownNote.ReplaceAllString(o, "")))
			if isDirOwn(entry) {
				continue
			}
			if _, any := Covers([]string{o}, files); any {
				s.touched++
				break
			}
		}
	}
	return s
}

// Select picks the one plan under home (dated, not in skip) whose tasks the commits' files match best; plans tied on the
// best score narrow to the one whose claim names worktree, else none is chosen and tied lists them.
func Select(home, worktree string, skip []string, commits []Commit) (plan string, tied []string) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return "", nil
	}
	var files []string
	for _, c := range commits {
		files = append(files, c.Files...)
	}
	all, newestOpen := loadDatedPlans(entries, home, skip)
	cands, claimed := bestCandidates(all, newestOpen, worktree, files)
	if len(cands) == 1 {
		return cands[0], nil
	}
	var linked []string
	for _, p := range cands {
		if claimed[p] {
			linked = append(linked, p)
		}
	}
	if len(linked) == 1 {
		return linked[0], nil
	}
	return "", cands
}

type datedPlan struct {
	path string
	plan Plan
}

// loadDatedPlans parses the dated, non-skipped plans and returns them with the newest date holding an open owned task.
func loadDatedPlans(entries []os.DirEntry, home string, skip []string) (all []datedPlan, newestOpen string) {
	for _, e := range entries {
		p := filepath.Join(home, e.Name())
		if !e.Type().IsRegular() || !datedName.MatchString(e.Name()) || slices.Contains(skip, p) {
			continue
		}
		parsed, err := Parse(p)
		if err != nil {
			continue
		}
		all = append(all, datedPlan{p, parsed})
		if hasOpenOwnedTask(parsed) && e.Name()[:10] > newestOpen {
			newestOpen = e.Name()[:10]
		}
	}
	return all, newestOpen
}

// bestCandidates returns the plans tied on the best score of the newest touched date, with whether each claims worktree.
func bestCandidates(all []datedPlan, newestOpen, worktree string, files []string) (cands []string, claimed map[string]bool) {
	var best planScore
	var bestDate string
	claimed = map[string]bool{}
	for _, d := range all {
		name := filepath.Base(d.path)
		if name[:10] != newestOpen && !hasLiveClaim(d.plan) { // a stale plan sharing a file is never ticked
			continue
		}
		s := scorePlan(d.plan, files)
		if s == (planScore{}) || name[:10] < bestDate {
			continue
		}
		if name[:10] > bestDate { // the newest touched plan wins; scores compare only within one date
			bestDate, best, cands, claimed = name[:10], planScore{}, nil, map[string]bool{}
		}
		if s.below(best) {
			continue
		}
		if s != best {
			best, cands, claimed = s, nil, map[string]bool{}
		}
		cands = append(cands, d.path)
		claimed[d.path] = worktree != "" && claimsWorktree(d.plan, worktree)
	}
	return cands, claimed
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
			out = append(out, summarize(plan))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// summarize counts plan's task states and lists its claims.
func summarize(plan Plan) Summary {
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
	return s
}

// TemplatePath is the plan template every plan starts from, under the harness config dir.
func TemplatePath(configDir string) string {
	return filepath.Join(configDir, "docs", "templates", "plan.md")
}

// Skeleton reads the plan template; a file without its "# Plan:" heading is an error, not a template.
func Skeleton(configDir string) (string, error) {
	src := TemplatePath(configDir)
	b, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if !strings.Contains(string(b), "# Plan:") {
		return "", fmt.Errorf("plans: %s: no # Plan: heading", src)
	}
	return string(b), nil
}

// CommitFiles lists the files sha changed in the repo at dir.
func CommitFiles(ctx context.Context, dir, sha string) ([]string, error) {
	raw, err := exec.CommandContext(ctx, "git", "-C", dir, "show", "-z", "--name-only", "--format=", sha).Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s: %w", sha, err)
	}
	var files []string
	for f := range strings.SplitSeq(string(raw), "\x00") {
		if f = strings.TrimPrefix(f, "\n"); f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// RangeCommits lists rng's non-merge commits oldest first, each with a 7-char sha and its changed files.
func RangeCommits(ctx context.Context, dir, rng string) ([]Commit, error) {
	if !strings.Contains(rng, "..") || strings.HasPrefix(rng, "-") {
		return nil, fmt.Errorf("range %q: want <a>..<b>", rng)
	}
	raw, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-list", "--reverse", "--no-merges", rng, "--").Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-list %s: %w", rng, err)
	}
	var commits []Commit
	for sha := range strings.FieldsSeq(string(raw)) {
		files, err := CommitFiles(ctx, dir, sha)
		if err != nil {
			return nil, err
		}
		commits = append(commits, Commit{Sha: sha[:7], Files: files})
	}
	return commits, nil
}

// Dirs is the repo's Home by default, else the plans home plus every direct repo subdir.
func Dirs(all bool) []string {
	root := filepath.Join(paths.ConfigDir(), "plans")
	if !all {
		cwd, err := os.Getwd()
		if err != nil {
			return []string{root}
		}
		top, _ := paths.CheckoutOf(cwd)
		if top == "" {
			top = cwd
		}
		return []string{Home(top)}
	}
	dirs := []string{root}
	entries, err := os.ReadDir(root)
	if err != nil {
		return dirs
	}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs
}
