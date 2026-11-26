package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// buildBankIndex renders one routing line per durable entry (two-stage recall); "" when nothing is indexable.
func buildBankIndex() (string, error) {
	var sb strings.Builder
	section := func(name, prefix, unit string) error {
		entries, err := entriesWithPrefix(Path(name), prefix)
		if err != nil || len(entries) == 0 {
			return err
		}
		fmt.Fprintf(&sb, "\n## %s (%d %s)\n\n", name, len(entries), unit)
		for _, e := range entries {
			sb.WriteString("- ")
			sb.WriteString(e)
			sb.WriteString("\n")
		}
		return nil
	}
	for _, s := range [][3]string{{"decisionLog.md", "## ", "entries"}, {"sessionHistory.md", "## ", "entries"}, {"conventions.md", "- ", "items"}} {
		if err := section(s[0], s[1], s[2]); err != nil {
			return "", err
		}
	}
	return sb.String(), nil
}

// rotateSessionHistory moves entries past the cap to archive/sessionHistory-<yyyy-mm>.md, archive first.
func rotateSessionHistory(now time.Time) (int, error) {
	path := Path("sessionHistory.md")
	data, err := readFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("rotate session history: %w", err)
	}
	header, keep, overflow := sessionOverflow(string(data))
	if len(overflow) == 0 {
		return 0, nil
	}
	archive := paths.MemorySub("archive", "sessionHistory-"+now.Format("2006-01")+".md")
	if ok, err := appendUnarchived(archive, overflow); err != nil || !ok {
		return 0, err // never truncate unarchived entries
	}
	if ok, err := writeIfStill(path, data, true, []byte(rejoin(header, keep)), 0o600); err != nil || !ok {
		return 0, err
	}
	return len(overflow), nil
}

// sessionOverflow counts sessionHistory entries past the cap, shared by rotateSessionHistory and `dream --dry-run`.
func sessionOverflow(body string) (header string, keep, overflow []string) {
	d := ParseDoc("sessionHistory.md", []byte(body))
	header, entries := docBlocks(d, false)
	if len(entries) <= sessionHistoryMaxEntries {
		return header, entries, nil
	}
	order := d.OldestFirst(0, len(entries))
	// Keep the newest by Doc date/sequence, but render each group in source order.
	move := make(map[int]bool, len(entries)-sessionHistoryMaxEntries)
	for _, i := range order[:len(entries)-sessionHistoryMaxEntries] {
		move[i] = true
	}
	for i, entry := range entries {
		if move[i] {
			overflow = append(overflow, entry)
		} else {
			keep = append(keep, entry)
		}
	}
	return header, keep, overflow
}

// appendUnarchived appends the blocks archive does not already hold, so a retried rotation never duplicates them.
func appendUnarchived(archive string, blocks []string) (bool, error) {
	existing, err := os.ReadFile(archive)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	_, held := splitHeadingBlocks(string(existing))
	seen := make(map[string]bool, len(held))
	for _, b := range held {
		seen[b] = true
	}
	var missing []string
	for _, b := range blocks {
		if !seen[b] {
			missing = append(missing, b)
		}
	}
	if len(missing) == 0 {
		return true, nil
	}
	return writeIfStill(archive, existing, err == nil, []byte(appendedText(archive, string(existing), missing)), 0o600)
}

// writeIfStill writes next over path only while it holds seen; an outside edit stays, next kept as a listed guard copy.
func writeIfStill(path string, seen []byte, existed bool, next []byte, mode os.FileMode) (bool, error) {
	cur, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if (err == nil) != existed || (err == nil && !bytes.Equal(cur, seen)) {
		kept := path + ".db-conflict-" + sha256Hex(next)[:12]
		if err := keepSidecar(kept, path, next); err != nil {
			return false, fmt.Errorf("memory settle: keep %s: %w", kept, err)
		}
		if err := paths.SyncDir(filepath.Dir(kept)); err != nil {
			return false, fmt.Errorf("memory settle: sync %s: %w", filepath.Dir(kept), err)
		}
		fmt.Fprintln(os.Stderr, "notice: memory settle kept an outside edit of", path, "and saved its own result as", kept)
		return false, nil
	}
	return true, paths.AtomicWrite(path, next, mode)
}

// lockPath serializes bank writers; the physical lock lives under state per store key, never beside the bank.
func lockPathOf() string { return filepath.Join(storeLockDir(), ".postupdate") }

