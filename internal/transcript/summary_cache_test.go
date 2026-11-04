package transcript

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// cacheFixtures copies the usage fixtures into a temp dir, back-dated past the cache's settle window.
func cacheFixtures(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	var files []string
	for _, n := range []string{"usage_a.jsonl", "usage_b.jsonl", "usage_dedup.jsonl"} {
		raw, err := os.ReadFile(filepath.Join("testdata", n))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		backdate(t, p)
		files = append(files, p)
	}
	return files
}

func backdate(t *testing.T, p string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

// aggregate sums files, through c when non-nil, and saves the cache.
func aggregate(t *testing.T, c *SummaryCache, files []string) *UsageAggregate {
	t.Helper()
	agg := NewUsageAggregate()
	for _, f := range files {
		var err error
		if c == nil {
			err = addFile(agg, f)
		} else {
			err = agg.AddFileCached(c, f)
		}
		if err != nil {
			t.Fatalf("add %s: %v", f, err)
		}
	}
	if c != nil {
		if err := c.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	return agg
}

// wantCacheRun aggregates files through the cache at cachePath and checks the result and hit/miss counts.
func wantCacheRun(t *testing.T, label, cachePath string, files []string, want *UsageAggregate, hits, misses int) *UsageAggregate {
	t.Helper()
	c := OpenSummaryCache(cachePath)
	got := aggregate(t, c, files)
	if want != nil && !reflect.DeepEqual(got, want) {
		t.Errorf("%s aggregate = %+v (peak %d), want %+v (peak %d)", label, got, got.PeakInputTokensPerMin, want, want.PeakInputTokensPerMin)
	}
	if h, m := c.Stats(); h != hits || m != misses {
		t.Errorf("%s stats = %d hits, %d misses; want %d, %d", label, h, m, hits, misses)
	}
	return got
}

func TestSummaryCacheMatchesUncachedAcrossColdWarmAndChange(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	files := cacheFixtures(t)
	cachePath := filepath.Join(t.TempDir(), "transcript", "summary.json")

	want := aggregate(t, nil, files)
	if want.PeakInputTokensPerMin == 0 {
		t.Fatal("baseline peak is 0: fixtures carry no per-minute input")
	}
	wantCacheRun(t, "cold", cachePath, files, want, 0, len(files))
	wantCacheRun(t, "warm", cachePath, files, want, len(files), 0)

	// Rewrite one file, back-dated: only it misses and its new content shows up.
	editFixture(t, files[0])
	got := wantCacheRun(t, "after one change", cachePath, files, nil, len(files)-1, 1)
	if fresh := aggregate(t, nil, files); !reflect.DeepEqual(got, fresh) {
		t.Errorf("post-change cached aggregate = %+v, want uncached %+v", got, fresh)
	}
}

// editFixture bumps a token count in path without changing its size, then back-dates it.
func editFixture(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(raw, []byte(`"input_tokens":100,`), []byte(`"input_tokens":101,`), 1) // same size: only mtime differs
	if bytes.Equal(edited, raw) {
		t.Fatal("fixture has no input_tokens:100 to edit")
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	backdate(t, path)
}

func TestSummaryCacheCorruptFileRefills(t *testing.T) {
	files := cacheFixtures(t)
	cachePath := filepath.Join(t.TempDir(), "summary.json")
	if err := os.WriteFile(cachePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := OpenSummaryCache(cachePath)
	got := aggregate(t, c, files)
	if h, m := c.Stats(); h != 0 || m != len(files) {
		t.Errorf("corrupt-cache stats = %d hits, %d misses; want 0, %d", h, m, len(files))
	}
	if want := aggregate(t, nil, files); !reflect.DeepEqual(got, want) {
		t.Errorf("aggregate = %+v, want %+v", got, want)
	}
	warm := OpenSummaryCache(cachePath)
	aggregate(t, warm, files)
	if h, _ := warm.Stats(); h != len(files) {
		t.Errorf("refilled cache served %d hits, want %d", h, len(files))
	}
}

func TestSummaryCacheNeverStoresRecentFile(t *testing.T) {
	files := cacheFixtures(t)
	now := time.Now()
	if err := os.Chtimes(files[0], now, now); err != nil { // still being appended to
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "summary.json")
	aggregate(t, OpenSummaryCache(cachePath), files)
	warm := OpenSummaryCache(cachePath)
	aggregate(t, warm, files)
	if h, m := warm.Stats(); h != len(files)-1 || m != 1 {
		t.Errorf("stats = %d hits, %d misses; want %d, 1 (recent file must re-read every run)", h, m, len(files)-1)
	}
}

func TestSummaryCacheKeepsUnvisitedRowsAndPrunesGoneFiles(t *testing.T) {
	files := cacheFixtures(t)
	cachePath := filepath.Join(t.TempDir(), "summary.json")
	aggregate(t, OpenSummaryCache(cachePath), files)
	aggregate(t, OpenSummaryCache(cachePath), files[:1]) // a narrow run, like usage --session
	warm := OpenSummaryCache(cachePath)
	aggregate(t, warm, files)
	if h, m := warm.Stats(); h != len(files) || m != 0 {
		t.Errorf("stats = %d hits, %d misses; want %d, 0 (narrow run must keep unvisited rows)", h, m, len(files))
	}
	if err := os.Remove(files[2]); err != nil {
		t.Fatal(err)
	}
	aggregate(t, OpenSummaryCache(cachePath), files[:1])
	if n := len(OpenSummaryCache(cachePath).old); n != len(files)-1 {
		t.Errorf("cache holds %d rows, want %d (deleted file's row must be pruned)", n, len(files)-1)
	}
}
