package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// buildBankIndex renders one routing line per durable entry (two-stage recall); "" when nothing is indexable.
func buildBankIndex(b Bank) (string, error) {
	var sb strings.Builder
	section := func(name, prefix, unit string) error {
		entries, err := entriesWithPrefix(b.Path(name), prefix)
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

// rotateSessionHistory moves entries past the cap to b's archive/sessionHistory-<yyyy-mm>.md, archive first.
func rotateSessionHistory(b Bank, now time.Time) (int, error) {
	path := b.Path("sessionHistory.md")
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
	archive := b.Sub("archive", "sessionHistory-"+now.Format("2006-01")+".md")
	if err := appendUnarchived(archive, overflow); err != nil {
		return 0, err // never truncate unarchived entries
	}
	if err := paths.AtomicWrite(path, []byte(rejoin(header, keep)), 0o600); err != nil {
		return 0, err
	}
	return len(overflow), nil
}

// sessionOverflow counts sessionHistory entries past the cap, shared by rotateSessionHistory and `dream --dry-run`.
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

// lockPath serializes bank writers; the physical lock lives under state per store key, never beside the bank.
func lockPathOf(b Bank) string { return filepath.Join(storeLockDir(b), ".postupdate") }

// settleBank rotates sessionHistory, archives overflow, regenerates region and catalog, and commits, under the lock.
func settleBank(root string, now time.Time) (rotated int, err error) {
	err = withFlatLock(BankLockWait, func() error {
		rotated, err = settleBankLocked(root, now)
		return err
	})
	return rotated, err
}

// settleBankLocked is settleBank for a caller already holding the bank lock.
func settleBankLocked(root string, now time.Time) (int, error) {
	return settleOwnLocked(BankFor(root), now)
}

func settleOwnLocked(own Bank, now time.Time) (rotated int, err error) {
	if skipped, err := recoverEach(); err != nil || skipped != nil {
		return 0, errors.Join(err, skipped)
	}
	banks := bankSet(own) // an unscaffolded repo gets its store from Init (with its origin), never here
	for _, b := range banks {
		n, err := settleDir(b, now)
		rotated += n
		if err != nil {
			return rotated, err
		}
	}
	if err := writeCatalog(now); err != nil {
		return rotated, err
	}
	return rotated, commitBanks("settle: "+stateKey(banks[0]), banks)
}

// Git runs with literal pathspecs, so flat scopes must name files rather than prefix globs.
func commitBanks(subject string, banks []Bank) error {
	var scope []string
	if !StoreRepo().Exists() {
		if err := StoreRepo().Ensure(); err != nil {
			return err
		}
		scope = append(scope, ".gitignore")
	}
	dirty, err := StoreRepo().Status()
	if err != nil {
		return err
	}
	for _, e := range dirty {
		owner := BankOfFile(e.Path)
		if owner.Home() && strings.Contains(e.Path, "--") {
			continue
		}
		if e.Path == nativeIndexFile || slices.Contains(banks, owner) && !strings.HasPrefix(e.Path, ".") {
			scope = append(scope, e.Path)
		}
	}
	return commitStore(subject, append(scope, catalogFile)...)
}

// bankSet is the banks a settle covers: root's own when scaffolded, then the home bank.
func bankSet(own Bank) []Bank {
	var out []Bank
	if !own.Home() && scaffolded(own) {
		out = append(out, own)
	}
	return append(out, HomeBank())
}

// scaffolded reports whether b has any typed file.
func scaffolded(b Bank) bool {
	return slices.ContainsFunc(bankFiles, func(n string) bool { return statOK(b.Path(n)) })
}

func settleLockedBank(b Bank, now time.Time) error {
	if _, err := settleOwnLocked(b, now); err != nil {
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
func newestMtime(b Bank) time.Time {
	var newest time.Time
	for _, name := range bankFiles {
		if fi, err := os.Stat(b.Path(name)); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}

// settleDir stamps IDs, strikes revised entries, rotates sessionHistory, writes the MEMORY.md region, wraps files.
func settleDir(b Bank, now time.Time) (int, error) {
	rotated, wrapped, err := settleSteps(b, now)
	return rotated, errors.Join(err, wrapDir(b, wrapped)) // a failed step still leaves the dir wrapped
}

// settleSteps is settleDir's writing steps; wrapped names the typed files they already left wrapped (nil on error).
func settleSteps(b Bank, now time.Time) (rotated int, wrapped map[string]string, err error) {
	_, texts, err := assignIDTexts(b, now)
	if err != nil {
		return 0, nil, err
	}
	if err := reviseDir(b, texts); err != nil {
		return 0, nil, err
	}
	if rotated, err = rotateSessionHistory(b, now); err != nil {
		return rotated, nil, err
	}
	_, err = writeBankRegion(b, now)
	return rotated, texts, err
}

// wrapDir wraps b's markdown, skipping the typed files in held: settle wrapped their text before writing it.
func wrapDir(b Bank, held map[string]string) error {
	targets, err := dirFmtTargets(b)
	if err != nil {
		return err
	}
	targets = slices.DeleteFunc(targets, func(p string) bool {
		_, ok := held[filepath.Base(p)]
		return ok && p == b.Path(filepath.Base(p)) // only the typed files at b's top level
	})
	_, err = formatFiles(targets, false) // update writes land wrapped, so lint-docs stays clean
	return err
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