// settleBank rotates sessionHistory, regenerates MEMORY.md and the catalog, and commits, under the lock.
func settleBank(now time.Time) (rotated int, err error) {
	err = withFlatLock(BankLockWait, func() error {
		rotated, err = settleBankLocked(now)
		return err
	})
	return rotated, tolerateIndex(err)
}

// tolerateIndex logs a refresh that wrote MEMORY.md without every pin, the doctor row carries it, and drops only that.
func tolerateIndex(err error) error {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		if errors.Is(err, errIndexProblem) {
			fmt.Fprintln(os.Stderr, "memory index:", err)
			return nil
		}
		return err
	}
	var rest []error
	for _, e := range joined.Unwrap() {
		if errors.Is(e, errIndexProblem) {
			fmt.Fprintln(os.Stderr, "memory index:", e)
		} else {
			rest = append(rest, e)
		}
	}
	return errors.Join(rest...)
}

// settleBankLocked is settleBank for a caller already holding the bank lock; a blocked MEMORY.md refresh is its error.
func settleBankLocked(now time.Time) (rotated int, err error) {
	return settleBankWith(now, nil, "settle: "+stateKey())
}

// settleBankWith settles with extra entries folded into the catalog (an import's), committing under subject.
func settleBankWith(now time.Time, extra []string, subject string) (rotated int, err error) {
	defer func() { recordIndexProblem(err) }()
	if skipped, err := recoverEach(); err != nil || skipped != nil {
		return 0, errors.Join(err, skipped)
	}
	truth, terr := TruthErr()
	switch {
	case terr != nil:
		return 0, terr // never settle files on a truth that cannot be read
	case truth == truthDB:
		return settleDB(now, extra, subject)
	}
	return settleFiles(now, extra, subject)
}

func settleFiles(now time.Time, extra []string, subject string) (rotated int, err error) {
	// an unscaffolded repo gets its store from Init (with its origin), never here
	if rotated, err = settleDir(now); err != nil {
		return rotated, err
	}
	if err := sweepNotes(); err != nil {
		return rotated, err
	}
	rep, ierr := refreshIndexFiles(now, extra)
	if ierr != nil && !errors.Is(ierr, errIndexProblem) {
		return rotated, ierr
	}
	if rep.notable() {
		fmt.Fprintln(os.Stderr, "memory index:", rep)
	}
	commitErr := commitBanks(subject)
	if commitErr == nil {
		shadowSettle(now)
	}
	return rotated, errors.Join(commitErr, ierr)
}

// settleDB settles in db truth under the held bank lock: guard, file steps on the export, guard, index, commit.
func settleDB(now time.Time, extra []string, subject string) (rotated int, err error) {
	if OpenExportDB == nil {
		return 0, errors.New("memory truth is db but no export db is linked")
	}
	edb, closeDB, err := OpenExportDB()
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, closeDB()) }()
	root := paths.MemoryStoreDir()
	ierr, err := guardPass(edb, root) // imports outside edits first, so the file steps see them
	if err != nil {
		return 0, err
	}
	if rotated, err = settleDir(now); err != nil {
		return rotated, err
	}
	if err := sweepNotes(); err != nil {
		return rotated, err
	}
	gerr, err := guardPass(edb, root) // imports the file steps' result (the rotation) and exports the db
	if err != nil {
		return rotated, err
	}
	rep, rerr := refreshIndexFiles(now, extra)
	if rerr != nil && !errors.Is(rerr, errIndexProblem) {
		return rotated, rerr
	}
	if rep.notable() {
		fmt.Fprintln(os.Stderr, "memory index:", rep)
	}
	return rotated, errors.Join(commitBanks(subject), ierr, gerr, rerr)
}

// guardPass runs the export guard without retaking the held lock, printing what it imported or kept apart.
func guardPass(db ExportDB, root string) (indexErr, err error) {
	rep, err := ExportGuardLocked(db, root)
	if len(rep.Imported) > 0 {
		fmt.Fprintln(os.Stderr, "notice: memory settle imported outside edits:", strings.Join(rep.Imported, ", "))
	}
	if len(rep.Conflicts) > 0 {
		fmt.Fprintln(os.Stderr, "notice: memory export conflicts (both sides kept):", strings.Join(rep.Conflicts, ", "))
	}
	if errors.Is(err, errIndexProblem) && !errors.Is(err, errExportSync) {
		return err, nil
	}
	return nil, err
}

