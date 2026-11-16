package memory

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	pre := dir + "--"
	if dir == homeDir {
		pre = ""
	}
	parts := strings.Split(rel, "/")
	if slices.Contains(parts, "") {
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

func sharedTop(top string) bool {
	return top == ".git" || top == "receipts" || slices.Contains(flatSubs, top)
}

// planMigrate sorts tracked files into moves, unplanned shapes and target collisions; it reads nothing.
func planMigrate(files []string, harness string) *flatPlan {
	p := &flatPlan{harness: harness, writes: map[string][]byte{}}
	p.Blockers, p.Moves, p.Rewrites, p.Unplanned, p.Collisions = []string{}, []migMove{}, []migRewrite{}, []string{}, []string{}
	targets := map[string]string{}
	for _, f := range files {
		top, rel, nested := strings.Cut(f, "/")
		if !nested || sharedTop(top) {
			targets[f] = f
			continue
		}
		p.place(targets, f, top, rel)
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
	if !ok || strings.Contains(top, "--") {
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
			to, ok := flatTarget(sub[2], rest)
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
		to, ok := flatTarget(dir, rest)
		if !ok {
			return m
		}
		n++
		return sub[1] + to + sub[2][len(rest):]
	})
	return text, n
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

func pointerLines(text string) []string {
	var out []string
	for l := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(l, "- ") {
			out = append(out, l)
		}
	}
	return out
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
		region, err := buildBankRegion(filepath.Join(StoreDir(), p.harness), now)
		if err != nil {
			return "", err
		}
		sb.WriteString(region)
	}
	sb.WriteString(ensureNL(texts[p.harness]))
	for _, l := range pointerLines(texts[homeDir]) {
		if !strings.Contains(texts[p.harness], l) {
			sb.WriteString(l + "\n")
		}
	}
	for _, d := range p.indexDirs {
		if lines := pointerLines(texts[d]); len(lines) > 0 && d != p.harness && d != homeDir {
			sb.WriteString("\n## " + d + "\n" + strings.Join(lines, "\n") + "\n")
		}
	}
	return sb.String(), nil
}

