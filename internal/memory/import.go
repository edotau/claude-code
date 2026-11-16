package memory

import (
	"archive/tar"
	"bytes"
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

	"github.com/edotau/claude-code/internal/textutil"
)

// replaceFiles keep the target and archive the source verbatim; idFiles merge by block.
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
	if err == nil {
		top, b, _ := ResolveBank("")
		refreshBankNotice(os.Stderr, "memory import", top, b)
	}
	return res, err
}

type importStats struct{ merged, copied, kept, conflicts, skipped, left int }

type importPlan struct {
	changes []Change
	leaks   []string // planned writes the store commit would refuse as credential-shaped
	lines   []string
	st      importStats
	topics  bool
}

// planImport reads both sides and builds the changes without writing.
func planImport(opt ImportOptions) (*importPlan, error) {
	p := &importPlan{topics: opt.Topics}
	if err := checkImportLayout(opt.From); err != nil {
		return p, err
	}
	if err := p.planStore(opt.From); err != nil {
		return p, err
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
	if _, err := settleDir(HomeBank(), opt.Now); err != nil {
		return err
	}
	if err := writeCatalog(opt.Now); err != nil {
		return err
	}
	return commitBanks("settle: import "+label, []Bank{HomeBank()})
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

// flatImportFile reports a source file import reads: MEMORY.md once under --topics, never a dotfile.
func (p *importPlan) flatImportFile(rel string) bool {
	if rel == nativeIndexFile {
		return p.topics
	}
	return !strings.HasPrefix(path.Base(rel), ".")
}

// planStore walks the source store through os.Root, so no entry can resolve outside it.
func (p *importPlan) planStore(from string) error {
	root, err := os.OpenRoot(from)
	if err != nil {
		return importInput("--from %s: %v", from, err)
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			p.skipFile(rel, err.Error())
		case d.IsDir() && skipImportDir(rel, d.Name()):
			return fs.SkipDir
		case d.IsDir(), foldSkips(rel), rel == layoutFile:
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
		data = unarchived(StoreDir(), rel, data)
	}
	dst := filepath.Join(StoreDir(), filepath.FromSlash(rel))
	cur, err := os.ReadFile(dst)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.record(rel, importAction{kind: impConflict, line: "conflict " + rel + ": kept local (" + err.Error() + ")"})
		return
	}
	p.record(rel, decideImport(rel, cur, data, err == nil))
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
	if textutil.CredentialRE().Match(a.data) {
		p.leaks = append(p.leaks, rel)
	}
	p.changes = append(p.changes, Change{Rel: filepath.FromSlash(rel), Data: a.data})
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

// foldSkips reports the generated index and lock files, which do not travel.
func foldSkips(rel string) bool {
	return rel == catalogFile || strings.HasSuffix(rel, ".lock") || filepath.Base(rel) == ".postupdate"
}

// pointerLines lists src's non-blank, non-header lines whose link target (else full text) tgt lacks, each once.
func pointerLines(tgt, src string) []string {
	key := func(l string) string {
		if m := indexLink().FindStringSubmatch(l); m != nil {
			return m[1]
		}
		return l
	}
	seen := map[string]bool{}
	for l := range strings.SplitSeq(tgt, "\n") {
		seen[key(strings.TrimSpace(l))] = true
	}
	var add []string
	for l := range strings.SplitSeq(src, "\n") {
		t := strings.TrimSpace(l)
		if k := key(t); t != "" && !strings.HasPrefix(t, "## ") && !seen[k] {
			seen[k] = true
			add = append(add, l)
		}
	}
	return add
}

// mergeNativeIndex adds the source pointer lines the target lacks (outside the generated region), before any header.
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
	add := pointerLines(tgt, src)
	if len(add) == 0 {
		return tgt, 0, nil
	}
	at := len(tgt)
	if strings.HasPrefix(tgt, "## ") {
		at = 0
	} else if i := strings.Index(tgt, "\n## "); i >= 0 {
		at = i + 1
	}
	for at >= 2 && tgt[at-2:at] == "\n\n" {
		at-- // insert above the blank line that precedes the header
	}
	head, tail := tgt[:at], tgt[at:]
	if head != "" && !strings.HasSuffix(head, "\n") {
		head += "\n"
	}
	return head + strings.Join(add, "\n") + "\n" + tail, len(add), nil
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

var indexLink = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\]\(([^)\s]+)\)`)
})
