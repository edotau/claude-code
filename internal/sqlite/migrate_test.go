package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	want := map[string]bool{"m-v0-20261125T100009Z.db": true, "m-v1-20260101T000003Z.db": true}
	ents, _ := os.ReadDir(snaps)
	if len(ents) != len(want) {
		t.Fatalf("kept %d files, want %v", len(ents), want)
	}
	for _, e := range ents {
		if !want[e.Name()] {
			t.Fatalf("unexpected survivor %s", e.Name())
		}
	}
}
