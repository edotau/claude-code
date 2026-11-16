package memory

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

const (
	flatSubject         = "flatten: one bank directory (layout flat 1)"
	unflatSubject       = "unflatten: per-repo layout"
	homeDir             = "home"
	kib                 = 1024
	mergedIndexMaxLines = 180 // the loader reads 200 lines / 25 KB: keep headroom
	mergedIndexMaxBytes = 22 * kib
)

// flatSubs are the shared store subdirs whose first path element carries the bank prefix when flat.
var flatSubs = []string{"archive", "handoffs", "reports", "knowledge", "worktrees", "waves"}

// ErrMigrateRefused marks a precondition or plan failure of an apply or revert (exit 2).
var ErrMigrateRefused = errors.New("refused")

// ErrMigrateBlocked marks a dry run whose apply would be refused (exit 1).
var ErrMigrateBlocked = errors.New("apply would be refused (see blockers)")

// MigrateOptions is one `memory migrate`; Format is text or json.
type MigrateOptions struct {
	DryRun, Revert bool
	Format         string
	Out            io.Writer
}

type migMove struct {
	From string `json:"from"`
	To   string `json:"to"` // "" for a MEMORY.md merged into the shared index
	SHA  string `json:"sha,omitempty"`
}

type migRewrite struct {
	File  string `json:"file"`
	Count int    `json:"count"`
}

type migSize struct {
	Lines int `json:"lines"`
	Bytes int `json:"bytes"`
}

type migReport struct {
	AlreadyFlat bool         `json:"already_flat"`
	Blockers    []string     `json:"blockers"`
	Moves       []migMove    `json:"moves"`
	Rewrites    []migRewrite `json:"rewrites"`
	MemoryMD    migSize      `json:"memory_md"`
	Unplanned   []string     `json:"unplanned"`
	Collisions  []string     `json:"collisions"`
	FilesBefore int          `json:"files_before"`
	FilesAfter  int          `json:"files_after"`
}

// flatPlan is the migration: the report plus the exact bytes to write and paths to delete.
type flatPlan struct {
	migReport
	writes    map[string][]byte
	dirs      []string // per-repo bank dirs, home included
	indexDirs []string // dirs that held a MEMORY.md
	harness   string
}

// migRecord is the revert record: everything --revert needs to invert the migration.
type migRecord struct {
	Time      string    `json:"time"`
	Reverting bool      `json:"reverting,omitempty"`
	Pre       string    `json:"pre"`
	Post      string    `json:"post"`
	Harness   string    `json:"harness"`
	Dirs      []string  `json:"dirs"`
	IndexDirs []string  `json:"index_dirs"`
	Links     []migLink `json:"links"`
	Moves     []migMove `json:"moves"`
}

type migLink struct {
	Link string `json:"link"`
	Old  string `json:"old"`
}

// Migrate flips the store from per-repo dirs to one flat directory, or back with Revert.
func Migrate(o MigrateOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Revert {
		return revertFlat(o)
	}
	return applyFlat(o)
}

func stampPresent() bool {
	b, _ := os.ReadFile(filepath.Join(StoreDir(), layoutFile))
	return strings.TrimSpace(string(b)) == layoutFlat
}

func refuse(blockers []string) error {
	return fmt.Errorf("%w: %s", ErrMigrateRefused, strings.Join(blockers, "; "))
}

// flatTarget is rel's flat path for bank dir; ok is false for a shape the migration does not know.
func flatTarget(dir, rel string) (string, bool) {
	pre := dashRun.ReplaceAllString(dir, "-") + "--"
	if dir == homeDir {
		pre = ""
	}
	parts := strings.Split(rel, "/")
	if ambiguousMigrationPath(rel) {
		return "", false
	}
	if len(parts) == 1 {
		name := parts[0]
		switch {
		case !strings.HasSuffix(name, ".md") || name == nativeIndexFile:
			return "", false
		case slices.Contains(bankFiles, name), dir != harnessStoreKey():
			return pre + name, true
		}
		return name, true
	}
	if !slices.Contains(flatSubs, parts[0]) {
		return "", false
	}
	return strings.Join(append([]string{parts[0], pre + parts[1]}, parts[2:]...), "/"), true
}

func ambiguousMigrationPath(rel string) bool {
	parts := strings.Split(rel, "/")
	return slices.Contains(parts, "") || slices.Contains(parts, ".") || slices.Contains(parts, "..") || strings.Contains(rel, "--")
}

func sharedTop(top string) bool {
	return top == ".git" || top == ".origins" || top == "receipts" || slices.Contains(flatSubs, top)
}

