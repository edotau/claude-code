package memory

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
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
)

// ExportDB is the db side of the export guard; the shadow package implements it over the shadow store.
type ExportDB interface {
	Walk() ([]string, error)             // in-scope files on disk under the store root, slash rel paths
	Paths() ([]string, error)            // docs rows
	Render(rel string) ([]byte, error)   // shadow renderShadowDoc
	Rendered(rel string) (string, error) // sum of bytes last rendered/imported for rel, "" if never
	Import(rel, wantSum string) error    // parse file into rows if the db render still has wantSum ("": no doc), else ErrExportStale
	MarkRendered(rel, sum string) error
	RecordConflict(rel, kind string) error // lists rel in the db conflicts table for `memory conflicts`
}

// ErrExportStale is Import's refusal: the db changed since the guard rendered it.
var ErrExportStale = errors.New("memory: export import: db changed since the check")

// exportAfterAside is a test hook run once dest is moved aside.
var exportAfterAside func(dest string)

// exportAfterHoldLink is a test hook run once dest is linked to its hold, before dest is dropped.
var exportAfterHoldLink func(dest string)

// exportBeforeSwap is a test hook run after the guard's last check and before the file swap.
var exportBeforeSwap func(dest string)

// ExportReport lists paths written to disk, imported to db, or skipped as conflicts.
type ExportReport struct {
	Written, Imported, Conflicts []string
	dirs                         map[string]bool // directories whose names the guard changed, synced before it returns
}

// errExportSync marks a failed directory fsync, which is never tolerated like an index problem.
var errExportSync = errors.New("memory: export guard sync dirs")

// exportSyncDir is a test seam over paths.SyncDir.
var exportSyncDir = paths.SyncDir

func (r *ExportReport) touch(dest string) {
	if r.dirs == nil {
		r.dirs = map[string]bool{}
	}
	r.dirs[filepath.Dir(dest)] = true
}

// syncTouched fsyncs each touched directory and its parents up to root, which may hold a directory the guard created.
func (r *ExportReport) syncTouched(root string) error {
	done := map[string]bool{}
	var err error
	for d := range r.dirs {
		for ; !done[d]; d = filepath.Dir(d) {
			done[d] = true
			if _, serr := os.Stat(d); errors.Is(serr, fs.ErrNotExist) {
				break // nothing was created there, so nothing to make durable
			}
			err = errors.Join(err, exportSyncDir(d))
			if d == filepath.Clean(root) || !strings.HasPrefix(d, filepath.Clean(root)+string(filepath.Separator)) {
				break
			}
		}
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errExportSync, err)
	}
	return nil
}

// OpenExportDB opens the export db; the shadow package sets it in init, nil until then.
var OpenExportDB func() (ExportDB, func() error, error)

// ExportGuard reconciles root's Markdown files with db and refreshes MEMORY.md.
func ExportGuard(db ExportDB, root string) (rep ExportReport, err error) {
	err = WithBankLock(0, func() (e error) { rep, e = ExportGuardLocked(db, root); return })
	return rep, err
}

// ExportGuardLocked is ExportGuard for a caller already holding the bank lock, which is not reentrant.
func ExportGuardLocked(db ExportDB, root string) (ExportReport, error) {
	var rep ExportReport
	walked, err := db.Walk()
	if err != nil {
		return rep, fmt.Errorf("memory: export guard walk: %w", err)
	}
	dbPaths, err := db.Paths()
	if err != nil {
		return rep, fmt.Errorf("memory: export guard paths: %w", err)
	}
	inDB := make(map[string]bool, len(dbPaths))
	for _, p := range dbPaths {
		inDB[p] = true
	}
	err = guardEach(db, root, unionSorted(walked, dbPaths), inDB, &rep)
	if err == nil {
		_, err = refreshNativeIndex(time.Now(), false, nil)
	}
	rep.touch(filepath.Join(root, routerFile)) // the index refresh renames files in root
	rep.touch(asidePath())                     // and keeps the previous MEMORY.md in the state dir
	if serr := rep.syncTouched(root); serr != nil {
		return rep, serr // alone, so no index-problem filter upstream can swallow it
	}
	return rep, err
}

