package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/nonet"
)

func openTemp(t *testing.T, opts Options) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "t.db")
	db, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestOpenPragmasAndModes(t *testing.T) {
	db, path := openTemp(t, Options{})
	var mode string
	var fk int
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, %v", fk, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode())
	}
}

func TestReadOnlyDoesNotBlockWriter(t *testing.T) {
	w, path := openTemp(t, Options{})
	if _, err := w.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rows, err := r.Query("SELECT x FROM t")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if _, err = w.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatalf("writer blocked by open reader: %v", err)
	}
	if _, err = r.Exec("INSERT INTO t VALUES (2)"); err == nil {
		t.Fatal("read-only handle accepted a write")
	}
}

func TestTxRetriesBusyButNotPlainErrors(t *testing.T) {
	a, path := openTemp(t, Options{Busy: 2 * time.Millisecond})
	if _, err := a.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	b, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = Tx(context.Background(), b, func(tx *sql.Tx) error {
			_, _ = tx.Exec("INSERT INTO t VALUES (0)")
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	time.AfterFunc(50*time.Millisecond, func() { close(release) })
	calls := 0
	err = Tx(context.Background(), a, func(tx *sql.Tx) error {
		calls++
		_, e := tx.Exec("INSERT INTO t VALUES (1)")
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 1 {
		t.Fatal("fn never ran")
	}
	plain := 0
	err = Tx(context.Background(), a, func(tx *sql.Tx) error {
		plain++
		_, e := tx.Exec("INSERT INTO nope VALUES (1)")
		return e
	})
	if err == nil || plain != 1 {
		t.Fatalf("plain error: err=%v calls=%d", err, plain)
	}
}

func writeLoop(path string, id int, errs chan<- error) {
	db, err := Open(path, Options{})
	if err != nil {
		errs <- err
		return
	}
	defer db.Close()
	for n := range 60 {
		if err = Tx(context.Background(), db, func(tx *sql.Tx) error {
			_, e := tx.Exec("INSERT INTO t VALUES (?)", fmt.Sprint(id, n))
			return e
		}); err != nil {
			errs <- err
		}
	}
}

func readLoop(path string, errs chan<- error) {
	db, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		errs <- err
		return
	}
	defer db.Close()
	for range 60 {
		var c int
		if err = db.QueryRow("SELECT count(*) FROM t").Scan(&c); err != nil {
			errs <- err
		}
	}
}

func TestContention(t *testing.T) {
	db, path := openTemp(t, Options{})
	if _, err := db.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 512)
	for i := range 6 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i < 2 {
				writeLoop(path, i, errs)
			} else {
				readLoop(path, errs)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
