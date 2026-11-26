// Package plans parses plan checklists and keeps their ticks, claims and status in one place.
package plans

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	ID         string // "Phase N.M", a leading lane id such as "3.2a", or a letter id such as "R3" or "D2"
	Title      string
	Owns       []string
	Sha        string
	Claim      string // inner text of a working:/finished: comment
	Partial    []string
	Reopened   string // trunk sha at the last reopen; a commit in its history never ticks the task again
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
	idPhase    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)^Phase\s+\d+(?:\.(?:\d+|[A-Z]))*[a-z]?\b`) })
	idDecision = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^D\d+\b`) })
	idLetter   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Z]\d+(?:\.\d+[a-z]?)?\b`) }) // S1, R2.1; D<n> stays a decision
	idLane     = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\d+(?:\.\d+)*[a-z]?\b`) })
	datedName  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-.*\.md$`) })
	claimRe    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)<!--\s*(?:working|finished):.*?-->`) })
	partialRe  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)<!--\s*partly landed:\s*(.*?)\s*-->`) })
	reopenRe   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)<!--\s*reopened\s+([0-9a-f]{7,40})\s*-->`) })
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
	p := Plan{Path: path}
	if !bytes.Contains(b, []byte("[ ] ")) && !bytes.Contains(b, []byte("[x] ")) && !bytes.Contains(b, []byte("[X] ")) {
		return p
	}
	lines := strings.Split(string(b), "\n")
	var open fenceOpen // the fence now open, zero when none
	for i := 0; i < len(lines); i++ {
		if ch, n, rest := fenceMarker(lines[i]); n > 0 {
			open = open.next(ch, n, rest)
			continue
		}
		indent, mark, first, ok := checkItem(lines[i])
		if open.n > 0 || !ok {
			continue
		}
		cont, end := continuation(lines, i, indent)
		// the leading strike marker is stripped so a struck task keeps its id
		text := strings.TrimPrefix(first+cont, "~~")
		t := Task{Start: i, End: end, Done: mark != ' ', Dropped: struck(first)}
		if t.Dropped { // a strike rewrap leaves ~~ inside attribute values
			text = strings.ReplaceAll(text, "~~", "")
		}
		t.ID, t.Title, t.Owns, t.Sha, t.Claim, t.Partial = parseTaskText(text)
		if m := reopenRe().FindStringSubmatch(text); m != nil {
			t.Reopened = m[1]
		}
		p.Tasks = append(p.Tasks, t)
		i = end
	}
	return p
}

// checkItem is checkLine's match without the regex: leading whitespace width, the box mark and the text after it.
func checkItem(line string) (indent int, mark byte, rest string, ok bool) {
	for indent < len(line) && strings.IndexByte("\t\n\f\r ", line[indent]) >= 0 {
		indent++
	}
	l := line[indent:]
	if len(l) < 6 || l[0] != '-' && l[0] != '*' || l[1:3] != " [" || l[4:6] != "] " || strings.IndexByte(" xX", l[3]) < 0 {
		return 0, 0, "", false
	}
	return indent, l[3], l[6:], true
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
	var clean strings.Builder
	last := 0
	for _, s := range commentRe().FindAllStringSubmatchIndex(text, -1) {
		clean.WriteString(text[last:s[0]])
		clean.WriteByte(' ')
		last = s[1]
		inner := strings.TrimSpace(text[s[2]:s[3]])
		switch {
		case claimWord().MatchString(inner):
			claim = inner
		case partialKey().MatchString(inner):
			partial = append(partial, textutil.SplitCSV(partialKey().FindStringSubmatch(inner)[1])...)
		}
	}
	clean.WriteString(text[last:])
	cleaned := strings.Join(strings.Fields(clean.String()), " ")
	if m := shaParen().FindStringSubmatch(cleaned); m != nil {
		sha = m[1]
		cleaned = strings.TrimSpace(cleaned[:len(cleaned)-len(m[0])])
	} else if m := shaDash().FindStringSubmatch(cleaned); m != nil {
		sha = m[1]
		cleaned = strings.TrimSpace(cleaned[:len(cleaned)-len(m[0])])
	}
	title = cleaned
	id = cmp.Or(idPhase().FindString(cleaned), idLane().FindString(cleaned), idLetter().FindString(cleaned))
	if locs := ownsKey().FindAllStringIndex(cleaned, -1); len(locs) > 0 { // regexp offsets index cleaned itself; ToLower can change byte length
		masked := ownParen().ReplaceAllStringFunc(cleaned[locs[len(locs)-1][1]:], func(n string) string { return strings.ReplaceAll(n, ",", "\x00") })
		for _, o := range textutil.SplitCSV(masked) { // a comma inside a "(note, more)" does not split
			owns = append(owns, strings.ReplaceAll(o, "\x00", ","))
		}
	}
	return id, title, owns, sha, claim, partial
}