func guardEach(db ExportDB, root string, rels []string, inDB map[string]bool, rep *ExportReport) error {
	holds, err := listHolds()
	if err != nil {
		return err
	}
	for _, rel := range rels {
		if err := sweepHolds(db, filepath.Join(root, filepath.FromSlash(rel)), rel, holds[rel], rep); err != nil {
			return err
		}
		if err := guardOne(db, root, rel, inDB[rel], rep); err != nil {
			return err
		}
	}
	return nil
}

func guardOne(db ExportDB, root, rel string, inDB bool, rep *ExportReport) error {
	dest := filepath.Join(root, filepath.FromSlash(rel))
	disk, err := os.ReadFile(dest)
	diskOK := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("memory: export guard read %s: %w", rel, err)
	}
	if !inDB {
		if !diskOK {
			return nil
		}
		return importExport(db, dest, rel, "", nil, rep)
	}
	last, err := db.Rendered(rel)
	if err != nil {
		return fmt.Errorf("memory: export guard rendered %s: %w", rel, err)
	}
	dbb, err := db.Render(rel)
	if err != nil {
		return fmt.Errorf("memory: export guard render %s: %w", rel, err)
	}
	dbbSum := sha256Hex(dbb)
	if !diskOK {
		if last != "" {
			return conflictKeep(db, dest, rel, "deleted-outside", dbb, rep)
		}
		return writeExport(db, rel, dest, "", dbb, dbbSum, rep)
	}
	if bytes.Equal(disk, dbb) {
		if last != dbbSum {
			return db.MarkRendered(rel, dbbSum)
		}
		return nil
	}
	if sha256Hex(disk) == last {
		return writeExport(db, rel, dest, last, dbb, dbbSum, rep)
	}
	if dbbSum == last {
		return importExport(db, dest, rel, dbbSum, dbb, rep)
	}
	return bothChanged(db, dest, rel, last, disk, dbb, rep)
}

// bothChanged merges an id-keyed log's two sides entry by entry; any other file keeps the db side as a conflict.
func bothChanged(db ExportDB, dest, rel, last string, disk, dbb []byte, rep *ExportReport) error {
	merged, ok := mergeSides(rel, historyVersion(storeRootOf(dest, rel), rel, last), disk, dbb)
	if !ok {
		return conflictKeep(db, dest, rel, "both-changed", dbb, rep)
	}
	if !bytes.Equal(merged, disk) {
		placed, stranded, err := placeFile(dest, rel, sha256Hex(disk), merged, rep)
		if stranded != "" {
			return conflictKeepAs(db, dest, rel, rel+strandedSuffix(stranded, rel), "stranded-outside-copy", dbb, rep)
		}
		if err != nil || !placed {
			return err // an edit landed meanwhile: the db is untouched, so the next pass merges again
		}
	}
	return importExport(db, dest, rel, sha256Hex(dbb), dbb, rep)
}

// HeldDir is the state folder for the guard's kept copies, mirroring bank-relative paths; never inside the bank.
func HeldDir() string {
	d := filepath.Join(paths.MemoryStateDir(), "held")
	_ = os.MkdirAll(d, 0o700)
	return d
}

// heldName is the held-dir name for a kept copy of bank file rel with suffix, its parent folder created.
func heldName(rel, suffix string) (string, error) {
	name := filepath.Join(HeldDir(), filepath.FromSlash(rel)) + suffix
	return name, os.MkdirAll(filepath.Dir(name), 0o700)
}

// storeRootOf is the store root dest sits in as rel.
func storeRootOf(dest, rel string) string {
	return filepath.Clean(strings.TrimSuffix(dest, filepath.FromSlash(rel)))
}

// conflictKeep keeps the db side in the held dir and records the conflict, unless nothing would be lost.
func conflictKeep(db ExportDB, dest, rel, kind string, dbb []byte, rep *ExportReport) error {
	return conflictKeepAs(db, dest, rel, rel, kind, dbb, rep)
}

