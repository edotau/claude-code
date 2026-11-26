package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// fakeExportDB keeps rendered docs in maps; Import reads the file under root like the shadow adapter.
type fakeExportDB struct {
	root       string
	docs, last map[string]string
	conflicts  []string
	hook       func(op, rel string) error // "import" runs after the file is read, "render" before the db side is returned
}

func (f *fakeExportDB) Walk() ([]string, error) {
	var out []string
	if err := os.MkdirAll(f.root, 0o700); err != nil {
		return nil, err
	}
	err := filepath.WalkDir(f.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") || slices.Contains([]string{"MEMORY.md", "index.md", "catalog.md"}, d.Name()) {
			return err
		}
		rel, err := filepath.Rel(f.root, p)
		out = append(out, filepath.ToSlash(rel))
		return err
	})
	return out, err
}

func (f *fakeExportDB) Paths() ([]string, error) {
	var out []string
	for p := range f.docs {
		out = append(out, p)
	}
	slices.Sort(out)
	return out, nil
}

func (f *fakeExportDB) Render(rel string) ([]byte, error) {
	if f.hook != nil {
		if err := f.hook("render", rel); err != nil {
			return nil, err
		}
	}
	return []byte(f.docs[rel]), nil
}

func (f *fakeExportDB) Rendered(rel string) (string, error) { return f.last[rel], nil }

func (f *fakeExportDB) MarkRendered(rel, sum string) error { f.last[rel] = sum; return nil }

func (f *fakeExportDB) RecordConflict(rel, kind string) error {
	f.conflicts = append(f.conflicts, kind+" "+rel)
	return nil
}

func (f *fakeExportDB) Import(rel, _ string) error {
	b, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(rel)))
	f.docs[rel], f.last[rel] = string(b), sha256Hex(b)
	if err == nil && f.hook != nil {
		err = f.hook("import", rel)
	}
	return err
}

