package memory

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// AliasOptions names the checkout to record and the store key it joins.
type AliasOptions struct {
	Repo, Into string
	DryRun     bool
	Now        time.Time
}

// aliasResult is the plan (dry run) or the outcome (applied): one human line per action.
type aliasResult struct {
	Key, Source string // target key; folded source key ("" when no separate store existed)
	Lines       []string
}

// appendOnly files merge by block; replaceFiles keep the target and archive the source verbatim.
var (
	appendOnly   = map[string]bool{"decisionLog.md": true, "conventions.md": true, "sessionHistory.md": true}
	replaceFiles = map[string]bool{"activeContext.md": true, "progress.md": true, "projectContext.md": true}
	headingDate  = regexp.MustCompile(`^## (\d{4}-\d{2}-\d{2})`)
)

// AliasRepo records an alias and folds its separate store, preserving content, under the bank lock.
func AliasRepo(opt AliasOptions) (aliasResult, error) {
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	abs, err := filepath.Abs(opt.Repo)
	if err != nil {
		return aliasResult{}, err
	}
	canon := paths.MainCheckout(abs, gitOut)
	if canon == "" {
		return aliasResult{}, fmt.Errorf("%s is not a git checkout", abs)
	}
	canon = paths.CanonDeep(canon)
	if opt.DryRun {
		res, _, err := planAlias(opt, canon)
		return res, err
	}
	res, p, err := planAlias(opt, canon)
	if err != nil {
		return res, err
	}
	if p.source == "" { // origin-only: addOriginAlias takes the lock itself
		return res, addOriginAlias(p.target, canon)
	}
	return applyAlias(opt, canon, res, p)
}

func applyAlias(opt AliasOptions, canon string, res aliasResult, p *aliasPlan) (aliasResult, error) {
	err := WithBankLock(BankLockWait, func() error {
		var perr error
		if res, p, perr = planAlias(opt, canon); perr != nil { // replan: the store may have moved since the preview read
			return perr
		}
		if p.source == "" { // the separate store vanished before the lock; nothing to fold
			return nil
		}
		return p.apply(opt.Now)
	})
	if err == nil && p.source == "" {
		return res, addOriginAlias(p.target, canon)
	}
	return res, err
}

// aliasPlan is everything apply needs: store changes, the source dir to remove and the symlinks to repoint.
type aliasPlan struct {
	target, source string // store dirs; source "" = origin-only alias
	origin         Origin // the target's record after the fold
	changes        []Change
	links          []string // projects/*/memory symlinks that pointed at source
}

func findStore(entries []repoEntry, key string) (repoEntry, bool) {
	for _, e := range entries {
		if e.Key == key {
			return e, true
		}
	}
	return repoEntry{}, false
}

// planAlias reads the store and builds the plan without writing; apply (under the lock) executes it.
func planAlias(opt AliasOptions, canon string) (aliasResult, *aliasPlan, error) {
	entries, err := RepoDirs()
	if err != nil {
		return aliasResult{}, nil, err // an unreadable vault must not look like "no separate store"
	}
	tgt, ok := findStore(entries, opt.Into)
	if !ok {
		return aliasResult{}, nil, fmt.Errorf("unknown store key %q (see the catalog: %s)", opt.Into, filepath.Join(StoreDir(), CatalogFile))
	}
	if tgt.Origin.Accepts(canon) {
		return aliasResult{}, nil, fmt.Errorf("%s already resolves to store %q", canon, tgt.Key)
	}
	res := aliasResult{Key: tgt.Key}
	p := &aliasPlan{target: tgt.Dir}
	src := FindEntry(entries, canon)
	res.Lines = append(res.Lines, fmt.Sprintf("alias %s -> %s", canon, tgt.Key))
	if src == nil {
		res.Lines = append(res.Lines, "no separate store for that checkout; recording the alias only")
		return res, p, nil
	}
	res.Source, p.source = src.Key, src.Dir
	res.Lines = append(res.Lines, fmt.Sprintf("fold store %s into %s", src.Key, tgt.Key))
	origin := tgt.Origin
	origin.Aliases = append(slices.Clone(origin.Aliases), canon)
	origin.Aliases = append(origin.Aliases, src.Origin.Origin)     // the source's primary checkout keeps resolving here
	origin.Aliases = append(origin.Aliases, src.Origin.Aliases...) // the source's own explicit aliases follow it
	p.origin = origin
	res.Lines = append(res.Lines, "record alias in the "+tgt.Key+" origin record (vault)")
	if err := p.foldFiles(*src, tgt, &res); err != nil {
		return res, nil, err
	}
	links, err := projectLinksTo(src.Dir)
	if err != nil {
		return res, nil, err
	}
	p.links = links
	for _, l := range p.links {
		res.Lines = append(res.Lines, "repoint "+l+" -> "+tgt.Dir)
	}
	res.Lines = append(res.Lines, "remove "+src.Dir, "settle + index "+tgt.Key+", regenerate catalog, commit")
	return res, p, nil
}