// buildFlatContent fills the plan's new bytes: rewritten files, the merged index and the layout stamp.
func buildFlatContent(p *flatPlan, read func(string) ([]byte, error), now time.Time) error {
	for i, m := range p.Moves {
		if m.To == "" {
			continue
		}
		data, err := read(m.From)
		if err != nil {
			return err
		}
		if strings.HasSuffix(m.From, ".md") {
			top, _, _ := strings.Cut(m.From, "/")
			next, n := rewriteLinks(string(data), top, p.dirs)
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
		if filepath.Ext(c) == "" && !watcherDead(c) { // *.log and *.lock are siblings, not claims
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

func preconditions() []string { return append(dirtyBlockers(), liveMemoryWorkers()...) }

// withMigrateLocks takes every per-bank lock in order, then the store lock, around fn.
func withMigrateLocks(dirs []string, fn func() error) error {
	var lock func(int) error
	lock = func(i int) error {
		if i == len(dirs) {
			return WithBankLock(BankLockWait, fn)
		}
		ran, err := paths.TryFileLock(lockPath(filepath.Join(StoreDir(), dirs[i]))+".lock", updateLockWait, func() error { return lock(i + 1) })
		if err == nil && !ran {
			return fmt.Errorf("%w: bank %s is busy", ErrMigrateRefused, dirs[i])
		}
		return err
	}
	return lock(0)
}

func applyFlat(o MigrateOptions) error {
	if stampPresent() {
		return report(o, &migReport{AlreadyFlat: true}, "already flat")
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

// commitFlat re-plans under the locks, applies the journaled commit, re-points links, forces flat, writes the record.
func commitFlat(o MigrateOptions, p *flatPlan) error {
	var rec migRecord
	err := withMigrateLocks(p.dirs, func() error {
		if b := dirtyBlockers(); len(b) > 0 {
			return refuse(b)
		}
		fresh, err := planFlat(time.Now())
		if err != nil {
			return err
		}
		if len(fresh.Blockers)+len(fresh.Unplanned)+len(fresh.Collisions) > 0 {
			return refuse(append(append(fresh.Blockers, fresh.Unplanned...), fresh.Collisions...))
		}
		pre, _ := StoreRepo().head()
		r, err := commitChangesLocked(flatSubject, flatChanges(fresh))
		if err != nil {
			return err
		}
		rec = migRecord{Time: time.Now().UTC().Format(time.RFC3339), Pre: pre, Post: r.Commit, Harness: fresh.harness,
			Dirs: fresh.dirs, IndexDirs: fresh.indexDirs, Moves: fresh.Moves}
		p = fresh
		return nil
	})
	if err != nil {
		return err
	}
	rec.Links = repointLinks(StoreDir())
	flatForced.Store(2)
	forgetLayout()
	pruneEmptyDirs(p.dirs)
	path, err := writeRevertRecord(rec)
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "flattened %d files into one directory (layout %s); revert record %s\n", len(p.Moves), layoutFlat, path)
	fmt.Fprintln(o.Out, "note: run claude-code memory index after the binary is updated (index.md and embeddings are stale)")
	return nil
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

// repointLinks sends every projects/*/memory symlink that points into the store at the store root.
func repointLinks(store string) []migLink {
	var out []migLink
	links, _ := filepath.Glob(filepath.Join(paths.ConfigDir(), "projects", "*", "memory"))
	canon := paths.CanonDeep(store)
	for _, l := range links {
		raw, err := os.Readlink(l)
		if err != nil {
			continue
		}
		t := raw
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(l), t)
		}
		t = paths.CanonDeep(t)
		if t == canon || !strings.HasPrefix(t, canon+string(filepath.Separator)) {
			continue
		}
		if os.Remove(l) == nil && os.Symlink(store, l) == nil {
			out = append(out, migLink{Link: l, Old: raw})
		}
	}
	return out
}

// writeRevertRecord stores the record --revert reads; a fresh timestamp keeps older records.
func writeRevertRecord(rec migRecord) (string, error) {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(StateDir(), "migrate-flat-"+time.Now().UTC().Format("20060102-150405.000")+".json")
	return path, paths.AtomicWrite(path, data, 0o600)
}

func newestRecord() (migRecord, error) {
	recs, _ := filepath.Glob(filepath.Join(StateDir(), "migrate-flat-*.json"))
	if len(recs) == 0 {
		return migRecord{}, fmt.Errorf("%w: no migrate-flat record under %s", ErrMigrateRefused, StateDir())
	}
	slices.Sort(recs)
	data, err := os.ReadFile(recs[len(recs)-1])
	if err != nil {
		return migRecord{}, err
	}
	var rec migRecord
	return rec, json.Unmarshal(data, &rec)
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
	if !stampPresent() {
		fmt.Fprintln(o.Out, "already per-repo layout")
		return nil
	}
	if b := preconditions(); len(b) > 0 {
		return refuse(b)
	}
	rec, err := newestRecord()
	if err != nil {
		return err
	}
	flatForced.Store(2) // BankOfFile reads the flat layout while the stamp is still there
	err = withMigrateLocks(rec.Dirs, func() error {
		if b := dirtyBlockers(); len(b) > 0 {
			return refuse(b)
		}
		cs, err := revertChanges(rec)
		if err != nil || o.DryRun {
			fmt.Fprintf(o.Out, "revert would apply %d changes\n", len(cs))
			return err
		}
		_, err = commitChangesLocked(unflatSubject, cs)
		return err
	})
	if err != nil || o.DryRun {
		return err
	}
	for _, l := range rec.Links {
		if os.Remove(l.Link) == nil {
			_ = os.Symlink(l.Old, l.Link)
		}
	}
	flatForced.Store(1)
	forgetLayout()
	pruneEmptyDirs(flatSubs)
	fmt.Fprintln(o.Out, "reverted to the per-repo layout")
	fmt.Fprintln(o.Out, "note: run claude-code memory index to refresh index.md")
	return nil
}

func revertible(f string) bool {
	top, _, nested := strings.Cut(f, "/")
	if !nested {
		return strings.HasSuffix(f, ".md") && f != nativeIndexFile
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
		cur, err := readStoreFile(f)
		if err != nil {
			return nil, err
		}
		to, data := invertTarget(f, rec), []byte(unflattenLinks(string(cur), rec))
		if m, ok := byTo[f]; ok {
			to = m.From
			if textutil.SHA256Hex(cur) == m.SHA {
				if data, err = gitShow(rec.Pre, m.From); err != nil {
					return nil, err
				}
			}
		}
		cs = append(cs, Change{Rel: f, Delete: true}, Change{Rel: to, Data: data})
	}
	idx, err := revertIndexes(rec)
	return append(cs, idx...), err
}

func ownerOf(name string, rec migRecord) string {
	if b := BankOfFile(name); !b.Home() {
		return b.Key
	}
	for _, d := range rec.Dirs {
		if d != homeDir && strings.HasPrefix(name, d+"--") {
			return d
		}
	}
	return ""
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
		parts[i] = strings.TrimPrefix(parts[i], owner+"--")
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

// revertIndexes restores each per-bank MEMORY.md from the pre commit and appends pointer lines added since.
func revertIndexes(rec migRecord) ([]Change, error) {
	cur, err := readStoreFile(nativeIndexFile)
	if err != nil {
		return nil, err
	}
	text := string(cur)
	if s, e := bankRegionBounds(text); s >= 0 {
		text = text[:s] + text[e:]
	}
	sections, name := map[string][]string{}, ""
	for l := range strings.SplitSeq(text, "\n") {
		if h, ok := strings.CutPrefix(l, "## "); ok {
			name = h
		} else if strings.HasPrefix(l, "- ") {
			sections[name] = append(sections[name], l)
		}
	}
	old := map[string]string{}
	for _, d := range rec.IndexDirs {
		b, err := gitShow(rec.Pre, d+"/"+nativeIndexFile)
		if err != nil {
			return nil, err
		}
		old[d] = string(b)
	}
	cs := []Change{{Rel: nativeIndexFile, Delete: true}}
	for _, d := range rec.IndexDirs {
		known, lines := old[d], sections[d]
		if d == rec.Harness {
			known, lines = known+old[homeDir], sections[""]
		}
		out := ensureNL(old[d])
		for _, l := range lines {
			if l = unrelinkIndex(l, d); !strings.Contains(known, l) {
				out += l + "\n"
			}
		}
		cs = append(cs, Change{Rel: d + "/" + nativeIndexFile, Data: []byte(out)})
	}
	return cs, nil
}

// unrelinkIndex strips bank d's prefix from a pointer line's link target (top level or under a shared subdir).
func unrelinkIndex(line, d string) string {
	return indexLink.ReplaceAllStringFunc(line, func(m string) string {
		parts := strings.Split(indexLink.FindStringSubmatch(m)[1], "/")
		i := min(1, len(parts)-1)
		if i == 1 && !slices.Contains(flatSubs, parts[0]) {
			return m
		}
		parts[i] = strings.TrimPrefix(parts[i], d+"--")
		return "](" + strings.Join(parts, "/") + ")"
	})
}
