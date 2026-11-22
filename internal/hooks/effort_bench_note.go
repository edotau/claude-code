package hooks

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

const maxBenchPackages = 3

var (
	benchOwnedRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(?:owned(?:\s+files)?|owns):[ \t]*([^\r\n]+)`)
	})
	benchParenRe   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\([^)]*\)`) })
	benchFuncRe    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^func Benchmark`) })
	benchStopWords = []string{"and", "or", "only", "none"}
)

type benchDirCache struct {
	mu      sync.Mutex
	entries map[string]benchDirStamp
}

type benchDirStamp struct {
	names  []string
	mtimes []int64
	sizes  []int64
	has    bool
}

var pkgBenchCache = &benchDirCache{entries: map[string]benchDirStamp{}}

// benchNote lists up to 3 benched Go packages for the brief's owned paths in normal and deferred modes.
func (e *effortPlan) benchNote(cwd, task string) string {
	if e.mode == modeQuery {
		return ""
	}
	var lines []string
	for _, pkg := range benchPackages(cwd, task) {
		dashed := strings.ReplaceAll(strings.TrimPrefix(pkg, "internal/"), "/", "-")
		lines = append(lines, fmt.Sprintf(
			"perf: %s has benchmarks; run rtk bypass go test -run ^$ -bench . -benchmem -count=10 ./%s/ before and after, compare with benchstat against state/harness/bench/%s/rolling.txt, and report the table",
			pkg, pkg, dashed))
	}
	return strings.Join(lines, "\n")
}

// benchPackages returns up to maxBenchPackages sorted repo-relative Go package directories with benchmarks.
func benchPackages(cwd, task string) []string {
	tokens := parseBriefOwned(task)
	if len(tokens) == 0 {
		return nil
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	baseTop, _ := paths.CheckoutOf(cwd)
	seen := map[string]bool{}
	var pkgs []string
	for _, tok := range tokens {
		dir, top := resolveOwnedPkgDir(cwd, baseTop, tok)
		if rel, ok := paths.RelWithin(top, dir); ok && rel != "." {
			if pkg := filepath.ToSlash(rel); !seen[pkg] && dirHasBenchmarks(dir) {
				seen[pkg] = true
				pkgs = append(pkgs, pkg)
			}
		}
	}
	slices.Sort(pkgs)
	return pkgs[:min(len(pkgs), maxBenchPackages)]
}

// parseBriefOwned extracts owned file/dir tokens from Owned:, Owned files:, or owns: segments in task.
func parseBriefOwned(task string) []string {
	var out []string
	for _, m := range benchOwnedRe().FindAllStringSubmatch(task, -1) {
		raw := m[1]
		for _, sep := range []string{" — ", " -- ", ". "} {
			raw, _, _ = strings.Cut(raw, sep)
		}
		raw = strings.TrimSpace(benchParenRe().ReplaceAllString(raw, ""))
		if strings.HasPrefix(raw, "[") {
			var owned []string
			_ = json.Unmarshal([]byte(raw), &owned)
			out = append(out, owned...)
			continue
		}
		for _, part := range textutil.SplitCSV(raw) {
			for f := range strings.FieldsSeq(part) {
				f = strings.Trim(f, "`\"'.,;:")
				if f != "" && !slices.Contains(benchStopWords, strings.ToLower(f)) {
					out = append(out, f)
				}
			}
		}
	}
	return out
}

// resolveOwnedPkgDir maps an owned token to its existing Go package directory and enclosing checkout root.
func resolveOwnedPkgDir(cwd, baseTop, tok string) (dir, top string) {
	p := paths.AnchorPath(paths.ExpandHome(tok), cmp.Or(baseTop, cwd))
	if p == "" {
		return "", ""
	}
	dir = filepath.Clean(p)
	if !paths.IsDir(dir) {
		if strings.HasSuffix(tok, "/") || !paths.IsDir(filepath.Dir(dir)) {
			return "", ""
		}
		dir = filepath.Dir(dir)
	}
	if baseTop != "" && strings.HasPrefix(dir+string(filepath.Separator), baseTop+string(filepath.Separator)) {
		return dir, baseTop
	}
	top, _ = paths.CheckoutOf(dir)
	return dir, top
}

// dirHasBenchmarks checks whether dir has a *_test.go file with a Benchmark function, cached by test file mtimes.
func dirHasBenchmarks(dir string) bool {
	st, ok := readBenchStamp(dir)
	if !ok {
		return false
	}
	pkgBenchCache.mu.Lock()
	cached, hit := pkgBenchCache.entries[dir]
	pkgBenchCache.mu.Unlock()
	if hit && slices.Equal(cached.names, st.names) && slices.Equal(cached.mtimes, st.mtimes) && slices.Equal(cached.sizes, st.sizes) {
		return cached.has
	}
	for _, name := range st.names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil && bytes.Contains(b, []byte("func Benchmark")) && benchFuncRe().Match(b) {
			st.has = true
			break
		}
	}
	pkgBenchCache.mu.Lock()
	pkgBenchCache.entries[dir] = st
	pkgBenchCache.mu.Unlock()
	return st.has
}

func readBenchStamp(dir string) (benchDirStamp, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return benchDirStamp{}, false
	}
	var st benchDirStamp
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.go") {
			if fi, err := e.Info(); err == nil {
				st.names = append(st.names, e.Name())
				st.mtimes = append(st.mtimes, fi.ModTime().UnixNano())
				st.sizes = append(st.sizes, fi.Size())
			}
		}
	}
	return st, len(st.names) > 0
}
