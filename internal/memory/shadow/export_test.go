package shadow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/sqlite"
)

func setupShadowExport(t *testing.T) (string, map[string]string, memory.ExportDB) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", tmp)
	t.Setenv("HARNESS_KEYRING", "off")
	pinFilesTruth(t)
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

func truthAdd(t *testing.T, e *parityEnv, text string) string {
	t.Helper()
	st := memory.NewBankStore(false)
	_, base, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	op := memory.Op{Op: "add", Target: "activeContext", Section: "Current Focus", Text: text}
	rs, err := st.Apply(memory.Batch{Ops: []memory.Op{op}}, base, e.now())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return rs[0].ID
}

func truthUpdate(t *testing.T, e *parityEnv, id, text string, base memory.Versions) error {
	t.Helper()
	op := memory.Op{Op: "update", ID: id, Text: text}
	_, err := memory.NewBankStore(false).Apply(memory.Batch{Ops: []memory.Op{op}}, base, e.now())
	return err
}

func truthVersions(t *testing.T) memory.Versions {
	t.Helper()
	_, v, err := memory.NewBankStore(false).Load()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func shadowCount(t *testing.T, q string) int {
	t.Helper()
	db, err := openMigratedShadow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seedHistory(t *testing.T, text string) {
	t.Helper()
	db, err := openMigratedShadow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := db.Exec(`INSERT INTO vectors(entry_rowid, model, dim, vec, minted_at)
		SELECT rowid, 'm', 1, x'00', '' FROM entries WHERE lines LIKE '%' || ? || '%'`, text)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("setup: seeded %d vector rows for %q, want 1", n, text)
	}
	if _, err := db.Exec(`INSERT INTO changes(origin, origin_seq, schema, entry_rowid, base_version, op, payload, at)
		SELECT 'test', 1, 1, rowid, '1', 'update', '', '' FROM entries WHERE lines LIKE '%' || ? || '%'`, text); err != nil {
		t.Fatal(err)
	}
}

func TestExportDoesNotTriggerReimport(t *testing.T) {
	e := newParityEnv(t, "db")
	id := truthAdd(t, e, "first")
	v1 := truthVersions(t)
	if err := truthUpdate(t, e, id, "second", v1); err != nil {
		t.Fatal(err)
	}
	v2 := truthVersions(t) // this open and the one below must not re-import the export's own files
	if v2[id] == v1[id] {
		t.Fatalf("version did not move: %v", v2[id])
	}
	seedHistory(t, "second")
	changes := shadowCount(t, `SELECT count(*) FROM changes`)
	if got := truthVersions(t); got[id] != v2[id] {
		t.Fatalf("reopen reset the version: %v -> %v", v2[id], got[id])
	}
	if n := shadowCount(t, `SELECT count(*) FROM vectors`); n != 1 {
		t.Fatalf("vector rows after reopen = %d, want 1", n)
	}
	if n := shadowCount(t, `SELECT count(*) FROM changes`); n != changes || n == 0 {
		t.Fatalf("history rows after reopen = %d, want %d", n, changes)
	}
	var ce *memory.ConflictError
	if err := truthUpdate(t, e, id, "stale", v1); !errors.As(err, &ce) {
		t.Fatalf("update on the stale version = %v, want a ConflictError", err)
	}
}

func TestOutsideEditAfterExportIsStillSeen(t *testing.T) {
	e := newParityEnv(t, "db")
	truthAdd(t, e, "exported")
	body := activeMarkdown(t)
	if err := os.WriteFile(memory.Path("activeContext.md"), []byte(body+"- sneaked in outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	edb, closeDB, err := memory.OpenExportDB() // an open no longer imports under db truth: the guard does
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.ExportGuard(edb, paths.MemoryStoreDir()); err != nil {
		t.Fatal(err)
	}
	if err := closeDB(); err != nil {
		t.Fatal(err)
	}
	docs, _, err := memory.NewBankStore(false).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(docs["activeContext.md"].Render()), "sneaked in outside") {
		t.Fatal("an outside edit after the export was not imported by the guard")
	}
}

func activeMarkdown(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(memory.Path("activeContext.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDBWriteReachesMarkdownAtOnce(t *testing.T) {
	e := newParityEnv(t, "db")
	truthAdd(t, e, "written under db truth")
	if !strings.Contains(activeMarkdown(t), "written under db truth") {
		t.Fatal("a db write did not reach the Markdown")
	}
}

func TestRebuildUnderDBTruthKeepsDBOnlyChange(t *testing.T) {
	e := newParityEnv(t, "db")
	old := memory.OpenExportDB
	t.Cleanup(func() { memory.OpenExportDB = old })
	memory.OpenExportDB = nil // leave the write db-only, as a crash before export would
	truthAdd(t, e, "db only change")
	memory.OpenExportDB = old
	if strings.Contains(activeMarkdown(t), "db only change") {
		t.Fatal("setup: the change reached the Markdown")
	}
	if _, err := RebuildShadow(context.Background()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if !strings.Contains(activeMarkdown(t), "db only change") {
		t.Fatal("rebuild under db truth lost a db-only change")
	}
}

func TestOutsideEditWinsAfterDBWrite(t *testing.T) {
	e := newParityEnv(t, "db")
	truthAdd(t, e, "db write")
	body := activeMarkdown(t)
	if !strings.Contains(body, "db write") {
		t.Fatal("the db write was not on disk before the outside edit")
	}
	outside := strings.Replace(body, "db write", "db write\n- outside edit line", 1)
	if err := os.WriteFile(memory.Path("activeContext.md"), []byte(outside), 0o600); err != nil {
		t.Fatal(err)
	}
	guardOnce(t)
	wantStamped(t, "activeContext.md")
	if got := activeMarkdown(t); !strings.Contains(got, "outside edit line") || !strings.Contains(got, "db write") {
		t.Fatalf("outside edit or db write lost on disk:\n%s", got)
	}
	docs, _, err := memory.NewBankStore(false).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var all strings.Builder
	for _, d := range docs {
		for _, en := range d.Entries {
			all.WriteString(strings.Join(en.Lines, "\n"))
		}
	}
	if !strings.Contains(all.String(), "outside edit line") {
		t.Fatal("the db did not take the outside edit")
	}
}

// guardOnce runs the export guard as settle does on the next open, wanting an import and no conflict.
func guardOnce(t *testing.T) {
	t.Helper()
	edb, closeDB, err := memory.OpenExportDB()
	if err != nil {
		t.Fatal(err)
	}
	rep, err := memory.ExportGuard(edb, paths.MemoryStoreDir())
	if cerr := closeDB(); err == nil {
		err = cerr
	}
	if err != nil || len(rep.Imported) == 0 || len(rep.Conflicts) != 0 {
		t.Fatalf("guard: %+v err=%v; want the outside edit imported", rep, err)
	}
}

func wantStamped(t *testing.T, rel string) {
	t.Helper()
	fi, err := os.Stat(memory.Path(rel))
	if err != nil {
		t.Fatal(err)
	}
	q := fmt.Sprintf(`SELECT count(*) FROM meta WHERE key = 'stat:%s' AND value = '%d:%d'`, rel, fi.Size(), fi.ModTime().UnixNano())
	if shadowCount(t, q) != 1 {
		t.Fatalf("%s: stale fingerprint, so the next open would re-import it", rel)
	}
}

func TestRebuildUnderDBTruthRefusesOnExportConflict(t *testing.T) {
	e := newParityEnv(t, "db")
	truthAdd(t, e, "base")
	old := memory.OpenExportDB
	t.Cleanup(func() { memory.OpenExportDB = old })
	memory.OpenExportDB = nil
	truthAdd(t, e, "db only change")
	memory.OpenExportDB = old
	if err := os.WriteFile(memory.Path("activeContext.md"), []byte(activeMarkdown(t)+"- outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildShadow(context.Background()); err == nil {
		t.Fatal("rebuild under db truth went ahead over an export conflict")
	}
	if n := shadowCount(t, `SELECT count(*) FROM entries WHERE lines LIKE '%db only change%'`); n != 1 {
		t.Fatalf("db rows with the db-only change = %d after the refused rebuild, want 1", n)
	}
}

func TestMatchRowsKeepsEditedEntryWithoutID(t *testing.T) {
	old := []liveEntry{{rowid: 7, seq: 0, lines: "- a"}, {rowid: 8, seq: 1, lines: "- b"}}
	got := matchRows(old, []shadowEntry{{seq: 0, lines: "- a edited"}, {seq: 1, lines: "- b"}})
	if got[0] != 7 || got[1] != 8 {
		t.Fatalf("matchRows = %v, want [7 8]", got)
	}
}