func storeRel(dir, name string) string {
	r, _ := filepath.Rel(StoreDir(), filepath.Join(dir, name))
	return r
}

func (p *aliasPlan) addWrite(dir, name string, data []byte, res *aliasResult, line string) {
	p.changes = append(p.changes, Change{Rel: storeRel(dir, name), Data: data})
	res.Lines = append(res.Lines, line)
}

// foldFiles plans every regular file of the source store; anything unsafe or colliding aborts the whole plan.
func (p *aliasPlan) foldFiles(src, tgt repoEntry, res *aliasResult) error {
	return filepath.WalkDir(src.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(src.Dir, path)
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file; refusing to fold", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		p.changes = append(p.changes, Change{Rel: storeRel(src.Dir, rel), Delete: true})
		return p.foldOne(path, rel, data, src, tgt, res)
	})
}

// foldSkips reports the identity, generated index and lock files, which do not travel.
// Migration: remove the legacy-origin match after 2026-11-17 once every machine has run a post-2026-10-20 build.
func foldSkips(rel string) bool {
	return rel == legacyOriginFile || rel == bankIndexFile || strings.HasSuffix(rel, ".lock") || filepath.Base(rel) == ".postupdate"
}

// isSessionArchive reports whether rel is a rotated monthly archive/sessionHistory-<yyyy-mm>.md.
func isSessionArchive(rel string) bool {
	return filepath.Dir(rel) == "archive" && strings.HasPrefix(filepath.Base(rel), "sessionHistory-") && strings.HasSuffix(rel, ".md")
}

// logName maps an archive path to the log grammar its blocks follow; other names pass through.
func logName(rel string) string {
	if isSessionArchive(rel) {
		return "sessionHistory.md"
	}
	return rel
}

// foldOne plans one source file against its target counterpart.
func (p *aliasPlan) foldOne(path, rel string, data []byte, src, tgt repoEntry, res *aliasResult) error {
	if foldSkips(rel) {
		return nil
	}
	dst := filepath.Join(tgt.Dir, rel)
	cur, rerr := os.ReadFile(dst)
	if rerr != nil && !os.IsNotExist(rerr) {
		return rerr
	}
	if rel == NativeIndexFile {
		return p.foldNativeIndex(path, rel, string(cur), string(data), tgt, res)
	}
	if rerr != nil {
		p.addWrite(tgt.Dir, rel, data, res, "copy "+rel)
		return nil
	}
	switch {
	case appendOnly[rel] || isSessionArchive(rel):
		merged, n := mergeLog(logName(rel), string(cur), string(data))
		if n > 0 {
			p.addWrite(tgt.Dir, rel, []byte(merged), res, fmt.Sprintf("%s: merge %d entr(ies)", rel, n))
		}
	case replaceFiles[rel]:
		return p.foldReplace(rel, cur, data, src, tgt, res)
	case !bytes.Equal(cur, data):
		return fmt.Errorf("%s exists with different bytes; refusing to overwrite", dst)
	}
	return nil
}

