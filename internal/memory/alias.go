package memory

import (
	"bytes"
	"encoding/json"
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
)

// AliasOptions names the checkout to record and the store key it joins.
type AliasOptions struct {
	Repo, Into string
	DryRun     bool
	Now        time.Time
}

// AliasResult is the plan (dry run) or the outcome (applied): one human line per action.
type AliasResult struct {
	Key, Source string // target key; folded source key ("" when no separate store existed)
	Lines       []string
}

// appendOnly files merge by block; replaceFiles keep the target and archive the source verbatim.
var (
	appendOnly   = map[string]bool{"decisionLog.md": true, "conventions.md": true, "sessionHistory.md": true}
	replaceFiles = map[string]bool{"activeContext.md": true, "progress.md": true, "projectContext.md": true}
	headingDate  = regexp.MustCompile(`^## (\d{4}-\d{2}-\d{2})`)
)

// AliasRepo records opt.Repo as an explicit alias of store opt.Into and folds any separate store of that
// checkout into it, content-preserving, as one journaled commit under the bank lock.
func AliasRepo(opt AliasOptions) (AliasResult, error) {
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	abs, err := filepath.Abs(opt.Repo)
	if err != nil {
		return AliasResult{}, err
	}
	canon := mainCheckout(abs)
	if canon == "" {
		return AliasResult{}, fmt.Errorf("%s is not a git checkout", abs)
	}
	if opt.DryRun {
		res, _, err := planAlias(opt, canon)
		return res, err
	}
	res, p, err := planAlias(opt, canon)
	if err != nil {
		return res, err
	}
	if p.source == "" { // origin-only: AddOriginAlias takes the lock itself
		tgt, _ := findStore(opt.Into)
		return res, AddOriginAlias(tgt.Dir, canon)
	}
	err = WithWritableBankLock(BankLockWait, func() error {
		var perr error
		if res, p, perr = planAlias(opt, canon); perr != nil { // replan: the store may have moved since the preview read
			return perr
		}
		return p.apply(opt.Now)
	})
	return res, err
}

// aliasPlan is everything apply needs: store changes, the source dir to remove and the symlinks to repoint.
type aliasPlan struct {
	target, source string // store dirs; source "" = origin-only alias
	changes        []Change
	links          []string // projects/*/memory symlinks that pointed at source
}

func findStore(key string) (RepoEntry, bool) {
	entries, _ := RepoDirs()
	for _, e := range entries {
		if e.Key == key {
			return e, true
		}
	}
	return RepoEntry{}, false
}

// planAlias reads the store and builds the plan without writing; apply (under the lock) executes it.
func planAlias(opt AliasOptions, canon string) (AliasResult, *aliasPlan, error) {
	tgt, ok := findStore(opt.Into)
	if !ok {
		return AliasResult{}, nil, fmt.Errorf("unknown store key %q (see the catalog: %s)", opt.Into, filepath.Join(StoreDir(), CatalogFile))
	}
	if tgt.Origin.Accepts(canon) {
		return AliasResult{}, nil, fmt.Errorf("%s already resolves to store %q", canon, tgt.Key)
	}
	res := AliasResult{Key: tgt.Key}
	p := &aliasPlan{target: tgt.Dir}
	entries, _ := RepoDirs()
	var src *RepoEntry
	if e := findEntry(entries, canon); e != nil {
		src = e
	}
	res.Lines = append(res.Lines, fmt.Sprintf("alias %s -> %s", canon, tgt.Key))
	if src == nil {
		res.Lines = append(res.Lines, "no separate store for that checkout; recording the alias only")
		return res, p, nil
	}
	res.Source, p.source = src.Key, src.Dir
	res.Lines = append(res.Lines, fmt.Sprintf("fold store %s into %s", src.Key, tgt.Key))
	origin := tgt.Origin
	origin.Aliases = append(slices.Clone(origin.Aliases), canon)
	origin.Aliases = append(origin.Aliases, src.Origin.Aliases...) // the source's own explicit aliases follow it
	data, err := marshalOrigin(origin)
	if err != nil {
		return res, nil, err
	}
	if cur, _ := os.ReadFile(filepath.Join(tgt.Dir, OriginFile)); !bytes.Equal(cur, data) {
		p.addWrite(tgt.Dir, OriginFile, data, &res, "record alias in "+tgt.Key+"/"+OriginFile)
	}
	if err := p.foldFiles(*src, tgt, &res); err != nil {
		return res, nil, err
	}
	p.links = projectLinksTo(src.Dir)
	for _, l := range p.links {
		res.Lines = append(res.Lines, "repoint "+l+" -> "+tgt.Dir)
	}
	res.Lines = append(res.Lines, "remove "+src.Dir, "settle + index "+tgt.Key+", regenerate catalog, commit")
	return res, p, nil
}

func marshalOrigin(o Origin) ([]byte, error) {
	o.Aliases = cleanAliases(o.Aliases)
	data, err := json.MarshalIndent(o, "", "  ")
	return append(data, '\n'), err
}

