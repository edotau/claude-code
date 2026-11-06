package paths

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func stampCacheFixture(t *testing.T, mtime time.Time) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "cached")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func advanceStampClock(t *testing.T) {
	t.Helper()
	orig := stampNow
	stampNow = func() time.Time { return time.Now().Add(time.Hour) }
	t.Cleanup(func() { stampNow = orig })
}

func TestStampCacheSettledFile(t *testing.T) {
	advanceStampClock(t)
	// Given a file whose mtime is settled.
	path := stampCacheFixture(t, time.Now().Add(-time.Hour))
	var cache StampCache[int]
	calls := 0
	load := func() (int, error) { calls++; return calls, nil }
	// When the same file is requested twice.
	for range 2 {
		got, err := cache.Get(path, load)
		if err != nil || got != 1 {
			t.Fatalf("Get = %d, %v; want 1, nil", got, err)
		}
	}
	// Then the second request reuses the loaded value.
	if calls != 1 {
		t.Fatalf("load calls = %d, want 1", calls)
	}
}

func TestStampCacheSizeChange(t *testing.T) {
	advanceStampClock(t)
	// Given a cached settled file.
	old := time.Now().Add(-time.Hour)
	path := stampCacheFixture(t, old)
	var cache StampCache[int]
	calls := 0
	load := func() (int, error) { calls++; return calls, nil }
	if got, err := cache.Get(path, load); err != nil || got != 1 {
		t.Fatalf("initial Get = %d, %v; want 1, nil", got, err)
	}
	// When a rewrite changes only its size, keeping the settled mtime identical.
	if err := os.WriteFile(path, []byte("longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get(path, load)
	// Then the loader runs again rather than returning the stale value.
	if err != nil || got != 2 || calls != 2 {
		t.Fatalf("Get = %d, %v; load calls = %d, want 2, nil, 2", got, err, calls)
	}
}

func TestStampCacheSameSizePreservedMtime(t *testing.T) {
	advanceStampClock(t)
	// Given a cached settled file.
	old := time.Now().Add(-time.Hour)
	path := stampCacheFixture(t, old)
	var cache StampCache[int]
	calls := 0
	load := func() (int, error) { calls++; return calls, nil }
	if _, err := cache.Get(path, load); err != nil {
		t.Fatal(err)
	}
	// When it is rewritten with same-size contents and its mtime restored (cp -p, rsync -t, a backup restore).
	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	// Then the changed ctime forces a reload instead of serving the old value.
	if got, err := cache.Get(path, load); err != nil || got != 2 {
		t.Fatalf("Get = %d, %v; want 2, nil (stale value served after same-size rewrite)", got, err)
	}
}

func TestStampCacheLoadErrorNotCached(t *testing.T) {
	advanceStampClock(t)
	// Given a settled file whose first load fails.
	path := stampCacheFixture(t, time.Now().Add(-time.Hour))
	var cache StampCache[int]
	calls := 0
	wantErr := errors.New("load failed")
	load := func() (int, error) {
		calls++
		if calls == 1 {
			return 0, wantErr
		}
		return calls, nil
	}
	if _, err := cache.Get(path, load); !errors.Is(err, wantErr) {
		t.Fatalf("first Get error = %v, want %v", err, wantErr)
	}
	// When the same file is requested after the error.
	got, err := cache.Get(path, load)
	// Then the failed load was not cached.
	if err != nil || got != 2 || calls != 2 {
		t.Fatalf("retry Get = %d, %v; load calls = %d, want 2, nil, 2", got, err, calls)
	}
}

func TestStampCacheFreshFile(t *testing.T) {
	// Given a file with a fresh mtime inside the racy window.
	path := stampCacheFixture(t, time.Now())
	var cache StampCache[int]
	calls := 0
	load := func() (int, error) { calls++; return calls, nil }
	// When the unchanged file is requested twice.
	for want := 1; want <= 2; want++ {
		got, err := cache.Get(path, load)
		if err != nil || got != want {
			t.Fatalf("Get = %d, %v; want %d, nil", got, err, want)
		}
	}
	// Then every request reloads instead of trusting the fresh stamp.
	if calls != 2 {
		t.Fatalf("load calls = %d, want 2", calls)
	}
}
