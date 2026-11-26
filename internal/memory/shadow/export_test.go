package shadow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/sqlite"
)

func setupShadowExport(t *testing.T) (string, map[string]string, memory.ExportDB) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", tmp)
	t.Setenv("HARNESS_KEYRING", "off")
	root := filepath.Join(tmp, "memory")
	fixture := map[string]string{
		"decisionLog.md": "# Decision Log\n\n## 2026-01-15 — Pick db <!-- id:0123abcd -->\n\nWhy: fast.\n",
		"conventions.md": "# Conventions\n\n- use gofmt\n",
		"feedback/x.md":  "---\nname: x\ndescription: d\nmetadata:\n  type: feedback\n---\n# X\n\nbody\n",
		"notes/y.md":     "# Y\n\nnote body\n",
	}
	for rel, body := range fixture {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	srep, err := RebuildShadow(context.Background())
	if err != nil || len(srep.Mismatched) != 0 {
		t.Fatalf("RebuildShadow: %+v err=%v", srep, err)
	}
	db, closeDB, err := memory.OpenExportDB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDB() })
	return root, fixture, db
}

func TestExportGuardShadow(t *testing.T) {
	root, fixture, db := setupShadowExport(t)
	rep, err := memory.ExportGuard(db, root)
	if err != nil || len(rep.Written) != 0 || len(rep.Imported) != 0 || len(rep.Conflicts) != 0 {
		t.Fatalf("initial ExportGuard: %+v err=%v", rep, err)
	}
	for rel, want := range fixture {
		if got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))); err != nil || string(got) != want {
			t.Fatalf("%s mismatch: %q err=%v", rel, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "MEMORY.md")); err != nil {
		t.Fatalf("MEMORY.md missing: %v", err)
	}
	testShadowExportImport(t, db, root, fixture["feedback/x.md"])
	testShadowExportConflict(t, db, root)
}

func testShadowExportImport(t *testing.T, db memory.ExportDB, root, fbBody string) {
	t.Helper()
	newNote := "---\nname: new\ndescription: fresh\nmetadata:\n  type: note\n---\n# New\n\nhi\n"
	editedConv := "# Conventions\n\n- use gofmt\n- hand edit\n"
	if err := os.WriteFile(filepath.Join(root, "notes", "new.md"), []byte(newNote), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "conventions.md"), []byte(editedConv), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := memory.ExportGuard(db, root)
	if err != nil || !slices.Equal(rep.Imported, []string{"conventions.md", "notes/new.md"}) {
		t.Fatalf("import ExportGuard: %+v err=%v", rep, err)
	}
	for rel, want := range map[string]string{"notes/new.md": newNote, "conventions.md": editedConv, "feedback/x.md": fbBody} {
		if got, err := db.Render(rel); err != nil || !bytes.Equal(got, []byte(want)) {
			t.Fatalf("db.Render(%s) = %q (err=%v), want %q", rel, got, err, want)
		}
	}
	rep, err = memory.ExportGuard(db, root)
	if err != nil || len(rep.Written) != 0 || len(rep.Imported) != 0 || len(rep.Conflicts) != 0 {
		t.Fatalf("idempotent ExportGuard: %+v err=%v", rep, err)
	}
}

