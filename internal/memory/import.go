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
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

// replaceFiles keep the target and archive the source verbatim; idFiles merge by block.
var (
	replaceFiles = map[string]bool{"activeContext.md": true, "progress.md": true, "projectContext.md": true}
	headingDate  = regexp.MustCompile(`^## (\d{4}-\d{2}-\d{2})`)
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
	err := withFlatLock(BankLockWait, func() error {
		p, err := planImport(opt)
		if err != nil {
			return err
		}
		res = p.result(opt)
		return p.apply(opt)
	})
	if root, b, rerr := ResolveBank(opt.Repo); err == nil && rerr == nil {
		refreshBankNotice(os.Stderr, "memory import", root, b)
	}
	return res, err
}

type importStats struct{ merged, copied, kept, conflicts, skipped, left int }

type importPlan struct {
	changes []Change
	leaks   []string // planned writes the store commit would refuse as credential-shaped
	lines   []string
	touched []Bank
	native  bool
	st      importStats
	topics  bool
}

// planImport reads both sides and builds the changes without writing.
func planImport(opt ImportOptions) (*importPlan, error) {
	p := &importPlan{topics: opt.Topics}
	if err := checkImportLayout(opt.From); err != nil {
		return p, err
	}
	pairs, err := selectPairs(opt, p)
	if err != nil {
		return p, err
	}
	for _, key := range pairs {
		if err := p.planStore(opt.From, key); err != nil {
			return p, err
		}
	}
	if len(p.leaks) > 0 { // refuse before apply: the commit gate would fail only after the writes
		var msg strings.Builder
		for _, rel := range p.leaks {
			msg.WriteString("credential-shaped content: " + rel + "\n")
		}
		return p, errors.New(msg.String() + "nothing was imported; scrub the source file(s) and re-run")
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
	for _, b := range p.touched {
		if _, err := settleDir(b, opt.Now); err != nil {
			return err
		}
	}
	if err := writeCatalog(opt.Now); err != nil {
		return err
	}
	return commitBanks("settle: import "+label, p.touched)
}

// Mixed layouts cannot share file attribution safely; read-only, as the apply path's withFlatLock already stamped.
func checkImportLayout(from string) error {
	if err := checkFlat(); err != nil {
		return err
	}
	if stamp, _ := os.ReadFile(filepath.Join(from, layoutFile)); strings.TrimSpace(string(stamp)) != layoutFlat {
		return importInput("--from %s uses the retired per-repo layout; migrate it with a pre-removal claude-code binary", from)
	}
	return nil
}

// sourceStores maps each store key in the flat source to the source dir; home is the unprefixed files.
func sourceStores(from string) (map[string]string, error) {
	ents, err := os.ReadDir(from)
	if err != nil {
		return nil, importInput("--from %s: %v", from, err)
	}
	stores := map[string]string{homeDir: from}
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		key, name, prefixed := strings.Cut(e.Name(), "--")
		key = stateKey(Bank{Key: key})
		if !prefixed {
			key, name = homeDir, e.Name()
		}
		if slices.Contains(bankFiles, name) {
			stores[key] = from
		}
	}
	return stores, nil
}

func importName(rel string) string {
	parts := strings.Split(rel, "/")
	i := 0
	if len(parts) > 1 {
		i = 1
	}
	if _, name, ok := strings.Cut(parts[i], "--"); ok {
		parts[i] = name
	}
	return strings.Join(parts, "/")
}

func (p *importPlan) flatImportFile(key, rel string) bool {
	if rel == nativeIndexFile {
		if !p.topics || p.native {
			return false
		}
		p.native = true
		return true
	}
	owner := homeDir
	name, _, _ := strings.Cut(rel, "/")
	if parts := strings.Split(rel, "/"); len(parts) > 1 {
		name = parts[1]
	}
	if prefix, _, ok := strings.Cut(name, "--"); ok {
		owner = stateKey(Bank{Key: prefix})
	}
	return owner == key && !strings.HasPrefix(path.Base(rel), ".")
}

func localNativePointers(data []byte) []byte {
	keys := BankKeys()
	lines := strings.Split(string(data), "\n")
	lines = slices.DeleteFunc(lines, func(line string) bool {
		for _, link := range indexLink.FindAllStringSubmatch(line, -1) {
			parts := strings.Split(strings.TrimPrefix(link[1], "./"), "/")
			name := parts[0]
			if len(parts) > 1 && slices.Contains(flatSubs, parts[0]) {
				name = parts[1]
			}
			if key, _, ok := strings.Cut(name, "--"); ok && !slices.Contains(keys, key) {
				return true
			}
		}
		return false
	})
	return []byte(strings.Join(lines, "\n"))
}

// localStores limits imports to banks already known on this host.
func localStores() map[string]bool {
	out := map[string]bool{}
	for _, b := range Banks() {
		if statOK(b.SubDir("")) {
			out[stateKey(b)] = true
		}
	}
	return out
}

