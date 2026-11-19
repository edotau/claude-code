package doctor

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	secretredact "github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

var transcriptIdle = 10 * time.Minute

type transcriptStore struct {
	name, root string
	disabled   bool
	accept     func(string) bool
}

type transcriptCounts struct {
	scanned, found, matches, masked, maskedMatches, failed int
	bytes                                                  int64
	skipped                                                map[string]int
}

func transcriptStores() []transcriptStore {
	cfg := resolvedPath(paths.ConfigDir())
	home, _ := os.UserHomeDir()
	taskRoot := filepath.Join(os.TempDir(), fmt.Sprintf("claude-%d", os.Getuid()))
	task := func(p string) bool {
		return filepath.Ext(p) == ".output" && strings.Contains(filepath.ToSlash(p), "/tasks/")
	}
	text := func(p string) bool {
		return slices.Contains([]string{".jsonl", ".json", ".log", ".txt", ".md", ".output"}, filepath.Ext(p))
	}
	project := func(p string) bool {
		return filepath.Ext(p) == ".jsonl" || filepath.Ext(p) == ".output" || strings.Contains(filepath.ToSlash(p), "/tool-results/")
	}
	return []transcriptStore{
		{"claude projects", filepath.Join(cfg, "projects"), false, project},
		{"claude history", filepath.Join(cfg, "history.jsonl"), false, text},
		{"task outputs", filepath.Join(cfg, "tasks"), false, text},
		{"temporary task outputs", taskRoot, false, task},
		{"dispatch", filepath.Join(cfg, "state", "harness", "dispatch"), false, text},
		{"saved results", filepath.Join(cfg, "state", "harness", "results"), false, text},
		{"proxy log", filepath.Join(cfg, "state", "harness", "proxy.log"), false, text},
		{"reports", filepath.Join(cfg, "memory", "reports"), false, text},
		{"codex sessions", filepath.Join(home, ".codex", "sessions"), true, text},
		{"copilot sessions", filepath.Join(home, ".copilot", "session-state"), true, text},
	}
}

// transcriptMatcher counts the same records the live hooks mask: a labeled cache (model listings) is no credential.
func transcriptMatcher() (*secretredact.Matcher, error) {
	records, err := secretredact.VaultRecords()
	if err != nil {
		return nil, err
	}
	return secretredact.WithEnvKey(secretredact.FromRecords(records)), nil
}

func transcriptAudit(w io.Writer, fix bool, targets []string, days int) int {
	ew := &textutil.ErrWriter{W: w}
	fmt.Fprintln(ew, "\n== transcript stores ==")
	m, err := transcriptMatcher()
	rc := 0
	if err != nil {
		m = secretredact.WithEnvKey(secretredact.FromValues(nil))
		fmt.Fprintln(ew, "  ⚠ vault unavailable: shapes only")
		if !errors.Is(err, vault.ErrLocked) {
			rc = 1
		}
	}
	fmt.Fprintf(ew, "  vault values: %d, skipped: %d\n", m.Values(), m.Skipped)
	for result := range transcriptResults(m, fix, targets, days) {
		printTranscriptCounts(ew, result.store, result.counts)
		printTranscriptTally(ew, result.tally)
		if result.counts.failed > 0 || (!fix && result.counts.matches > 0) {
			rc = 1
		}
	}
	fmt.Fprintln(ew, "  agy and OpenCode: skipped: SQLite out of scope")
	if ew.Err != nil {
		return 1
	}
	return rc
}

func printTranscriptCounts(w io.Writer, store transcriptStore, c transcriptCounts) {
	fmt.Fprintf(w, "  %s: scanned %d, with matches %d, matches %d, masked files %d, matches masked %d, bytes %d",
		store.name, c.scanned, c.found, c.matches, c.masked, c.maskedMatches, c.bytes)
	for _, reason := range []string{"older (include with --days 0)", "not yet enabled", "symlink or outside root", "not idle", "would not stay valid", "error"} {
		if n := c.skipped[reason]; n > 0 || (reason == "not yet enabled" && store.disabled) {
			fmt.Fprintf(w, ", skipped: %s %d", reason, n)
		}
	}
	fmt.Fprintln(w)
}