// conflictKeepAs is conflictKeep recording the conflict under doc.
func conflictKeepAs(db ExportDB, dest, rel, doc, kind string, dbb []byte, rep *ExportReport) error {
	cur, _ := os.ReadFile(dest) // the bank file as it stands after the write; absent reads as nil
	if !worthKeeping(storeRootOf(dest, rel), rel, dbb, cur) {
		if kind == "deleted-outside" { // the deletion itself is unsettled, so it stays listed with no copy
			return conflict(db, doc, kind, rep)
		}
		return nil
	}
	name, err := heldName(rel, ".db-conflict-"+sha256Hex(dbb)[:12])
	if err == nil {
		err = keepSidecar(name, dbb)
	}
	if err != nil {
		return fmt.Errorf("memory: export guard keep db side of %s: %w", rel, err)
	}
	rep.touch(name)
	rep.touch(HeldDir())
	return conflict(db, doc, kind, rep)
}

// keepSidecar writes data at name only when absent or already equal; an edited sidecar gets a new unique name.
func keepSidecar(name string, data []byte) error {
	tmp, err := writeTemp(name, data)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	err = os.Link(tmp, name)
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	if fi, lerr := os.Lstat(name); lerr == nil && fi.Mode().IsRegular() {
		if cur, rerr := os.ReadFile(name); rerr == nil && bytes.Equal(cur, data) {
			return nil
		}
	}
	_, err = keepUnique(tmp, name)
	return err
}

func conflict(db ExportDB, rel, kind string, rep *ExportReport) error {
	rep.Conflicts = append(rep.Conflicts, rel)
	if err := db.RecordConflict(rel, kind); err != nil {
		return fmt.Errorf("memory: export guard conflict %s: %w", rel, err)
	}
	return nil
}

func importExport(db ExportDB, dest, rel, want string, dbb []byte, rep *ExportReport) error {
	err := db.Import(rel, want)
	if errors.Is(err, ErrExportStale) {
		cur, rerr := db.Render(rel)
		if rerr != nil {
			return fmt.Errorf("memory: export guard render %s: %w", rel, rerr)
		}
		return conflictKeep(db, dest, rel, "changed-during-export", cur, rep)
	}
	if err != nil {
		return fmt.Errorf("memory: export guard import %s: %w", rel, err)
	}
	rep.Imported = append(rep.Imported, rel)
	return nil
}

func writeExport(db ExportDB, rel, dest, seen string, data []byte, sum string, rep *ExportReport) error {
	placed, stranded, err := placeFile(dest, rel, seen, data, rep)
	if stranded != "" {
		return conflictKeepAs(db, dest, rel, rel+strandedSuffix(stranded, rel), "stranded-outside-copy", data, rep)
	}
	if err != nil {
		return err
	}
	if !placed {
		return conflictKeep(db, dest, rel, "changed-during-export", data, rep)
	}
	if err := db.MarkRendered(rel, sum); err != nil {
		return fmt.Errorf("memory: export guard mark %s: %w", rel, err)
	}
	rep.Written = append(rep.Written, rel)
	return nil
}

// placeFile swaps data in at dest while dest still holds seen; placed is false when an outside edit got there first.
func placeFile(dest, rel, seen string, data []byte, rep *ExportReport) (placed bool, stranded string, err error) {
	rep.touch(dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return false, "", fmt.Errorf("memory: export guard mkdir %s: %w", rel, err)
	}
	same, err := unchanged(dest, seen)
	if err != nil {
		return false, "", fmt.Errorf("memory: export guard reread %s: %w", rel, err)
	}
	if !same {
		return false, "", nil
	}
	hold := ""
	if seen != "" {
		if hold, err = heldName(rel, holdMark+seen[:12]); err != nil {
			return false, "", fmt.Errorf("memory: export guard held dir %s: %w", rel, err)
		}
		rep.touch(hold)
	}
	swapped, kept, err := swapIn(dest, hold, data, seen)
	if !swapped && kept != "" {
		return false, kept, nil
	}
	if err != nil {
		return false, "", fmt.Errorf("memory: export guard write %s: %w", rel, err)
	}
	if kept != "" {
		return true, "", dropNeedlessHold(dest, rel, kept, data)
	}
	return swapped, "", nil
}

