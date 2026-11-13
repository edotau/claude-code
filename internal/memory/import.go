package memory

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// importMaxFile caps one imported file; importMaxTotal caps a fetched archive.
const (
	importMaxFile  = 4 << 20
	importMaxTotal = 64 << 20
)

// ImportOptions is one `memory import`: From is a memory root or a store dir; Label names the source in the commit.
type ImportOptions struct {
	From, Repo, Label   string
	All, DryRun, Topics bool // Topics also imports non-log files: topic files, backups, MEMORY.md pointers
	Now                 time.Time
}

// ImportResult is the plan (dry run) or the outcome: one line per action, then the summary line.
type ImportResult struct {
	Lines   []string
	Summary string
}

// ImportInputError is a bad --from or flag combination, as opposed to a failed run.
type ImportInputError struct{ Msg string }

func (e *ImportInputError) Error() string { return e.Msg }

func importInput(format string, a ...any) error { return &ImportInputError{fmt.Sprintf(format, a...)} }

// ImportBank merges another bank into the local one, additively and under the bank lock; nothing local is overwritten.
func ImportBank(opt ImportOptions) (ImportResult, error) {
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	if opt.DryRun {
		p, err := planImport(opt)
		return p.result(opt), err
	}
	var res ImportResult
	err := WithBankLock(BankLockWait, func() error {
		p, err := planImport(opt)
		if err != nil {
			return err
		}
		res = p.result(opt)
		return p.apply(opt)
	})
	return res, err
}

type importPair struct{ key, src, tgt string }

type importStats struct{ merged, copied, kept, conflicts, skipped, left int }

type importPlan struct {
	changes []Change
	lines   []string
	touched []string // target store dirs with a change
	st      importStats
	topics  bool
}