// planMigrate sorts tracked files into moves, unplanned shapes and target collisions; it reads nothing.
func planMigrate(files []string, harness string) *flatPlan {
	p := &flatPlan{harness: harness, writes: map[string][]byte{}}
	p.Blockers, p.Moves, p.Rewrites, p.Unplanned, p.Collisions = []string{}, []migMove{}, []migRewrite{}, []string{}, []string{}
	targets := map[string]string{}
	for _, f := range files {
		top, _, nested := strings.Cut(f, "/")
		if !nested || sharedTop(top) {
			targets[f] = f
		}
	}
	for _, f := range files {
		top, rel, nested := strings.Cut(f, "/")
		if !nested || sharedTop(top) {
			targets[f] = f
			continue
		}
		p.place(targets, f, top, rel)
	}
	for _, d := range p.dirs {
		if d != homeDir && !slices.ContainsFunc(bankFiles, func(n string) bool {
			return slices.ContainsFunc(p.Moves, func(m migMove) bool { return m.From == d+"/"+n })
		}) {
			p.Blockers = append(p.Blockers, d+" has no typed file, so the flat store would not see it as a bank; add one first")
		}
	}
	if _, dup := targets[nativeIndexFile]; dup && len(p.indexDirs) > 0 {
		p.Collisions = append(p.Collisions, nativeIndexFile+" exists beside the per-bank indexes")
	}
	slices.Sort(p.dirs)
	slices.Sort(p.indexDirs)
	p.FilesBefore = len(files)
	p.FilesAfter = len(targets) + 1 // the layout stamp
	if len(p.indexDirs) > 0 {
		p.FilesAfter++
	}
	return p
}

// place records one bank file as a move, a merged index, an unplanned shape or a collision.
func (p *flatPlan) place(targets map[string]string, f, top, rel string) {
	if !slices.Contains(p.dirs, top) {
		p.dirs = append(p.dirs, top)
	}
	if rel == nativeIndexFile {
		p.indexDirs = append(p.indexDirs, top)
		p.Moves = append(p.Moves, migMove{From: f})
		return
	}
	to, ok := flatTarget(top, rel)
	if !ok {
		p.Unplanned = append(p.Unplanned, f)
		return
	}
	if prev, dup := targets[to]; dup {
		p.Collisions = append(p.Collisions, prev+" + "+f+" -> "+to)
		return
	}
	targets[to] = f
	p.Moves = append(p.Moves, migMove{From: f, To: to})
}

var (
	linkTokenChars = `[A-Za-z0-9._@+~/-]+`
	subAlt         = strings.Join(flatSubs, "|")
	passB          = regexp.MustCompile("(?m)(^|[\\s(\\[`\"'])((?:" + subAlt + ")/" + linkTokenChars + ")")
	indexLink      = regexp.MustCompile(`\]\(([^)\s]+)\)`)
)

// rewriteLinks: pass A rewrites memory/<dir>/<rest>; pass B bank-relative sub paths when dir is the file's bank.
func rewriteLinks(text, dir string, dirs []string) (string, int) {
	n := 0
	sorted := slices.SortedFunc(slices.Values(dirs), func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for i, d := range sorted {
		sorted[i] = regexp.QuoteMeta(d)
	}
	if len(sorted) > 0 {
		passA := regexp.MustCompile(`(^|[^A-Za-z0-9_-])memory/(` + strings.Join(sorted, "|") + `)/(` + linkTokenChars + `)`)
		text = passA.ReplaceAllStringFunc(text, func(m string) string {
			sub := passA.FindStringSubmatch(m)
			rest := strings.TrimRight(sub[3], ".")
			to, ok := linkFlatTarget(sub[2], rest)
			if !ok {
				return m
			}
			n++
			return sub[1] + "memory/" + to + sub[3][len(rest):]
		})
	}
	if dir == "" {
		return text, n
	}
	text = passB.ReplaceAllStringFunc(text, func(m string) string {
		sub := passB.FindStringSubmatch(m)
		rest := strings.TrimRight(sub[2], ".")
		to, ok := linkFlatTarget(dir, rest)
		if !ok {
			return m
		}
		n++
		return sub[1] + to + sub[2][len(rest):]
	})
	return text, n
}

func migrationTargets(moves []migMove, reverse bool) map[string]string {
	targets := map[string]string{}
	for _, m := range moves {
		if m.To != "" {
			if reverse {
				targets[m.To] = m.From
			} else {
				targets[m.From] = m.To
			}
		}
	}
	return targets
}

func relativeLinkTarget(target, from, to string, targets map[string]string) string {
	if strings.HasPrefix(target, "/") || strings.Contains(target, ":") {
		return target
	}
	name, suffix := target, ""
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name, suffix = name[:i], name[i:]
	}
	if name == "" {
		return target
	}
	dest, ok := targets[path.Join(path.Dir(from), name)]
	if !ok {
		return target
	}
	rel, err := filepath.Rel(filepath.FromSlash(path.Dir(to)), filepath.FromSlash(dest))
	if err != nil {
		return target
	}
	return filepath.ToSlash(rel) + suffix
}

