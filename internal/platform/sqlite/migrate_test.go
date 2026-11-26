package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlite3 "modernc.org/sqlite"
)

const testYear = 2026

func twoSteps() []Step {
	return []Step{
		{"create", func(tx *sql.Tx) (string, error) {
			_, err := tx.Exec("CREATE TABLE a(x)")
			return "table a", err
		}},
		{"seed", func(tx *sql.Tx) (string, error) {
			_, err := tx.Exec("INSERT INTO a VALUES (1)")
			return "1 row in a", err
		}},
	}
}

func fixedNow(sec int) func() time.Time {
	return func() time.Time { return time.Date(testYear, 11, 25, 10, 0, sec, 0, time.UTC) }
}

func TestMigrateAppliesOnceThenNoOp(t *testing.T) {
	db, _ := openTemp(t, Options{})
	snaps := t.TempDir()
	opts := MigrateOptions{Name: "m", SnapshotDir: snaps, Now: fixedNow(0)}
	rep, err := Migrate(context.Background(), db, twoSteps(), opts)
	if err != nil || rep.From != 0 || rep.To != 2 || len(rep.Applied) != 2 || rep.Snapshot == "" {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	rep, err = Migrate(context.Background(), db, twoSteps(), opts)
	v, _ := Version(db)
	if err != nil || v != 2 || len(rep.Applied) != 0 || rep.Snapshot != "" {
		t.Fatalf("second run rep=%+v v=%d err=%v", rep, v, err)
	}
}

func TestFailedStepRollsBack(t *testing.T) {
	db, _ := openTemp(t, Options{})
	steps := append(twoSteps()[:1], Step{"bad", func(tx *sql.Tx) (string, error) {
		_, _ = tx.Exec("CREATE TABLE b(x)")
		return "", errors.New("boom")
	}})
	rep, err := Migrate(context.Background(), db, steps, MigrateOptions{})
	if err == nil || rep.To != 1 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if v, _ := Version(db); v != 1 {
		t.Fatalf("version %d", v)
	}
	if _, err = db.Exec("SELECT 1 FROM b"); err == nil {
		t.Fatal("failed step left its table behind")
	}
}

// TestKilledMigrationResumes stops after step 1 commits, then drops step 2's connection mid-transaction.
func TestKilledMigrationResumes(t *testing.T) {
	ctx := context.Background()
	db, path := openTemp(t, Options{})
	opts := MigrateOptions{Name: "m", SnapshotDir: t.TempDir(), Now: fixedNow(0)}
	rep, err := Migrate(ctx, db, twoSteps()[:1], opts)
	if err != nil || rep.To != 1 {
		t.Fatalf("step 1: rep=%+v err=%v", rep, err)
	}
	db.Close() // the process stops between steps
	dieMidTx(t, path, "BEGIN IMMEDIATE", "INSERT INTO a VALUES (1)", "PRAGMA user_version = 2")
	db, _ = Open(path, Options{})
	defer db.Close()
	snap, _ := Open(rep.Snapshot, Options{ReadOnly: true})
	defer snap.Close()
	sv, _ := Version(snap)
	if v, rows := versionRows(t, db); v != 1 || rows != 0 || sv != 0 {
		t.Fatalf("after kill: v=%d rows=%d snapshot v=%d", v, rows, sv)
	}
	if rep, err = Migrate(ctx, db, twoSteps(), opts); err != nil || rep.From != 1 {
		t.Fatalf("resume: rep=%+v err=%v", rep, err)
	}
	if v, rows := versionRows(t, db); v != 2 || rows != 1 {
		t.Fatalf("resumed: v=%d rows=%d", v, rows)
	}
}

// dieMidTx runs qs on a raw driver connection and closes it with no COMMIT, as a killed process leaves it.
func dieMidTx(t *testing.T, path string, qs ...string) {
	t.Helper()
	c, err := (&sqlite3.Driver{}).Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range qs {
		if _, err = c.(driver.ExecerContext).ExecContext(context.Background(), q, nil); err != nil {
			t.Fatal(q, err)
		}
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
}

func versionRows(t *testing.T, db *sql.DB) (v, rows int) {
	t.Helper()
	v, err := Version(db)
	if err == nil {
		err = db.QueryRow("SELECT count(*) FROM a").Scan(&rows)
	}
	if err != nil {
		t.Fatal(err)
	}
	return v, rows
}

func TestDryRunWritesNothing(t *testing.T) {
	db, _ := openTemp(t, Options{})
	snaps := t.TempDir()
	rep, err := Migrate(context.Background(), db, twoSteps(), MigrateOptions{Name: "m", SnapshotDir: snaps, DryRun: true})
	if err != nil || rep.To != 2 || len(rep.Applied) != 2 || rep.Snapshot != "" {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if v, _ := Version(db); v != 0 {
		t.Fatalf("version %d", v)
	}
	if _, err = db.Exec("SELECT 1 FROM a"); err == nil {
		t.Fatal("dry run left a table")
	}
	if ents, _ := os.ReadDir(snaps); len(ents) != 0 {
		t.Fatalf("dry run took a snapshot: %v", ents)
	}
}

func TestNewerStoreRefused(t *testing.T) {
	db, _ := openTemp(t, Options{})
	if _, err := db.Exec("PRAGMA user_version = 7"); err != nil {
		t.Fatal(err)
	}
	_, err := Migrate(context.Background(), db, twoSteps(), MigrateOptions{})
	var ne *NewerError
	if !errors.As(err, &ne) || ne.Have != 7 || ne.Known != 2 {
		t.Fatalf("err = %v", err)
	}
}

func TestSnapshotRetention(t *testing.T) {
	db, _ := openTemp(t, Options{})
	snaps := t.TempDir()
	old := []string{"m-v1-20260101T000001Z.db", "m-v1-20260101T000002Z.db", "m-v1-20260101T000003Z.db", "m-v0-20250101T000000Z.db"}
	for _, n := range old {
		if err := os.WriteFile(filepath.Join(snaps, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opts := MigrateOptions{Name: "m", SnapshotDir: snaps, Keep: 1, Now: fixedNow(9)}
	if _, err := Migrate(context.Background(), db, twoSteps(), opts); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"m-v0-*-20261125T100009Z.db": true, "m-v1-20260101T000003Z.db": true}
	ents, _ := os.ReadDir(snaps)
	if len(ents) != len(want) {
		t.Fatalf("kept %d files, want %v", len(ents), want)
	}
	for _, e := range ents {
		fresh := strings.HasPrefix(e.Name(), "m-v0-") && strings.HasSuffix(e.Name(), "-20261125T100009Z.db")
		if !want[e.Name()] && !fresh {
			t.Fatalf("unexpected survivor %s", e.Name())
		}
	}
}

// Two migrators on one file in the same second apply each step once and both finish at the newest version.
func TestConcurrentMigrateAppliesEachStepOnce(t *testing.T) {
	first, path := openTemp(t, Options{})
	second, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	snaps := t.TempDir()
	opts := MigrateOptions{Name: "m", SnapshotDir: snaps, Now: fixedNow(0)}
	errs := make(chan error, 2)
	for _, db := range []*sql.DB{first, second} {
		go func() { _, err := Migrate(context.Background(), db, twoSteps(), opts); errs <- err }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var rows, v int
	if err := first.QueryRow("SELECT count(*) FROM a").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("step 2 ran %d times (%v), want once", rows, err)
	}
	if v, _ = Version(first); v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}
}
