package memory

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// replaceFiles keep the target and drop the source (nothing is archived); idFiles merge by block.
var (
	replaceFiles = map[string]bool{"activeContext.md": true, "progress.md": true, "projectContext.md": true}
	headingDate  = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^## (\d{4}-\d{2}-\d{2})`)
	})
)

// importMaxFile caps one imported file; importMaxTotal caps a fetched archive.
const (
	importMaxFile  = 4 << 20
	importMaxTotal = 64 << 20
)

// ImportOptions is one `memory import`: From is a memory root or a store dir; Label names the source in the commit.
type ImportOptions struct {
	From, Label    string
	DryRun, Topics bool // Topics also imports non-log files: topic files, backups, MEMORY.md pointers
	Now            time.Time
	Replace        map[string]string // store-relative file -> sha256 hex of the local bytes the caller saw; a match lets the source replace it
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

// ImportBank merges another bank into the local one under the job and bank locks; Replace lets a source file win.
func ImportBank(opt ImportOptions) (ImportResult, error) {
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	if opt.DryRun {
		p, err := planImport(opt)
		return p.result(opt), err
	}
	var res ImportResult
	ran, err := paths.TryFileLock(lockPathOf()+".lock", lockWait(), func() error { // the job lock memory update and dream hold
		return withFlatLock(BankLockWait, func() error {
			p, err := planImport(opt)
			if err == nil {
				err = p.apply(opt)
			}
			res = p.result(opt)
			return err
		})
	})
	if err == nil && !ran {
		err = ErrBankBusy
	}
	if err == nil || errors.Is(err, errIndexProblem) {
		refreshBankNotice(os.Stderr, "memory import", ResolveBank())
	}
	return res, err
}

type importStats struct{ merged, copied, kept, conflicts, skipped, left int }

type importPlan struct {
	changes  []Change
	acts     []importAction // parallel to changes: what each one was counted as
	leaks    []string       // planned writes the store commit would refuse as credential-shaped
	lines    []string
	st       importStats
	topics   bool
	replace  map[string]string
	pointers []string // the source's entries, folded into the destination catalog after its own
	newPtrs  int      // how many of them the destination does not hold yet
}

// planImport reads both sides and builds the changes without writing.
func planImport(opt ImportOptions) (*importPlan, error) {
	p := &importPlan{topics: opt.Topics, replace: opt.Replace}
	if err := checkImportLayout(opt.From); err != nil {
		return p, err
	}
	if err := p.planStore(opt.From); err != nil {
		return p, err
	}
	if len(p.leaks) > 0 { // refuse before apply: the commit gate would fail only after the writes
		var msg strings.Builder
		for _, rel := range p.leaks {
			fmt.Fprintf(&msg, "credential-shaped content: %s\n", rel)
		}
		return p, errors.New(msg.String() + "nothing was imported; scrub the source file(s) and re-run")
	}
	p.countNewPointers()
	return p, nil
}

func (p *importPlan) result(opt ImportOptions) ImportResult {
	s, left := p.st, ""
	if s.left > 0 {
		left = fmt.Sprintf("; %d non-log file(s) left out (--topics imports them)", s.left)
	}
	if len(p.changes) == 0 && p.newPtrs == 0 {
		return ImportResult{Lines: p.lines, Summary: "nothing to import" + left}
	}
	if p.newPtrs > 0 {
		left = fmt.Sprintf("; %d pointer line(s)", p.newPtrs) + left
	}
	verb := "imported"
	if opt.DryRun {
		verb = "would import"
	}
	return ImportResult{Lines: p.lines, Summary: fmt.Sprintf("%s: %d merged, %d copied, %d kept local, %d conflict(s), %d skipped",
		verb, s.merged, s.copied, s.kept, s.conflicts, s.skipped) + left}
}

// countNewPointers counts the source entries the destination's index.md and pending MEMORY.md input do not hold.
func (p *importPlan) countNewPointers() {
	_, held, _, _ := readStored()
	if held == nil {
		held = map[string]bool{}
	}
	for _, f := range append(keptCopies(), NativeIndex()) {
		data, _ := readIfExists(f)
		in, _ := inputEntries(string(data), lastGenerated())
		for _, e := range in {
			held[e] = true
		}
	}
	p.newPtrs = len(fresh(p.pointers, held))
}

// apply commits the changes atomically, settles the touched stores and commits that, all under the caller's lock.
func (p *importPlan) apply(opt ImportOptions) error {
	if len(p.changes) == 0 && p.newPtrs == 0 {
		return nil
	}
	if err := backupStoreBundle(opt.Now); err != nil {
		return fmt.Errorf("backup before import: %w (nothing was imported)", err)
	}
	label := strings.Join(strings.Fields(opt.Label), " ")
	if len(p.changes) > 0 {
		rec, err := commitChangesLocked("memory: import "+label, p.changes)
		if errors.Is(err, errDivergent) {
			p.diverged(rec.Divergent)
			err = nil
		}
		if err != nil {
			return err
		}
	}
	_, err := settleBankWith(opt.Now, p.pointers, "settle: import "+label)
	return err
}

// diverged recounts the changes whose target was edited after the plan as conflicts: the edit stays, unwritten.
func (p *importPlan) diverged(rels []string) {
	for i, c := range p.changes {
		if !slices.Contains(rels, c.Rel) {
			continue
		}
		switch a := p.acts[i]; a.kind {
		case impMerge:
			p.st.merged -= a.n
		case impCopy:
			p.st.copied--
		}
		p.st.conflicts++
		if j := slices.Index(p.lines, p.acts[i].line); j >= 0 {
			p.lines[j] = "conflict " + filepath.ToSlash(c.Rel) + ": kept local (changed during the import)"
		}
	}
}

// Mixed layouts cannot share file attribution safely; read-only, as the apply path's withFlatLock already stamped.
func checkImportLayout(from string) error {
	if err := checkFlat(); err != nil {
		return err
	}
	if stamp, _ := os.ReadFile(filepath.Join(from, layoutFile)); strings.TrimSpace(string(stamp)) != layoutFlat {
		return importInput("--from %s uses an older store layout; run `claude-code memory migrate` on it", from)
	}
	return nil
}

// flatImportFile reports a source file import reads: never MEMORY.md (its pointer lines merge) or a dotfile.
func (p *importPlan) flatImportFile(rel string) bool {
	return rel != nativeIndexFile && !strings.HasPrefix(path.Base(rel), ".")
}

// readPointers collects the source's entries: its index.md records, then its moved-aside and live MEMORY.md input.
func (p *importPlan) readPointers(root *os.Root) {
	gen, _ := readSource(root, legacyGen) // a source store's own dotfile, as older binaries left it; the source's own proof of which edited-block lines it generated
	for i, rel := range []string{catalogFile, legacyAside, nativeIndexFile} {
		data, err := readSource(root, rel)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			p.skipFile(rel, err.Error()+"; its entries were not imported")
		case i == 0:
			p.pointers = append(p.pointers, slices.DeleteFunc(decodeCatalog(string(data)), generatedCounter)...)
		default:
			in, _ := inputEntries(string(data), string(gen))
			p.pointers = append(p.pointers, in...)
		}
	}
}