// Home is the plans directory for repoRoot: plans/<Name(repoRoot)>.
func Home(repoRoot string) string {
	return filepath.Join(paths.ConfigDir(), "plans", Name(repoRoot))
}

// nameCache memoizes Name per config dir and checkout, since hooks resolve it on every plan write.
var nameCache sync.Map

// Name is repoRoot's plans folder: the repo's name, owner-qualified when another repo claimed it first.
func Name(repoRoot string) string {
	repo := paths.RepoName(repoRoot)
	if repo.Key == "" {
		return repo.Name
	}
	ck := paths.ConfigDir() + "\x00" + repo.Key
	if v, ok := nameCache.Load(ck); ok {
		return v.(string)
	}
	name := claimName(repo)
	nameCache.Store(ck, name)
	return name
}

// peekName is Name without claiming, for a lookup that must not register repos it only inspects.
func peekName(repoRoot string) string {
	repo := paths.RepoName(repoRoot)
	reg, err := readNames()
	if repo.Key == "" || err != nil {
		return repo.Name
	}
	name, _ := pickName(reg, repo)
	return name
}

// PeekHome is Home without claiming the name: a read-only lookup writes nothing.
func PeekHome(repoRoot string) string {
	return filepath.Join(paths.ConfigDir(), "plans", peekName(repoRoot))
}

// NamesFile records which repo key owns each plans folder name, so a later same-named repo is owner-qualified.
func NamesFile() string { return filepath.Join(paths.StateDir(), "plans", "names.json") }

// claimName records repo under its name, or its owner-qualified name on a clash; an unreadable registry writes nothing.
func claimName(repo paths.Repo) string {
	reg, err := readNames()
	if err != nil {
		return repo.Name
	}
	name, claim := pickName(reg, repo)
	if !claim {
		return name
	}
	_ = paths.WithFileLockWithin(NamesFile(), time.Second, func() error { // a lost claim only costs a later re-claim
		reg, err := readNames()
		if err != nil {
			return err
		}
		if name, claim = pickName(reg, repo); claim {
			return writeNames(reg, name, repo.Key)
		}
		return nil
	})
	return name
}

// pickName is repo's name under reg and whether reg lacks that claim.
func pickName(reg map[string]string, repo paths.Repo) (string, bool) {
	if owner, ok := reg[repo.Name]; !ok || owner == repo.Key || repo.Owner == "" {
		return repo.Name, !ok
	}
	q := paths.Slug(repo.Owner+"-"+repo.Name, "._-", 0, repo.Name)
	return q, reg[q] == ""
}

func readNames() (map[string]string, error) {
	data, err := os.ReadFile(NamesFile())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	reg := map[string]string{}
	return reg, json.Unmarshal(data, &reg)
}

// writeNames is the registry's one write site (the schemas registry keys on it).
func writeNames(reg map[string]string, name, key string) error {
	next := maps.Clone(reg)
	next[name] = key
	return paths.WriteJSON(NamesFile(), next)
}

// PlanRepo is the checkout whose plans home holds plan: cwdTop, a path-keyed registry repo or the harness; else "".
func PlanRepo(plan, cwdTop string) string {
	name := filepath.Base(filepath.Dir(plan))
	cands := []string{cwdTop}
	if reg, err := readNames(); err == nil && filepath.IsAbs(reg[name]) {
		cands = append(cands, reg[name]) // a repo without an origin is keyed by its main checkout's path
	}
	for _, c := range append(cands, paths.ConfigDir()) {
		if top, main := paths.CheckoutOf(c); top != "" && peekName(main) == name {
			return main
		}
	}
	return ""
}

