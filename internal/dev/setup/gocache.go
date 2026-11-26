package setup

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/textutil"
)

// GoCacheCap is the GOCACHE size past which `clean` trims it to half (Go trims only after 5 idle days).
var GoCacheCap int64 = 50 << 30

// GoCacheDir is `go env GOCACHE`, "" when go is missing or fails.
func GoCacheDir() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "env", "GOCACHE").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// goCacheBudget bounds the doctor's walk of the cache dir.
const goCacheBudget = 2 * time.Second

// goCacheSize sums dir; with bounded it stops past GoCacheCap or after goCacheBudget (done=false).
func goCacheSize(dir string, bounded bool) (size int64, done bool) {
	deadline := time.Now().Add(goCacheBudget)
	done = true
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree contributes 0
		}
		if info, err := d.Info(); err == nil {
			size += info.Size()
		}
		if bounded && (size > GoCacheCap || time.Now().After(deadline)) {
			done = size > GoCacheCap
			return fs.SkipAll
		}
		return nil
	})
	return size, done
}

// GoCacheDetail is the doctor row: dir, size and cap; a bounded walk says ">cap" or "large, not measured".
func GoCacheDetail() (detail string, ok bool) {
	dir := GoCacheDir()
	if dir == "" {
		return "go not found", true
	}
	size, done := goCacheSize(dir, true)
	cp := textutil.HumanBytes(GoCacheCap)
	switch {
	case size > GoCacheCap:
		return fmt.Sprintf("%s >%s (cap %s; make sweep)", dir, cp, cp), false
	case !done:
		return dir + " large, not measured (cap " + cp + ")", true
	}
	return fmt.Sprintf("%s %s (cap %s)", dir, textutil.HumanBytes(size), cp), true
}

// goCacheMinAge keeps entries a live build may still read; Go touches cache entries on use.
const goCacheMinAge = time.Hour

type cacheFile struct {
	path  string
	size  int64
	mtime time.Time
}

// trimGoCache deletes files older than minAge, oldest first, until the total is <= target; returns bytes freed.
func trimGoCache(dir string, total, target int64, minAge time.Duration, apply bool) (freed int64, failed int) {
	cutoff := time.Now().Add(-minAge)
	var files []cacheFile
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree contributes 0
		}
		switch d.Name() {
		case "README", "trim.txt", "testexpire.txt":
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().Before(cutoff) {
			files = append(files, cacheFile{p, info.Size(), info.ModTime()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	for _, f := range files {
		if total-freed <= target {
			break
		}
		if apply && os.Remove(f.path) != nil {
			failed++
			continue
		}
		freed += f.size
	}
	return freed, failed
}

// sweepGoCache trims the Go build cache to half of GoCacheCap once over it; never `go clean -cache` (live builds).
func sweepGoCache(errw io.Writer, apply bool, tot *sweepTotals) {
	dir := GoCacheDir()
	if dir == "" {
		return
	}
	size, _ := goCacheSize(dir, false)
	if size <= GoCacheCap {
		return
	}
	freed, failed := trimGoCache(dir, size, GoCacheCap/2, goCacheMinAge, apply)
	tot.failed += failed
	if freed == 0 {
		return
	}
	tot.files++
	tot.bytes += freed
	fmt.Fprintf(errw, "%-16s %s %s (%s over the %s cap, %s freed)\n", "go-cache", removeVerb(apply), dir,
		textutil.HumanBytes(size), textutil.HumanBytes(GoCacheCap), textutil.HumanBytes(freed))
}
