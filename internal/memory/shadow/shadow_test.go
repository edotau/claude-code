package shadow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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

// shadowFixture is the test store: imported files, then the skipped ones.
var shadowFixture = []struct{ rel, body string }{
	{"decisionLog.md", "# Decision Log\n\n> Dated decisions.\n\n## 2026-01-15 — Pick zebrafish storage <!-- id:0123abcd -->\n\nWhy: speed.\n\n## 2026-01-10 — Older one\n\nbody\n"},
	{"conventions.md", "# Conventions\n\n- use gofmt\n- wrap errors\n  continued\n"},
	{"progress.md", "# Progress\n\n> contract: keep it short\n\n## Done\n\n```text\n## not a heading\n- not a bullet\n```\n\n- shipped\n"},
	{"archive/decisionLog-2026-10.md", "# Decision Log archive\n\n## 2026-10-01 — Archived\n\nold\n"},
	{"archive/odd.md", "just text\n## heading\n"},
	{"feedback/x.md", "---\nname: x\ndescription: d\n---\n# X\n\nbody text\n"},
	{"notes/empty.md", "---\nname: empty\n---"},
	{"reports/r.md", "# Report\n\nline\n"},
	{"activeContext.md", ""},
	{"notes/blank.md", "\n"},
	{"handoffs/h.md", "# Only header"},
	{"worktrees/w.md", "a\r\nno trailing newline"},
	{"MEMORY.md", "generated\n"},
	{"notes/MEMORY.md", "generated\n"},
	{"reports/index.md", "generated\n"},
	{"index.md", "generated\n"},
	{".layout", "x\n"},
	{"a.lock", "x\n"},
	{"notes/b.err", "x\n"},
}

const shadowImported = 12

func writeShadowStore(t *testing.T, dir string, reverse bool) {
	t.Helper()
	files := slices.Clone(shadowFixture)
	if reverse {
		slices.Reverse(files)
	}
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func openShadowDB(t *testing.T, now func() time.Time) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "memory.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := migrateMemoryDB(context.Background(), db, sqlite.MigrateOptions{SnapshotDir: t.TempDir(), Now: now}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestShadowRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeShadowStore(t, root, false)
	db := openShadowDB(t, nil)
	n, err := shadowRebuild(ctx, db, root)
	if err != nil {
		t.Fatal(err)
	}
	if n.Docs != shadowImported || len(n.Mismatched) != 0 {
		t.Fatalf("docs = %d, want %d; mismatched %v", n.Docs, shadowImported, n.Mismatched)
	}
	var c int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM docs").Scan(&c); err != nil || c != shadowImported {
		t.Errorf("docs rows = %d (err %v), want %d: a skipped file was imported", c, err, shadowImported)
	}
	for _, f := range shadowFixture[:shadowImported] {
		got, err := renderShadowDoc(ctx, db, f.rel)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != f.body {
			t.Errorf("%s: render\n%q\nwant\n%q", f.rel, got, f.body)
		}
	}
	wantShadowMatch(t, db, "zebrafish", "decisionLog.md", "0123abcd")
}

func TestShadowImportEditedFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := paths.MemoryStoreDir()
	if docs, changed, err := importShadow(); err != nil || changed || len(docs) != 0 {
		t.Fatalf("missing db import = %v, %v, %v", docs, changed, err)
	}
	if _, err := os.Stat(ShadowDBPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing db was created: %v", err)
	}
	writeShadowStore(t, root, false)
	if _, err := RebuildShadow(context.Background()); err != nil {
		t.Fatal(err)
	}
	newBody := "# Conventions\n\n- use gofmt\n- wrap errors\n- keep edits\n"
	if err := os.WriteFile(filepath.Join(root, "conventions.md"), []byte(newBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if docs, _, err := importShadow(); err != nil || len(docs) != 0 {
		t.Fatalf("edited file import = %v, %v", docs, err)
	}
	wantShadowRender(t, "conventions.md", newBody)
}

func wantShadowRender(t *testing.T, doc, want string) {
	t.Helper()
	db, err := sqlite.Open(ShadowDBPath(), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := renderShadowDoc(context.Background(), db, doc)
	if err != nil || string(got) != want {
		t.Fatalf("imported render = %q, %v; want %q", got, err, want)
	}
}

func TestShadowImportIncremental(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := paths.MemoryStoreDir()
	writeShadowStore(t, root, false)
	ctx := context.Background()
	if _, err := RebuildShadow(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(ShadowDBPath(), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// a trigger makes the next import of conventions.md render unequal: divergence through the real seam
	execShadow(t, db, `CREATE TRIGGER skew AFTER INSERT ON entries WHEN new.doc = 'conventions.md'
		BEGIN UPDATE entries SET lines = lines || 'x' WHERE rowid = new.rowid; END`)
	before := dumpShadow(t, db)
	if docs, changed, err := importShadow(); err != nil || changed || len(docs) != 0 {
		t.Fatalf("unchanged import = %v, %v", docs, err)
	}
	if dumpShadow(t, db) != before {
		t.Fatal("an unchanged store rewrote rows")
	}
	if err := os.WriteFile(filepath.Join(root, "conventions.md"), []byte("# Conventions\n\n- one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 2 { // a divergent doc is never recorded, so it is reported again, but changed only the first time
		docs, changed, err := importShadow()
		if err != nil || !slices.Equal(docs, []string{"conventions.md"}) || changed != (i == 0) {
			t.Fatalf("divergent import %d = %v, %v, %v", i, docs, changed, err)
		}
	}
	execShadow(t, db, "DROP TRIGGER skew")
	wantVanishedDropped(t, db, root)
	wantFullOnMovedSchema(t, db)
	if err := os.Remove(filepath.Join(root, "reports", "r.md")); err != nil {
		t.Fatal(err)
	}
	if docs, _, err := importShadow(); err != nil || len(docs) != 0 {
		t.Fatalf("settled import = %v, %v", docs, err)
	}
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM docs WHERE path = 'reports/r.md'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("removed file still has a docs row: %d (%v)", n, err)
	}
}

// wantVanishedDropped: entry-keyed rows go with their doc; a never-fingerprinted doc that vanishes is dropped.
func wantVanishedDropped(t *testing.T, db *sql.DB, root string) {
	t.Helper()
	ctx := context.Background()
	execShadow(t, db, "INSERT INTO conflicts(entry_rowid, peer_origin, peer_seq, payload, at, resolved) SELECT rowid, 'peer', 1, 'p', 't', 0 FROM entries WHERE doc = 'conventions.md'",
		`INSERT INTO changes(origin, origin_seq, schema, entry_rowid, entry_uid, base_version, op, payload, at)
		SELECT 'o', 1, 1, rowid, uid, 'v', 'op', 'p', 't' FROM entries WHERE doc = 'conventions.md'`)
	var kept int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM changes`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "conventions.md")); err != nil {
		t.Fatal(err)
	}
	if docs, changed, err := importShadow(); err != nil || !changed || len(docs) != 0 { // the set cleared
		t.Fatalf("vanished divergent doc import = %v, %v, %v", docs, changed, err)
	}
	var entries, live int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT value FROM meta WHERE key = 'entries'),
		(SELECT count(*) FROM entries WHERE deleted = 0)`).Scan(&entries, &live); err != nil || entries != live {
		t.Fatalf("meta entries = %d, live rows %d (%v)", entries, live, err)
	}
	var left int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM docs WHERE path = 'conventions.md') +
		(SELECT count(*) FROM entries WHERE doc = 'conventions.md') + (SELECT count(*) FROM conflicts) +
		abs((SELECT count(*) FROM changes) - ?)`, kept).Scan(&left); err != nil || left != 0 { // every changes row is kept
		t.Fatalf("rows left for a vanished doc (or changes pruned): %d (%v)", left, err)
	}
}

// wantFullOnMovedSchema: a moved schema marker forces a full import, so an unchanged-looking doc is re-imported.
func wantFullOnMovedSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	execShadow(t, db, "UPDATE meta SET value = '0' WHERE key = 'imported_schema'", "UPDATE entries SET lines = 'tampered' WHERE doc = 'progress.md'")
	if docs, _, err := importShadow(); err != nil || len(docs) != 0 {
		t.Fatalf("full import = %v, %v", docs, err)
	}
	var tampered int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM entries WHERE lines = 'tampered'`).Scan(&tampered); err != nil || tampered != 0 {
		t.Fatalf("moved schema did not force a full import: %d (%v)", tampered, err)
	}
}

// TestShadowRenderVersion fails when parsing or rendering changes: bump renderVersion, then update want here.
func TestShadowRenderVersion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeShadowStore(t, root, false)
	db := openShadowDB(t, nil)
	if _, err := shadowRebuild(ctx, db, root); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	for _, f := range shadowFixture[:shadowImported] {
		got, err := renderShadowDoc(ctx, db, f.rel)
		if err != nil {
			t.Fatal(err)
		}
		h.Write(got)
	}
	const want = "a2a36b630358ba553893fc53a16bb70cf4cb756f8df341e38655baee3337ab42"
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		t.Fatalf("render hash %s (renderVersion %s): bump renderVersion and update want", got, renderVersion)
	}
}

