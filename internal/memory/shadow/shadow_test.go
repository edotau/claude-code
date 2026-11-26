package shadow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestShadowMismatch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeShadowStore(t, root, false)
	db := openShadowDB(t, nil)
	if _, err := shadowRebuild(ctx, db, root); err != nil {
		t.Fatal(err)
	}
	docs, err := shadowDocs(root)
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
	for _, q := range []string{"SELECT * FROM docs ORDER BY path", "SELECT * FROM entries ORDER BY rowid",
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
		"INSERT INTO conflicts VALUES(1, 'peer', 1, 'p', 't', 0)", "INSERT INTO changes VALUES('o', 1, 1, 1, 'v', 'op', 'p', 't')")
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
		(SELECT count(*) FROM changes)`).Scan(&c)
	if err != nil || c != 0 {
		t.Fatalf("rowid-keyed rows left after rebuild: %d (%v)", c, err)
	}
}

func TestShadowMigrateVersion(t *testing.T) {
	ctx := context.Background()
	db := openShadowDB(t, nil)
	if v, err := sqlite.Version(db); err != nil || v != 1 {
		t.Fatalf("version = %d (%v), want 1", v, err)
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