// dropNeedlessHold removes a fresh hold whose bytes git or the new file already has: it would only fill the held dir.
func dropNeedlessHold(dest, rel, hold string, data []byte) error {
	old, err := os.ReadFile(hold)
	if err != nil || worthKeeping(storeRootOf(dest, rel), rel, old, data) {
		return nil // an unreadable hold stays for the sweep, which reports it
	}
	if err := os.Remove(hold); err != nil {
		return fmt.Errorf("memory: export guard drop hold %s: %w", hold, err)
	}
	return nil
}

// strandedSuffix is a stranded hold's name past its bank file's held path, which conflictsTouch matches.
func strandedSuffix(stranded, rel string) string {
	return strings.TrimPrefix(stranded, filepath.Join(HeldDir(), filepath.FromSlash(rel)))
}

// swapIn moves dest aside to a hold from holdBase, then links data in; kept is the hold left, stranded if !swapped.
func swapIn(dest, holdBase string, data []byte, seen string) (swapped bool, kept string, err error) {
	tmp, err := writeTemp(dest, data)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = os.Remove(tmp) }()
	if exportBeforeSwap != nil {
		exportBeforeSwap(dest)
	}
	if seen == "" {
		ok, err := linkIn(tmp, dest)
		return ok, "", err
	}
	hold, moved, err := holdAside(dest, holdBase)
	if !moved {
		return false, "", err
	}
	cur, err := os.ReadFile(hold)
	if err != nil || sha256Hex(cur) != seen {
		if lerr := linkBack(hold, dest); lerr != nil {
			return false, hold, err
		}
		return false, "", err
	}
	ok, err := linkIn(tmp, dest)
	if !ok {
		_ = linkBack(hold, dest) // fails when a newer outside file owns dest; hold keeps only the seen bytes
		return false, "", err
	}
	return true, hold, nil
}

