package plans

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// bornFile holds each dated plan's first-write time as "path\tunix", first line wins; rewrites move the mtime.
func bornFile() string { return filepath.Join(paths.StateDir(), "plans-born.tsv") }

// RecordBorn pins a dated plan's first-write time when it has no record yet; a Write tool call bypasses writePlan.
func RecordBorn(plan string) { planBorn(plan, true) }

// planBorn is plan's first-write time (record, else mtime of a non-empty file), never before its day; record pins it.
func planBorn(plan string, record bool) time.Time {
	key := paths.CanonDeep(plan)
	if born, ok := recordedBorn(key); ok {
		return born
	}
	born := time.Now()
	if fi, err := os.Stat(plan); err == nil && fi.Size() > 0 {
		born = fi.ModTime() // an atomic rewrite moves mtime later, which can only skip a tick
	}
	if !datedName().MatchString(filepath.Base(plan)) {
		return born
	}
	if day, err := time.ParseInLocation(time.DateOnly, filepath.Base(plan)[:10], time.Local); err == nil && born.Before(day) {
		born = day
	}
	if record {
		return pinBorn(key, born)
	}
	return born
}

// pinBorn appends key's born time under a lock unless a record exists, and returns the winning one.
func pinBorn(key string, born time.Time) time.Time {
	if !datedName().MatchString(filepath.Base(key)) {
		return born
	}
	_ = paths.WithFileLock(bornFile(), func() error {
		if won, ok := recordedBorn(key); ok {
			born = won
		} else {
			appendBorn(key, born)
		}
		return nil
	})
	return born
}

func recordedBorn(plan string) (time.Time, bool) {
	f, err := os.Open(bornFile())
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	for sc := bufio.NewScanner(f); sc.Scan(); {
		path, unix, ok := strings.Cut(sc.Text(), "\t")
		if !ok || path != plan {
			continue
		}
		if n, err := strconv.ParseInt(unix, 10, 64); err == nil {
			return time.Unix(n, 0), true
		}
	}
	return time.Time{}, false
}

// appendBorn is best effort: a lost record only falls back to the later-or-equal mtime.
func appendBorn(plan string, born time.Time) {
	if strings.ContainsAny(plan, "\t\n") || os.MkdirAll(paths.StateDir(), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(bornFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s\t%d\n", plan, born.Unix())
}
