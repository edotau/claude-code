package setup

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

// GoCacheCap is the GOCACHE size past which `clean` runs `go clean -cache` (Go trims only after 5 idle days).
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
		return fmt.Sprintf("%s >%s (cap %s; make sweep APPLY=1)", dir, cp, cp), false
	case !done:
		return dir + " large, not measured (cap " + cp + ")", true
	}
	return fmt.Sprintf("%s %s (cap %s)", dir, textutil.HumanBytes(size), cp), true
}

// sweepGoCache clears the whole Go build cache when it is over GoCacheCap; the cache is rebuildable, so age is moot.
func sweepGoCache(errw io.Writer, apply bool, tot *sweepTotals) {
	dir := GoCacheDir()
	if dir == "" {
		return
	}
	size, _ := goCacheSize(dir, false)
	if size <= GoCacheCap {
		return
	}
	if apply {
		if out, err := exec.CommandContext(context.Background(), "go", "clean", "-cache").CombinedOutput(); err != nil {
			fmt.Fprintf(errw, "%-16s go clean -cache failed: %v: %s\n", "go-cache", err, strings.TrimSpace(string(out)))
			tot.failed++
			return
		}
	}
	tot.files++
	tot.bytes += size
	fmt.Fprintf(errw, "%-16s %s %s (%s over the %s cap)\n", "go-cache", removeVerb(apply), dir,
		textutil.HumanBytes(size), textutil.HumanBytes(GoCacheCap))
}
