package paths

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiskTTLGet(t *testing.T) {
	d := &DiskTTL[string]{
		Name: "test.diskttl", Path: filepath.Join(t.TempDir(), "c.json"), TTL: time.Hour,
		Decode: func(b []byte) (string, error) { return string(b), nil },
		Encode: func(s string) ([]byte, error) { return []byte(s), nil },
	}
	var fetches atomic.Int32
	fetch := func(context.Context) (string, error) { fetches.Add(1); return "v", nil }
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { d.Get(context.Background(), fetch) })
	}
	wg.Wait()
	if n := fetches.Load(); n != 1 {
		t.Fatalf("concurrent Gets fetched %d times; want 1", n)
	}
	if v, err := d.Get(context.Background(), fetch); v != "v" || err != nil || fetches.Load() != 1 {
		t.Errorf("fresh Get = %q, %v after %d fetches; want cached", v, err, fetches.Load())
	}
}