// Fenced examples and Markdown targets are excluded from the legacy bare-path passes.
func rewriteFileLinks(text, from, to string, dirs []string, targets map[string]string) (string, int) {
	var out strings.Builder
	n, fence := 0, ""
	dir, _, _ := strings.Cut(from, "/")
	for line := range strings.SplitAfterSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			out.WriteString(line)
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]) == "" {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = trimmed[:len(trimmed)-len(strings.TrimLeft(trimmed, trimmed[:1]))]
			out.WriteString(line)
			continue
		}
		next, count := rewriteMarkdownLine(line, from, to, dir, dirs, targets)
		out.WriteString(next)
		n += count
	}
	return out.String(), n
}

func rewriteMarkdownLine(line, from, to, dir string, dirs []string, targets map[string]string) (string, int) {
	var out strings.Builder
	n, start := 0, 0
	for _, loc := range indexLink.FindAllStringSubmatchIndex(line, -1) {
		before, count := rewriteLinks(line[start:loc[0]], dir, dirs)
		out.WriteString(before)
		target := line[loc[2]:loc[3]]
		next := relativeLinkTarget(target, from, to, targets)
		if next != target {
			count++
		}
		out.WriteString("](" + next + ")")
		n += count
		start = loc[1]
	}
	after, count := rewriteLinks(line[start:], dir, dirs)
	out.WriteString(after)
	return out.String(), n + count
}

func linkFlatTarget(dir, rel string) (string, bool) {
	if !strings.HasSuffix(rel, ".md") {
		return "", false
	}
	return flatTarget(dir, rel)
}

func relinkIndex(text, dir string) string {
	return indexLink.ReplaceAllStringFunc(text, func(m string) string {
		t := indexLink.FindStringSubmatch(m)[1]
		if to, ok := flatTarget(dir, t); ok {
			return "](" + to + ")"
		}
		return m
	})
}

func ensureNL(s string) string {
	if s != "" && !strings.HasSuffix(s, "\n") {
		return s + "\n"
	}
	return s
}

// indexWithoutRegion is dir's MEMORY.md minus its generated region.
func indexWithoutRegion(read func(string) ([]byte, error), dir string) (string, error) {
	b, err := read(dir + "/" + nativeIndexFile)
	if err != nil {
		return "", err
	}
	t := string(b)
	if _, _, err := scanBankRegion(t); err != nil {
		return "", fmt.Errorf("%s/%s: %w", dir, nativeIndexFile, err)
	}
	if s, e := bankRegionBounds(t); s >= 0 {
		t = t[:s] + t[e:]
	}
	return t, nil
}

// mergeIndexes builds the one shared MEMORY.md: fresh region, harness lines, home's new pointers, a section per bank.
func mergeIndexes(p *flatPlan, read func(string) ([]byte, error), now time.Time) (string, error) {
	texts := map[string]string{}
	for _, d := range p.indexDirs {
		t, err := indexWithoutRegion(read, d)
		if err != nil {
			return "", err
		}
		texts[d] = relinkIndex(t, d)
	}
	var sb strings.Builder
	if slices.Contains(p.dirs, p.harness) {
		region, err := buildRegionFrom(Bank{Key: p.harness}.Path, now)
		if err != nil {
			return "", err
		}
		sb.WriteString(region)
	}
	sb.WriteString(ensureNL(texts[p.harness]))
	order := append([]string{homeDir}, slices.DeleteFunc(slices.Clone(p.indexDirs), func(d string) bool { return d == homeDir })...)
	for _, d := range order {
		if d == p.harness {
			continue
		}
		if text := texts[d]; text != "" {
			sb.WriteString("\n## " + dashRun.ReplaceAllString(d, "-") + "\n" + ensureNL(text))
		}
	}
	return sb.String(), nil
}

// buildFlatContent fills the plan's new bytes: rewritten files, the merged index and the layout stamp.
func buildFlatContent(p *flatPlan, read func(string) ([]byte, error), now time.Time) error {
	targets := migrationTargets(p.Moves, false)
	for i, m := range p.Moves {
		if m.To == "" {
			continue
		}
		data, err := read(m.From)
		if err != nil {
			return err
		}
		if strings.HasSuffix(m.From, ".md") {
			next, n := rewriteFileLinks(string(data), m.From, m.To, p.dirs, targets)
			data = []byte(next)
			if n > 0 {
				p.Rewrites = append(p.Rewrites, migRewrite{File: m.From, Count: n})
			}
		}
		p.writes[m.To] = data
		p.Moves[i].SHA = textutil.SHA256Hex(data)
	}
	if len(p.indexDirs) > 0 {
		merged, err := mergeIndexes(p, read, now)
		if err != nil {
			return err
		}
		p.writes[nativeIndexFile] = []byte(merged)
		p.MemoryMD = migSize{Lines: strings.Count(merged, "\n"), Bytes: len(merged)}
	}
	p.writes[layoutFile] = []byte(layoutFlat + "\n")
	return nil
}

