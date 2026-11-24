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

// bornFile holds each dated plan's first-write time as "path\tunix", first line wins; atomic writes reset birth times.
func bornFile() string { return filepath.Join(paths.StateDir(), "plans-born.tsv") }

// planBorn is plan's first-write time (record, else file birth), never before its day; record pins an unrecorded one.
func planBorn(plan string, record bool) time.Time {
	var day time.Time
	if datedName().MatchString(filepath.Base(plan)) {
		day, _ = time.ParseInLocation(time.DateOnly, filepath.Base(plan)[:10], time.Local)
	}
	born, ok := recordedBorn(plan)
	if !ok {
		born = time.Now()
		if fi, err := os.Stat(plan); err == nil {
			born = fileBirth(fi)
		}
		if record && !day.IsZero() {
			appendBorn(plan, born)
		}
	}
	if born.Before(day) {
		return day
	}
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

// appendBorn is best effort: a lost record only falls back to the file's own, later-or-equal, time.
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