// holdAside hard-links dest to a fresh hold name from base, then drops dest unless a newer file took its place.
func holdAside(dest, base string) (hold string, moved bool, err error) {
	hold, err = keepUnique(dest, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if exportAfterHoldLink != nil {
		exportAfterHoldLink(dest)
	}
	a, aerr := os.Lstat(dest)
	b, berr := os.Lstat(hold)
	if aerr != nil || berr != nil || !os.SameFile(a, b) {
		return hold, false, nil // dest was replaced meanwhile: the newer file stays, the hold keeps the old inode
	}
	if err := os.Remove(dest); err != nil {
		return hold, false, err
	}
	if exportAfterAside != nil {
		exportAfterAside(dest)
	}
	return hold, true, nil
}

const holdMark = ".export-old-"

// keptCopyRE matches the basenames of the copies the guard keeps in the held dir.
var keptCopyRE = regexp.MustCompile(`^(.+)\.(?:outside-[0-9a-f]{12}-[0-9a-z]+|export-old-[0-9a-f]{12}-[0-9a-z]+|db-conflict-[0-9a-f]{12}(?:-[0-9a-z]+)?)$`)

// KeptCopyBase is the file a kept copy belongs to; ok is false unless the basename is exactly a kept-copy name.
func KeptCopyBase(rel string) (string, bool) {
	m := keptCopyRE.FindStringSubmatch(path.Base(rel))
	if m == nil {
		return rel, false
	}
	return path.Join(path.Dir(rel), m[1]), true
}

// heldCopies lists the held dir once, mapping each bank rel to its kept copies' paths.
func heldCopies() (map[string][]string, error) {
	root := HeldDir()
	out := map[string][]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if base, ok := KeptCopyBase(filepath.ToSlash(rel)); ok && err == nil {
			out[base] = append(out[base], p)
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("memory: export guard list held copies: %w", err)
	}
	return out, nil
}

// listHolds is heldCopies narrowed to the swap holds sweepHolds watches.
func listHolds() (map[string][]string, error) {
	all, err := heldCopies()
	for rel, ps := range all {
		prefix := path.Base(rel) + holdMark
		all[rel] = slices.DeleteFunc(ps, func(p string) bool { return !strings.HasPrefix(filepath.Base(p), prefix) })
	}
	return all, err
}

// sweepHolds checks dest's holds: an old-descriptor write is merged into dest when it can be, else kept and listed.
func sweepHolds(db ExportDB, dest, rel string, holds []string, rep *ExportReport) error {
	prefix := filepath.Base(dest) + holdMark
	for _, h := range holds {
		want, _, _ := strings.Cut(strings.TrimPrefix(filepath.Base(h), prefix), "-")
		cur, err := os.ReadFile(h)
		if err != nil {
			return fmt.Errorf("memory: export guard read held copy %s: %w", h, err)
		}
		if strings.HasPrefix(sha256Hex(cur), want) {
			continue
		}
		if err := releaseHold(db, dest, rel, h, want, cur, rep); err != nil {
			return err
		}
	}
	return nil
}

// releaseHold folds a written hold into dest, else keeps it as an outside copy when needed; then drops the hold.
func releaseHold(db ExportDB, dest, rel, h, want string, cur []byte, rep *ExportReport) error {
	root := storeRootOf(dest, rel)
	disk, rerr := os.ReadFile(dest)
	folded := false
	if rerr == nil {
		merged, ok := mergeSides(rel, historyVersion(root, rel, want), disk, cur)
		if folded = ok && bytes.Equal(merged, disk); ok && !folded {
			placed, stranded, err := placeFile(dest, rel, sha256Hex(disk), merged, rep)
			if err != nil {
				return err
			}
			folded = placed && stranded == ""
		}
	}
	if folded || !worthKeeping(root, rel, cur, disk) {
		return removeHold(h)
	}
	kept, err := heldName(rel, ".outside-"+sha256Hex(cur)[:12])
	if err == nil {
		_, err = keepUnique(h, kept)
	}
	if err != nil {
		return fmt.Errorf("memory: export guard keep outside write %s: %w", h, err)
	}
	rep.touch(h)
	if err := removeHold(h); err != nil { // the inode lives on under its kept name
		return err
	}
	return conflict(db, rel, "changed-during-export", rep)
}

func removeHold(h string) error {
	if err := os.Remove(h); err != nil {
		return fmt.Errorf("memory: export guard release hold %s: %w", h, err)
	}
	return nil
}

// keepUnique hard-links src to a fresh name starting with base; it never replaces an existing file.
func keepUnique(src, base string) (string, error) {
	for i := 0; ; i++ {
		name := base + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.Itoa(i)
		err := os.Link(src, name)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
}

// linkBack returns the held outside copy to dest unless a newer file already owns it, in which case hold is kept.
func linkBack(hold, dest string) error {
	if err := os.Link(hold, dest); err != nil {
		return err
	}
	return os.Remove(hold)
}

// linkIn publishes tmp as dest only when dest is absent.
func linkIn(tmp, dest string) (bool, error) {
	err := os.Link(tmp, dest)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	return err == nil, err
}

func writeTemp(dest string, data []byte) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(dest), ".export-*")
	if err != nil {
		return "", err
	}
	_, werr := f.Write(data)
	if serr := f.Sync(); werr == nil {
		werr = serr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(f.Name())
	}
	return f.Name(), werr
}

// unchanged: dest holds seen's bytes and kept its stat.
func unchanged(dest, seen string) (bool, error) {
	fi1, err := os.Stat(dest)
	if errors.Is(err, fs.ErrNotExist) {
		_, err2 := os.Stat(dest)
		return seen == "" && errors.Is(err2, fs.ErrNotExist), nil
	}
	if err != nil {
		return false, err
	}
	cur, err := os.ReadFile(dest)
	if err != nil || sha256Hex(cur) != seen {
		return false, err
	}
	fi2, err := os.Stat(dest)
	if err != nil {
		return false, nil
	}
	return os.SameFile(fi1, fi2) && fi1.Size() == fi2.Size() && fi1.ModTime().Equal(fi2.ModTime()), nil
}