// planImport reads both sides and builds the changes without writing.
func planImport(opt ImportOptions) (*importPlan, error) {
	p := &importPlan{topics: opt.Topics}
	pairs, err := selectPairs(opt, p)
	if err != nil {
		return p, err
	}
	for _, pr := range pairs {
		if err := p.planStore(pr); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (p *importPlan) result(opt ImportOptions) ImportResult {
	s, left := p.st, ""
	if s.left > 0 {
		left = fmt.Sprintf("; %d non-log file(s) left out (--topics imports them)", s.left)
	}
	if len(p.changes) == 0 {
		return ImportResult{Lines: p.lines, Summary: "nothing to import" + left}
	}
	verb := "imported"
	if opt.DryRun {
		verb = "would import"
	}
	return ImportResult{Lines: p.lines, Summary: fmt.Sprintf("%s: %d merged, %d copied, %d kept local, %d conflict(s), %d skipped",
		verb, s.merged, s.copied, s.kept, s.conflicts, s.skipped) + left}
}

// apply commits the changes atomically, settles the touched stores and commits that, all under the caller's lock.
func (p *importPlan) apply(opt ImportOptions) error {
	if len(p.changes) == 0 {
		return nil
	}
	label := strings.Join(strings.Fields(opt.Label), " ")
	if _, err := commitChangesLocked("memory: import "+label, p.changes); err != nil {
		return err
	}
	for _, d := range p.touched {
		if _, err := settleDir(d, opt.Now); err != nil {
			return err
		}
	}
	if err := writeCatalog(opt.Now); err != nil {
		return err
	}
	return commitStore("settle: import "+label, append(storeRels(p.touched), catalogFile)...)
}

// sourceStores maps store key to dir: a dir with typed bank files is one store (single), else its store subdirs.
func sourceStores(from string) (stores map[string]string, single bool, err error) {
	if hasTypedFile(from) {
		return map[string]string{filepath.Base(from): from}, true, nil
	}
	ents, err := os.ReadDir(from)
	if err != nil {
		return nil, false, importInput("--from %s: %v", from, err)
	}
	stores = map[string]string{}
	for _, e := range ents {
		if d := filepath.Join(from, e.Name()); e.IsDir() && e.Name() != ".git" && hasTypedFile(d) {
			stores[e.Name()] = d
		}
	}
	if len(stores) == 0 {
		return nil, false, importInput("--from %s is neither a memory root nor a store (no typed bank files)", from)
	}
	return stores, false, nil
}

func hasTypedFile(dir string) bool {
	return slices.ContainsFunc(bankFiles, func(f string) bool {
		fi, err := os.Lstat(filepath.Join(dir, f))
		return err == nil && fi.Mode().IsRegular()
	})
}

// localStores maps every local store key (plus home) to its dir.
func localStores() (map[string]string, error) {
	ents, err := RepoDirs()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range ents {
		out[e.Key] = e.Dir
	}
	if fi, err := os.Stat(HomeBankDir()); err == nil && fi.IsDir() {
		out["home"] = HomeBankDir()
	}
	return out, nil
}

func (p *importPlan) skipStore(key, why string) {
	p.lines = append(p.lines, "skipped "+key+": "+why)
	p.st.skipped++
}

// selectPairs pairs source and local stores; a store missing on either side is skipped and listed.
func selectPairs(opt ImportOptions, p *importPlan) ([]importPair, error) {
	stores, single, err := sourceStores(opt.From)
	if err != nil {
		return nil, err
	}
	if opt.All {
		if single {
			return nil, importInput("--all needs a memory root; --from %s is one store", opt.From)
		}
		return allPairs(stores, p)
	}
	root, err := ResolveRepo(opt.Repo)
	if err != nil {
		return nil, importInput("%v", err)
	}
	tgt := Dir(root)
	key := filepath.Base(tgt)
	if _, ok := insideStore(tgt); !ok {
		return nil, importInput("no local store for %s (run `claude-code memory init` there)", root)
	}
	src, ok := stores[key]
	switch fi, serr := os.Stat(tgt); {
	case serr != nil || !fi.IsDir():
		p.skipStore(key, "no local store")
	case !ok:
		p.skipStore(key, "no store in the source")
	default:
		return []importPair{{key, src, tgt}}, nil
	}
	return nil, nil
}

func allPairs(stores map[string]string, p *importPlan) ([]importPair, error) {
	local, err := localStores()
	if err != nil {
		return nil, err
	}
	var pairs []importPair
	for _, key := range slices.Sorted(maps.Keys(stores)) {
		if tgt, ok := local[key]; ok {
			pairs = append(pairs, importPair{key, stores[key], tgt})
		} else {
			p.skipStore(key, "no local store")
		}
	}
	return pairs, nil
}

// planStore walks one source store through os.Root, so no entry can resolve outside it.
func (p *importPlan) planStore(pr importPair) error {
	root, err := os.OpenRoot(pr.src)
	if err != nil {
		p.skipStore(pr.key, err.Error())
		return nil
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			p.skipFile(pr, rel, err.Error())
		case d.IsDir() && d.Name() == ".git":
			return fs.SkipDir
		case d.IsDir(), foldSkips(rel):
		case !filepath.IsLocal(filepath.FromSlash(rel)):
			p.skipFile(pr, rel, "path escapes the store")
		case !d.Type().IsRegular():
			p.skipFile(pr, rel, "not a regular file")
		default:
			p.importFile(pr, root, rel)
		}
		return nil
	})
}

func (p *importPlan) skipFile(pr importPair, rel, why string) {
	p.lines = append(p.lines, pr.key+": skipped "+rel+": "+why)
	p.st.skipped++
}

// readSource reads one regular file of the source, refusing anything over the size cap.
func readSource(root *os.Root, rel string) ([]byte, error) {
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, importMaxFile+1))
	if err == nil && len(data) > importMaxFile {
		err = fmt.Errorf("over the %d MiB cap", importMaxFile>>20)
	}
	return data, err
}

func (p *importPlan) importFile(pr importPair, root *os.Root, rel string) {
	if !p.topics && !appendOnly[rel] && !monthlyArchive(rel) { // logs only unless --topics
		p.st.left++
		return
	}
	data, err := readSource(root, rel)
	if err != nil {
		p.skipFile(pr, rel, err.Error())
		return
	}
	dst := filepath.Join(pr.tgt, filepath.FromSlash(rel))
	cur, err := os.ReadFile(dst)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.record(pr, rel, importAction{kind: impConflict, line: "conflict " + rel + ": kept local (" + err.Error() + ")"})
		return
	}
	p.record(pr, rel, decideImport(rel, cur, data, err == nil))
}