func listTracked() ([]string, error) {
	out, err := proc.RunGitRaw(context.Background(), StoreDir(), "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(strings.Split(out, "\x00"), func(s string) bool { return s == "" }), nil
}

func readStoreFile(rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(StoreDir(), filepath.FromSlash(rel)))
}

func planFlat(now time.Time) (*flatPlan, error) {
	files, err := listTracked()
	if err != nil {
		return nil, err
	}
	p := planMigrate(files, harnessStoreKey())
	if len(p.Unplanned)+len(p.Collisions) > 0 {
		return p, nil
	}
	if err := buildFlatContent(p, readStoreFile, now); err != nil {
		return nil, err
	}
	for _, rel := range slices.Sorted(maps.Keys(p.writes)) { // the store commit would refuse it after apply
		if textutil.CredentialRE.Match(p.writes[rel]) {
			p.Blockers = append(p.Blockers, "credential-shaped content in "+rel+"; scrub it and commit first")
		}
	}
	if p.MemoryMD.Lines > mergedIndexMaxLines || p.MemoryMD.Bytes > mergedIndexMaxBytes {
		p.Blockers = append(p.Blockers, fmt.Sprintf("merged MEMORY.md is %d lines / %d bytes (cap %d / %d)",
			p.MemoryMD.Lines, p.MemoryMD.Bytes, mergedIndexMaxLines, mergedIndexMaxBytes))
	}
	return p, nil
}

func dirtyBlockers() []string {
	st, err := StoreRepo().Status()
	if err != nil {
		return []string{"store git status failed: " + err.Error()}
	}
	if len(st) > 0 {
		return []string{fmt.Sprintf("memory store git is dirty (%d paths, first %s); commit or settle it first", len(st), st[0].Path)}
	}
	return nil
}

// liveMemoryWorkers names every live watcher claim and every held memory lock.
func liveMemoryWorkers() []string {
	var live []string
	claims, _ := filepath.Glob(filepath.Join(StateDir(), "watch-*"))
	for _, c := range claims {
		if !strings.HasSuffix(c, ".log") && !strings.HasSuffix(c, ".lock") && !watcherDead(c) { // *.log and *.lock are siblings, not claims
			live = append(live, "live memory watcher "+filepath.Base(c))
		}
	}
	locks, _ := filepath.Glob(filepath.Join(StateDir(), "locks", "*", "*.lock"))
	locks = append(locks, BankLockPath())
	for _, l := range locks {
		if _, err := os.Stat(l); err != nil {
			continue
		}
		if ran, err := paths.TryFileLock(l, 0, func() error { return nil }); err == nil && !ran {
			live = append(live, "held memory lock "+strings.TrimPrefix(l, StateDir()+string(filepath.Separator)))
		}
	}
	return live
}

func preconditions() []string {
	return append(migrateStoreBlockers(), liveMemoryWorkers()...)
}

func migrateStoreBlockers() []string {
	b := dirtyBlockers()
	entries, err := os.ReadDir(JournalDir())
	if err != nil && !os.IsNotExist(err) {
		return append(b, "read pending journals: "+err.Error())
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			b = append(b, "pending memory journal "+e.Name()+"; recover it before migrating")
		}
	}
	return b
}

func migrationBank(key string) Bank {
	if key == homeDir {
		return HomeBank()
	}
	return Bank{Key: key}
}

// withMigrateLocks takes every per-bank lock in order, then the store lock, around fn.
func withMigrateLocks(dirs []string, fn func() error) error {
	var lock func(int) error
	lock = func(i int) error {
		if i == len(dirs) {
			return WithBankLock(BankLockWait, fn)
		}
		ran, err := paths.TryFileLock(lockPathOf(migrationBank(dirs[i]))+".lock", updateLockWait, func() error { return lock(i + 1) })
		if err == nil && !ran {
			return fmt.Errorf("%w: bank %s is busy", ErrMigrateRefused, dirs[i])
		}
		return err
	}
	return lock(0)
}

func applyFlat(o MigrateOptions) error {
	if rec, _, err := readRevertRecord(); err == nil && rec.Reverting {
		return fmt.Errorf("%w: unfinished revert; rerun memory migrate --revert", ErrMigrateRefused)
	}
	if stampPresent() {
		return resumeFlat(o)
	}
	blockers := preconditions()
	p, err := planFlat(time.Now())
	if err != nil {
		return err
	}
	p.Blockers = append(blockers, p.Blockers...)
	ok := len(p.Blockers)+len(p.Unplanned)+len(p.Collisions) == 0
	if o.DryRun {
		if err := report(o, &p.migReport, ""); err != nil {
			return err
		}
		if !ok {
			return ErrMigrateBlocked
		}
		return nil
	}
	if !ok {
		return refuse(append(append(p.Blockers, p.Unplanned...), p.Collisions...))
	}
	return commitFlat(o, p)
}

