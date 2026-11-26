// Package sqlite opens SQLite stores (pure Go, WAL), runs retried transactions and applies numbered migrations.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	sqlite3 "modernc.org/sqlite"
)

const (
	defaultBusy = 250 * time.Millisecond
	txAttempts  = 8
	txBackoff   = 10 * time.Millisecond
)

// Options tunes Open; the zero value is a read-write open with the default busy timeout.
type Options struct {
	ReadOnly bool
	Busy     time.Duration
}

// Open opens path in WAL mode with foreign keys on and writers beginning IMMEDIATE (file 0600, parent 0700).
func Open(path string, opts Options) (*sql.DB, error) {
	if !opts.ReadOnly {
		if err := touch(path); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", dsn(path, opts))
	if err != nil {
		return nil, err
	}
	if !opts.ReadOnly {
		db.SetMaxOpenConns(1)
	}
	if err = db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	return db, nil
}

func touch(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func dsn(path string, opts Options) string {
	busy := opts.Busy
	if busy <= 0 {
		busy = defaultBusy
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busy.Milliseconds(), 10)+")")
	q.Add("_pragma", "foreign_keys(1)")
	if opts.ReadOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(FULL)")
		q.Set("_txlock", "immediate")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// Tx runs fn in one retried transaction; fn has no effect outside it and never makes a model or network call.
func Tx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	var err error
	for i := 1; i <= txAttempts; i++ {
		if err = txOnce(ctx, db, fn); err == nil || !isBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i) * txBackoff):
		}
	}
	return fmt.Errorf("sqlite: still busy after %d attempts: %w", txAttempts, err)
}

func txOnce(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func isBusy(err error) bool {
	var e *sqlite3.Error
	if !errors.As(err, &e) {
		return false
	}
	c := e.Code() & 0xff
	return c == 5 || c == 6 // SQLITE_BUSY, SQLITE_LOCKED
}

// Version reports the store's PRAGMA user_version.
func Version(db *sql.DB) (int, error) {
	var v int
	err := db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&v)
	return v, err
}