func unionSorted(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, s := range append(slices.Clone(a), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// worthKeeping is false when a copy would save nothing: its bytes are in the store's git history, or cur holds it all.
func worthKeeping(root, rel string, data, cur []byte) bool {
	return !entriesHeld(rel, data, cur) && !inHistory(root, rel, data)
}

// entriesHeld reports whether cur has data's preamble and every one of its entries, by id (by text when unstamped).
func entriesHeld(rel string, data, cur []byte) bool {
	if cur == nil {
		return false
	}
	d, c := parseSide(ParseDoc(rel, data)), parseSide(ParseDoc(rel, cur))
	if d.pre != c.pre {
		return false
	}
	for _, k := range d.keys {
		if e, ok := c.ents[k]; !ok || e.body != d.ents[k].body {
			return false
		}
	}
	return true
}

// historyBlobs lists the blob ids rel had in the store's git history, newest first; none without a repo.
func historyBlobs(root, rel string) []string {
	repo := storeRepo{Dir: root}
	if !repo.Exists() {
		return nil
	}
	out, err := repo.run("log", "--all", "--format=", "--raw", "--no-abbrev", "--", rel)
	if err != nil {
		return nil
	}
	var ids []string
	for _, ln := range splitLines(out) {
		f := strings.Fields(ln)
		if len(f) < 5 || !strings.HasPrefix(f[0], ":") {
			continue
		}
		for _, id := range f[2:4] {
			if strings.Trim(id, "0") != "" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// inHistory reports whether data is byte for byte a version of rel in the store's git history.
func inHistory(root, rel string, data []byte) bool {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return slices.Contains(historyBlobs(root, rel), hex.EncodeToString(h.Sum(nil)))
}

// historyMax bounds the versions historyVersion reads back.
const historyMax = 8

// historyVersion is the newest of rel's recent git versions whose sha256 starts with sum, or nil: the merge base.
func historyVersion(root, rel, sum string) []byte {
	if sum == "" {
		return nil
	}
	gitDir := filepath.Join(root, ".git")
	for i, id := range historyBlobs(root, rel) {
		if i == historyMax {
			break
		}
		out, err := proc.GitRawTimeout(root, "-C", root, "--git-dir="+gitDir, "cat-file", "blob", id)
		if err == nil && strings.HasPrefix(sha256Hex([]byte(out)), sum) {
			return []byte(out)
		}
	}
	return nil
}

// mergeable reports a top-level or archived id-keyed log the grammar orders newest-first or appended.
func mergeable(rel string) bool {
	g := GrammarOf(rel)
	top := !strings.Contains(rel, "/") || strings.HasPrefix(rel, "archive/")
	return top && (g == GrammarDated || g == GrammarBullets)
}

// mergeEntry is a merge side entry: text without trailing blank lines, how many it had, whether it ended the file.
type mergeEntry struct {
	body string
	tail int
	last bool
}

// mergeSide is one version of a log: its preamble, its entry keys in order ("id:" or "text:"), and the entries.
type mergeSide struct {
	doc  Doc
	pre  string
	keys []string
	ents map[string]mergeEntry
	dup  bool // a key repeats, so entries cannot be matched one to one
}

func parseSide(d Doc) mergeSide {
	s := mergeSide{doc: d, ents: map[string]mergeEntry{}}
	s.pre = strings.TrimRight(strings.Join(slices.Concat(d.Front, []string{d.Header}, d.Hidden), "\n"), "\n")
	for i, e := range d.Entries {
		n := len(e.Lines)
		for n > 1 && e.Lines[n-1] == "" {
			n--
		}
		body := strings.Join(e.Lines[:n], "\n")
		k := "text:" + body
		if e.ID != "" {
			k = "id:" + e.ID
		}
		if _, seen := s.ents[k]; seen {
			s.dup = true
		}
		s.keys = append(s.keys, k)
		s.ents[k] = mergeEntry{body: body, tail: len(e.Lines) - n, last: i == len(d.Entries)-1}
	}
	return s
}

// mergeSides unions a log's entries from file and other in file's order; ok is false on an edit both sides made.
func mergeSides(rel string, base, file, other []byte) (merged []byte, ok bool) {
	if !mergeable(rel) {
		return nil, false
	}
	f, o := parseSide(ParseDoc(rel, file)), parseSide(ParseDoc(rel, other))
	b := mergeSide{ents: map[string]mergeEntry{}, pre: "\x00"} // no base: nothing is known deleted or unedited
	if base != nil {
		b = parseSide(ParseDoc(rel, base))
	}
	if f.dup || o.dup || b.dup {
		return nil, false
	}
	pre := f.doc
	if f.pre != o.pre {
		switch {
		case f.pre == b.pre:
			pre = o.doc
		case o.pre != b.pre:
			return nil, false
		}
	}
	var out []mergeEntry
	for _, k := range mergeOrder(f, o) {
		e, keep, ok := resolveEntry(k, f, o, b)
		if !ok {
			return nil, false
		}
		if keep {
			out = append(out, e)
		}
	}
	return renderMerged(pre, out, f, o), true
}

// resolveEntry picks key k's entry three ways against base b; keep is false for a one-sided delete.
func resolveEntry(k string, f, o, b mergeSide) (e mergeEntry, keep, ok bool) {
	fe, inF := f.ents[k]
	oe, inO := o.ents[k]
	be, inB := b.ents[k]
	switch {
	case inF && inO && (fe.body == oe.body || (inB && oe.body == be.body)):
		return fe, true, true
	case inF && inO:
		return oe, true, inB && fe.body == be.body
	case inB: // one side dropped it: fine only when the other left it as it was
		return mergeEntry{}, false, (inF && fe.body == be.body) || (inO && oe.body == be.body)
	case inF:
		return fe, true, true
	}
	return oe, true, true
}

// mergeOrder is file's keys, each key only other has placed after the file-only run that follows its shared anchor.
func mergeOrder(f, o mergeSide) []string {
	after := map[string][]string{} // shared key, "" for the start, to the other-only keys other has after it
	var anchors []string
	anchor := ""
	for _, k := range o.keys {
		if _, ok := f.ents[k]; ok {
			anchor = k
		} else if after[anchor] = append(after[anchor], k); len(after[anchor]) == 1 {
			anchors = append(anchors, anchor)
		}
	}
	var out []string
	flush := func(a string) { out = append(out, after[a]...); delete(after, a) }
	anchor = ""
	for _, k := range f.keys {
		if _, shared := o.ents[k]; shared {
			flush(anchor)
			anchor = k
		}
		out = append(out, k)
	}
	for _, a := range append([]string{anchor}, anchors...) { // the tail run, then any anchor file orders differently
		flush(a)
	}
	return out
}

// renderMerged writes the entries under pre's preamble, separated as file separates them and ending as file ends.
func renderMerged(pre Doc, ents []mergeEntry, f, o mergeSide) []byte {
	sep, end := 0, 1
	if pre.Grammar == GrammarDated {
		sep = 1
	}
	for _, s := range []mergeSide{o, f} { // file's habits win over other's
		if n := len(s.keys); n > 0 {
			end = s.ents[s.keys[n-1]].tail
			if n > 1 {
				sep = s.ents[s.keys[0]].tail
			}
		}
	}
	d := Doc{Name: pre.Name, Grammar: pre.Grammar, Front: pre.Front, Header: pre.Header, Hidden: pre.Hidden}
	for i, e := range ents {
		tail := e.tail
		if i == len(ents)-1 {
			tail = end
		} else if e.last {
			tail = sep
		}
		d.Entries = append(d.Entries, DocEntry{Lines: append(strings.Split(e.body, "\n"), make([]string, tail)...)})
	}
	return d.Render()
}