// The revert record must survive before any store bytes or symlinks change.
func commitFlat(o MigrateOptions, p *flatPlan) error {
	err := withMigrateLocks(p.dirs, func() error {
		if b := migrateStoreBlockers(); len(b) > 0 {
			return refuse(b)
		}
		fresh, err := planFlat(time.Now())
		if err != nil {
			return err
		}
		if len(fresh.Blockers)+len(fresh.Unplanned)+len(fresh.Collisions) > 0 {
			return refuse(append(append(fresh.Blockers, fresh.Unplanned...), fresh.Collisions...))
		}
		return commitFlatPlan(fresh)
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "flattened %d files into one directory (layout %s)\n", len(p.Moves), layoutFlat)
	return Index(o.Out, os.Stderr, IndexOptions{Repo: paths.ConfigDir()})
}

func commitFlatPlan(p *flatPlan) error {
	pre, err := StoreRepo().head()
	if err != nil {
		return err
	}
	links, err := migrationLinks(StoreDir())
	if err != nil {
		return err
	}
	rec := migRecord{Time: time.Now().UTC().Format(time.RFC3339Nano), Pre: pre, Harness: p.harness,
		Dirs: p.dirs, IndexDirs: p.indexDirs, Moves: p.Moves, Links: links}
	path, err := writeRevertRecord(rec)
	if err != nil {
		return err
	}
	if _, err = commitChangesLocked(flatSubject, flatChanges(p)); err != nil {
		return err
	}
	return finishFlat(rec, path)
}

func finishFlat(rec migRecord, path string) error {
	if err := swapMigrationLinks(rec.Links, false); err != nil {
		return err
	}
	post, err := StoreRepo().head()
	if err != nil {
		return err
	}
	rec.Post = post
	if err := saveRevertRecord(path, rec); err != nil {
		return err
	}
	forgetLayout()
	pruneEmptyDirs(rec.Dirs)
	return nil
}

func resumeFlat(o MigrateOptions) error {
	rec, path, err := readRevertRecord()
	if err != nil {
		return err
	}
	if rec.Post == "" {
		if b := preconditions(); len(b) > 0 {
			return refuse(b)
		}
		if !o.DryRun {
			if err := withMigrateLocks(rec.Dirs, func() error { return finishFlat(rec, path) }); err != nil {
				return err
			}
		}
	}
	if !o.DryRun {
		if err := Index(o.Out, os.Stderr, IndexOptions{Repo: paths.ConfigDir()}); err != nil {
			return err
		}
	}
	return report(o, &migReport{AlreadyFlat: true}, "already flat")
}

func flatChanges(p *flatPlan) []Change {
	var cs []Change
	for _, m := range p.Moves {
		cs = append(cs, Change{Rel: m.From, Delete: true})
	}
	for _, rel := range slices.Sorted(func(yield func(string) bool) {
		for k := range p.writes {
			if !yield(k) {
				return
			}
		}
	}) {
		cs = append(cs, Change{Rel: rel, Data: p.writes[rel]})
	}
	return cs
}

// pruneEmptyDirs removes the per-bank dirs (deepest first) the move left empty.
func pruneEmptyDirs(dirs []string) {
	for _, d := range dirs {
		var all []string
		_ = filepath.WalkDir(filepath.Join(StoreDir(), d), func(p string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				all = append(all, p)
			}
			return nil
		})
		slices.Reverse(all)
		for _, p := range all {
			_ = os.Remove(p) // fails on a non-empty dir, which is the point
		}
	}
}

func migrationLinks(store string) ([]migLink, error) {
	var out []migLink
	links, err := filepath.Glob(filepath.Join(paths.ConfigDir(), "projects", "*", "memory"))
	if err != nil {
		return nil, err
	}
	canon := paths.CanonDeep(store)
	for _, l := range links {
		info, err := os.Lstat(l)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		raw, err := os.Readlink(l)
		if err != nil {
			return nil, err
		}
		t := raw
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(l), t)
		}
		t = paths.CanonDeep(t)
		if t != canon && strings.HasPrefix(t, canon+string(filepath.Separator)) {
			out = append(out, migLink{Link: l, Old: raw})
		}
	}
	return out, nil
}

func swapMigrationLinks(links []migLink, revert bool) error {
	for _, l := range links {
		target := StoreDir()
		if revert {
			target = l.Old
		}
		if err := paths.SwapSymlink(target, l.Link); err != nil {
			return fmt.Errorf("repoint %s: %w", l.Link, err)
		}
	}
	return nil
}

// writeRevertRecord stores the record --revert reads; a fresh timestamp keeps older records.
func writeRevertRecord(rec migRecord) (string, error) {
	path := filepath.Join(StateDir(), "migrate-flat-"+time.Now().UTC().Format("20060102-150405.000000000")+".json")
	return path, saveRevertRecord(path, rec)
}