// shadowSettle imports into the shadow db and logs a change in its divergent docs; it never fails the settle.
func shadowSettle(now time.Time) {
	if ShadowImport == nil {
		return
	}
	docs, changed, err := guardedShadowImport()
	if changed { // a failed import still logs the partial set it found
		err = errors.Join(err, appendShadowLog(now, append([]string{}, docs...)))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "notice: memory shadow:", err)
	}
}

func guardedShadowImport() (docs []string, changed bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			docs, changed, err = nil, false, fmt.Errorf("import panicked: %v", r)
		}
	}()
	return ShadowImport()
}

func shadowLogPath() string { return filepath.Join(paths.MemoryStateDir(), "shadow.log") }

func appendShadowLog(now time.Time, docs []string) error {
	b, err := json.Marshal(struct {
		Time string   `json:"time"`
		Docs []string `json:"docs"`
	}{now.UTC().Format(time.RFC3339), docs})
	if err != nil {
		return err
	}
	path := shadowLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	return errors.Join(werr, f.Close())
}

// refreshIndexFiles writes catalog.md and refreshes MEMORY.md, after the alias check that guards both.
func refreshIndexFiles(now time.Time, extra []string) (indexReport, error) {
	if err := checkIndexFiles(); err != nil {
		return indexReport{}, err
	}
	if err := writeCatalog(now); err != nil {
		return indexReport{}, err
	}
	return refreshNativeIndex(now, false, extra)
}

// Git runs with literal pathspecs, so flat scopes must name files rather than prefix globs.
func commitBanks(subject string) error {
	repo := StoreRepo()
	if !repo.Exists() {
		if err := repo.Ensure(); err != nil {
			return err
		}
	}
	dirty, err := repo.Status() // every dirty path is in scope, so a second status scoped to them would say the same
	if err != nil {
		return err
	}
	var rels []string
	for _, e := range dirty {
		rels = append(rels, e.Path)
	}
	if len(rels) == 0 {
		return nil
	}
	_, _, err = repo.Commit(rels, subject, "")
	return err
}

// scaffolded reports whether the bank has any typed file.
func scaffolded() bool {
	return slices.ContainsFunc(bankFiles, func(n string) bool { return statOK(Path(n)) })
}

func settleLockedBank(now time.Time) error {
	if _, err := settleBankLocked(now); tolerateIndex(err) != nil {
		return fmt.Errorf("settle: %w", err)
	}
	return nil
}

// fileSnapshot is a file's bytes before an append; existed is false when it was absent.
type fileSnapshot struct {
	path    string
	prev    []byte
	existed bool
}

// snapshotFile records path's current bytes; an absent file is a snapshot with existed false.
func snapshotFile(path string) (fileSnapshot, error) {
	prev, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fileSnapshot{}, err
	}
	return fileSnapshot{path: path, prev: prev, existed: err == nil}, nil
}