// tallyTop is how many vault needles and mask lengths the breakdown names, busiest first.
const tallyTop = 5

// printTranscriptTally prints one indented line per kind; vault needles by record, field, form and length, never bytes.
func printTranscriptTally(w io.Writer, t *secretredact.Tally) {
	for _, kind := range slices.Sorted(maps.Keys(t.Kinds)) {
		if kind != "vault" {
			fmt.Fprintf(w, "    %s: %d\n", kind, t.Kinds[kind])
		}
	}
	if n := t.Kinds["vault"]; n > 0 {
		fmt.Fprintf(w, "    vault value: %d from %d needles\n", n, len(t.Needles))
		origins := slices.SortedFunc(maps.Keys(t.Needles), func(a, b secretredact.Origin) int {
			return cmp.Or(t.Needles[b]-t.Needles[a], cmp.Compare(a.Record+a.Field+a.Form, b.Record+b.Field+b.Form))
		})
		for _, o := range origins[:min(tallyTop, len(origins))] {
			fmt.Fprintf(w, "      %s %q %s %d B: %d\n", o.Record, o.Field, o.Form, o.Len, t.Needles[o])
		}
	}
	if len(t.Masks) == 0 {
		return
	}
	lengths := slices.SortedFunc(maps.Keys(t.Masks), func(a, b int) int { return cmp.Or(t.Masks[b]-t.Masks[a], a-b) })
	total, parts := 0, []string{}
	for i, n := range lengths {
		total += t.Masks[n]
		if i < tallyTop {
			parts = append(parts, fmt.Sprintf("%d B: %d", n, t.Masks[n]))
		}
	}
	fmt.Fprintf(w, "    existing masks: %d (%s)\n", total, strings.Join(parts, ", "))
}

func transcriptSelected(path string, targets []string) bool {
	if len(targets) == 0 {
		return true
	}
	return slices.Contains(targets, path)
}

func scanTranscriptStore(s transcriptStore, counter *secretredact.Counter, fix bool, targets []string, cutoff time.Time, slots chan struct{}) transcriptCounts {
	c := transcriptCounts{skipped: map[string]int{}}
	var wg sync.WaitGroup
	results := make(chan transcriptCounts)
	total := collectTranscriptCounts(results)
	root, err := filepath.Abs(s.root)
	if err != nil {
		close(results)
		mergeTranscriptCounts(&c, <-total)
		transcriptFailure(&c, err)
		return c
	}
	root = resolvedPath(root) // a link leading to the store is the owner's layout; the walk and the fix stay below it
	s.root = root
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if !transcriptWalkEntry(&c, s, root, path, d, walkErr, targets) {
			return nil
		}
		slots <- struct{}{}
		wg.Go(func() {
			entry := transcriptCounts{skipped: map[string]int{}}
			scanTranscriptEntry(&entry, s, path, d, counter, fix, cutoff)
			<-slots
			results <- entry
		})
		return nil
	})
	if err != nil {
		c.failed++
		c.skipped["error"]++
	}
	wg.Wait()
	close(results)
	mergeTranscriptCounts(&c, <-total)
	return c
}

func scanTranscriptEntry(c *transcriptCounts, s transcriptStore, path string, d fs.DirEntry, counter *secretredact.Counter, fix bool, cutoff time.Time) {
	info, err := d.Info()
	if err != nil {
		c.failed++
		c.skipped["error"]++
		return
	}
	if !info.Mode().IsRegular() {
		return
	}
	if s.disabled {
		c.skipped["not yet enabled"]++
		c.bytes += info.Size()
		return
	}
	if !cutoff.IsZero() && info.ModTime().Before(cutoff) {
		c.skipped["older (include with --days 0)"]++
		return
	}
	resolved := path
	if fix {
		resolved, err = filepath.EvalSymlinks(path)
	}
	if err != nil || resolved != path {
		c.skipped["symlink or outside root"]++
		return
	}
	if fix && time.Since(info.ModTime()) < transcriptIdle {
		c.skipped["not idle"]++
		return
	}
	c.scanned++
	c.bytes += info.Size()
	hits, err := transcriptCountFile(s.root, path, counter, fix)
	if err != nil {
		transcriptFailure(c, err)
		return
	}
	recordTranscriptHits(c, hits, fix)
}