type importKind int

const (
	impNone importKind = iota
	impMerge
	impCopy
	impKept
	impConflict
)

// importAction is one file's verdict: the bytes to write (nil = none), its report line and its summary bucket.
type importAction struct {
	kind importKind
	n    int
	data []byte
	line string
}

func (p *importPlan) record(pr importPair, rel string, a importAction) {
	if a.kind == impNone {
		return
	}
	p.lines = append(p.lines, pr.key+": "+a.line)
	switch a.kind {
	case impMerge:
		p.st.merged += a.n
	case impCopy:
		p.st.copied++
	case impKept:
		p.st.kept++
	case impConflict:
		p.st.conflicts++
	}
	if a.data == nil {
		return
	}
	p.changes = append(p.changes, Change{Rel: storeRel(pr.tgt, filepath.FromSlash(rel)), Data: a.data})
	if !slices.Contains(p.touched, pr.tgt) {
		p.touched = append(p.touched, pr.tgt)
	}
}

// decideImport applies the import rules: logs and the native index merge, replace files keep local, new files copy.
func decideImport(rel string, cur, data []byte, exists bool) importAction {
	switch {
	case !exists:
		return importAction{kind: impCopy, data: data, line: "copy " + rel}
	case rel == nativeIndexFile:
		merged, n, err := mergeNativeIndex(string(cur), string(data))
		if err != nil {
			return importAction{kind: impConflict, line: "conflict " + rel + ": kept local (" + err.Error() + ")"}
		}
		return mergedAction(rel, "append %d pointer line(s)", merged, n)
	case appendOnly[rel] || monthlyArchive(rel):
		merged, n := mergeLog(logicalName(rel), string(cur), string(data))
		return mergedAction(rel, "merge %d entr(ies)", merged, n)
	case bytes.Equal(cur, data):
		return importAction{}
	case replaceFiles[rel]:
		return importAction{kind: impKept, line: "kept local " + rel}
	}
	return importAction{kind: impConflict, line: "conflict " + rel + ": kept local"}
}

// monthlyArchive reports a rotated monthly log (archive/decisionLog-2026-10.md), which merges like its typed file.
func monthlyArchive(rel string) bool {
	return path.Dir(rel) == "archive" && archiveName.MatchString(path.Base(rel))
}

func mergedAction(rel, format, merged string, n int) importAction {
	if n == 0 {
		return importAction{}
	}
	return importAction{kind: impMerge, n: n, data: []byte(merged), line: rel + ": " + fmt.Sprintf(format, n)}
}

// ExtractStoreTar unpacks a tar of a memory root into dir: only regular files inside a store dir, within the caps.
func ExtractStoreTar(r io.Reader, dir string) ([]string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	tr := tar.NewReader(io.LimitReader(r, importMaxTotal))
	var skipped []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return skipped, nil
		}
		if err != nil {
			return skipped, fmt.Errorf("reading the archive (truncated, or over %d MiB): %w", importMaxTotal>>20, err)
		}
		name, why := tarEntry(h)
		if why != "" {
			skipped = append(skipped, "skipped "+h.Name+": "+why)
		}
		if name == "" {
			continue
		}
		if err := writeTarFile(root, name, tr); err != nil {
			return skipped, err
		}
	}
}

// tarEntry returns the clean store-relative name to extract, or "" with the reason (none for dirs and root files).
func tarEntry(h *tar.Header) (name, why string) {
	switch name = path.Clean(h.Name); {
	case h.Typeflag == tar.TypeDir || h.Typeflag == tar.TypeXGlobalHeader:
		return "", ""
	case h.Typeflag != tar.TypeReg:
		return "", "not a regular file"
	case !filepath.IsLocal(name):
		return "", "path escapes the store"
	case !strings.Contains(name, "/"):
		return "", "" // a file beside the stores (the catalog) is never imported
	case h.Size > importMaxFile:
		return "", fmt.Sprintf("over the %d MiB cap", importMaxFile>>20)
	}
	return name, ""
}

func writeTarFile(root *os.Root, name string, r io.Reader) error {
	if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}
