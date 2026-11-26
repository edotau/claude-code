package memory

import (
	"bytes"
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
type ExportReport struct{ Written, Imported, Conflicts []string }

// OpenExportDB opens the export db; the shadow package sets it in init, nil until then.
var OpenExportDB func() (ExportDB, func() error, error)

// ExportGuard reconciles root's Markdown files with db and refreshes MEMORY.md.
func ExportGuard(db ExportDB, root string) (ExportReport, error) {
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
	err = WithBankLock(0, func() error {
		for _, rel := range unionSorted(walked, dbPaths) {
			if err := sweepHolds(db, filepath.Join(root, filepath.FromSlash(rel)), rel, &rep); err != nil {
				return err
			}
			if err := guardOne(db, root, rel, inDB[rel], &rep); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return rep, err
	}
	_, ierr := refreshNativeIndex(time.Now(), false, nil)
	return rep, ierr
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
	return conflictKeep(db, dest, rel, "both-changed", dbb, rep)
}

// conflictKeep saves the db side beside the file, one file per content, as a rebuild clears the db rows.
func conflictKeep(db ExportDB, dest, rel, kind string, dbb []byte, rep *ExportReport) error {
	if err := keepSidecar(dest+".db-conflict-"+sha256Hex(dbb)[:12], dest, dbb); err != nil {
		return fmt.Errorf("memory: export guard keep db side of %s: %w", rel, err)
	}
	return conflict(db, rel, kind, rep)
}

// keepSidecar writes data at name only when absent or already equal; an edited sidecar gets a new unique name.
func keepSidecar(name, dest string, data []byte) error {
	tmp, err := writeTemp(dest, data)
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
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("memory: export guard mkdir %s: %w", rel, err)
	}
	same, err := unchanged(dest, seen)
	if err != nil {
		return fmt.Errorf("memory: export guard reread %s: %w", rel, err)
	}
	if !same {
		return conflictKeep(db, dest, rel, "changed-during-export", data, rep)
	}
	swapped, stranded, err := swapIn(dest, data, seen)
	if stranded != "" {
		return conflictKeep(db, dest, rel+strings.TrimPrefix(stranded, dest), "stranded-outside-copy", data, rep)
	}
	if err != nil {
		return fmt.Errorf("memory: export guard write %s: %w", rel, err)
	}
	if !swapped {
		return conflictKeep(db, dest, rel, "changed-during-export", data, rep)
	}
	if err := db.MarkRendered(rel, sum); err != nil {
		return fmt.Errorf("memory: export guard mark %s: %w", rel, err)
	}
	rep.Written = append(rep.Written, rel)
	return nil
}

// swapIn moves dest aside and links the new file in; the kept hold lets sweepHolds catch an old-descriptor write.
func swapIn(dest string, data []byte, seen string) (swapped bool, stranded string, err error) {
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
	hold, moved, err := holdAside(dest, seen)
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
	return true, "", nil
}

// holdAside hard-links dest to a fresh hold name, then drops the dest name unless a newer file took its place.
func holdAside(dest, seen string) (hold string, moved bool, err error) {
	hold, err = keepUnique(dest, dest+holdMark+seen[:12])
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

// keptCopyRE matches the basenames the guard writes for copies it never deletes.
var keptCopyRE = regexp.MustCompile(`^(.+)\.(?:outside-[0-9a-f]{12}-[0-9a-z]+|export-old-[0-9a-f]{12}-[0-9a-z]+|db-conflict-[0-9a-f]{12}(?:-[0-9a-z]+)?)$`)

// KeptCopyBase is the file a kept copy belongs to; ok is false unless the basename is exactly a kept-copy name.
func KeptCopyBase(rel string) (string, bool) {
	m := keptCopyRE.FindStringSubmatch(path.Base(rel))
	if m == nil {
		return rel, false
	}
	return path.Join(path.Dir(rel), m[1]), true
}

// sweepHolds keeps every moved-aside copy; an editor open across an export writes into a kept copy, which is listed.
func sweepHolds(db ExportDB, dest, rel string, rep *ExportReport) error {
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("memory: export guard list %s: %w", filepath.Dir(dest), err)
	}
	prefix := filepath.Base(dest) + holdMark
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		h := filepath.Join(filepath.Dir(dest), e.Name())
		want, _, _ := strings.Cut(strings.TrimPrefix(e.Name(), prefix), "-")
		cur, err := os.ReadFile(h)
		if err != nil {
			return fmt.Errorf("memory: export guard read held copy %s: %w", h, err)
		}
		if strings.HasPrefix(sha256Hex(cur), want) {
			continue
		}
		if _, err := keepUnique(h, dest+".outside-"+sha256Hex(cur)[:12]); err != nil {
			return fmt.Errorf("memory: export guard keep outside write %s: %w", h, err)
		}
		if err := os.Remove(h); err != nil { // the inode lives on under its kept name
			return fmt.Errorf("memory: export guard release hold %s: %w", h, err)
		}
		if err := conflict(db, rel, "changed-during-export", rep); err != nil {
			return err
		}
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