// restore puts the snapshot back: removes a file that was absent (if it now exists), else rewrites its prior bytes.
func (s fileSnapshot) restore() error {
	if !s.existed {
		if err := os.Remove(s.path); !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return paths.AtomicWrite(s.path, s.prev, 0o600)
}

// snapshotPaths snapshots each path before a locked write (fileSnapshot); unreadable refuses it.
func snapshotPaths(files ...string) ([]fileSnapshot, error) {
	snaps := make([]fileSnapshot, 0, len(files))
	for _, p := range files {
		s, err := snapshotFile(p)
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
	}
	return snaps, nil
}

// restoreSnaps restores every snapshot and joins the failures.
func restoreSnaps(snaps []fileSnapshot) error {
	var rerr error
	for _, s := range snaps {
		rerr = errors.Join(rerr, s.restore())
	}
	return rerr
}

// newestMtime is the newest mtime across dir's six typed files — the "did the update write?" probe.
func newestMtime() time.Time {
	var newest time.Time
	for _, name := range bankFiles {
		if fi, err := os.Stat(Path(name)); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}

// settleDir stamps IDs, strikes revised entries, rotates sessionHistory and wraps files.
func settleDir(now time.Time) (int, error) {
	rotated, wrapped, err := settleSteps(now)
	return rotated, errors.Join(err, wrapDir(wrapped)) // a failed step still leaves the dir wrapped
}

// settleSteps is settleDir's writing steps; wrapped names the typed files they already left wrapped (nil on error).
func settleSteps(now time.Time) (rotated int, wrapped map[string]string, err error) {
	_, texts, err := assignIDTexts(now)
	if err != nil {
		return 0, nil, err
	}
	if err := reviseDir(texts); err != nil {
		return 0, nil, err
	}
	if rotated, err = rotateSessionHistory(now); err != nil {
		return rotated, nil, err
	}
	return rotated, texts, nil
}

// wrapDir wraps the bank's markdown, skipping the typed files in held: settle wrapped their text before writing it.
func wrapDir(held map[string]string) error {
	targets, err := dirFmtTargets()
	if err != nil {
		return err
	}
	targets = slices.DeleteFunc(targets, func(p string) bool {
		_, ok := held[filepath.Base(p)]
		return ok && p == Path(filepath.Base(p)) // only the typed files at b's top level
	})
	_, err = formatFiles(targets, false) // update writes land wrapped, so lint-docs stays clean
	return err
}

// catalogDescMax caps a catalog description in runes, ellipsis included.
const catalogDescMax = 160

// writeCatalog writes catalog.md: the router, then the complete native-memory catalog; it holds no input.
func writeCatalog(now time.Time) error {
	c, err := BuildCatalog(now)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(filepath.Join(paths.MemoryStoreDir(), routerFile), []byte(c+nativeCatalog()), 0o644)
}

// nativeCatalog lists every native note by folder, one line each, from its front matter alone.
func nativeCatalog() string {
	var b strings.Builder
	b.WriteString(assets.MemoryNativeHeading())
	bad := 0
	rows := scanRows(noteDirs...)
	for _, d := range noteDirs {
		first := true
		for _, r := range rows {
			if !strings.HasPrefix(r.Rel, d+"/") {
				continue
			}
			if first {
				b.WriteString(assets.MemoryNativeFolder(d))
				first = false
			}
			if !r.OK {
				bad++
			}
			fmt.Fprintln(&b, r.line())
		}
	}
	if bad > 0 {
		b.WriteString(assets.MemoryNativeUnparsed(bad))
	}
	return b.String()
}

// clipDesc folds whitespace to one line and clips it to catalogDescMax runes with an ellipsis.
func clipDesc(s string) string {
	return textutil.Truncate(strings.Join(strings.Fields(s), " "), catalogDescMax)
}

// storeRels maps paths to store-relative pathspecs, dropping any outside the store (an inert unresolved-store path).
func storeRels(dirs []string) []string {
	var rels []string
	for _, d := range dirs {
		if rel, err := filepath.Rel(paths.MemoryStoreDir(), d); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			rels = append(rels, filepath.ToSlash(rel))
		}
	}
	return rels
}

// commitStore commits the dirty paths under scope as one commit (none without scope); Commit refuses credentials.
func commitStore(subject string, scope ...string) error {
	repo := StoreRepo()
	if len(scope) == 0 {
		return nil
	}
	if !repo.Exists() {
		if _, err := os.Stat(repo.Dir); err != nil {
			return nil
		}
		if err := repo.Ensure(); err != nil {
			return err
		}
	}
	st, err := repo.Status(scope...)
	if err != nil {
		return err
	}
	var rels []string
	for _, e := range st {
		rels = append(rels, e.Path)
	}
	if len(rels) == 0 {
		return nil
	}
	_, _, err = repo.Commit(rels, subject, "")
	return err
}

// NoteFolder is the store folder a top-level native note belongs in ("" keeps it at the top: typed or reserved).
func NoteFolder(name string) string {
	switch {
	case filepath.Ext(name) != ".md" || reservedNames[name]:
		return ""
	case strings.HasPrefix(strings.ToLower(name), "feedback"):
		return "feedback"
	}
	return "notes"
}

// noteDirs are the folders Claude Code's native notes are sorted into.
var noteDirs = []string{"feedback", "notes", techdebtDir}

// techdebtDir is searched and cataloged but kept out of prompt recall.
const techdebtDir = "techdebt"

// sweepNotes moves top-level native notes into feedback/ or notes/.
func sweepNotes() error {
	dir := paths.MemoryStoreDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		folder := NoteFolder(e.Name())
		if e.IsDir() || folder == "" {
			continue
		}
		to := filepath.Join(dir, folder, e.Name())
		if _, err := os.Stat(to); err == nil {
			continue // a same-named note already lives in the folder: leave the stray for a person to merge
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(dir, e.Name()), to); err != nil {
			return err
		}
	}
	return nil
}