// homeOf is the plan's directory when it is a direct child of the plans root, else the @home folder.
func homeOf(plan string) string {
	dir := filepath.Dir(plan)
	if paths.SamePath(filepath.Dir(dir), filepath.Join(paths.ConfigDir(), "plans")) {
		return dir
	}
	return Home("")
}

// ResolvePlan confines explicit writes to dated regular plans in this repository's home.
func ResolvePlan(root, plan string) (string, error) {
	home := Home(root)
	if abs, err := filepath.Abs(plan); err == nil && root == "" && filepath.Base(plan) != plan {
		home = homeOf(abs) // outside a checkout the plan's own dir is the home, if it is under the plans root
	}
	refusal := fmt.Errorf("plans: --plan is not a dated plan in %s (regular non-symlink required)", home)
	if filepath.Base(plan) == plan {
		if !strings.HasSuffix(plan, ".md") {
			plan += ".md"
		}
		plan = filepath.Join(home, plan)
	}
	if abs, err := filepath.Abs(plan); err == nil {
		plan = abs // a relative --plan must compare against the absolute home
	}
	info, err := os.Lstat(plan)
	if err != nil || !info.Mode().IsRegular() || !datedName().MatchString(filepath.Base(plan)) {
		return "", refusal
	}
	real, err := filepath.EvalSymlinks(plan)
	if err != nil {
		return "", refusal
	}
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil || !paths.SamePath(filepath.Dir(real), canonicalHome) {
		return "", refusal
	}
	return filepath.Abs(plan)
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
	all := loadPlans(entries, home, skip, mayHaveOpenOwned)
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
	// with no commit to check against, a pathed entry is taken as written
	if prev == "" || first == "" || files == nil || matchesCommitted(entry, files) {
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
	// "none" and prose such as "and their tests" name no file, so they stay as written
	if prev == "" || entry == "" || strings.EqualFold(entry, "none") || strings.Contains(entry, " ") ||
		slices.Contains(files, entry) || !strings.Contains(prev, "/") {
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

type datedPlan struct {
	path string
	plan Plan
}

// loadDatedPlans parses the dated, non-skipped plans in home.
func loadDatedPlans(entries []os.DirEntry, home string, skip []string) []datedPlan {
	return loadPlans(entries, home, skip, nil)
}

// loadPlans is loadDatedPlans that parses only files keep accepts (nil keeps all).
func loadPlans(entries []os.DirEntry, home string, skip []string, keep func([]byte) bool) (all []datedPlan) {
	for _, e := range entries {
		p := filepath.Join(home, e.Name())
		if !e.Type().IsRegular() || !datedName().MatchString(e.Name()) || slices.Contains(skip, p) {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil || keep != nil && !keep(b) {
			continue
		}
		all = append(all, datedPlan{p, parseBytes(p, b)})
	}
	return all
}

// mayHaveOpenOwned is a cheap superset of hasOpenOwnedTask: an unstruck open item whose text has an owns: key.
func mayHaveOpenOwned(b []byte) bool {
	if !bytes.Contains(b, []byte("[ ] ")) {
		return false
	}
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		k := strings.Index(l, "[ ] ")
		if k < 2 {
			continue
		}
		if lead := strings.TrimLeft(l[:k], "\t\f\r "); lead != "- " && lead != "* " || struck(l[k+4:]) {
			continue
		}
		cont, _ := continuation(lines, i, k-2)
		if ownsKey().MatchString(l[k+4:] + cont) {
			return true
		}
	}
	return false
}

// Status summarizes every *.md directly in each dir that carries at least one task, sorted by path.
func Status(dirs ...string) ([]Summary, error) {
	files, err := planFiles(dirs)
	if err != nil {
		return nil, err
	}
	plans, errs := make([]Plan, len(files)), make([]error, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, runtime.GOMAXPROCS(0)))
	for i, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, f string) {
			defer wg.Done()
			plans[i], errs[i] = Parse(f)
			<-sem
		}(i, f)
	}
	wg.Wait()
	var out []Summary
	for _, plan := range plans {
		if len(plan.Tasks) > 0 {
			out = append(out, summarize(plan))
		}
	}
	slices.SortFunc(out, func(a, b Summary) int { return strings.Compare(a.Path, b.Path) })
	if err := errors.Join(errs...); err != nil {
		return nil, err // as before: any unreadable plan fails the whole status
	}
	return out, nil
}