func (p *aliasPlan) foldNativeIndex(path, rel, cur, data string, tgt repoEntry, res *aliasResult) error {
	merged, n, err := mergeNativeIndex(cur, data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if n > 0 {
		p.addWrite(tgt.Dir, rel, []byte(merged), res, fmt.Sprintf("%s: append %d pointer line(s)", rel, n))
	}
	return nil
}

func (p *aliasPlan) foldReplace(rel string, cur, data []byte, src, tgt repoEntry, res *aliasResult) error {
	if bytes.Equal(cur, data) {
		return nil
	}
	name := "merged-" + src.Key + "-" + rel
	arch := filepath.Join(archiveDir(tgt.Dir), name)
	old, err := readIfExists(arch)
	if err != nil {
		return err
	}
	if old != nil && !bytes.Equal(old, data) {
		return fmt.Errorf("%s exists with different bytes; refusing", arch)
	}
	p.addWrite(archiveDir(tgt.Dir), name, data, res, fmt.Sprintf("%s: target wins; source kept at archive/%s", rel, name))
	return nil
}

// linesMissingFrom lists src's non-blank lines whose trimmed text tgt lacks, each once.
func linesMissingFrom(tgt, src string) []string {
	seen := map[string]bool{}
	for l := range strings.SplitSeq(tgt, "\n") {
		seen[strings.TrimSpace(l)] = true
	}
	var add []string
	for l := range strings.SplitSeq(src, "\n") {
		if t := strings.TrimSpace(l); t != "" && !seen[t] {
			seen[t] = true
			add = append(add, l)
		}
	}
	return add
}

// mergeNativeIndex appends the source's pointer lines (outside the generated region) the target lacks.
func mergeNativeIndex(tgt, src string) (string, int, error) {
	if _, _, err := scanBankRegion(tgt); err != nil {
		return "", 0, err
	}
	s, e, err := scanBankRegion(src)
	if err != nil {
		return "", 0, err
	}
	if s >= 0 {
		src = src[:s] + src[e:]
	}
	add := linesMissingFrom(tgt, src)
	if len(add) == 0 {
		return tgt, 0, nil
	}
	if tgt != "" && !strings.HasSuffix(tgt, "\n") {
		tgt += "\n"
	}
	return tgt + strings.Join(add, "\n") + "\n", len(add), nil
}

// mergeLog adds the source blocks missing from the target (by persisted id, else same text); dated logs
// stay newest-first, conventions append.
func mergeLog(name, tgt, src string) (string, int) {
	hdr, blocks := entryBlocks(name, tgt)
	_, sblocks := entryBlocks(name, src)
	add := blocksMissingFrom(blocks, sblocks)
	if len(add) == 0 {
		return tgt, 0
	}
	if name == "conventions.md" {
		return strings.TrimRight(tgt, "\n") + "\n" + strings.Join(add, "\n") + "\n", len(add)
	}
	return rejoin(hdr, sortNewestFirst(append(slices.Clone(blocks), add...))), len(add)
}

// blocksMissingFrom lists sblocks whose persisted id and trimmed text are both absent from blocks.
func blocksMissingFrom(blocks, sblocks []string) []string {
	ids, texts := map[string]bool{}, map[string]bool{}
	for _, b := range blocks {
		ids[blockID(textutil.FirstLine(b))] = true
		texts[strings.TrimSpace(stripID(b))] = true
	}
	var add []string
	for _, b := range sblocks {
		id, t := blockID(textutil.FirstLine(b)), strings.TrimSpace(stripID(b))
		if (id != "" && ids[id]) || texts[t] {
			continue
		}
		ids[id], texts[t] = true, true
		add = append(add, b)
	}
	return add
}

// sortNewestFirst stably orders dated blocks newest first; an undated block inherits the date above it, so it never moves.
func sortNewestFirst(all []string) []string {
	last := "9999-99-99"
	dates := make([]string, len(all))
	for i, b := range all {
		if m := headingDate.FindStringSubmatch(b); m != nil {
			last = m[1]
		}
		dates[i] = last
	}
	idx := make([]int, len(all))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return dates[idx[a]] > dates[idx[b]] })
	sorted := make([]string, len(all))
	for i, j := range idx {
		sorted[i] = all[j]
	}
	return sorted
}