func recordTranscriptHits(c *transcriptCounts, hits int, fix bool) {
	if hits > 0 {
		c.found++
		c.matches += hits
	}
	if fix && hits > 0 {
		c.masked++
		c.maskedMatches += hits
	}
}

func transcriptWalkEntry(c *transcriptCounts, s transcriptStore, root, path string, d fs.DirEntry, err error, targets []string) bool {
	if errors.Is(err, fs.ErrNotExist) && path == root {
		return false
	}
	if err != nil {
		transcriptFailure(c, err)
		return false
	}
	if d.Type()&os.ModeSymlink != 0 {
		c.skipped["symlink or outside root"]++
		return false
	}
	return !d.IsDir() && s.accept(path) && transcriptSelected(path, targets)
}

func transcriptFailure(c *transcriptCounts, err error) {
	// The core currently exposes validation failure only as a sanitized error string.
	if strings.Contains(err.Error(), "invalid changed JSON line") || errors.Is(err, errTranscriptInvalid) {
		c.skipped["would not stay valid"]++
		return
	}
	c.failed++
	c.skipped["error"]++
}

var errTranscriptInvalid = errors.New("transcript would not stay valid")

// transcriptFile masks path under the store root; a one-file store is opened from its directory.
func transcriptFile(root, path string, m *secretredact.Matcher, fix bool) (int, error) {
	if !fix || filepath.Ext(path) == ".json" {
		b, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		hits := m.Mask(b)
		if !fix {
			return hits, nil
		}
		if hits > 0 && !json.Valid(b) {
			return 0, errTranscriptInvalid
		}
	}
	if path == root {
		root = filepath.Dir(root)
	}
	return secretredact.File(root, path, m, transcriptIdle)
}

type transcriptResult struct {
	store  transcriptStore
	counts transcriptCounts
	tally  *secretredact.Tally
}

func transcriptResults(m *secretredact.Matcher, fix bool, targets []string, days int) <-chan transcriptResult {
	results := make(chan transcriptResult)
	cutoff := time.Time{}
	if days > 0 {
		cutoff = time.Now().AddDate(0, 0, -days)
	}
	resolved := make([]string, 0, len(targets))
	for _, target := range targets {
		if absolute, err := filepath.Abs(target); err == nil {
			resolved = append(resolved, resolvedPath(absolute))
		}
	}
	workers := max(1, min(4, runtime.GOMAXPROCS(0)/2))
	if fix {
		workers = 1
	}
	slots := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, store := range transcriptStores() {
		wg.Go(func() {
			counter := secretredact.NewCounter(m)
			counter.Tally = secretredact.NewTally()
			results <- transcriptResult{store, scanTranscriptStore(store, counter, fix, resolved, cutoff, slots), counter.Tally}
		})
	}
	go func() { wg.Wait(); close(results) }()
	return results
}

func collectTranscriptCounts(results <-chan transcriptCounts) <-chan transcriptCounts {
	total := make(chan transcriptCounts, 1)
	go func() {
		c := transcriptCounts{skipped: map[string]int{}}
		for entry := range results {
			mergeTranscriptCounts(&c, entry)
		}
		total <- c
	}()
	return total
}

func mergeTranscriptCounts(c *transcriptCounts, entry transcriptCounts) {
	c.scanned += entry.scanned
	c.found += entry.found
	c.matches += entry.matches
	c.masked += entry.masked
	c.maskedMatches += entry.maskedMatches
	c.failed += entry.failed
	c.bytes += entry.bytes
	for reason, n := range entry.skipped {
		c.skipped[reason] += n
	}
}

func transcriptCountFile(root, path string, counter *secretredact.Counter, fix bool) (int, error) {
	if fix {
		return transcriptFile(root, path, counter.Matcher(), true)
	}
	return counter.File(path)
}