// planFiles lists the regular *.md files directly in each existing dir.
func planFiles(dirs []string) ([]string, error) {
	var files []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, e := range entries {
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".md") { // a symlink may point outside plans/
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
	}
	return files, nil
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

// rangeCommits lists rng's commits oldest first, merges included (one may carry the trailer), each with its files.
func rangeCommits(ctx context.Context, dir, rng string) ([]Commit, error) {
	if !strings.Contains(rng, "..") || strings.HasPrefix(rng, "-") {
		return nil, fmt.Errorf("range %q: want <a>..<b>", rng)
	}
	raw, err := proc.RunGitInherit(ctx, dir, "rev-list", "--reverse", rng, "--")
	if err != nil {
		return nil, fmt.Errorf("git rev-list %s: %w", rng, err)
	}
	var commits []Commit
	for sha := range strings.FieldsSeq(raw) {
		c, err := readCommit(ctx, dir, sha)
		if err != nil {
			return nil, err
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// Dirs is the cwd repo's Home by default, else every repo folder under the plans root (the root holds only drafts).
func Dirs(all bool) []string {
	if !all {
		cwd, _ := os.Getwd() // no cwd is outside any repo
		return []string{Home(cwd)}
	}
	root := filepath.Join(paths.ConfigDir(), "plans")
	var dirs []string
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

// OpenPlan is a recent dated plan that still has open tasks: its slug, progress and first open task.
type OpenPlan struct {
	Slug          string
	Ticked, Total int
	Next          string
}

// OpenPlans lists repoRoot's recent dated plans with open tasks, newest first, at most limit.
func OpenPlans(repoRoot string, limit int) []OpenPlan {
	home := Home(repoRoot)
	entries, _ := os.ReadDir(home) // no plans home is no plans
	cutoff := now().AddDate(0, 0, -recentDays).Format(time.DateOnly)
	entries = slices.DeleteFunc(entries, func(e os.DirEntry) bool { return len(e.Name()) < 10 || e.Name()[:10] < cutoff })
	var out []OpenPlan
	for _, d := range slices.Backward(loadDatedPlans(entries, home, nil)) {
		base := filepath.Base(d.path)
		s, op := summarize(d.plan), OpenPlan{Slug: strings.TrimSuffix(base, ".md")}
		op.Ticked, op.Total = s.Ticked, s.Total
		if i := slices.IndexFunc(d.plan.Tasks, open); i >= 0 {
			op.Next = cmp.Or(d.plan.Tasks[i].ID, d.plan.Tasks[i].Title)
			out = append(out, op)
		}
		if len(out) == limit {
			break
		}
	}
	return out
}

// Archive moves every dated plan in root's home whose tasks are all ticked or struck into the memory store.
func Archive(ctx context.Context, root string, dryRun bool) ([]Change, []Finding, error) {
	dated, _, err := auditPlans(Home(root), true)
	if err != nil {
		return nil, nil, err
	}
	dest := archiveDir(root)
	changes, findings := []Change{}, []Finding{}
	for _, d := range dated {
		if err := ctx.Err(); err != nil {
			return changes, findings, err
		}
		err := paths.WithFileLock(plansLock(), func() error {
			c, f, err := archivePlan(d.path, dest, dryRun)
			if c != nil {
				changes = append(changes, *c)
			}
			if f != nil {
				findings = append(findings, *f)
			}
			return err
		})
		if err != nil {
			return changes, findings, err
		}
	}
	return changes, findings, nil
}

// archivePlan requires the plans lock through the eligibility check, move and audit rollback.
func archivePlan(planPath, dest string, dryRun bool) (*Change, *Finding, error) {
	b, err := os.ReadFile(planPath)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	p := parseBytes(planPath, b)
	s, ok := archivable(p)
	if !ok {
		return nil, nil, nil
	}
	to := filepath.Join(dest, filepath.Base(planPath))
	if _, err := os.Lstat(to); err == nil {
		return nil, &Finding{strings.TrimSuffix(filepath.Base(planPath), ".md"), "", "archive target exists", to}, nil
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	c := Change{
		Time: now(), Plan: planPath, Action: "archive", Rule: "all tasks closed",
		Evidence: fmt.Sprintf("%d/%d ticked, %d struck", s.Ticked, len(p.Tasks), s.Dropped),
		Reopen:   fmt.Sprintf("mv %s %s", to, planPath),
	}
	if !dryRun {
		if err := movePlan(c, to); err != nil {
			return nil, nil, err
		}
	}
	return &c, nil, nil
}

// archivable is a plan with tasks, none open and none still claimed.
func archivable(p Plan) (Summary, bool) {
	s := summarize(p)
	return s, s.Ticked == s.Total && s.Total+s.Dropped > 0 && !slices.ContainsFunc(p.Tasks, func(t Task) bool { return t.Claim != "" })
}

func archiveDir(root string) string {
	return filepath.Join(paths.MemoryStoreDir(), "archive", "plans", Name(root))
}

// movePlan renames c.Plan to to and logs c; a failed log moves the plan back, so every move has its Reopen line.
func movePlan(c Change, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	if err := moveFile(c.Plan, to); err != nil {
		return err
	}
	if err := appendAuditLog(c); err != nil {
		return errors.Join(err, moveFile(to, c.Plan))
	}
	return nil
}

// rename is a seam so a test can simulate a cross-device rename.
var rename = os.Rename

// moveFile renames from to to; across devices it copies (mode and mtime kept), then removes from.
func moveFile(from, to string) error {
	err := rename(from, to)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := writePlan(to, b, info.Mode().Perm()); err != nil {
		return err
	}
	if err := errors.Join(os.Chtimes(to, info.ModTime(), info.ModTime()), os.Remove(from)); err != nil {
		return errors.Join(err, os.Remove(to))
	}
	return nil
}

// bornFile holds each dated plan's first-write time as "path\tunix", first line wins; rewrites move the mtime.
func bornFile() string { return filepath.Join(paths.StateDir(), "plans-born.tsv") }

// RecordBorn pins a dated plan's first-write time when it has no record yet; a Write tool call bypasses writePlan.
func RecordBorn(plan string) { planBorn(plan, true) }

// planBorn is plan's first-write time (record, else mtime of a non-empty file), never before its day; record pins it.
func planBorn(plan string, record bool) time.Time {
	key := paths.CanonDeep(plan)
	if born, ok := recordedBorn(key); ok {
		return born
	}
	born := time.Now()
	if fi, err := os.Stat(plan); err == nil && fi.Size() > 0 {
		born = fi.ModTime() // an atomic rewrite moves mtime later, which can only skip a tick
	}
	if !datedName().MatchString(filepath.Base(plan)) {
		return born
	}
	if day, err := time.ParseInLocation(time.DateOnly, filepath.Base(plan)[:10], time.Local); err == nil && born.Before(day) {
		born = day
	}
	if record {
		return pinBorn(key, born)
	}
	return born
}

// pinBorn appends key's born time under a lock unless a record exists, and returns the winning one.
func pinBorn(key string, born time.Time) time.Time {
	if !datedName().MatchString(filepath.Base(key)) {
		return born
	}
	_ = paths.WithFileLock(bornFile(), func() error {
		if won, ok := recordedBorn(key); ok {
			born = won
		} else {
			appendBorn(key, born)
		}
		return nil
	})
	return born
}

func recordedBorn(plan string) (time.Time, bool) {
	f, err := os.Open(bornFile())
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	for sc := bufio.NewScanner(f); sc.Scan(); {
		path, unix, ok := strings.Cut(sc.Text(), "\t")
		if !ok || path != plan {
			continue
		}
		if n, err := strconv.ParseInt(unix, 10, 64); err == nil {
			return time.Unix(n, 0), true
		}
	}
	return time.Time{}, false
}

// appendBorn is best effort: a lost record only falls back to the later-or-equal mtime.
func appendBorn(plan string, born time.Time) {
	if strings.ContainsAny(plan, "\t\n") || os.MkdirAll(paths.StateDir(), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(bornFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s\t%d\n", plan, born.Unix())
}