func TestShadowMismatch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeShadowStore(t, root, false)
	db := openShadowDB(t, nil)
	if _, err := shadowRebuild(ctx, db, root); err != nil {
		t.Fatal(err)
	}
	docs, err := shadowDocs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	execShadow(t, db, "UPDATE entries SET lines = lines || 'x' WHERE doc = 'conventions.md' AND seq = 0")
	if got, err := shadowMismatches(ctx, db, docs); err != nil || !slices.Equal(got, []string{"conventions.md"}) {
		t.Fatalf("mismatches after a row edit = %v (%v), want [conventions.md]", got, err)
	}
}

func wantShadowMatch(t *testing.T, db *sql.DB, word, doc, id string) {
	t.Helper()
	var gotDoc, gotID string
	err := db.QueryRowContext(context.Background(), `SELECT e.doc, e.id FROM entries_fts f JOIN entries e
		ON e.rowid = f.rowid WHERE entries_fts MATCH ?`, word).Scan(&gotDoc, &gotID)
	if err != nil || gotDoc != doc || gotID != id {
		t.Fatalf("fts match %q: doc %q id %q err %v", word, gotDoc, gotID, err)
	}
}

func dumpShadow(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{"SELECT * FROM docs ORDER BY path", `SELECT rowid, doc, id, seq, kind, lines, date, area, version,
		deleted, origin, origin_seq, rendered_rev FROM entries ORDER BY rowid`, // uid is random per db
		"SELECT * FROM vectors", "SELECT * FROM conflicts", "SELECT * FROM changes"} {
		rows, err := db.QueryContext(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%q\n", vals)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

func TestShadowDeterministic(t *testing.T) {
	var dumps []string
	for i, reverse := range []bool{false, true} {
		root := t.TempDir()
		writeShadowStore(t, root, reverse)
		clock := time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC)
		db := openShadowDB(t, func() time.Time { return clock })
		for range 2 {
			if _, err := shadowRebuild(context.Background(), db, root); err != nil {
				t.Fatal(err)
			}
		}
		dumps = append(dumps, dumpShadow(t, db))
	}
	if dumps[0] != dumps[1] || dumps[0] == "" {
		t.Fatalf("dumps differ:\n%s\n---\n%s", dumps[0], dumps[1])
	}
}

func execShadow(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestShadowRebuildAtomic(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeShadowStore(t, root, false)
	db := openShadowDB(t, nil)
	if _, err := shadowRebuild(ctx, db, root); err != nil {
		t.Fatal(err)
	}
	execShadow(t, db, "INSERT INTO vectors VALUES(1, 'm', 1, x'00', 't')",
		"INSERT INTO conflicts(entry_rowid, peer_origin, peer_seq, payload, at, resolved) VALUES(1, 'peer', 1, 'p', 't', 0)", "INSERT INTO changes VALUES('o', 1, 1, 1, 'v', 'op', 'p', 't', 'u', NULL)")
	before := dumpShadow(t, db)
	if _, err := shadowRebuild(ctx, db, filepath.Join(root, "missing")); err == nil {
		t.Fatal("rebuild from a missing root succeeded")
	}
	execShadow(t, db, `CREATE TRIGGER boom BEFORE INSERT ON docs WHEN new.path = 'reports/r.md'
		BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	if _, err := shadowRebuild(ctx, db, root); err == nil {
		t.Fatal("rebuild with a failing insert succeeded")
	}
	if after := dumpShadow(t, db); after != before {
		t.Fatalf("failed rebuild changed rows:\n%s\n---\n%s", before, after)
	}
	wantShadowMatch(t, db, "zebrafish", "decisionLog.md", "0123abcd")
	execShadow(t, db, "DROP TRIGGER boom")
	if _, err := shadowRebuild(ctx, db, root); err != nil {
		t.Fatal(err)
	}
	var c int
	err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM vectors) + (SELECT count(*) FROM conflicts) +
		abs((SELECT count(*) FROM changes) - 1)`).Scan(&c) // the uid-keyed changes row survives
	if err != nil || c != 0 {
		t.Fatalf("rowid-keyed rows left after rebuild, or changes pruned: %d (%v)", c, err)
	}
}

func TestShadowMigrateVersion(t *testing.T) {
	ctx := context.Background()
	db := openShadowDB(t, nil)
	if v, err := sqlite.Version(db); err != nil || v != SchemaVersion() {
		t.Fatalf("version = %d (%v), want %d", v, err, SchemaVersion())
	}
	rep, err := migrateMemoryDB(ctx, db, sqlite.MigrateOptions{})
	if err != nil || len(rep.Applied) != 0 {
		t.Fatalf("second migrate applied %v (%v)", rep.Applied, err)
	}
	fresh, err := sqlite.Open(filepath.Join(t.TempDir(), "fresh.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if _, err := migrateMemoryDB(ctx, fresh, sqlite.MigrateOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if v, err := sqlite.Version(fresh); err != nil || v != 0 {
		t.Fatalf("dry-run version = %d (%v), want 0", v, err)
	}
}

func TestShadowAreaStep(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "memory.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := sqlite.Migrate(ctx, db, memorySteps[:1], sqlite.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	execShadow(t, db, `INSERT INTO docs VALUES('d.md','note','h',NULL,'bullets',NULL)`)
	for i, l := range []string{"see internal/hooks/a.go and cmd/zed, internal/hooks", "cmd/b then internal/a", "no paths", "x/internal/memory"} {
		execShadow(t, db, fmt.Sprintf(`INSERT INTO entries(doc,id,seq,kind,lines,date,version) VALUES('d.md','i%d',%d,'bullet','%s','',1)`, i, i, l))
	}
	if _, err := migrateMemoryDB(ctx, db, sqlite.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	var got []string
	rows, err := db.QueryContext(ctx, `SELECT area FROM entries ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		got = append(got, a)
	}
	if want := []string{"hooks", "a", "", "memory"}; !slices.Equal(got, want) {
		t.Fatalf("areas = %q, want %q", got, want)
	}
	wantShadowMatch(t, db, "hooks", "d.md", "i0")
}

func TestShadowRebuildKeepsArea(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "conventions.md"), []byte("# C\n\n- see internal/zzpkg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := openShadowDB(t, nil)
	if _, err := shadowRebuild(context.Background(), db, root); err != nil {
		t.Fatal(err)
	}
	var id string
	err := db.QueryRowContext(context.Background(), `SELECT e.id FROM entries_fts f JOIN entries e ON e.rowid = f.rowid
		WHERE entries_fts MATCH 'area:zzpkg' AND e.area = 'zzpkg'`).Scan(&id)
	if err != nil {
		t.Fatalf("area column match: %v", err)
	}
}

func TestShadowConflictKindStep(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "memory.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := sqlite.Migrate(ctx, db, memorySteps[:2], sqlite.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	execShadow(t, db, `INSERT INTO conflicts(entry_rowid, peer_origin, peer_seq, payload, at) VALUES(7, 'p', 1, 'x', 't')`,
		`INSERT INTO conflicts(entry_rowid, peer_origin, peer_seq, payload, at) VALUES(0, 'both-changed', 0, 'a.md', 't')`)
	if _, err := migrateMemoryDB(ctx, db, sqlite.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	var kind string
	var doc sql.NullString
	var legacy string
	if err := db.QueryRowContext(ctx, `SELECT kind || '|' || doc FROM conflicts WHERE entry_rowid = 0`).Scan(&legacy); err != nil || legacy != "both-changed|a.md" {
		t.Fatalf("legacy export row = %q (%v), want both-changed|a.md", legacy, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT kind, doc FROM conflicts WHERE entry_rowid = 7`).Scan(&kind, &doc); err != nil || kind != "envelope" || doc.Valid {
		t.Fatalf("migrated conflict = %q %v (%v), want envelope, NULL", kind, doc, err)
	}
}

// flat3Fixture is an un-upgraded store: repeated headings, an undated entry, an id-less file.
var flat3Fixture = []struct{ rel, body string }{
	{"decisionLog.md", "# Decision Log\n\n## 2026-01-15 — Same title <!-- id:aaaa0001 -->\n\nfirst body\n\n## 2026-01-15 — Same title\n\nsecond body\n\n## Undated entry\n\nno date here\n"},
	{"conventions.md", "# Conventions\n\n## Rules\n\n- one\n\n## Rules\n\n- two\n"},
	{"notes/idless.md", "# No id\n\nplain note without markers\n"},
}

func TestShadowImportFlat3StoreLossless(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, f := range flat3Fixture {
		p := filepath.Join(root, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db := openShadowDB(t, nil)
	rep, err := shadowRebuild(ctx, db, root)
	if err != nil || rep.Docs != len(flat3Fixture) || len(rep.Mismatched) != 0 {
		t.Fatalf("rebuild = %+v, %v; want %d docs, no mismatch", rep, err, len(flat3Fixture))
	}
	for _, f := range flat3Fixture {
		got, err := renderShadowDoc(ctx, db, f.rel)
		if err != nil || string(got) != f.body {
			t.Errorf("%s: export\n%q (%v)\nwant\n%q", f.rel, got, err, f.body)
		}
	}
	want := map[string][]wantEntry{
		"decisionLog.md": {
			{id: "aaaa0001", date: "2026-01-15", title: "Same title", body: "first body"},
			{date: "2026-01-15", title: "Same title", body: "second body"},
			{title: "Undated entry", body: "no date here"},
		},
	}
	seen := map[string]string{}
	for rel, w := range want {
		checkStoredEntries(t, db, rel, w, seen)
	}
}

// wantEntry is one fixture entry; an empty id means the file carries none.
type wantEntry struct{ id, date, title, body string }

// storedEntries loads rel's entries (id, date) and their raw lines from the db in order.
func storedEntries(t *testing.T, db *sql.DB, rel string) (got []wantEntry, lines []string) {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT id, date, lines FROM entries WHERE doc = ? ORDER BY seq`, rel)
	if err != nil {
		t.Fatalf("%s: load entries: %v", rel, err)
	}
	defer rows.Close()
	for rows.Next() {
		var e wantEntry
		var l string
		if err := rows.Scan(&e.id, &e.date, &l); err != nil {
			t.Fatal(err)
		}
		got, lines = append(got, e), append(lines, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got, lines
}

// checkStoredEntries compares rel's stored entries to want in order; a minted id must be unique across seen.
func checkStoredEntries(t *testing.T, db *sql.DB, rel string, want []wantEntry, seen map[string]string) {
	t.Helper()
	got, lines := storedEntries(t, db, rel)
	if len(got) != len(want) {
		t.Fatalf("%s: %d stored entries, want %d", rel, len(got), len(want))
	}
	for i, w := range want {
		g, l := got[i], lines[i]
		first, _, _ := strings.Cut(l, "\n")
		if w.id != "" && g.id != w.id {
			t.Errorf("%s[%d]: id %q, want %q", rel, i, g.id, w.id)
		}
		if w.id == "" && g.id != "" {
			key := fmt.Sprintf("%s[%d]", rel, i)
			if prev, dup := seen[g.id]; dup {
				t.Errorf("%s: minted id %q already used by %s", key, g.id, prev)
			}
			seen[g.id] = key
		}
		if g.date != w.date {
			t.Errorf("%s[%d]: date %q, want %q", rel, i, g.date, w.date)
		}
		if !strings.Contains(first, w.title) {
			t.Errorf("%s[%d]: first line %q lacks title %q", rel, i, first, w.title)
		}
		if !strings.Contains(l, w.body) {
			t.Errorf("%s[%d]: lines %q lack body %q", rel, i, l, w.body)
		}
		for j, o := range want {
			if j != i && o.body != w.body && strings.Contains(l, o.body) && !strings.Contains(w.body, o.body) {
				t.Errorf("%s[%d]: lines %q hold entry %d's body %q", rel, i, l, j, o.body)
			}
		}
	}
}

func TestOpenBankStoreRefusesNewerSchema(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(ShadowDBPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(ShadowDBPath(), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrateMemoryDB(context.Background(), db, sqlite.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	execShadow(t, db, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion()+1))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, _, err := memory.OpenBankStore(false)
	var ne *sqlite.NewerError
	if s != nil || !errors.As(err, &ne) {
		t.Fatalf("store=%v err=%v; want nil store and a NewerError", s, err)
	}
}

func TestShadowImportDuplicateIDsStayDistinct(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	files := []struct{ rel, body string }{
		{"decisionLog.md", "# Decision Log\n\n## 2026-01-15 — One <!-- id:dddd0001 -->\n\nalpha body\n\n## 2026-01-16 — Two <!-- id:dddd0001 -->\n\nbeta body\n"},
		{"conventions.md", "# Conventions\n\n- gamma rule <!-- id:dddd0001 -->\n"},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(root, f.rel), []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db := openShadowDB(t, nil)
	if rep, err := shadowRebuild(ctx, db, root); err != nil || len(rep.Mismatched) != 0 {
		t.Fatalf("rebuild = %+v, %v", rep, err)
	}
	var rows, bodies int
	q := `SELECT count(*), count(DISTINCT lines) FROM entries WHERE id = 'dddd0001'`
	if err := db.QueryRowContext(ctx, q).Scan(&rows, &bodies); err != nil || rows != 3 || bodies != 3 {
		t.Fatalf("rows=%d distinct bodies=%d (%v), want 3 and 3", rows, bodies, err)
	}
	seen := map[string]string{}
	checkStoredEntries(t, db, "decisionLog.md", []wantEntry{
		{id: "dddd0001", date: "2026-01-15", title: "One", body: "alpha body"},
		{id: "dddd0001", date: "2026-01-16", title: "Two", body: "beta body"},
	}, seen)
	checkStoredEntries(t, db, "conventions.md", []wantEntry{{id: "dddd0001", title: "gamma rule", body: "gamma rule"}}, seen)
	for _, f := range files {
		if got, err := renderShadowDoc(ctx, db, f.rel); err != nil || string(got) != f.body {
			t.Errorf("%s: export %q (%v), want %q", f.rel, got, err, f.body)
		}
	}
}