func saveRevertRecord(path string, rec migRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(path, data, 0o600)
}

func readRevertRecord() (migRecord, string, error) {
	recs, err := filepath.Glob(filepath.Join(StateDir(), "migrate-flat-*.json"))
	if err != nil {
		return migRecord{}, "", err
	}
	if len(recs) == 0 {
		return migRecord{}, "", fmt.Errorf("%w: no migrate-flat record under %s", ErrMigrateRefused, StateDir())
	}
	slices.Sort(recs)
	path := recs[len(recs)-1]
	data, err := os.ReadFile(path)
	if err != nil {
		return migRecord{}, "", err
	}
	var rec migRecord
	err = json.Unmarshal(data, &rec)
	return rec, path, err
}

func report(o MigrateOptions, r *migReport, textNote string) error {
	if o.Format == helptext.JSON {
		return helptext.WriteJSON(o.Out, r)
	}
	if textNote != "" {
		fmt.Fprintln(o.Out, textNote)
		return nil
	}
	for _, m := range r.Moves {
		fmt.Fprintf(o.Out, "move %s -> %s\n", m.From, cmp.Or(m.To, nativeIndexFile+" (merged)"))
	}
	for _, w := range r.Rewrites {
		fmt.Fprintf(o.Out, "rewrite %s (%d links)\n", w.File, w.Count)
	}
	fmt.Fprintf(o.Out, "MEMORY.md: %d lines, %d bytes\nfiles: %d before, %d after\n",
		r.MemoryMD.Lines, r.MemoryMD.Bytes, r.FilesBefore, r.FilesAfter)
	for _, s := range [][]string{r.Blockers, r.Unplanned, r.Collisions} {
		for _, x := range s {
			fmt.Fprintln(o.Out, "BLOCKED: "+x)
		}
	}
	return nil
}

func gitShow(pre, rel string) ([]byte, error) {
	out, err := proc.RunGitRaw(context.Background(), StoreDir(), "show", pre+":"+rel)
	return []byte(out), err
}

func revertFlat(o MigrateOptions) error {
	rec, path, err := readRevertRecord()
	if !stampPresent() && (err != nil || !rec.Reverting) {
		fmt.Fprintln(o.Out, "already per-repo layout")
		return nil
	}
	if err != nil {
		return err
	}
	if b := preconditions(); len(b) > 0 {
		return refuse(b)
	}
	err = withMigrateLocks(rec.Dirs, func() error { return revertStore(rec, path, o) })
	if err != nil || o.DryRun {
		return err
	}
	fmt.Fprintln(o.Out, "reverted to the per-repo layout; reinstall a pre-flat binary (before the 8.1 commit) to keep writing memory")
	return nil
}

func revertStore(rec migRecord, path string, o MigrateOptions) error {
	if b := migrateStoreBlockers(); len(b) > 0 {
		return refuse(b)
	}
	if stampPresent() {
		cs, err := revertChanges(rec)
		if err != nil {
			return err
		}
		if o.DryRun {
			_, err := fmt.Fprintf(o.Out, "revert would apply %d changes\n", len(cs))
			return err
		}
		rec.Reverting = true
		if err := saveRevertRecord(path, rec); err != nil {
			return err
		}
		if _, err := commitChangesLocked(unflatSubject, cs); err != nil {
			return err
		}
	}
	if o.DryRun {
		return nil
	}
	return finishRevert(rec, path)
}

func finishRevert(rec migRecord, path string) error {
	if err := swapMigrationLinks(rec.Links, true); err != nil {
		return err
	}
	rec.Reverting = false
	if err := saveRevertRecord(path, rec); err != nil {
		return err
	}
	forgetLayout()
	pruneEmptyDirs(flatSubs)
	return nil
}

func revertible(f string) bool {
	top, _, nested := strings.Cut(f, "/")
	if !nested {
		return strings.HasSuffix(f, ".md") && f != nativeIndexFile && f != catalogFile
	}
	return slices.Contains(flatSubs, top)
}

// revertChanges inverts the move: exact bytes from the pre commit when untouched, prefix-rule placement otherwise.
func revertChanges(rec migRecord) ([]Change, error) {
	files, err := listTracked()
	if err != nil {
		return nil, err
	}
	byTo := map[string]migMove{}
	for _, m := range rec.Moves {
		if m.To != "" {
			byTo[m.To] = m
		}
	}
	cs := []Change{{Rel: layoutFile, Delete: true}}
	for _, f := range files {
		if !revertible(f) {
			continue
		}
		to, data, err := revertFile(f, rec, byTo)
		if err != nil {
			return nil, err
		}
		cs = append(cs, Change{Rel: f, Delete: true}, Change{Rel: to, Data: data})
	}
	idx, err := revertIndexes(rec)
	if err != nil {
		return nil, err
	}
	cs = append(cs, idx...)
	return cs, checkRevertTargets(files, cs)
}