// planStore walks the source store through os.Root, so no entry can resolve outside it.
func (p *importPlan) planStore(from string) error {
	root, err := os.OpenRoot(from)
	if err != nil {
		return importInput("--from %s: %v", from, err)
	}
	defer root.Close()
	if p.topics {
		p.readPointers(root)
	}
	return fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			p.skipFile(rel, err.Error())
		case d.IsDir() && skipImportDir(rel, d.Name()):
			return fs.SkipDir
		case d.IsDir(), foldSkips(rel), rel == layoutFile, rel == catalogFile: // index.md's entries go by readPointers
		case !p.flatImportFile(rel):
		case !filepath.IsLocal(filepath.FromSlash(rel)):
			p.skipFile(rel, "path escapes the store")
		case !d.Type().IsRegular():
			p.skipFile(rel, "not a regular file")
		default:
			p.importFile(root, rel)
		}
		return nil
	})
}

func skipImportDir(rel, name string) bool {
	return name == ".git" || name == "locks" || name == ".locks" ||
		rel != "." && !slices.Contains(flatSubs, strings.Split(rel, "/")[0])
}

func (p *importPlan) skipFile(rel, why string) {
	p.lines = append(p.lines, "skipped "+rel+": "+why)
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

func (p *importPlan) importFile(root *os.Root, rel string) {
	if !p.topics && idFiles[rel] == "" && !monthlyArchive(rel) { // logs only unless --topics
		p.st.left++
		return
	}
	data, err := readSource(root, rel)
	if err != nil {
		p.skipFile(rel, err.Error())
		return
	}
	if idFiles[rel] != "" {
		data = unarchived(paths.MemoryStoreDir(), rel, data)
	}
	dst := filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(rel))
	if f := NoteFolder(rel); f != "" && !strings.Contains(rel, "/") && !statOK(dst) && statOK(paths.MemorySub(f, rel)) {
		dst = paths.MemorySub(f, rel) // settle already filed the loose note, so compare against it
	}
	cur, err := os.ReadFile(dst)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.record(rel, importAction{kind: impConflict, line: "conflict " + rel + ": kept local (" + err.Error() + ")"})
		return
	}
	a := decideImport(rel, cur, data, err == nil, p.replace[rel])
	if err == nil && dst == filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(rel)) {
		a.pre = sha256Hex(cur) // else rel itself was absent at the plan
	}
	p.record(rel, a)
}