// projectLinksTo lists <cfg>/projects/*/memory symlinks that resolve to dir; an unreadable entry is an error,
// since a link it hides would dangle once the source store is removed.
func projectLinksTo(dir string) ([]string, error) {
	want := paths.CanonDeep(dir)
	projects := filepath.Join(paths.ConfigDir(), "projects")
	ents, err := os.ReadDir(projects)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		l := filepath.Join(projects, e.Name(), "memory")
		fi, err := os.Lstat(l)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if paths.SamePath(nativeLinkTarget(l), want) {
			out = append(out, l)
		}
	}
	return out, nil
}

// apply runs under the bank lock: reversible steps first (undone on error), then the journaled commit, then cleanup.
func (p *aliasPlan) apply(now time.Time) error {
	undo, err := p.repoint()
	if err != nil {
		return err
	}
	if len(p.changes) > 0 {
		rec, err := commitChangesLocked("memory: fold "+filepath.Base(p.source)+" into "+filepath.Base(p.target), p.changes)
		if err != nil && rec.ID != "" { // a journal exists: it rolls forward to the repointed target, so undo would split them
			return err
		}
		if err != nil {
			return errors.Join(err, undo())
		}
	}
	if err := removeEmptyTree(p.source); err != nil {
		return err
	}
	if _, err := settleDir(p.target, now); err != nil {
		return err
	}
	if err := writeCatalog(now); err != nil {
		return err
	}
	return commitStore("settle: alias " + filepath.Base(p.target))
}

// repoint folds the origin row and swaps the links; on error it rolls back, else returns an undo for a later failure.
func (p *aliasPlan) repoint() (func() error, error) {
	var undos []func() error
	undo := func() error {
		var errs []error
		for _, u := range slices.Backward(undos) {
			errs = append(errs, u())
		}
		return errors.Join(errs...)
	}
	tPrev, tErr := ReadOrigin(p.target)
	if tErr == nil {
		undos = append(undos, func() error { return foldOrigin(p.target, tPrev, "") })
	}
	if sPrev, err := ReadOrigin(p.source); err == nil {
		drop := "" // a target that had no row loses the folded one in the same transaction
		if errors.Is(tErr, fs.ErrNotExist) {
			drop = filepath.Base(p.target)
		}
		undos = append(undos, func() error { return foldOrigin(p.source, sPrev, drop) })
	}
	if err := foldOrigin(p.target, p.origin, filepath.Base(p.source)); err != nil {
		return nil, err
	}
	for _, l := range p.links {
		old, err := os.Readlink(l)
		if err != nil {
			return nil, errors.Join(err, undo())
		}
		if err := paths.SwapSymlink(p.target, l); err != nil {
			return nil, errors.Join(err, undo())
		}
		undos = append(undos, func() error { return paths.SwapSymlink(old, l) })
	}
	return undo, nil
}

// removeEmptyTree deletes dir only when no file remains in it; a survivor means a delete diverged.
func removeEmptyTree(dir string) error {
	var dirs []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return fmt.Errorf("%s survived the fold (edited meanwhile?); left in place", p)
		}
		dirs = append(dirs, p)
		return nil
	})
	if err != nil {
		return err
	}
	for _, dir := range slices.Backward(dirs) {
		if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
