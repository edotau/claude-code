package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
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
	f.docs[rel], f.last[rel] = string(b), textutil.SHA256Hex(b)
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

// heldRead reads the held copy of bank file rel with suffix, which the guard keeps under the state dir.
func heldRead(t *testing.T, rel, suffix string) string {
	t.Helper()
	return exportRead(t, HeldDir(), rel+suffix)
}

// heldGlob lists held copies of bank file rel whose suffix matches pattern, and fails on any copy left in the bank.
func heldGlob(t *testing.T, rel, pattern string) []string {
	t.Helper()
	if m, _ := filepath.Glob(filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(rel)) + ".*-*"); len(m) != 0 {
		t.Fatalf("kept copies in the bank: %v", m)
	}
	m, _ := filepath.Glob(filepath.Join(HeldDir(), filepath.FromSlash(rel)) + pattern)
	return m
}

func exportGuard(t *testing.T, db *fakeExportDB) ExportReport {
	t.Helper()
	rep, err := ExportGuardLocked(db, db.root)
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
	if _, err := os.Stat(nativeIndex()); err != nil {
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
	db := &fakeExportDB{root: root, docs: map[string]string{"notes/c.md": "# C\n"}, last: map[string]string{}}
	exportGuard(t, db)
	db.docs["notes/c.md"] = "# C\n\n- db edit\n"
	exportWrite(t, root, "notes/c.md", "# C\n\n- disk edit\n")
	if rep := exportGuard(t, db); !slices.Equal(rep.Conflicts, []string{"notes/c.md"}) ||
		exportRead(t, root, "notes/c.md") != "# C\n\n- disk edit\n" || db.docs["notes/c.md"] != "# C\n\n- db edit\n" {
		t.Fatalf("both moved = %+v, want a conflict with both sides untouched", rep)
	}
	if got := heldRead(t, "notes/c.md", ".db-conflict-"+textutil.SHA256Hex([]byte("# C\n\n- db edit\n"))[:12]); got != "# C\n\n- db edit\n" {
		t.Fatalf("db side kept as %q, want the db render", got)
	}
	if err := os.Remove(filepath.Join(root, "notes/c.md")); err != nil {
		t.Fatal(err)
	}
	rep := exportGuard(t, db)
	if _, err := os.Stat(filepath.Join(root, "notes/c.md")); !errors.Is(err, fs.ErrNotExist) || !slices.Contains(rep.Conflicts, "notes/c.md") ||
		!slices.Contains(db.conflicts, "deleted-outside notes/c.md") {
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
	if got := heldRead(t, "conventions.md", ".db-conflict-"+textutil.SHA256Hex([]byte("# C\n\n- db edit\n"))[:12]); got != "# C\n\n- db edit\n" {
		t.Fatalf("db side snapshot = %q", got)
	}
}

func TestExportGuardOldDescriptorWrite(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	db := &fakeExportDB{root: root, docs: map[string]string{"notes/c.md": "# C\n\nold body\n"}, last: map[string]string{}}
	exportGuard(t, db)
	f, err := os.OpenFile(filepath.Join(root, "notes/c.md"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	db.docs["notes/c.md"] = "# C\n\n- db edit\n"
	if rep := exportGuard(t, db); !slices.Equal(rep.Written, []string{"notes/c.md"}) {
		t.Fatalf("swap = %+v, want a write", rep)
	}
	if _, err := f.WriteString("- late write\n"); err != nil {
		t.Fatal(err)
	}
	rep := exportGuard(t, db)
	if !slices.Contains(rep.Conflicts, "notes/c.md") || !slices.Contains(db.conflicts, "changed-during-export notes/c.md") {
		t.Fatalf("late write = %+v %v, want a conflict", rep, db.conflicts)
	}
	m := heldGlob(t, "notes/c.md", ".outside-*")
	if got, _ := os.ReadFile(m[0]); len(m) != 1 || string(got) != "# C\n\nold body\n- late write\n" {
		t.Fatalf("outside copy = %v %q, want the late write kept", m, got)
	}
	if h := heldGlob(t, "notes/c.md", holdMark+"*"); len(h) != 0 {
		t.Fatalf("hold left after detection: %v", h)
	}
}

func TestExportGuardMergesOldDescriptorAppendToLog(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	db := &fakeExportDB{root: root, docs: map[string]string{"conventions.md": "# C\n\n- a\n- b\n"}, last: map[string]string{}}
	exportGuard(t, db)
	f, err := os.OpenFile(filepath.Join(root, "conventions.md"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	db.docs["conventions.md"] = "# C\n\n- a\n- db edit\n" // drops "- b", so the swap keeps a hold
	exportGuard(t, db)
	if _, err := f.WriteString("- late write\n"); err != nil {
		t.Fatal(err)
	}
	rep := exportGuard(t, db)
	if want := "# C\n\n- a\n- db edit\n- b\n- late write\n"; exportRead(t, root, "conventions.md") != want || db.docs["conventions.md"] != want {
		t.Fatalf("late append = %q / db %q, want both entries merged", exportRead(t, root, "conventions.md"), db.docs["conventions.md"])
	}
	if len(rep.Conflicts)+len(db.conflicts) != 0 || len(heldGlob(t, "conventions.md", ".outside-*")) != 0 {
		t.Fatalf("merged late append = %+v %v, want no conflict and no copy", rep, db.conflicts)
	}
}

func TestExportGuardNeverDeletesHold(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	db := &fakeExportDB{root: root, docs: map[string]string{"conventions.md": "# C\n\n- a\n"}, last: map[string]string{}}
	exportGuard(t, db)
	db.docs["conventions.md"] = "# C\n\n- db edit\n"
	exportGuard(t, db)
	for range 3 {
		exportGuard(t, db)
	}
	if h := heldGlob(t, "conventions.md", holdMark+"*"); len(h) != 1 {
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
	if got := heldRead(t, "conventions.md", ".db-conflict-"+textutil.SHA256Hex([]byte("# C\n\n- db edit\n"))[:12]); got != "# C\n\n- db edit\n" {
		t.Fatalf("db sidecar = %q", got)
	}
}

func TestExportGuardBracketPath(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	rel := "notes[1]/a.md"
	db := &fakeExportDB{root: root, docs: map[string]string{rel: "# A\n\nold\n"}, last: map[string]string{}}
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
	seen := textutil.SHA256Hex([]byte("x"))
	var holds []string
	for _, body := range []string{"first", "second"} {
		if err := os.WriteFile(dest, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		h, moved, err := holdAside(dest, dest+holdMark+seen[:12])
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
	hold, moved, err := holdAside(dest, dest+holdMark+textutil.SHA256Hex([]byte("old"))[:12])
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
	name := dest + ".db-conflict-" + textutil.SHA256Hex(data)[:12]
	if err := keepSidecar(name, data); err != nil {
		t.Fatal(err)
	}
	if err := keepSidecar(name, data); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(name + "*"); len(m) != 1 {
		t.Fatalf("equal sidecar duplicated: %v", m)
	}
	if err := os.WriteFile(name, []byte("edited by hand"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := keepSidecar(name, data); err != nil {
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
	name := dest + ".db-conflict-" + textutil.SHA256Hex(data)[:12]
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, name); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := keepSidecar(name, data); err != nil {
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
	if _, err := ExportGuardLocked(db, root); err == nil {
		t.Fatal("a failed directory sync reported success")
	} else if !errors.Is(err, errExportSync) {
		t.Fatalf("sync failure = %v, want errExportSync", err)
	}
}

// sessionLog is a sessionHistory.md body of stamped, newest-first entries.
func sessionLog(entries ...string) string {
	return "# Session history\n\n" + strings.Join(entries, "\n")
}

func sessionEntry(id, title, body string) string {
	return "## 2026-11-26 — " + title + " <!-- id:" + id + " -->\n\n" + body + "\n"
}

func TestExportGuardMergesSessionHistoryNewOnBothSides(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	old := sessionEntry("aaaa0001", "old", "old body.")
	db := &fakeExportDB{root: root, docs: map[string]string{"sessionHistory.md": sessionLog(old)}, last: map[string]string{}}
	exportGuard(t, db)
	dbNew, fileNew := sessionEntry("bbbb0002", "db new", "db body."), sessionEntry("cccc0003", "file new", "file body.")
	db.docs["sessionHistory.md"] = sessionLog(dbNew, old)
	exportWrite(t, root, "sessionHistory.md", sessionLog(fileNew, old))
	rep := exportGuard(t, db)
	want := sessionLog(fileNew, dbNew, old)
	if got := exportRead(t, root, "sessionHistory.md"); got != want || db.docs["sessionHistory.md"] != want {
		t.Fatalf("merge = %q / db %q, want %q", got, db.docs["sessionHistory.md"], want)
	}
	if len(rep.Conflicts)+len(db.conflicts) != 0 || len(heldGlob(t, "sessionHistory.md", ".*")) != 0 {
		t.Fatalf("merge = %+v %v, want no conflict and no copy", rep, db.conflicts)
	}
}

func TestExportGuardSameIDEditKeepsOneHeldCopy(t *testing.T) {
	isolatedConfig(t)
	root := paths.MemoryStoreDir()
	db := &fakeExportDB{root: root, docs: map[string]string{"sessionHistory.md": sessionLog(sessionEntry("aaaa0001", "s", "base."))}, last: map[string]string{}}
	exportGuard(t, db)
	db.docs["sessionHistory.md"] = sessionLog(sessionEntry("aaaa0001", "s", "db edit."))
	disk := sessionLog(sessionEntry("aaaa0001", "s", "disk edit."))
	exportWrite(t, root, "sessionHistory.md", disk)
	for range 2 {
		exportGuard(t, db)
	}
	if got := exportRead(t, root, "sessionHistory.md"); got != disk || !slices.Equal(slices.Compact(db.conflicts), []string{"both-changed sessionHistory.md"}) {
		t.Fatalf("same-id edit = %q %v, want the disk kept and one both-changed conflict", got, db.conflicts)
	}
	m := heldGlob(t, "sessionHistory.md", ".db-conflict-*")
	if got, _ := os.ReadFile(m[0]); len(m) != 1 || string(got) != db.docs["sessionHistory.md"] {
		t.Fatalf("held copies = %v %q, want one holding the db side", m, got)
	}
}

func TestExportGuardKeepsNoCopyOfAGitVersion(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	root := paths.MemoryStoreDir()
	v1 := "# X\n\nv1\n"
	exportWrite(t, root, "notes/x.md", v1)
	if _, _, err := StoreRepo().commit([]string{"notes/x.md"}, "seed", ""); err != nil {
		t.Fatal(err)
	}
	db := &fakeExportDB{root: root, docs: map[string]string{"notes/x.md": "# X\n\nv2\n"}, last: map[string]string{"notes/x.md": textutil.SHA256Hex([]byte(v1))}}
	exportGuard(t, db)
	db.docs["notes/x.md"] = v1 // the db side is a committed version, so holding it saves nothing
	exportWrite(t, root, "notes/x.md", "# X\n\nv3\n")
	if rep := exportGuard(t, db); len(rep.Conflicts)+len(db.conflicts) != 0 {
		t.Fatalf("guard = %+v %v, want no conflict for a db side git already has", rep, db.conflicts)
	}
	if m := heldGlob(t, "notes/x.md", ".*"); len(m) != 0 {
		t.Fatalf("held copies = %v, want none", m)
	}
}
