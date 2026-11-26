package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultKeep   = 5
	snapshotStamp = "20060102T150405Z"
)

var snapshotRE = regexp.MustCompile(`-v(\d+)-(?:\d+-\d+-)?(\d{8}T\d{6}Z)\.db$`)

// Step is migration number index+1; Up returns a one-line summary of what it changed.
type Step struct {
	Name string
	Up   func(*sql.Tx) (string, error)
}

// MigrateOptions configures Migrate; SnapshotDir empty means no snapshot.
type MigrateOptions struct {
	Name        string           // snapshot file prefix
	SnapshotDir string           // where VACUUM INTO writes
	Keep        int              // newest snapshots kept, default 5
	DryRun      bool             // run every pending step, then roll back
	Now         func() time.Time // snapshot stamp clock, default time.Now
}

// Report describes a Migrate run.
type Report struct {
	From, To int
	Applied  []string // "<n> <name>: <summary>"
	Snapshot string
	DryRun   bool
}

// NewerError means the store was written by newer code than this build.
type NewerError struct{ Have, Known int }

func (e *NewerError) Error() string {
	return fmt.Sprintf("sqlite: store is at version %d but this build knows %d", e.Have, e.Known)
}

// Migrate applies steps after the store's user_version, each in one transaction that also bumps the version.
func Migrate(ctx context.Context, db *sql.DB, steps []Step, opts MigrateOptions) (Report, error) {
	from, err := Version(db)
	if err != nil {
		return Report{}, err
	}
	rep := Report{From: from, To: from, DryRun: opts.DryRun}
	if from > len(steps) {
		return rep, &NewerError{Have: from, Known: len(steps)}
	}
	if from == len(steps) {
		return rep, nil
	}
	if opts.DryRun {
		return dryRun(ctx, db, steps, rep)
	}
	if opts.SnapshotDir != "" {
		if rep.Snapshot, err = snapshot(ctx, db, from, opts); err != nil {
			return rep, err
		}
	}
	for n := from + 1; n <= len(steps); n++ {
		line, err := apply(ctx, db, n, steps[n-1])
		if err != nil {
			return rep, err
		}
		rep.To = n
		if line != "" { // empty: a concurrent migrator applied it first
			rep.Applied = append(rep.Applied, line)
		}
	}
	return rep, nil
}

// apply runs step n and its version bump in one transaction; a step another process applied first is skipped.
func apply(ctx context.Context, db *sql.DB, n int, s Step) (string, error) {
	var line string
	err := Tx(ctx, db, func(tx *sql.Tx) (e error) {
		line = ""
		var have int
		if e = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&have); e != nil || have >= n {
			return e
		}
		if line, e = runStep(tx, n, s); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(n))
		return e
	})
	return line, err
}

func runStep(tx *sql.Tx, n int, s Step) (string, error) {
	sum, err := s.Up(tx)
	if err != nil {
		return "", fmt.Errorf("sqlite: migration %d %s: %w", n, s.Name, err)
	}
	return fmt.Sprintf("%d %s: %s", n, s.Name, sum), nil
}

func dryRun(ctx context.Context, db *sql.DB, steps []Step, rep Report) (Report, error) {
	errDone := errors.New("dry run")
	err := Tx(ctx, db, func(tx *sql.Tx) error {
		rep.Applied = nil
		for n := rep.From + 1; n <= len(steps); n++ {
			line, err := runStep(tx, n, steps[n-1])
			if err != nil {
				return err
			}
			rep.Applied = append(rep.Applied, line)
		}
		return errDone
	})
	if err != errDone {
		return rep, err
	}
	rep.To = len(steps)
	return rep, nil
}

func snapshot(ctx context.Context, db *sql.DB, from int, opts MigrateOptions) (string, error) {
	if err := os.MkdirAll(opts.SnapshotDir, 0o700); err != nil {
		return "", err
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	name := fmt.Sprintf("%s-v%d-%d-%d-%s.db", opts.Name, from, time.Now().UnixNano(), os.Getpid(), now().UTC().Format(snapshotStamp))
	path := filepath.Join(opts.SnapshotDir, name)
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return "", fmt.Errorf("sqlite: snapshot %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, prune(opts)
}

// prune keeps the newest Keep snapshots and never removes the last copy of a version.
func prune(opts MigrateOptions) error {
	keep := opts.Keep
	if keep <= 0 {
		keep = defaultKeep
	}
	ents, err := os.ReadDir(opts.SnapshotDir)
	if err != nil {
		return err
	}
	type snap struct{ file, ver, stamp string }
	var all []snap
	perVer := map[string]int{}
	for _, e := range ents {
		m := snapshotRE.FindStringSubmatch(e.Name())
		if m == nil || !strings.HasPrefix(e.Name(), opts.Name+"-v") {
			continue
		}
		all = append(all, snap{e.Name(), m[1], m[2]})
		perVer[m[1]]++
	}
	sort.Slice(all, func(i, j int) bool { return all[i].stamp > all[j].stamp })
	for i := len(all) - 1; i >= keep; i-- {
		s := all[i]
		if perVer[s.ver] <= 1 {
			continue
		}
		if err := os.Remove(filepath.Join(opts.SnapshotDir, s.file)); err != nil && !errors.Is(err, os.ErrNotExist) { // a peer pruned it
			return err
		}
		perVer[s.ver]--
	}
	return nil
}
