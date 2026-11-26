package paths

import (
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"
)

// RacyWindow excludes recent modification or change times that may hide a same-tick rewrite.
const RacyWindow = 2 * time.Second

var stampNow = time.Now

// SetStampNowForTest replaces the stamp clock and returns its restore; callers must not run in parallel.
func SetStampNowForTest(now func() time.Time) func() {
	old := stampNow
	stampNow = now
	return func() { stampNow = old }
}

// stamp identifies a file by presence, modification time, size, inode and change time.
type stamp struct {
	present bool
	mtime   int64
	size    int64
	ino     uint64
	ctime   int64
}

// StampCache memoizes a file-derived value while its stamp holds; errors and racy reads are never kept.
type StampCache[T any] struct {
	mu sync.Mutex
	m  map[string]stampEntry[T]

	stats *cacheStats
}

// NewStampCache returns a StampCache whose counts show in CacheStats under name.
func NewStampCache[T any](name string) *StampCache[T] {
	return &StampCache[T]{stats: registerStats(name)}
}

type stampEntry[T any] struct {
	stamp stamp
	val   T
}

// Get returns the value for path, calling load unless the file's stamp matches a settled earlier read.
func (c *StampCache[T]) Get(path string, load func() (T, error)) (T, error) {
	now := stampNow()
	st, statErr := statStamp(path)
	if statErr == nil {
		c.mu.Lock()
		e, ok := c.m[path]
		c.mu.Unlock()
		if ok && e.stamp == st {
			c.stats.hit()
			return e.val, nil
		}
		c.countMiss(ok)
	} else {
		c.stats.miss()
	}
	val, err := load()
	if err != nil {
		return val, err
	}
	after, afterErr := statStamp(path)
	if statErr == nil && afterErr == nil && st == after && settled(after, now) {
		c.mu.Lock()
		if c.m == nil {
			c.m = map[string]stampEntry[T]{}
		}
		c.m[path] = stampEntry[T]{stamp: st, val: val}
		c.mu.Unlock()
	}
	return val, nil
}

func (c *StampCache[T]) countMiss(replaced bool) {
	c.stats.miss()
	if replaced {
		c.stats.refresh()
	}
}

// statStamp reads path's stamp; a missing file is the absent stamp, any other stat error is returned.
func statStamp(path string) (stamp, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return stamp{}, nil
	}
	if err != nil {
		return stamp{}, err
	}
	ino, ctime := statIdentity(fi)
	return stamp{present: true, mtime: fi.ModTime().UnixNano(), size: fi.Size(), ino: ino, ctime: ctime}, nil
}

func settled(st stamp, now time.Time) bool {
	return !st.present || max(st.mtime, st.ctime) <= now.Add(-RacyWindow).UnixNano()
}
