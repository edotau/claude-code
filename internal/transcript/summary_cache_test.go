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
			err = agg.AddFile(f)
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

func TestSummaryCacheMatchesUncachedAcrossColdWarmAndChange(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	files := cacheFixtures(t)
	cachePath := filepath.Join(t.TempDir(), "transcript", "summary.json")

	want := aggregate(t, nil, files)
	if want.PeakInputTokensPerMin == 0 {
		t.Fatal("baseline peak is 0: fixtures carry no per-minute input")
	}

	cold := OpenSummaryCache(cachePath)
	if got := aggregate(t, cold, files); !reflect.DeepEqual(got, want) {
		t.Errorf("cold aggregate = %+v, want %+v", got, want)
	}
	if h, m := cold.Stats(); h != 0 || m != len(files) {
		t.Errorf("cold stats = %d hits, %d misses; want 0, %d", h, m, len(files))
	}

	warm := OpenSummaryCache(cachePath)
	if got := aggregate(t, warm, files); !reflect.DeepEqual(got, want) {
		t.Errorf("warm aggregate = %+v (peak %d), want %+v (peak %d)", got, got.PeakInputTokensPerMin, want, want.PeakInputTokensPerMin)
	}
	if h, m := warm.Stats(); h != len(files) || m != 0 {
		t.Errorf("warm stats = %d hits, %d misses; want %d, 0", h, m, len(files))
	}

	// Rewrite one file, back-dated: only it misses and its new content shows up.
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(raw, []byte(`"input_tokens":100,`), []byte(`"input_tokens":101,`), 1) // same size: only mtime differs
	if bytes.Equal(edited, raw) {
		t.Fatal("fixture has no input_tokens:100 to edit")
	}
	if err := os.WriteFile(files[0], edited, 0o600); err != nil {
		t.Fatal(err)
	}
	backdate(t, files[0])
	changed := OpenSummaryCache(cachePath)
	got := aggregate(t, changed, files)
	if h, m := changed.Stats(); h != len(files)-1 || m != 1 {
		t.Errorf("after one change stats = %d hits, %d misses; want %d, 1", h, m, len(files)-1)
	}
	if fresh := aggregate(t, nil, files); !reflect.DeepEqual(got, fresh) {
		t.Errorf("post-change cached aggregate = %+v, want uncached %+v", got, fresh)
	}
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