func exportRead(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exportWrite(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exportGuard(t *testing.T, db *fakeExportDB) ExportReport {
	t.Helper()
	rep, err := ExportGuard(db, db.root)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestExportGuard(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	note := "---\nname: x\ndescription: d\nmetadata:\n  type: feedback\n---\n# X\n\nbody\n"
	db := &fakeExportDB{root: root, docs: map[string]string{"feedback/x.md": note, "conventions.md": "# C\n\n- a\n"}, last: map[string]string{}}

	if rep := exportGuard(t, db); !slices.Equal(rep.Written, []string{"conventions.md", "feedback/x.md"}) || exportRead(t, root, "feedback/x.md") != note {
		t.Fatalf("first export = %+v, want both files rendered byte-equal", rep)
	}
	if _, err := os.Stat(NativeIndex()); err != nil {
		t.Fatalf("MEMORY.md missing after export: %v", err)
	}
	exportWrite(t, root, "notes/new.md", "# New\n\nhi\n")
	exportWrite(t, root, "conventions.md", "# C\n\n- a\n- hand edit\n")
	rep := exportGuard(t, db)
	if !slices.Equal(rep.Imported, []string{"conventions.md", "notes/new.md"}) || len(rep.Written) != 0 {
		t.Fatalf("outside edits = %+v, want both imported and nothing written", rep)
	}
	if db.docs["notes/new.md"] != "# New\n\nhi\n" || db.docs["conventions.md"] != "# C\n\n- a\n- hand edit\n" {
		t.Fatalf("db docs = %v, want the outside bytes", db.docs)
	}
	if rep := exportGuard(t, db); len(rep.Written)+len(rep.Imported)+len(rep.Conflicts) != 0 {
		t.Fatalf("second export = %+v, want nothing to do", rep)
	}
}

func TestExportGuardConflictAndDelete(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	db := &fakeExportDB{root: root, docs: map[string]string{"conventions.md": "# C\n"}, last: map[string]string{}}
	exportGuard(t, db)
	db.docs["conventions.md"] = "# C\n\n- db edit\n"
	exportWrite(t, root, "conventions.md", "# C\n\n- disk edit\n")
	if rep := exportGuard(t, db); !slices.Equal(rep.Conflicts, []string{"conventions.md"}) ||
		exportRead(t, root, "conventions.md") != "# C\n\n- disk edit\n" || db.docs["conventions.md"] != "# C\n\n- db edit\n" {
		t.Fatalf("both moved = %+v, want a conflict with both sides untouched", rep)
	}
	if got := exportRead(t, root, "conventions.md.db-conflict-"+sha256Hex([]byte("# C\n\n- db edit\n"))[:12]); got != "# C\n\n- db edit\n" {
		t.Fatalf("db side kept as %q, want the db render", got)
	}
	if err := os.Remove(filepath.Join(root, "conventions.md")); err != nil {
		t.Fatal(err)
	}
	rep := exportGuard(t, db)
	if _, err := os.Stat(filepath.Join(root, "conventions.md")); !errors.Is(err, fs.ErrNotExist) || !slices.Contains(rep.Conflicts, "conventions.md") ||
		!slices.Contains(db.conflicts, "deleted-outside conventions.md") {
		t.Fatalf("outside delete = %+v (stat %v), want it left deleted and reported", rep, err)
	}
}

func TestExportGuardOutsideEditDuringSwap(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	db := &fakeExportDB{root: root, docs: map[string]string{"conventions.md": "# C\n"}, last: map[string]string{}}
	exportGuard(t, db)
	db.docs["conventions.md"] = "# C\n\n- db edit\n"
	exportBeforeSwap = func(string) { exportWrite(t, root, "conventions.md", "# C\n\n- typed meanwhile\n") }
	t.Cleanup(func() { exportBeforeSwap = nil })
	rep := exportGuard(t, db)
	if !slices.Equal(rep.Conflicts, []string{"conventions.md"}) || len(rep.Written) != 0 {
		t.Fatalf("edit during swap = %+v, want a conflict and no write", rep)
	}
	if got := exportRead(t, root, "conventions.md"); got != "# C\n\n- typed meanwhile\n" {
		t.Fatalf("outside edit lost: %q", got)
	}
	if got := exportRead(t, root, "conventions.md.db-conflict-"+sha256Hex([]byte("# C\n\n- db edit\n"))[:12]); got != "# C\n\n- db edit\n" {
		t.Fatalf("db side snapshot = %q", got)
	}
}

func TestExportGuardHoldsBankLock(t *testing.T) {
	isolatedConfig(t)
	old := BankLockWait
	BankLockWait = 50 * time.Millisecond
	t.Cleanup(func() { BankLockWait = old })
	db := &fakeExportDB{root: paths.MemoryStoreDir(), docs: map[string]string{}, last: map[string]string{}}
	err := WithBankLock(time.Second, func() error {
		_, gerr := ExportGuard(db, db.root)
		return gerr
	})
	if !errors.Is(err, ErrBankBusy) {
		t.Fatalf("guard under a held bank lock = %v, want ErrBankBusy", err)
	}
}

func TestExportGuardOldDescriptorWrite(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	db := &fakeExportDB{root: root, docs: map[string]string{"conventions.md": "# C\n"}, last: map[string]string{}}
	exportGuard(t, db)
	f, err := os.OpenFile(filepath.Join(root, "conventions.md"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	db.docs["conventions.md"] = "# C\n\n- db edit\n"
	if rep := exportGuard(t, db); !slices.Equal(rep.Written, []string{"conventions.md"}) {
		t.Fatalf("swap = %+v, want a write", rep)
	}
	if _, err := f.WriteString("- late write\n"); err != nil {
		t.Fatal(err)
	}
	rep := exportGuard(t, db)
	if !slices.Contains(rep.Conflicts, "conventions.md") || !slices.Contains(db.conflicts, "changed-during-export conventions.md") {
		t.Fatalf("late write = %+v %v, want a conflict", rep, db.conflicts)
	}
	m, _ := filepath.Glob(filepath.Join(root, "conventions.md.outside-*"))
	if got, _ := os.ReadFile(m[0]); len(m) != 1 || string(got) != "# C\n- late write\n" {
		t.Fatalf("outside copy = %v %q, want the late write kept", m, got)
	}
	if h, _ := filepath.Glob(filepath.Join(root, "conventions.md"+holdMark+"*")); len(h) != 0 {
		t.Fatalf("hold left after detection: %v", h)
	}
}

func TestExportGuardNeverDeletesHold(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	db := &fakeExportDB{root: root, docs: map[string]string{"conventions.md": "# C\n"}, last: map[string]string{}}
	exportGuard(t, db)
	db.docs["conventions.md"] = "# C\n\n- db edit\n"
	exportGuard(t, db)
	for range 3 {
		exportGuard(t, db)
	}
	if h, _ := filepath.Glob(filepath.Join(root, "conventions.md"+holdMark+"*")); len(h) != 1 {
		t.Fatalf("holds after repeated guards = %v, want the one kept", h)
	}
}

func TestExportKeepUniqueNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for p, body := range map[string]string{a: "first", b: "second"} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	k1, err1 := keepUnique(a, filepath.Join(dir, "kept"))
	k2, err2 := keepUnique(b, filepath.Join(dir, "kept"))
	if err1 != nil || err2 != nil || k1 == k2 {
		t.Fatalf("keepUnique = %q %q (%v, %v), want two names", k1, k2, err1, err2)
	}
	if got, _ := os.ReadFile(k1); string(got) != "first" {
		t.Fatalf("first kept copy = %q, want it untouched", got)
	}
}

func TestExportGuardStrandedKeepsDBSide(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	db := &fakeExportDB{root: root, docs: map[string]string{"conventions.md": "# C\n"}, last: map[string]string{}}
	exportGuard(t, db)
	db.docs["conventions.md"] = "# C\n\n- db edit\n"
	exportBeforeSwap = func(string) { exportWrite(t, root, "conventions.md", "# C\n\n- typed meanwhile\n") }
	exportAfterAside = func(string) { exportWrite(t, root, "conventions.md", "# C\n\n- someone else\n") }
	t.Cleanup(func() { exportBeforeSwap, exportAfterAside = nil, nil })
	rep := exportGuard(t, db)
	if !slices.ContainsFunc(rep.Conflicts, func(c string) bool { return strings.HasPrefix(c, "conventions.md"+holdMark) }) {
		t.Fatalf("stranded = %+v, want a conflict naming the held copy", rep)
	}
	if got := exportRead(t, root, "conventions.md.db-conflict-"+sha256Hex([]byte("# C\n\n- db edit\n"))[:12]); got != "# C\n\n- db edit\n" {
		t.Fatalf("db sidecar = %q", got)
	}
}

func TestExportGuardBracketPath(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	rel := "notes[1]/a.md"
	db := &fakeExportDB{root: root, docs: map[string]string{rel: "# A\n"}, last: map[string]string{}}
	exportGuard(t, db)
	f, err := os.OpenFile(filepath.Join(root, "notes[1]", "a.md"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	db.docs[rel] = "# A\n\n- db\n"
	exportGuard(t, db)
	if _, err := f.WriteString("- late\n"); err != nil {
		t.Fatal(err)
	}
	if rep := exportGuard(t, db); !slices.Contains(rep.Conflicts, rel) {
		t.Fatalf("late write under a bracket path = %+v, want a conflict", rep)
	}
}

func TestExportKeptCopyBaseIsExact(t *testing.T) {
	for rel, want := range map[string]bool{
		"notes/design.outside-review.md":           false,
		"notes.outside-abcdef012345-1/x.md":        false,
		"notes/y.md.outside-abcdef012345-k1a0":     true,
		"notes/y.md.export-old-abcdef012345-k1a0":  true,
		"notes/y.md.db-conflict-abcdef012345":      true,
		"notes/y.md.db-conflict-abcdef012345-k1a0": true,
	} {
		if _, ok := KeptCopyBase(rel); ok != want {
			t.Fatalf("KeptCopyBase(%q) ok = %v, want %v", rel, ok, want)
		}
	}
}

func TestExportHoldAsideNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "a.md")
	seen := sha256Hex([]byte("x"))
	var holds []string
	for _, body := range []string{"first", "second"} {
		if err := os.WriteFile(dest, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		h, moved, err := holdAside(dest, seen)
		if err != nil || !moved {
			t.Fatalf("holdAside = %v %v", moved, err)
		}
		holds = append(holds, h)
	}
	if holds[0] == holds[1] {
		t.Fatalf("both holds named %q", holds[0])
	}
	if got, _ := os.ReadFile(holds[0]); string(got) != "first" {
		t.Fatalf("first hold = %q, want it untouched", got)
	}
}

func TestExportHoldAsideKeepsFileReplacedMidSwap(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "a.md")
	if err := os.WriteFile(dest, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	exportAfterHoldLink = func(d string) {
		repl := d + ".new"
		if err := os.WriteFile(repl, []byte("typed"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(repl, d); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { exportAfterHoldLink = nil })
	hold, moved, err := holdAside(dest, sha256Hex([]byte("old")))
	if err != nil || moved {
		t.Fatalf("holdAside = %v %v, want not moved", moved, err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "typed" {
		t.Fatalf("replacement lost: %q", got)
	}
	if got, _ := os.ReadFile(hold); string(got) != "old" {
		t.Fatalf("hold = %q, want the old inode kept", got)
	}
}

func TestExportSidecarEditedIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "a.md")
	data := []byte("db side")
	name := dest + ".db-conflict-" + sha256Hex(data)[:12]
	if err := keepSidecar(name, dest, data); err != nil {
		t.Fatal(err)
	}
	if err := keepSidecar(name, dest, data); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(name + "*"); len(m) != 1 {
		t.Fatalf("equal sidecar duplicated: %v", m)
	}
	if err := os.WriteFile(name, []byte("edited by hand"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := keepSidecar(name, dest, data); err != nil {
		t.Fatal(err)
	}
	m, _ := filepath.Glob(name + "*")
	if got, _ := os.ReadFile(name); string(got) != "edited by hand" || len(m) != 2 {
		t.Fatalf("edited sidecar = %q among %v, want it kept beside a new copy", got, m)
	}
}

func TestExportSidecarSymlinkIsNotReused(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "a.md")
	data := []byte("db side")
	name := dest + ".db-conflict-" + sha256Hex(data)[:12]
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, name); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := keepSidecar(name, dest, data); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(name); fi.Mode().IsRegular() {
		t.Fatal("symlink replaced")
	}
	if m, _ := filepath.Glob(name + "-*"); len(m) != 1 {
		t.Fatalf("unique sidecars = %v, want one written beside the link", m)
	}
}

func TestExportGuardSyncsTouchedDirs(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	old := exportSyncDir
	t.Cleanup(func() { exportSyncDir = old })
	var synced []string
	exportSyncDir = func(d string) error { synced = append(synced, d); return nil }
	db := &fakeExportDB{root: root, docs: map[string]string{"feedback/x.md": "# X\n\nbody\n"}, last: map[string]string{}}
	exportGuard(t, db)
	for _, want := range []string{filepath.Join(root, "feedback"), root} {
		if !slices.Contains(synced, want) {
			t.Fatalf("synced %v, want %s among them", synced, want)
		}
	}
	exportSyncDir = func(string) error { return errors.New("disk gone") }
	db.docs["feedback/y.md"] = "# Y\n"
	if _, err := ExportGuard(db, root); err == nil {
		t.Fatal("a failed directory sync reported success")
	} else if !errors.Is(err, errExportSync) {
		t.Fatalf("sync failure = %v, want errExportSync", err)
	}
}