func (p *aliasPlan) rel(dir, name string) string {
	r, _ := filepath.Rel(StoreDir(), filepath.Join(dir, name))
	return r
}

func (p *aliasPlan) addWrite(dir, name string, data []byte, res *AliasResult, line string) {
	p.changes = append(p.changes, Change{Rel: p.rel(dir, name), Data: data})
	res.Lines = append(res.Lines, line)
}

// foldFiles plans every regular file of the source store; anything unsafe or colliding aborts the whole plan.
func (p *aliasPlan) foldFiles(src, tgt RepoEntry, res *AliasResult) error {
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
		p.changes = append(p.changes, Change{Rel: p.rel(src.Dir, rel), Delete: true})
		dst := filepath.Join(tgt.Dir, rel)
		cur, rerr := os.ReadFile(dst)
		have := rerr == nil
		switch {
		case rel == OriginFile || rel == BankIndexFile || strings.HasSuffix(rel, ".lock") || filepath.Base(rel) == ".postharvest":
			return nil // identity, generated index and lock files do not travel
		case rel == NativeIndexFile:
			merged, n, err := mergeNativeIndex(string(cur), string(data))
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if n > 0 {
				p.addWrite(tgt.Dir, rel, []byte(merged), res, fmt.Sprintf("%s: append %d pointer line(s)", rel, n))
			}
		case appendOnly[rel] && have:
			merged, n := mergeLog(rel, string(cur), string(data))
			if n > 0 {
				p.addWrite(tgt.Dir, rel, []byte(merged), res, fmt.Sprintf("%s: merge %d entr(ies)", rel, n))
			}
		case replaceFiles[rel] && have:
			if bytes.Equal(cur, data) {
				return nil
			}
			name := "merged-" + src.Key + "-" + rel
			arch := filepath.Join(ArchiveDir(tgt.Dir), name)
			if old, err := os.ReadFile(arch); err == nil && !bytes.Equal(old, data) {
				return fmt.Errorf("%s exists with different bytes; refusing", arch)
			}
			p.addWrite(ArchiveDir(tgt.Dir), name, data, res, fmt.Sprintf("%s: target wins; source kept at archive/%s", rel, name))
		case have && bytes.Equal(cur, data):
			// identical: nothing to copy
		case have:
			return fmt.Errorf("%s exists with different bytes; refusing to overwrite", dst)
		default:
			p.addWrite(tgt.Dir, rel, data, res, "copy "+rel)
		}
		return nil
	})
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
	seen := map[string]bool{}
	for _, l := range strings.Split(tgt, "\n") {
		seen[strings.TrimSpace(l)] = true
	}
	var add []string
	for _, l := range strings.Split(src, "\n") {
		if t := strings.TrimSpace(l); t != "" && !seen[t] {
			seen[t] = true
			add = append(add, l)
		}
	}
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
	ids, texts := map[string]bool{}, map[string]bool{}
	for _, b := range blocks {
		ids[BlockID(firstLine(b))] = true
		texts[strings.TrimSpace(stripID(b))] = true
	}
	var add []string
	for _, b := range sblocks {
		id, t := BlockID(firstLine(b)), strings.TrimSpace(stripID(b))
		if (id != "" && ids[id]) || texts[t] {
			continue
		}
		ids[id], texts[t] = true, true
		add = append(add, b)
	}
	if len(add) == 0 {
		return tgt, 0
	}
	if name == "conventions.md" {
		return strings.TrimRight(tgt, "\n") + "\n" + strings.Join(add, "\n") + "\n", len(add)
	}
	all := append(slices.Clone(blocks), add...)
	last := "9999-99-99" // an undated block inherits the date above it, so it never moves
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
	return rejoin(hdr, sorted), len(add)
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// projectLinksTo lists <cfg>/projects/*/memory symlinks that resolve to dir.
func projectLinksTo(dir string) []string {
	want := resolvePath(dir)
	m, _ := filepath.Glob(filepath.Join(paths.ConfigDir(), "projects", "*", "memory"))
	var out []string
	for _, l := range m {
		if fi, err := os.Lstat(l); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if t, err := filepath.EvalSymlinks(l); err == nil && t == want {
			out = append(out, l)
		}
	}
	return out
}

// apply runs under the bank lock: journaled commit, remove the source dir, repoint links, settle target + catalog.
func (p *aliasPlan) apply(now time.Time) error {
	if len(p.changes) > 0 {
		if _, err := commitChangesLocked("memory: fold "+filepath.Base(p.source)+" into "+filepath.Base(p.target), p.changes); err != nil {
			return err
		}
	}
	if err := removeEmptyTree(p.source); err != nil {
		return err
	}
	for _, l := range p.links {
		tmp := l + ".alias-tmp"
		os.Remove(tmp)
		if err := os.Symlink(p.target, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, l); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	var s BankSettlement
	if err := settleDir(p.target, now, &s); err != nil {
		return err
	}
	if err := writeCatalog(now); err != nil {
		return err
	}
	return commitStore("settle: alias " + filepath.Base(p.target))
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
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Remove(dirs[i]); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