func revertFile(f string, rec migRecord, byTo map[string]migMove) (string, []byte, error) {
	cur, err := readStoreFile(f)
	if err != nil {
		return "", nil, err
	}
	to, data := invertTarget(f, rec), cur
	if strings.HasSuffix(f, ".md") {
		data = []byte(unflattenLinks(string(cur), rec))
	}
	if m, ok := byTo[f]; ok {
		to = m.From
		original, err := gitShow(rec.Pre, m.From)
		if err != nil {
			return "", nil, err
		}
		if textutil.SHA256Hex(cur) == m.SHA {
			data = original
		} else {
			data, err = undoRecordedLinks(cur, original, m.From, rec)
			if err != nil {
				return "", nil, fmt.Errorf("%w: revert %s: %w", ErrMigrateRefused, f, err)
			}
		}
	}
	return to, data, nil
}

func checkRevertTargets(files []string, cs []Change) error {
	deleted, writes := map[string]bool{}, map[string]bool{}
	for _, c := range cs {
		if c.Delete {
			deleted[c.Rel] = true
		}
	}
	for _, c := range cs {
		if c.Delete {
			continue
		}
		if writes[c.Rel] || slices.Contains(files, c.Rel) && !deleted[c.Rel] {
			return fmt.Errorf("%w: revert target collision: %s", ErrMigrateRefused, c.Rel)
		}
		writes[c.Rel] = true
	}
	return nil
}

var migrationChunks = regexp.MustCompile(linkTokenChars + `|[^A-Za-z0-9._@+~/-]`)

func undoRecordedLinks(current, original []byte, from string, rec migRecord) ([]byte, error) {
	if !strings.HasSuffix(from, ".md") {
		return current, nil
	}
	targets := migrationTargets(rec.Moves, false)
	to, ok := targets[from]
	if !ok {
		return current, nil
	}
	flat, n := rewriteFileLinks(string(original), from, to, rec.Dirs, targets)
	if n == 0 {
		return current, nil
	}
	return mergeMigrationEdits(string(current), flat, string(original))
}

// Tokens keep an appended paragraph independent of a rewritten path on the preceding line.
func mergeMigrationEdits(current, flat, original string) (data []byte, err error) {
	dir, err := os.MkdirTemp("", "memory-revert-")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	args := []string{"merge-file", "-p"}
	for i, text := range []string{current, flat, original} {
		path := filepath.Join(dir, strconv.Itoa(i))
		if err := os.WriteFile(path, []byte(quotedMigrationTokens(text)), 0o600); err != nil {
			return nil, err
		}
		args = append(args, path)
	}
	out, err := proc.RunGitRaw(context.Background(), StoreDir(), args...)
	if err != nil {
		return nil, fmt.Errorf("edited links conflict; store left unchanged: %w", err)
	}
	var sb strings.Builder
	for l := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if l == "" {
			continue
		}
		token, err := strconv.Unquote(l)
		if err != nil {
			return nil, err
		}
		sb.WriteString(token)
	}
	return []byte(sb.String()), nil
}

func quotedMigrationTokens(text string) string {
	var sb strings.Builder
	for _, token := range migrationChunks.FindAllString(text, -1) {
		sb.WriteString(strconv.Quote(token) + "\n")
	}
	return sb.String()
}

func ownerOf(name string, rec migRecord) string {
	for _, d := range rec.Dirs {
		if d != homeDir && strings.HasPrefix(name, dashRun.ReplaceAllString(d, "-")+"--") {
			return d
		}
	}
	return BankOfFile(name).Key
}

// invertTarget is f's per-repo path by the prefix rule; unprefixed top-level topics go to the harness bank.
func invertTarget(f string, rec migRecord) string {
	parts := strings.Split(f, "/")
	i := len(parts) - 1
	if i > 0 {
		i = 1
	}
	owner := ownerOf(parts[i], rec)
	dir := owner
	switch {
	case owner != "":
		parts[i] = strings.TrimPrefix(parts[i], dashRun.ReplaceAllString(owner, "-")+"--")
	case i > 0, slices.Contains(bankFiles, parts[i]):
		dir = homeDir
	default:
		dir = rec.Harness
	}
	return dir + "/" + strings.Join(parts, "/")
}

var passUnflat = regexp.MustCompile(`(^|[^A-Za-z0-9_-])memory/(` + linkTokenChars + `)`)

// unflattenLinks points memory/<flat path> links back at their bank dir when the prefix names a bank.
func unflattenLinks(text string, rec migRecord) string {
	return passUnflat.ReplaceAllStringFunc(text, func(m string) string {
		sub := passUnflat.FindStringSubmatch(m)
		rest := strings.TrimRight(sub[2], ".")
		parts := strings.Split(rest, "/")
		if !revertible(rest) && !slices.Contains(bankFiles, rest) || ownerOf(parts[min(1, len(parts)-1)], rec) == "" {
			return m
		}
		return sub[1] + "memory/" + invertTarget(rest, rec) + sub[2][len(rest):]
	})
}