func (p *importPlan) skipStore(key, why string) {
	p.lines = append(p.lines, "skipped "+key+": "+why)
	p.st.skipped++
}

// selectPairs pairs source and local stores; a store missing on either side is skipped and listed.
func selectPairs(opt ImportOptions, p *importPlan) ([]string, error) {
	stores, err := sourceStores(opt.From)
	if err != nil {
		return nil, err
	}
	if opt.All {
		return allPairs(stores, p)
	}
	_, own, err := ResolveBank(opt.Repo)
	if err != nil {
		return nil, importInput("%v", err)
	}
	key := stateKey(own)
	_, ok := stores[key]
	switch fi, serr := os.Stat(StoreDir()); {
	case serr != nil || !fi.IsDir() || (!own.Home() && !slices.Contains(BankKeys(), own.Key)):
		p.skipStore(key, "no local store")
	case !ok:
		p.skipStore(key, "no store in the source")
	default:
		return []string{key}, nil
	}
	return nil, nil
}

func allPairs(stores map[string]string, p *importPlan) ([]string, error) {
	local := localStores()
	var pairs []string
	for _, key := range slices.Sorted(maps.Keys(stores)) {
		if local[key] {
			pairs = append(pairs, key)
		} else {
			p.skipStore(key, "no local store")
		}
	}
	return pairs, nil
}

// planStore walks one source store through os.Root, so no entry can resolve outside it.
func (p *importPlan) planStore(from, key string) error {
	root, err := os.OpenRoot(from)
	if err != nil {
		p.skipStore(key, err.Error())
		return nil
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			p.skipFile(key, rel, err.Error())
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "locks" || d.Name() == ".locks"):
			return fs.SkipDir
		case d.IsDir() && rel != "." && !slices.Contains(flatSubs, strings.Split(rel, "/")[0]):
			return fs.SkipDir
		case d.IsDir(), foldSkips(rel), rel == layoutFile:
		case !p.flatImportFile(key, rel):
		case !filepath.IsLocal(filepath.FromSlash(rel)):
			p.skipFile(key, rel, "path escapes the store")
		case !d.Type().IsRegular():
			p.skipFile(key, rel, "not a regular file")
		default:
			p.importFile(key, root, rel)
		}
		return nil
	})
}

func (p *importPlan) skipFile(key, rel, why string) {
	p.lines = append(p.lines, key+": skipped "+rel+": "+why)
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

func (p *importPlan) importFile(key string, root *os.Root, rel string) {
	name := importName(rel)
	if !p.topics && idFiles[name] == "" && !monthlyArchive(name) { // logs only unless --topics
		p.st.left++
		return
	}
	data, err := readSource(root, rel)
	if err != nil {
		p.skipFile(key, rel, err.Error())
		return
	}
	if rel == nativeIndexFile {
		data = localNativePointers(data)
	}
	if idFiles[name] != "" {
		data = unarchived(StoreDir(), rel, data)
	}
	dst := filepath.Join(StoreDir(), filepath.FromSlash(rel))
	cur, err := os.ReadFile(dst)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.record(key, rel, importAction{kind: impConflict, line: "conflict " + rel + ": kept local (" + err.Error() + ")"})
		return
	}
	p.record(key, rel, decideImport(name, cur, data, err == nil))
}

// unarchived drops the source blocks the target's archives already hold, so a rotated entry is not imported again.
func unarchived(tgt, rel string, data []byte) []byte {
	archives, _ := filepath.Glob(filepath.Join(tgt, "archive", strings.TrimSuffix(rel, ".md")+"-*.md"))
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
}

func (p *importPlan) record(key, rel string, a importAction) {
	if a.kind == impNone {
		return
	}
	p.lines = append(p.lines, key+": "+a.line)
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
	if textutil.CredentialRE.Match(a.data) {
		p.leaks = append(p.leaks, rel)
	}
	p.changes = append(p.changes, Change{Rel: filepath.FromSlash(rel), Data: a.data})
	b := bankOfStateKey(key)
	if !slices.Contains(p.touched, b) {
		p.touched = append(p.touched, b)
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
	case idFiles[rel] != "" || monthlyArchive(rel):
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

// foldSkips reports the generated index and lock files, which do not travel.
func foldSkips(rel string) bool {
	return rel == catalogFile || strings.HasSuffix(rel, ".lock") || filepath.Base(rel) == ".postupdate"
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

// sortNewestFirst stably orders dated blocks newest first; an undated block inherits the date above it.
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

var indexLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// bankOfStateKey inverts stateKey: "home" is the home bank.
func bankOfStateKey(key string) Bank {
	switch key {
	case homeDir:
		return HomeBank()
	case repoHomeStateKey:
		return Bank{Key: homeDir}
	}
	return Bank{Key: key}
}
