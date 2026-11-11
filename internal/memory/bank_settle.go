package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// buildBankIndex renders one routing line per durable entry (two-stage recall); "" when nothing is indexable.
func buildBankIndex(dir string) (string, error) {
	var b strings.Builder
	section := func(name, prefix, unit string) error {
		entries, err := entriesWithPrefix(filepath.Join(dir, name), prefix)
		if err != nil || len(entries) == 0 {
			return err
		}
		fmt.Fprintf(&b, "\n## %s (%d %s)\n\n", name, len(entries), unit)
		for _, e := range entries {
			b.WriteString("- " + e + "\n")
		}
		return nil
	}
	for _, s := range [][3]string{{"decisionLog.md", "## ", "entries"}, {"sessionHistory.md", "## ", "entries"}, {"conventions.md", "- ", "items"}} {
		if err := section(s[0], s[1], s[2]); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

// rotateSessionHistory moves entries past the cap to <dir>/archive/sessionHistory-<yyyy-mm>.md, archive first.
func rotateSessionHistory(dir string, now time.Time) (int, error) {
	path := filepath.Join(dir, "sessionHistory.md")
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
	archive := filepath.Join(dir, "archive", "sessionHistory-"+now.Format("2006-01")+".md")
	if err := appendUnarchived(archive, overflow); err != nil {
		return 0, err // never truncate unarchived entries
	}
	if err := paths.AtomicWrite(path, []byte(rejoin(header, keep)), 0o600); err != nil {
		return 0, err
	}
	return len(overflow), nil
}

// sessionOverflow is the sessionHistory cap check rotateSessionHistory and `dream --dry-run` share: entries past the cap.
func sessionOverflow(body string) (header string, keep, overflow []string) {
	header, entries := splitHeadingBlocks(body)
	if len(entries) <= sessionHistoryMaxEntries {
		return header, entries, nil
	}
	return header, entries[:sessionHistoryMaxEntries], entries[sessionHistoryMaxEntries:]
}

// appendUnarchived appends the blocks archive does not already hold, so a retried rotation never duplicates them.
func appendUnarchived(archive string, blocks []string) error {
	existing, err := os.ReadFile(archive)
	if err != nil && !os.IsNotExist(err) {
		return err
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
		return nil
	}
	return appendBlocks(archive, missing)
}

// lockPath serializes bank writers (the update worker's headless pass, then its settle); the
// physical lock lives under state, namespaced per store key — never beside the committed bank.
func lockPath(dir string) string { return filepath.Join(storeLockDir(dir), ".postupdate") }

// settleBank rotates root's sessionHistory, archives settled overflow in scope (no auto-promotion to home), regenerates
// the region and catalog, and commits — all under the bank lock. Deletes nothing.
func settleBank(root string, now time.Time) (rotated int, err error) {
	err = WithBankLock(BankLockWait, func() error {
		rotated, err = settleBankLocked(root, now)
		return err
	})
	return rotated, err
}

// settleBankLocked is settleBank for a caller already holding the bank lock.
func settleBankLocked(root string, now time.Time) (rotated int, err error) {
	var dirs []string
	own := Dir(root)
	if _, err := os.Stat(own); err == nil { // an unscaffolded repo gets its store from Init (with its origin), never here
		dirs = append(dirs, own)
	}
	if g := HomeBankDir(); len(dirs) == 0 || !paths.SamePath(dirs[0], g) {
		dirs = append(dirs, g)
	}
	for _, d := range dirs {
		n, err := settleDir(d, now)
		rotated += n
		if err != nil {
			return rotated, err
		}
	}
	if err := writeCatalog(now); err != nil {
		return rotated, err
	}
	return rotated, commitStore("settle: "+filepath.Base(dirs[0]), append(storeRels(dirs), catalogFile)...)
}

// settleLocked is settleBankLocked for a writer that rolls back on its error, which names the settle.
func settleLocked(root string, now time.Time) error {
	if _, err := settleBankLocked(root, now); err != nil {
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

// restoreOnError restores snaps when err is set, so a retry never duplicates the write; both errors are returned.
func restoreOnError(err error, snaps []fileSnapshot) error {
	if err == nil {
		return nil
	}
	var rerr error
	for _, s := range snaps {
		rerr = errors.Join(rerr, s.restore())
	}
	if rerr != nil {
		return errors.Join(err, fmt.Errorf("roll back the write: %w", rerr))
	}
	return fmt.Errorf("%w (write rolled back)", err)
}

// newestMtime is the newest mtime across dir's six typed files — the "did the update write?" probe.
func newestMtime(dir string) time.Time {
	var newest time.Time
	for _, name := range bankFiles {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}

// settleDir stamps block IDs, rotates sessionHistory, regenerates dir's MEMORY.md region and wraps its files.
func settleDir(d string, now time.Time) (rotated int, err error) {
	if _, err := assignIDs(d, now); err != nil {
		return 0, err
	}
	rotated, err = rotateSessionHistory(d, now)
	if err != nil {
		return rotated, err
	}
	if _, err = writeBankRegion(d, now); err != nil {
		return rotated, err
	}
	targets, err := dirFmtTargets(d)
	if err != nil {
		return rotated, err
	}
	_, err = formatFiles(targets, false) // update writes land wrapped, so lint-docs stays clean
	return rotated, err
}

func writeCatalog(now time.Time) error {
	c, err := BuildCatalog(now)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(filepath.Join(StoreDir(), catalogFile), []byte(c), 0o644)
}

// storeRels maps paths to store-relative pathspecs, dropping any outside the store (an inert unresolved-store path).
func storeRels(dirs []string) []string {
	var rels []string
	for _, d := range dirs {
		if rel, err := filepath.Rel(StoreDir(), d); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			rels = append(rels, filepath.ToSlash(rel))
		}
	}
	return rels
}

// commitStore commits the dirty paths under scope (store-relative) as one commit; no scope commits nothing.
// A credential-shaped file inside the scope is refused by Commit.
func commitStore(subject string, scope ...string) error {
	repo := StoreRepo()
	if !repo.Exists() || len(scope) == 0 {
		return nil
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