// unarchived drops the source blocks the target's archives already hold, so a rotated entry is not imported again.
func unarchived(tgt, rel string, data []byte) []byte {
	archives, _ := filepath.Glob(filepath.Join(tgt, archivePrefix+strings.TrimSuffix(rel, ".md")+"-*.md"))
	var known []string
	for _, a := range archives {
		if b, err := os.ReadFile(a); err == nil {
			_, blocks := entryBlocks(logicalName(rel), string(b))
			known = append(known, blocks...)
		}
	}
	if len(known) == 0 {
		return data
	}
	hdr, blocks := entryBlocks(logicalName(rel), string(data))
	return []byte(rejoin(hdr, blocksMissingFrom(known, blocks)))
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
	pre  string // sha256 of the target the plan read, "" when absent
}

func (p *importPlan) record(rel string, a importAction) {
	if a.kind == impNone {
		return
	}
	p.lines = append(p.lines, a.line)
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
	if credentialShaped(a.data) {
		p.leaks = append(p.leaks, rel)
	}
	p.changes = append(p.changes, Change{Rel: filepath.FromSlash(rel), Data: a.data, Planned: true, PreSHA256: a.pre})
	p.acts = append(p.acts, a)
}

// decideImport applies the import rules: logs merge, new files copy, a file whose replace hash matches is replaced.
func decideImport(rel string, cur, data []byte, exists bool, replaceHash string) importAction {
	switch {
	case !exists:
		return importAction{kind: impCopy, data: data, line: "copy " + rel}
	case idFiles[rel] != "" || monthlyArchive(rel):
		merged, n := mergeLog(logicalName(rel), string(cur), string(data))
		return mergedAction(rel, "merge %d entr(ies)", merged, n)
	case bytes.Equal(cur, data):
		return importAction{}
	case replaceHash != "" && replaceHash == sha256Hex(cur):
		return importAction{kind: impCopy, data: data, line: "replace " + rel} // counted under "copied"
	case replaceFiles[rel]:
		return importAction{kind: impKept, line: "kept local " + rel}
	}
	return importAction{kind: impConflict, line: "conflict " + rel + ": kept local"}
}

// archivePrefix is the folder rotated logs live in: archive/decisionLog-2026-10.md.
const archivePrefix = "archive/"

