package paths

import (
	"testing"
	"time"
)

func TestCacheStatsCounts(t *testing.T) {
	m := NewMemo[string, int]("test.stats.memo")
	keep := func(int, error) time.Duration { return 0 }
	fn := func() (int, error) { return 1, nil }
	m.Do("a", keep, fn)
	m.Do("a", keep, fn)
	var got CacheStat
	for _, s := range CacheStats() {
		if s.Name == "test.stats.memo" {
			got = s
		}
	}
	if got.Hits != 1 || got.Misses != 1 {
		t.Errorf("stats = %+v; want 1 hit, 1 miss", got)
	}
}