func testShadowExportConflict(t *testing.T, db memory.ExportDB, root string) {
	t.Helper()
	sdb, err := sqlite.Open(ShadowDBPath(), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	store := NewSQLBankStore(sdb)
	_, vers, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(memory.Batch{Ops: []memory.Op{{Op: "update", ID: "0123abcd", Text: "Why: db edit."}}}, vers, time.Now()); err != nil {
		t.Fatal(err)
	}
	diskEdit := "# Decision Log\n\n## 2026-01-15 — Pick db <!-- id:0123abcd -->\n\nWhy: disk edit.\n"
	if err := os.WriteFile(filepath.Join(root, "decisionLog.md"), []byte(diskEdit), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := memory.ExportGuard(db, root)
	if err != nil || !slices.Equal(rep.Conflicts, []string{"decisionLog.md"}) {
		t.Fatalf("conflict ExportGuard: %+v err=%v", rep, err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "decisionLog.md")); string(got) != diskEdit {
		t.Fatalf("conflict clobbered disk: %q", got)
	}
	if err := os.Remove(filepath.Join(root, "notes", "new.md")); err != nil {
		t.Fatal(err)
	}
	rep, err = memory.ExportGuard(db, root)
	if err != nil || !slices.Contains(rep.Conflicts, "notes/new.md") {
		t.Fatalf("delete ExportGuard: %+v err=%v", rep, err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "new.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted file recreated or stat err: %v", err)
	}
}

func TestExportGuardRepeatedIDKeepsLinkage(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", tmp)
	t.Setenv("HARNESS_KEYRING", "off")
	p := filepath.Join(tmp, "memory", "decisionLog.md")
	a := "## 2026-01-15 — Same <!-- id:0123abcd -->\n\nfirst\n"
	b := "## 2026-01-14 — Same <!-- id:0123abcd -->\n\nsecond\n"
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("# Decision Log\n\n"+a+"\n"+b), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildShadow(context.Background()); err != nil {
		t.Fatal(err)
	}
	db, closeDB, err := memory.OpenExportDB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDB() })
	sdb := db.(*exportDB).db
	var second int64
	if err := sdb.QueryRow(`SELECT rowid FROM entries WHERE lines LIKE '%second%'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.Exec(`INSERT INTO vectors(entry_rowid, model, dim, vec, minted_at) VALUES(?, 'm', 1, x'00', '')`, second); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.ExportGuard(db, filepath.Join(tmp, "memory")); err != nil {
		t.Fatal(err)
	}
	newer := "## 2026-01-16 — Same <!-- id:0123abcd -->\n\nthird\n"
	if err := os.WriteFile(p, []byte("# Decision Log\n\n"+newer+"\n"+a+"\n"+b), 0o600); err != nil {
		t.Fatal(err)
	}
	if rep, err := memory.ExportGuard(db, filepath.Join(tmp, "memory")); err != nil || len(rep.Imported) != 1 {
		t.Fatalf("ExportGuard = %+v err=%v, want decisionLog.md imported", rep, err)
	}
	var lines string
	if err := sdb.QueryRow(`SELECT e.lines FROM vectors v JOIN entries e ON e.rowid = v.entry_rowid`).Scan(&lines); err != nil || !strings.Contains(lines, "second") {
		t.Fatalf("vector now on %q (err=%v), want the entry with the second text", lines, err)
	}
}

func TestExportGuardRecordsDeletedOutside(t *testing.T) {
	root, _, db := setupShadowExport(t)
	if _, err := memory.ExportGuard(db, root); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "notes", "y.md")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := memory.ExportGuard(db, root); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildShadow(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.(*exportDB).db.QueryRow(`SELECT count(*) FROM conflicts WHERE kind = 'deleted-outside'
		AND doc = 'notes/y.md' AND resolved = 0`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("deleted-outside rows = %d (err=%v), want exactly 1, kept through a rebuild", n, err)
	}
	wantExportConflictListed(t)
}

func wantExportConflictListed(t *testing.T) {
	t.Helper()
	if got, err := ExportConflicts(); err != nil || len(got) == 0 || !strings.HasPrefix(got[0], "deleted-outside notes/y.md ") {
		t.Fatalf("ExportConflicts = %q (%v)", got, err)
	}
}

func TestExportGuardRebuildBaselineIsImportedBytes(t *testing.T) {
	root, _, db := setupShadowExport(t)
	if _, err := RebuildShadow(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "conventions.md"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	got, err := db.Rendered("conventions.md")
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("baseline after rebuild = %q (%v), want the imported bytes' sum", got, err)
	}
}

func TestExportGuardDeletedBeforeFirstExport(t *testing.T) {
	root, _, db := setupShadowExport(t)
	if err := os.Remove(filepath.Join(root, "notes", "y.md")); err != nil {
		t.Fatal(err)
	}
	rep, err := memory.ExportGuard(db, root)
	if err != nil || !slices.Contains(rep.Conflicts, "notes/y.md") {
		t.Fatalf("guard after delete = %+v (%v), want deleted-outside for notes/y.md", rep, err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "y.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted file resurrected: %v", err)
	}
}

func TestExportGuardImportRefusesChangedDB(t *testing.T) {
	_, _, db := setupShadowExport(t)
	if err := db.Import("notes/y.md", "not-the-render"); !errors.Is(err, memory.ErrExportStale) {
		t.Fatalf("import over a changed render = %v, want ErrExportStale", err)
	}
	if err := db.Import("notes/y.md", ""); !errors.Is(err, memory.ErrExportStale) {
		t.Fatalf("import of a doc the db already has, want-absent = %v, want ErrExportStale", err)
	}
}

func TestExportGuardRebuildHoldsBankLock(t *testing.T) {
	setupShadowExport(t)
	old := memory.BankLockWait
	memory.BankLockWait = 50 * time.Millisecond
	t.Cleanup(func() { memory.BankLockWait = old })
	err := memory.WithBankLock(time.Second, func() error {
		_, rerr := RebuildShadow(context.Background())
		return rerr
	})
	if !errors.Is(err, memory.ErrBankBusy) {
		t.Fatalf("rebuild under a held bank lock = %v, want ErrBankBusy", err)
	}
}

func TestExportClearConflictRemovesOnlyNamedCopy(t *testing.T) {
	root, _, db := setupShadowExport(t)
	keep := filepath.Join(root, "notes", "y.md.outside-abcdef012345-1")
	other := filepath.Join(root, "notes", "y.md.outside-fedcba012345-2")
	for _, p := range []string{keep, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordConflict("notes/y.md", "changed-during-export"); err != nil {
		t.Fatal(err)
	}
	if got, err := ExportConflicts(); err != nil || !slices.Contains(got, "copy notes/y.md.outside-abcdef012345-1") {
		t.Fatalf("listing = %q (%v), want the kept copy", got, err)
	}
	if err := ClearExportConflict("notes/y.md.outside-abcdef012345-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cleared copy remains: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("other copy lost: %v", err)
	}
	if err := ClearExportConflict("../x"); err == nil {
		t.Fatal("clear accepted a path outside the store")
	}
}

func TestExportClearRefusesSymlinkAndLookalike(t *testing.T) {
	root, _, _ := setupShadowExport(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "y.md.outside-abcdef012345-1")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := ClearExportConflict("link/y.md.outside-abcdef012345-1"); err == nil {
		t.Fatal("clear followed a symlinked directory")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("file outside the store removed: %v", err)
	}
	if err := ClearExportConflict("notes/y.md"); err != nil {
		t.Fatalf("clearing a plain doc id must only resolve conflicts: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "y.md")); err != nil {
		t.Fatalf("plain document deleted: %v", err)
	}
}

func TestExportClearNeverDeletesLinkTarget(t *testing.T) {
	root, _, _ := setupShadowExport(t)
	link := filepath.Join(root, "notes", "y.md.outside-abcdef012345-9")
	if err := os.Symlink(filepath.Join(root, "notes", "y.md"), link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := ClearExportConflict("notes/y.md.outside-abcdef012345-9"); err == nil {
		t.Fatal("clear accepted a symlink as a kept copy")
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "y.md")); err != nil {
		t.Fatalf("link target deleted: %v", err)
	}
}