// monthlyArchive reports a rotated monthly log (archive/decisionLog-2026-10.md), which merges like its typed file.
func monthlyArchive(rel string) bool {
	rest, ok := strings.CutPrefix(rel, archivePrefix)
	return ok && archiveName().MatchString(rest)
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
	case foldSkips(name), strings.Contains("/"+name+"/", "/.git/"):
		return "", ""
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

// foldSkips reports the generated router and lock files, which do not travel.
func foldSkips(rel string) bool {
	return rel == routerFile || strings.HasSuffix(rel, ".lock") || filepath.Base(rel) == ".postupdate"
}

// mergeLog adds the source blocks missing from the target (by id, else text); dated logs stay newest-first.
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

// blocksMissingFrom lists sblocks the target lacks: matched by id when both have one, else by trimmed text.
func blocksMissingFrom(blocks, sblocks []string) []string {
	ids, texts, bare := map[string]bool{}, map[string]bool{}, map[string]bool{}
	note := func(b string) {
		id, t := blockID(textutil.FirstLine(b)), strings.TrimSpace(stripID(b))
		ids[id], texts[t] = true, true
		if id == "" {
			bare[t] = true
		}
	}
	for _, b := range blocks {
		note(b)
	}
	var add []string
	for _, b := range sblocks {
		id, t := blockID(textutil.FirstLine(b)), strings.TrimSpace(stripID(b))
		if (id != "" && (ids[id] || bare[t])) || (id == "" && texts[t]) {
			continue
		}
		note(b)
		add = append(add, b)
	}
	return add
}

// sortNewestFirst stably orders dated blocks newest first; an undated block inherits the date above it.
func sortNewestFirst(all []string) []string {
	last := "9999-99-99"
	dates := make([]string, len(all))
	for i, b := range all {
		if m := headingDate().FindStringSubmatch(b); m != nil {
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

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const (
	bundlePrefix, bundleSuffix = "memory-import-", ".bundle"
	bundleStampLayout          = "20060102-150405"
	bundleMinAge               = 24 * time.Hour
	bundleKeep                 = 3
)

// backupStoreBundle bundles the store repo before an import changes it; skipped when the newest is under a day old.
func backupStoreBundle(now time.Time) error {
	repo := StoreRepo()
	if _, ok := repo.headSHA(); !ok {
		if sha, err := repo.head(); err != nil || sha == "" { // no commit yet: git refuses an empty bundle
			return err
		}
	}
	dir := filepath.Join(paths.StateDir(), "backups")
	old, _ := filepath.Glob(filepath.Join(dir, bundlePrefix+"*"+bundleSuffix))
	sort.Strings(old) // the UTC stamp in the name sorts by time
	if len(old) > 0 && bundleAge(old[len(old)-1], now) < bundleMinAge {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	out := filepath.Join(dir, bundlePrefix+now.UTC().Format(bundleStampLayout)+bundleSuffix)
	if _, err := repo.run("bundle", "create", out, "--all"); err != nil {
		_ = os.Remove(out)
		return err
	}
	if all := append(old, out); len(all) > bundleKeep {
		for _, f := range all[:len(all)-bundleKeep] {
			_ = os.Remove(f)
		}
	}
	return nil
}

// bundleAge reads a bundle's age from its name; an unreadable name counts as old.
func bundleAge(file string, now time.Time) time.Duration {
	stamp := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), bundlePrefix), bundleSuffix)
	t, err := time.Parse(bundleStampLayout, stamp)
	if err != nil {
		return bundleMinAge
	}
	return now.Sub(t)
}

// storeTarName reports whether a store file may travel: what ExtractStoreTar and planStore would accept as a file.
func storeTarName(rel string) bool {
	if !filepath.IsLocal(filepath.FromSlash(rel)) || foldSkips(rel) || strings.HasPrefix(path.Base(rel), ".") {
		return false
	}
	parts := strings.Split(rel, "/")
	if len(parts) > 1 && !slices.Contains(flatSubs, parts[0]) {
		return false
	}
	return !slices.ContainsFunc(parts[:len(parts)-1], func(d string) bool { return d == ".git" || d == "locks" || d == ".locks" })
}

// WriteStoreTar writes the named store files, plus .layout, as a tar ExtractStoreTar reads; skipped names are dropped.
func WriteStoreTar(w io.Writer, rels []string) error {
	root, err := os.OpenRoot(paths.MemoryStoreDir())
	if err != nil {
		return err
	}
	defer root.Close()
	tw := tar.NewWriter(w)
	seen := map[string]bool{layoutFile: true}
	if err := addStoreTarFile(tw, root, layoutFile); err != nil {
		return err
	}
	for _, rel := range rels {
		rel = path.Clean(filepath.ToSlash(rel))
		if seen[rel] || !storeTarName(rel) {
			continue
		}
		seen[rel] = true
		if err := addStoreTarFile(tw, root, rel); err != nil {
			return err
		}
	}
	return tw.Close()
}

// addStoreTarFile adds one regular file within the size cap; anything else is left out.
func addStoreTarFile(tw *tar.Writer, root *os.Root, rel string) error {
	name := filepath.FromSlash(rel)
	if fi, err := root.Lstat(name); err != nil || !fi.Mode().IsRegular() || fi.Size() > importMaxFile {
		if rel == layoutFile {
			return fmt.Errorf("store %s is missing or not a regular file", layoutFile)
		}
		return nil
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o600, Size: fi.Size(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, fi.Size())
	return err
}