// Edited indexes are rebuilt from current sections; only an unchanged shared index restores historical bytes.
func revertIndexes(rec migRecord) ([]Change, error) {
	if len(rec.IndexDirs) == 0 {
		return nil, nil
	}
	cur, err := readStoreFile(nativeIndexFile)
	if err != nil {
		return nil, err
	}
	baseline, err := gitShow(rec.Post, nativeIndexFile)
	if err != nil {
		return nil, err
	}
	old, err := originalIndexes(rec)
	if err != nil {
		return nil, err
	}
	if string(cur) != string(baseline) {
		if err := checkIndexProse(string(cur), old, rec); err != nil {
			return nil, err
		}
		old = currentIndexes(string(cur), rec)
	}
	cs := []Change{{Rel: nativeIndexFile, Delete: true}}
	for _, d := range slices.Sorted(maps.Keys(old)) {
		cs = append(cs, Change{Rel: d + "/" + nativeIndexFile, Data: []byte(old[d])})
	}
	return cs, nil
}

func currentIndexes(text string, rec migRecord) map[string]string {
	sections := revertIndexSections(text, rec)
	out := map[string]string{}
	targets := migrationTargets(rec.Moves, true)
	for _, d := range rec.IndexDirs {
		out[d] = ""
	}
	for d, lines := range sections {
		body := strings.Trim(strings.Join(lines, "\n"), "\n")
		if body == "" {
			continue
		}
		body = indexLink.ReplaceAllStringFunc(body, func(m string) string {
			target := indexLink.FindStringSubmatch(m)[1]
			next := relativeLinkTarget(target, nativeIndexFile, d+"/"+nativeIndexFile, targets)
			if next != target {
				return "](" + next + ")"
			}
			return unrelinkIndex(m, d)
		})
		out[d] = ensureNL(body)
	}
	return out
}

func checkIndexProse(current string, old map[string]string, rec migRecord) error {
	p := &flatPlan{harness: rec.Harness, indexDirs: rec.IndexDirs}
	baseline, err := mergeIndexes(p, func(rel string) ([]byte, error) {
		dir, _, _ := strings.Cut(rel, "/")
		return []byte(old[dir]), nil
	}, time.Time{})
	if err != nil {
		return err
	}
	if !slices.Equal(indexProse(current, rec), indexProse(baseline, rec)) {
		return fmt.Errorf("%w: merged MEMORY.md prose changed; reconcile it before reverting", ErrMigrateRefused)
	}
	return nil
}

func indexProse(text string, rec migRecord) []string {
	if s, e := bankRegionBounds(text); s >= 0 {
		text = text[:s] + text[e:]
	}
	var lines []string
	for l := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "- ") {
			continue
		}
		if h, ok := strings.CutPrefix(l, "## "); ok && slices.ContainsFunc(rec.IndexDirs, func(d string) bool { return dashRun.ReplaceAllString(d, "-") == h }) {
			continue
		}
		lines = append(lines, l)
	}
	return lines
}

func revertIndexSections(text string, rec migRecord) map[string][]string {
	if s, e := bankRegionBounds(text); s >= 0 {
		text = text[:s] + text[e:]
	}
	sections, name := map[string][]string{}, rec.Harness
	for l := range strings.SplitSeq(text, "\n") {
		if h, ok := strings.CutPrefix(l, "## "); ok {
			if i := slices.IndexFunc(rec.IndexDirs, func(d string) bool { return dashRun.ReplaceAllString(d, "-") == h }); i >= 0 {
				sections[name] = strings.Split(strings.TrimRight(strings.Join(sections[name], "\n"), "\n"), "\n")
				name = rec.IndexDirs[i]
				continue
			}
		}
		sections[name] = append(sections[name], l)
	}
	return sections
}

func originalIndexes(rec migRecord) (map[string]string, error) {
	old := map[string]string{}
	for _, d := range rec.IndexDirs {
		b, err := gitShow(rec.Pre, d+"/"+nativeIndexFile)
		if err != nil {
			return nil, err
		}
		old[d] = string(b)
	}
	return old, nil
}

// unrelinkIndex strips bank d's prefix from a pointer line's link target (top level or under a shared subdir).
func unrelinkIndex(line, d string) string {
	return indexLink.ReplaceAllStringFunc(line, func(m string) string {
		parts := strings.Split(indexLink.FindStringSubmatch(m)[1], "/")
		i := min(1, len(parts)-1)
		if i == 1 && !slices.Contains(flatSubs, parts[0]) {
			return m
		}
		parts[i] = strings.TrimPrefix(parts[i], dashRun.ReplaceAllString(d, "-")+"--")
		return "](" + strings.Join(parts, "/") + ")"
	})
}
