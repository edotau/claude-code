// Package plans parses plan checklists and keeps their ticks, claims and status in one place.
package plans

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// Task is one checklist item: its line span, state, identity, owned files and plan metadata.
type Task struct {
	Start, End int // 0-based line indexes, inclusive
	Done       bool
	Dropped    bool   // struck-out tasks are neither done nor open, so they leave the total
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
	Dropped int
	Total   int
	Claims  []string
	Partial int
}

var (
	checkLine  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(\s*)[-*] \[([ xX])\] (.*)$`) })
	listLine   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`) })
	commentRe  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?s)<!--(.*?)-->`) })
	shaParen   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\s*\(([0-9a-f]{7,40})\)\s*$`) })
	shaDash    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\s+(?:—|--|-)\s+([0-9a-f]{7,40})\s*$`) })
	idPhase    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)^Phase\s+\d+(?:\.\d+)*[a-z]?\b`) })
	idLane     = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\d+(?:\.\d+)*[a-z]?\b`) })
	datedName  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-.*\.md$`) })
	claimRe    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)<!--\s*(?:working|finished):.*?-->`) })
	partialRe  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)<!--\s*partly landed:\s*(.*?)\s*-->`) })
	claimWord  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)^(?:working|finished):`) })
	partialKey = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)^partly landed:\s*(.*)$`) })
	ownsKey    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)owns:`) })
	ownNote    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\s*\([^)]*\)\s*$`) }) // "cli.go (verb row only)" owns cli.go
	ownParen   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\([^)]*\)`) })
)

// Parse reads and parses path's checklist.
func Parse(path string) (Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Plan{Path: path}, err
	}
	return parseBytes(path, b), nil
}

// parseBytes parses b; a checklist item runs from its bullet through its more-indented continuation lines.
func parseBytes(path string, b []byte) Plan {
	lines := strings.Split(string(b), "\n")
	p := Plan{Path: path}
	var open fenceOpen // the fence now open, zero when none
	check := checkLine()
	for i := 0; i < len(lines); i++ {
		if ch, n, rest := fenceMarker(lines[i]); n > 0 {
			open = open.next(ch, n, rest)
			continue
		}
		m := check.FindStringSubmatch(lines[i])
		if open.n > 0 || m == nil {
			continue
		}
		cont, end := continuation(lines, i, len(m[1]))
		// the leading strike marker is stripped so a struck task keeps its id
		text := strings.TrimPrefix(m[3]+cont, "~~")
		t := Task{Start: i, End: end, Done: m[2] == "x" || m[2] == "X", Dropped: struck(m[3])}
		t.ID, t.Title, t.Owns, t.Sha, t.Claim, t.Partial = parseTaskText(text)
		p.Tasks = append(p.Tasks, t)
		i = end
	}
	return p
}

// struck reports whether a checklist item's first line opens with "~~" and closes it again later on the same line.
func struck(first string) bool {
	return strings.HasPrefix(first, "~~") && strings.Contains(first[2:], "~~")
}

// continuation joins the wrapped lines after lines[i] that are indented past indent, and returns the last line used.
func continuation(lines []string, i, indent int) (string, int) {
	end := i
	var cont strings.Builder
	list := listLine()
	for j := i + 1; j < len(lines); j++ {
		line := lines[j]
		if strings.TrimSpace(line) == "" || list.MatchString(line) || isFence(line) {
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
	for _, m := range commentRe().FindAllStringSubmatch(text, -1) {
		inner := strings.TrimSpace(m[1])
		switch {
		case claimWord().MatchString(inner):
			claim = inner
		case partialKey().MatchString(inner):
			partial = append(partial, textutil.SplitCSV(partialKey().FindStringSubmatch(inner)[1])...)
		}
	}
	cleaned := strings.Join(strings.Fields(commentRe().ReplaceAllString(text, " ")), " ")
	if m := shaParen().FindStringSubmatch(cleaned); m != nil {
		sha = m[1]
		cleaned = strings.TrimSpace(cleaned[:len(cleaned)-len(m[0])])
	} else if m := shaDash().FindStringSubmatch(cleaned); m != nil {
		sha = m[1]
		cleaned = strings.TrimSpace(cleaned[:len(cleaned)-len(m[0])])
	}
	title = cleaned
	if id = idPhase().FindString(cleaned); id == "" {
		id = idLane().FindString(cleaned)
	}
	if locs := ownsKey().FindAllStringIndex(cleaned, -1); len(locs) > 0 { // regexp offsets index cleaned itself; ToLower can change byte length
		masked := ownParen().ReplaceAllStringFunc(cleaned[locs[len(locs)-1][1]:], func(n string) string { return strings.ReplaceAll(n, ",", "\x00") })
		for _, o := range textutil.SplitCSV(masked) { // a comma inside a "(note, more)" does not split
			owns = append(owns, strings.ReplaceAll(o, "\x00", ","))
		}
	}
	return id, title, owns, sha, claim, partial
}

// Home is the plans directory for repoRoot, tagged like every other repo-scoped artifact.
func Home(repoRoot string) string {
	return filepath.Join(paths.ConfigDir(), "plans", paths.RepoSlug(repoRoot))
}

// WorktreeName is a linked worktree's name (matched against task claims), or "" for the main checkout.
func WorktreeName(top string) string {
	if paths.MainCheckout(top) != top {
		return filepath.Base(top)
	}
	return ""
}

// Active picks home's tick target and its parsed plan: a dated plan with an open owned task, never a skipped draft.
func Active(home, worktree string, skip []string) (string, Plan, bool) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return "", Plan{}, false
	}
	all := loadDatedPlans(entries, home, skip)
	best, bestPlan, bestMod, bestClaimed := "", Plan{}, time.Time{}, false
	for _, d := range all {
		info, err := os.Stat(d.path)
		if err != nil || !hasOpenOwnedTask(d.plan) {
			continue
		}
		claimed := worktree != "" && claimsWorktree(d.plan, worktree)
		if best != "" && !beatsActive(claimed, info.ModTime(), bestClaimed, bestMod) {
			continue
		}
		best, bestPlan, bestMod, bestClaimed = d.path, d.plan, info.ModTime(), claimed
	}
	return best, bestPlan, best != ""
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
	return slices.ContainsFunc(plan.Tasks, func(t Task) bool { return open(t) && claimNamesWorktree(t.Claim, worktree) })
}

// open reports whether t still has to land: neither ticked nor struck.
func open(t Task) bool { return !t.Done && !t.Dropped }

// hasOpenOwnedTask reports whether plan still has a task to land.
func hasOpenOwnedTask(plan Plan) bool {
	return slices.ContainsFunc(plan.Tasks, func(t Task) bool { return open(t) && len(t.Owns) > 0 })
}

// spliceOwn expands an entry abbreviated against the previous one (a bare name takes its directory).
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

// covers reports whether a commit touches all and/or any of owns; a directory entry alone never completes a task.
func covers(root string, owns, files []string) (all, any bool) {
	if len(owns) == 0 {
		return false, false
	}
	all = true
	hasFile := false
	for _, entry := range ownEntries(owns, files) {
		hasFile = hasFile || !isDirOwn(entry)
		if touchesEntry(root, entry, strings.TrimSuffix(entry, "/"), files) {
			any = true
		} else {
			all = false
		}
	}
	return all && hasFile, any
}

// ownEntries normalizes owns: notes stripped, slashes forward, each entry spliced against the previous one.
func ownEntries(owns, files []string) []string {
	out := make([]string, len(owns))
	prev := ""
	for i, own := range owns {
		prev = spliceOwn(prev, filepath.ToSlash(strings.TrimSpace(ownNote().ReplaceAllString(own, ""))), files)
		out[i] = prev
	}
	return out
}

// absCovers reports whether an absolute entry names file, by root-relative path or repo-relative suffix.
func absCovers(root, entry, dir, file string) bool {
	if !path.IsAbs(entry) {
		return false
	}
	if root != "" {
		if rel, ok := strings.CutPrefix(entry, strings.TrimSuffix(filepath.ToSlash(root), "/")+"/"); ok {
			rel = strings.TrimSuffix(rel, "/")
			return file == rel || strings.HasPrefix(file, rel+"/")
		}
	}
	if strings.HasSuffix(entry, "/"+file) {
		return true
	}
	for d := path.Dir(file); d != "." && d != "/"; d = path.Dir(d) {
		if strings.HasSuffix(dir, "/"+d) {
			return true
		}
	}
	return false
}

// touchesEntry reports whether any file is entry, lives under dir, or matches entry as a glob.
func touchesEntry(root, entry, dir string, files []string) bool {
	for _, f := range files {
		file := filepath.ToSlash(f)
		if entry != "" && (file == entry || file == dir || strings.HasPrefix(file, dir+"/")) {
			return true
		}
		if absCovers(root, entry, dir, file) {
			return true
		}
		if ok, err := path.Match(entry, file); err == nil && ok {
			return true
		}
	}
	return false
}

// touchedFiles is the set of files an open owned task of plan names through a file entry.
func touchedFiles(root string, plan Plan, files []string) map[string]bool {
	out := map[string]bool{}
	for _, t := range plan.Tasks {
		if !open(t) || len(t.Owns) == 0 {
			continue
		}
		for _, entry := range ownEntries(t.Owns, files) {
			if isDirOwn(entry) {
				continue
			}
			for _, f := range files {
				if touchesEntry(root, entry, strings.TrimSuffix(entry, "/"), []string{f}) {
					out[f] = true
				}
			}
		}
	}
	return out
}

// Select returns every dated plan whose open task owns a touched file; a claim breaks a shared-file tie, else tied.
func Select(home, root, worktree string, skip []string, commits []Commit) (selected, tied []string) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, nil
	}
	var files []string
	for _, c := range commits {
		files = append(files, c.Files...)
	}
	all := loadDatedPlans(entries, home, skip)
	var cands []datedPlan
	var touched []map[string]bool
	for _, d := range all {
		d.plan.Tasks = slices.DeleteFunc(slices.Clone(d.plan.Tasks), func(t Task) bool { return !eligible(d.path, worktree)(t) })
		if t := touchedFiles(root, d.plan, files); len(t) > 0 {
			cands, touched = append(cands, d), append(touched, t)
		}
	}
	keep := make([]bool, len(cands))
	for i := range cands {
		keep[i] = true
		for j := range cands {
			if i != j && sharesFile(touched[i], touched[j]) && worktree != "" &&
				claimsWorktree(cands[j].plan, worktree) && !claimsWorktree(cands[i].plan, worktree) {
				keep[i] = false
			}
		}
	}
	for i := range cands {
		if !keep[i] {
			continue
		}
		selected = append(selected, cands[i].path)
		for j := range cands {
			if j != i && keep[j] && sharesFile(touched[i], touched[j]) {
				tied = append(tied, cands[i].path)
				break
			}
		}
	}
	return selected, tied
}

// recentDays is how many days old a plan's file-name date may be for a commit to tick or annotate it.
const recentDays = 7

// now is the clock behind plan recency, swapped in tests.
var now = time.Now

// eligible says which tasks of a plan a commit may tick: all in a recent plan, else the claimed ones.
func eligible(planPath, worktree string) func(Task) bool {
	cutoff := now().AddDate(0, 0, -recentDays).Format(time.DateOnly)
	recent := len(filepath.Base(planPath)) >= 10 && filepath.Base(planPath)[:10] >= cutoff
	return func(t Task) bool { return recent || claimNamesWorktree(t.Claim, cmp.Or(worktree, "main")) }
}

// sharesFile reports whether two file sets intersect.
func sharesFile(a, b map[string]bool) bool {
	for f := range a {
		if b[f] {
			return true
		}
	}
	return false
}

type datedPlan struct {
	path string
	plan Plan
}

// loadDatedPlans parses the dated, non-skipped plans in home.
func loadDatedPlans(entries []os.DirEntry, home string, skip []string) (all []datedPlan) {
	for _, e := range entries {
		p := filepath.Join(home, e.Name())
		if !e.Type().IsRegular() || !datedName().MatchString(e.Name()) || slices.Contains(skip, p) {
			continue
		}
		parsed, err := Parse(p)
		if err != nil {
			continue
		}
		all = append(all, datedPlan{p, parsed})
	}
	return all
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
	slices.SortFunc(out, func(a, b Summary) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// summarize counts plan's task states and lists its claims.
func summarize(plan Plan) Summary {
	s := Summary{Path: plan.Path}
	for _, t := range plan.Tasks {
		if t.Dropped {
			s.Dropped++
			continue
		}
		s.Total++
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

// CommitFiles lists the files sha changed in the repo at dir.
func CommitFiles(ctx context.Context, dir, sha string) ([]string, error) {
	raw, err := proc.RunGitInherit(ctx, dir, "show", "-z", "--name-only", "--format=", sha)
	if err != nil {
		return nil, fmt.Errorf("git show %s: %w", sha, err)
	}
	var files []string
	for f := range strings.SplitSeq(raw, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// rangeCommits lists rng's non-merge commits oldest first, each with a 7-char sha and its changed files.
func rangeCommits(ctx context.Context, dir, rng string) ([]Commit, error) {
	if !strings.Contains(rng, "..") || strings.HasPrefix(rng, "-") {
		return nil, fmt.Errorf("range %q: want <a>..<b>", rng)
	}
	raw, err := proc.RunGitInherit(ctx, dir, "rev-list", "--reverse", "--no-merges", rng, "--")
	if err != nil {
		return nil, fmt.Errorf("git rev-list %s: %w", rng, err)
	}
	var commits []Commit
	for sha := range strings.FieldsSeq(raw) {
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
